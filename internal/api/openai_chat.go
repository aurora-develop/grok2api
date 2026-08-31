package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/aurora-develop/grok2api/internal/account"
	"github.com/aurora-develop/grok2api/internal/config"
	"github.com/aurora-develop/grok2api/internal/grok"
	"github.com/aurora-develop/grok2api/internal/model"
	"github.com/aurora-develop/grok2api/internal/platform"
)

// chatCompletionRequest is the OpenAI-compatible chat request body.
type chatCompletionRequest struct {
	Model             string           `json:"model"`
	Messages          []map[string]any `json:"messages"`
	Stream            *bool            `json:"stream,omitempty"`
	ReasoningEffort   *string          `json:"reasoning_effort,omitempty"`
	Temperature       *float64         `json:"temperature,omitempty"`
	TopP              *float64         `json:"top_p,omitempty"`
	ImageConfig       *imageConfig     `json:"image_config,omitempty"`
	VideoConfig       *videoConfig     `json:"video_config,omitempty"`
	Tools             []map[string]any `json:"tools,omitempty"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	MaxTokens         *int             `json:"max_tokens,omitempty"`
}

type imageConfig struct {
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type videoConfig struct {
	Seconds        int    `json:"seconds,omitempty"`
	Size           string `json:"size,omitempty"`
	ResolutionName string `json:"resolution_name,omitempty"`
	Preset         string `json:"preset,omitempty"`
}

// handleChatCompletions dispatches by capability.
func (s *Server) handleChatCompletions(c *gin.Context) {
	var req chatCompletionRequest
	if err := readJSON(c, &req); err != nil {
		writeAppError(c, err)
		return
	}
	spec, ok := model.Resolve(req.Model)
	if !ok {
		writeAppError(c, platform.ValidationErrorCode("Model '"+req.Model+"' not found", "model", "model_not_found"))
		return
	}
	stream := config.Global().GetBool("features.stream", true)
	if req.Stream != nil {
		stream = *req.Stream
	}

	switch {
	case spec.IsConsoleChat():
		s.runConsoleChatWithRetry(c, &req, spec, stream)
	case spec.IsImageEdit():
		s.runGrokChatWithRetry(c, &req, spec, stream)
	case spec.IsImage():
		if grok.IsWSImageModel(spec.ModelName) {
			s.runWSImageChat(c, &req, spec, stream)
		} else {
			s.runGrokChatWithRetry(c, &req, spec, stream)
		}
	case spec.IsVideo():
		s.runGrokChatWithRetry(c, &req, spec, stream)
	default:
		s.runGrokChatWithRetry(c, &req, spec, stream)
	}
}

// runGrokChatWithRetry handles retry + account selection for grok.com chat.
// Falls back to the Bearer SSO token from the request when the pool is empty.
func (s *Server) runGrokChatWithRetry(c *gin.Context, req *chatCompletionRequest, spec *model.Spec, stream bool) {
	temp := 0.8
	if req.Temperature != nil {
		temp = *req.Temperature
	}
	topP := 0.95
	if req.TopP != nil {
		topP = *req.TopP
	}
	emitThink := resolveEmitThink(req.ReasoningEffort)
	message, fileInputs, perr := extractMessages(req.Messages)
	if perr != nil {
		writeAppError(c, perr)
		return
	}

	apiToken, _ := c.Get("api_token")
	ssoToken, _ := apiToken.(string)

	maxRetries := selectionMaxRetries()
	exclude := []string{}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		lease, _ := reserveAccount(c.Request.Context(), s.Directory, spec, exclude)
		if lease == nil {
			if s.Refresh != nil {
				_ = s.Refresh.RefreshOnDemand(c.Request.Context())
				lease, _ = reserveAccount(c.Request.Context(), s.Directory, spec, exclude)
			}
		}
		// Pool exhausted — fall back to the SSO token from Authorization header.
		if lease == nil && ssoToken != "" {
			lease = &account.Lease{Token: ssoToken, ModeID: int(spec.ModeId)}
		}
		if lease == nil {
			writeAppError(c, platform.RateLimitError("No available accounts"))
			return
		}
		exclude = append(exclude, lease.Token)
		err := s.runGrokChatOnce(c.Writer, c.Request, lease, spec, message, fileInputs, temp, topP, emitThink, stream, req.Model)
		s.Directory.Release(lease)
		if err == nil {
			s.feedback(lease.Token, account.FbSuccess, lease.ModeID, nil, nil)
			return
		}
		s.feedbackError(lease.Token, err, lease.ModeID)
		lastErr = err
		if !shouldRetryUpstream(err) || attempt == maxRetries {
			writeAppError(c, err)
			return
		}
	}
	if lastErr != nil {
		writeAppError(c, lastErr)
	}
}

// runGrokChatOnce executes one chat attempt against grok.com.
// For follow-up messages it uses /responses endpoint with conversation tracking.
func (s *Server) runGrokChatOnce(w http.ResponseWriter, r *http.Request, lease *account.Lease, spec *model.Spec, message string, fileInputs []string, temp, topP float64, emitThink, stream bool, modelName string) error {
	if usesGatewayChat(modelName) {
		return s.runGatewayChatOnce(w, r, lease, message, fileInputs, emitThink, stream, modelName)
	}
	// Check if we have an active conversation for this token.
	convCtx := s.ConvTracker.Get(lease.Token)
	var (
		payload   map[string]any
		targetURL string
		isNew     bool
	)
	if convCtx != nil && convCtx.ConversationID != "" && convCtx.LastResponseID != "" {
		// Follow-up message in existing conversation.
		payload = grok.BuildResponsePayload(message, convCtx.LastResponseID, model.ModeId(lease.ModeID), fileInputs, nil, nil)
		targetURL = fmt.Sprintf(grok.Responses, convCtx.ConversationID)
		isNew = false
	} else {
		// First message — create new conversation.
		payload = grok.BuildChatPayload(message, model.ModeId(lease.ModeID), fileInputs, nil, nil, nil)
		targetURL = grok.Chat
		isNew = true
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return platform.UpstreamError("encode chat payload: "+err.Error(), 500, "")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	bodyReader, err := s.Transport.PostStream(ctx, targetURL, lease.Token, body)
	if err != nil {
		// If responses endpoint fails, clear the conversation and fall back to
		// creating a new one on the next attempt.
		s.ConvTracker.Clear(lease.Token)
		return err
	}
	defer bodyReader.Close()

	adapter := grok.NewStreamAdapter()
	completionID := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()

	if stream {
		sw := newSSEWriter(w)
		sw.writeComment("heartbeat")
		first := true
		scanner := bufio.NewScanner(bodyReader)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			kind, data := grok.ClassifyLine(line)
			if kind == "done" {
				break
			}
			if kind != "data" {
				continue
			}
			events, errObj := adapter.Feed([]byte(data))
			if errObj != nil {
				sw.writeOpenAIError(errObj.Message, string(errObj.Kind), errObj.Code, errObj.Param)
				return nil
			}
			for _, ev := range events {
				switch ev.Kind {
				case grok.EventText:
					if first {
						first = false
					}
					chunk := makeStreamChunk(completionID, created, modelName, ev.Content, "", false)
					sw.writeJSONData(chunk)
				case grok.EventThinking:
					if !emitThink {
						continue
					}
					chunk := makeStreamChunk(completionID, created, modelName, "", ev.Content, false)
					chunk["choices"].([]any)[0].(map[string]any)["delta"] = map[string]any{"reasoning_content": ev.Content}
					sw.writeJSONData(chunk)
				case grok.EventImage:
					md := "![image](" + ev.Content + ")"
					chunk := makeStreamChunk(completionID, created, modelName, md, "", false)
					sw.writeJSONData(chunk)
				case grok.EventImageProgress:
					progress := "image generating " + ev.Content + "%"
					chunk := makeStreamChunk(completionID, created, modelName, "", progress, false)
					chunk["choices"].([]any)[0].(map[string]any)["delta"] = map[string]any{"reasoning_content": progress}
					sw.writeJSONData(chunk)
				case grok.EventSoftStop:
					finalChunk := makeStreamChunk(completionID, created, modelName, "", "", true)
					sw.writeJSONData(finalChunk)
				}
			}
		}
		finalChunk := makeStreamChunk(completionID, created, modelName, "", "", true)
		sw.writeJSONData(finalChunk)
		sw.writeDone()
		// Save conversation context for follow-up messages.
		if isNew && adapter.ConversationID != "" && adapter.LastResponseID != "" {
			s.ConvTracker.Set(lease.Token, adapter.ConversationID, adapter.LastResponseID)
		}
		return nil
	}

	// Non-streaming: aggregate text + thinking.
	textBuf := []string{}
	thinkingBuf := []string{}
	imageURLs := [][2]string{}
	scanner := bufio.NewScanner(bodyReader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		kind, data := grok.ClassifyLine(line)
		if kind == "done" {
			break
		}
		if kind != "data" {
			continue
		}
		events, errObj := adapter.Feed([]byte(data))
		if errObj != nil {
			return errObj
		}
		for _, ev := range events {
			switch ev.Kind {
			case grok.EventText:
				textBuf = append(textBuf, ev.Content)
			case grok.EventThinking:
				thinkingBuf = append(thinkingBuf, ev.Content)
			case grok.EventImageProgress:
				thinkingBuf = append(thinkingBuf, "image generating "+ev.Content+"%\n")
			case grok.EventImage:
				imageURLs = append(imageURLs, [2]string{ev.Content, ev.ImageID})
			}
		}
	}
	text := strings.Join(textBuf, "")
	thinking := strings.Join(thinkingBuf, "")
	if len(imageURLs) > 0 {
		var mds []string
		for _, u := range imageURLs {
			mds = append(mds, "![image]("+u[0]+")")
		}
		if text != "" {
			text += "\n\n"
		}
		text += strings.Join(mds, "\n\n")
	}
	resp := makeChatResponse(completionID, created, modelName, text, thinking, emitThink)
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
	// Save conversation context for follow-up messages.
	if isNew && adapter.ConversationID != "" && adapter.LastResponseID != "" {
		s.ConvTracker.Set(lease.Token, adapter.ConversationID, adapter.LastResponseID)
	}
	return nil
}

// makeStreamChunk builds a chat.completion.chunk frame.
func makeStreamChunk(id string, created int64, model, content, reasoning string, isFinal bool) map[string]any {
	delta := map[string]any{}
	if content != "" {
		delta["content"] = content
	} else if reasoning != "" {
		delta["reasoning_content"] = reasoning
	} else if !isFinal {
		delta["role"] = "assistant"
	}
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if isFinal {
		choice["delta"] = map[string]any{}
		choice["finish_reason"] = "stop"
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
}

// makeChatResponse builds a non-streaming chat.completion response.
func makeChatResponse(id string, created int64, modelName, text, thinking string, emitThink bool) map[string]any {
	msg := map[string]any{"role": "assistant", "content": text}
	if emitThink && thinking != "" {
		msg["reasoning_content"] = thinking
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
}

// resolveEmitThink decides whether to forward thinking tokens to the client.
func resolveEmitThink(effort *string) bool {
	if effort != nil {
		e := strings.ToLower(strings.TrimSpace(*effort))
		if e == "none" {
			return false
		}
		return e != ""
	}
	return config.Global().GetBool("features.thinking", true)
}

// extractMessages flattens OpenAI messages into a single prompt string and a
// list of uploaded file IDs.
func extractMessages(messages []map[string]any) (string, []string, *platform.AppError) {
	var b strings.Builder
	fileInputs := []string{}
	for i, msg := range messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		role, _ := msg["role"].(string)
		if role == "" {
			role = "user"
		}
		switch role {
		case "system", "developer":
			role = "system"
		case "assistant":
			role = "assistant"
		case "tool":
			role = "tool"
		default:
			role = "user"
		}
		switch c := msg["content"].(type) {
		case string:
			fmt.Fprintf(&b, "[%s]: %s", role, c)
		case []any:
			for _, item := range c {
				bm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				t, _ := bm["type"].(string)
				switch t {
				case "text":
					text, _ := bm["text"].(string)
					fmt.Fprintf(&b, "[%s]: %s\n", role, text)
				case "image_url":
					urlObj, _ := bm["image_url"].(map[string]any)
					if urlObj != nil {
						if u, _ := urlObj["url"].(string); u != "" {
							fileInputs = append(fileInputs, u)
						}
					}
				}
			}
		default:
			fmt.Fprintf(&b, "[%s]: %v", role, c)
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", nil, platform.UpstreamError("Empty message after extraction", 400, "")
	}
	return text, fileInputs, nil
}

// feedback posts request outcome to the directory (success path)
// and triggers an async quota sync for the used mode.
func (s *Server) feedback(token string, kind account.FeedbackKind, modeID int, remaining *int, resetAtMs *int64) {
	if s.Directory == nil {
		return
	}
	s.Directory.Feedback(token, kind, modeID, remaining, resetAtMs)
	// Fire-and-forget async quota sync for the used mode, mirroring
	// refresh_call_async in the Python reference.
	if kind == account.FbSuccess && s.Refresh != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _, _ = s.Refresh.RefreshTokens(ctx, []string{token})
		}()
	}
}

// feedbackError posts an error outcome to the directory.
func (s *Server) feedbackError(token string, err error, modeID int) {
	if s.Directory == nil {
		return
	}
	var appErr *platform.AppError
	if !asAppError(err, &appErr) {
		appErr = platform.NewAppError(err.Error(), platform.ErrServer, "internal_error", 500)
	}
	kind := account.FeedbackKindFromStatus(appErr.Status)
	// Override to Unauthorized when the response body indicates invalid credentials,
	// even for 403 (blocked-user) or 400 (session not found).
	if kind != account.FbUnauthorized && platform.IsInvalidCredentialsBody(appErr.Body) {
		kind = account.FbUnauthorized
	}
	s.Directory.Feedback(token, kind, modeID, nil, nil)
	// Also persist to the repository if unauthorized + expired.
	if kind == account.FbUnauthorized && s.Refresh != nil {
		s.Refresh.RecordFailure(context.Background(), token, modeID, appErr)
	}
}

// readAllBody reads up to limit bytes from r and returns the body.
func readAllBody(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// runWSImageChat handles chat completions for WS-based image models
// (grok-imagine-image, grok-imagine-image-pro).  It routes through the
// WS imagine endpoint and returns results in chat completion format.
func (s *Server) runWSImageChat(c *gin.Context, req *chatCompletionRequest, spec *model.Spec, stream bool) {
	// Extract prompt from messages.
	prompt := extractImagePrompt(req.Messages)
	if prompt == "" {
		writeAppError(c, platform.ValidationError("Empty prompt for image generation", "messages"))
		return
	}

	n := 1
	size := "1024x1024"
	if req.ImageConfig != nil {
		if req.ImageConfig.N > 0 {
			n = req.ImageConfig.N
		}
		if req.ImageConfig.Size != "" {
			size = req.ImageConfig.Size
		}
	}
	if n > 10 {
		n = 10
	}

	aspectRatio := grok.ResolveAspectRatio(size)
	enableNSFW := config.Global().GetBool("features.enable_nsfw", true)
	enablePro := grok.IsProImageModel(spec.ModelName)
	emitThink := resolveEmitThink(req.ReasoningEffort)

	apiToken, _ := c.Get("api_token")
	ssoToken, _ := apiToken.(string)

	// Reserve an account.
	lease, _ := reserveAccount(c.Request.Context(), s.Directory, spec, nil)
	if lease == nil {
		if s.Refresh != nil {
			_ = s.Refresh.RefreshOnDemand(c.Request.Context())
			lease, _ = reserveAccount(c.Request.Context(), s.Directory, spec, nil)
		}
	}
	if lease == nil && ssoToken != "" {
		lease = &account.Lease{Token: ssoToken, ModeID: int(spec.ModeId)}
	}
	if lease == nil {
		writeAppError(c, platform.RateLimitError("No available accounts"))
		return
	}
	defer s.Directory.Release(lease)

	completionID := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	modelName := req.Model

	streamGen := grok.NewImagineStream(lease.Token)
	events := streamGen.StreamImages(prompt, aspectRatio, n, enableNSFW, enablePro)

	if stream {
		sw := newSSEWriter(c.Writer)
		sw.writeComment("heartbeat")

		progressMap := map[string]int{}
		completedIDs := map[string]bool{}
		lastProgress := -1

		for ev := range events {
			switch ev.Type {
			case grok.ImagineEventProgress:
				progressMap[ev.ImageID] = ev.Progress
				aggregate := computeAggregateProgress(progressMap, n)
				if emitThink && aggregate > lastProgress {
					lastProgress = aggregate
					completed := countCompleted(completedIDs)
					reason := grok.FormatImageProgress("图片", aggregate, completed, n)
					chunk := makeStreamChunk(completionID, created, modelName, "", reason+"\n", false)
					chunk["choices"].([]any)[0].(map[string]any)["delta"] = map[string]any{"reasoning_content": reason + "\n"}
					sw.writeJSONData(chunk)
				}
			case grok.ImagineEventImage:
				completedIDs[ev.ImageID] = true
				progressMap[ev.ImageID] = 100
				url := ev.URL
				if url != "" {
					url = grok.ImageBaseURL + strings.TrimPrefix(url, "/")
				}
				md := "![image](" + url + ")"
				chunk := makeStreamChunk(completionID, created, modelName, md, "", false)
				sw.writeJSONData(chunk)
			case grok.ImagineEventError:
				sw.writeOpenAIError(ev.Error, "upstream_error", "", "")
				return
			}
		}

		finalChunk := makeStreamChunk(completionID, created, modelName, "", "", true)
		sw.writeJSONData(finalChunk)
		sw.writeDone()
		s.feedback(lease.Token, account.FbSuccess, lease.ModeID, nil, nil)
		return
	}

	// Non-streaming: collect all images.
	var imageURLs []string
	var thinkingUpdates []string
	progressMap := map[string]int{}
	completedIDs := map[string]bool{}

	for ev := range events {
		switch ev.Type {
		case grok.ImagineEventProgress:
			progressMap[ev.ImageID] = ev.Progress
			if emitThink {
				completed := countCompleted(completedIDs)
				reason := grok.FormatImageProgress("图片", computeAggregateProgress(progressMap, n), completed, n)
				if len(thinkingUpdates) == 0 || thinkingUpdates[len(thinkingUpdates)-1] != reason {
					thinkingUpdates = append(thinkingUpdates, reason)
				}
			}
		case grok.ImagineEventImage:
			completedIDs[ev.ImageID] = true
			progressMap[ev.ImageID] = 100
			url := ev.URL
			if url != "" {
				url = grok.ImageBaseURL + strings.TrimPrefix(url, "/")
				imageURLs = append(imageURLs, url)
			}
		case grok.ImagineEventError:
			writeAppError(c, platform.UpstreamError(ev.Error, 502, ""))
			s.feedbackError(lease.Token, platform.UpstreamError(ev.Error, 502, ""), lease.ModeID)
			return
		}
	}

	text := ""
	if len(imageURLs) > 0 {
		var mds []string
		for _, u := range imageURLs {
			mds = append(mds, "![image]("+u+")")
		}
		text = strings.Join(mds, "\n\n")
	}
	thinking := ""
	if len(thinkingUpdates) > 0 {
		thinking = strings.Join(thinkingUpdates, "\n")
	}
	resp := makeChatResponse(completionID, created, modelName, text, thinking, emitThink)
	b, _ := json.Marshal(resp)
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(b)
	s.feedback(lease.Token, account.FbSuccess, lease.ModeID, nil, nil)
}

// extractImagePrompt extracts the text prompt from chat messages.
func extractImagePrompt(messages []map[string]any) string {
	var prompt string
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			if c != "" {
				prompt = c
			}
		case []any:
			for _, item := range c {
				bm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				t, _ := bm["type"].(string)
				if t == "text" {
					text, _ := bm["text"].(string)
					if text != "" {
						prompt = text
					}
				}
			}
		}
	}
	return strings.TrimSpace(prompt)
}

// computeAggregateProgress computes the aggregate progress across all slots.
func computeAggregateProgress(progressMap map[string]int, total int) int {
	if total <= 0 {
		return 100
	}
	if len(progressMap) == 0 {
		return 0
	}
	sum := 0
	count := 0
	for _, v := range progressMap {
		p := v
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		sum += p
		count++
	}
	if count == 0 {
		return 0
	}
	result := sum / total
	if result > 100 {
		result = 100
	}
	return result
}

// countCompleted counts the number of completed image IDs.
func countCompleted(completedIDs map[string]bool) int {
	return len(completedIDs)
}
