package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
	"github.com/google/uuid"

	"github.com/aurora-develop/grok2api/internal/platform"
)

const (
	gatewayHandshakeTimeout = 20 * time.Second
	gatewayHeartbeatPeriod  = 25 * time.Second
	gatewayMaxFrameBytes    = 16 << 20
	gatewayDiagnosticBytes  = 768
)

var gatewaySensitiveValueRE = regexp.MustCompile(`(?i)(sso(?:-rw)?|authorization|cookie)\s*[:=]\s*(?:bearer\s+)?[^;\s<]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

// GatewayEvent is one normalized event from the Grok WebSocket Gateway.
type GatewayEvent struct {
	Kind           FrameEventKind
	Content        string
	ConversationID string
	ResponseID     string
	Err            error
}

// GatewayEventResult is the state accumulated while parsing a Gateway turn.
type GatewayEventResult struct {
	SessionID      string
	ConversationID string
	ResponseID     string
	Text           string
	Reasoning      string
	Done           bool

	lastKind    FrameEventKind
	lastContent string
}

// BuildGatewaySessionCreateMessage builds the first Gateway client event.
func BuildGatewaySessionCreateMessage(modelName, eventID string) []byte {
	return marshalGateway(map[string]any{
		"event": map[string]any{
			"type":     "session.create",
			"event_id": eventID,
			"session":  gatewaySession(modelName),
		},
	})
}

func gatewaySession(modelName string) map[string]any {
	return map[string]any{
		"model": modelName,
		"x_grok": map[string]any{
			"protocol_capabilities":   []string{"conversation_attached", "custom_methods_v1", "workspace_servers_v1"},
			"use_chunk":               true,
			"enable_side_by_side":     true,
			"force_side_by_side":      false,
			"enable_image_generation": true,
			"image_generation_count":  2,
			"disable_text_follow_ups": false,
			"disable_artifact":        true,
			"force_concise":           false,
			"keep_context":            false,
			"is_temporary":            true,
			"disable_memory":          true,
		},
	}
}

// BuildGatewayTurnMessages builds the response.create event used by Grok Web.
func BuildGatewayTurnMessages(sessionID, prompt string, attachments []string) map[string]any {
	chunks := make([]any, 0, len(attachments)+1)
	for _, attachment := range attachments {
		chunks = append(chunks, map[string]any{
			"mention": map[string]any{
				"target": map[string]any{
					"file_mention": map[string]any{"file_id": attachment},
				},
			},
		})
	}
	chunks = append(chunks, map[string]any{"text": map[string]any{"text": prompt}})
	item := map[string]any{
		"type": "message",
		"role": "user",
		"x_grok": map[string]any{
			"client_message_id": uuid.NewString(),
			"input_chunks":      chunks,
		},
	}
	if len(attachments) > 0 {
		item["file_attachment_ids"] = attachments
	}
	now := time.Now().UnixMilli()
	messageEvent := map[string]any{
		"session_id": sessionID,
		"event": map[string]any{
			"type":     "response.create",
			"event_id": fmt.Sprintf("evt_resp_%d", now),
			"item":     item,
		},
	}
	if len(attachments) > 0 {
		messageEvent["event"].(map[string]any)["file_attachment_ids"] = attachments
	}
	return messageEvent
}

// BuildGatewayHeaders builds the browser-like WebSocket handshake headers.
func BuildGatewayHeaders(origin, userID, token, userAgent string) fhttp.Header {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = DefaultUserAgent
	}
	h := fhttp.Header{}
	h.Set("Origin", origin)
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("User-Agent", userAgent)
	h.Set("Cookie", buildGatewayCookie(token, resolveProxyProfile(), userID))
	return h
}

func buildGatewayCookie(token string, profile proxyProfile, userID string) string {
	base := BuildSSOCookie(token, profile)
	parts := strings.Split(base, ";")
	filtered := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(strings.ToLower(part), "x-userid=") {
			continue
		}
		filtered = append(filtered, part)
	}
	if strings.TrimSpace(userID) != "" {
		filtered = append(filtered, "x-userid="+strings.TrimSpace(userID))
	}
	return strings.Join(filtered, "; ")
}

// ParseGatewayEvent normalizes one decoded Gateway event.
func ParseGatewayEvent(event map[string]any, parsed *GatewayEventResult) error {
	if parsed == nil {
		return fmt.Errorf("nil Gateway parse result")
	}
	parsed.lastKind = ""
	parsed.lastContent = ""
	typeName, _ := event["type"].(string)
	switch typeName {
	case "conversation.attached":
		conversation, _ := event["conversation"].(map[string]any)
		parsed.ConversationID, _ = conversation["id"].(string)
	case "response.chunk":
		chunk, _ := event["chunk"].(map[string]any)
		if chunk == nil {
			return nil
		}
		text, _ := chunk["text"].(map[string]any)
		if text == nil {
			return nil
		}
		delta, _ := text["text"].(string)
		channel, _ := text["channel"].(string)
		return appendGatewayText(parsed, channel, delta)
	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		return appendGatewayText(parsed, "CHANNEL_ASSISTANT_RESPONSE", delta)
	case "response.output_text.done":
		text, _ := event["text"].(string)
		if parsed.Text == "" && text != "" {
			return appendGatewayText(parsed, "CHANNEL_ASSISTANT_RESPONSE", text)
		}
	case "response.grok.output":
		output, _ := event["output"].(map[string]any)
		if streamError, _ := output["stream_error"].(map[string]any); streamError != nil {
			message, _ := streamError["message"].(string)
			if message == "" {
				message = "Grok Gateway stream error"
			}
			return platform.UpstreamError(message, 429, compactGatewayJSON(event))
		}
	case "response.done":
		response, _ := event["response"].(map[string]any)
		parsed.ResponseID, _ = response["id"].(string)
		status, _ := response["status"].(string)
		if status != "" && status != "completed" {
			if details, _ := response["status_details"].(map[string]any); details != nil {
				if reason, _ := details["reason"].(string); reason != "" {
					return fmt.Errorf("Grok Gateway response status is %s (%s)", status, reason)
				}
			}
			return fmt.Errorf("Grok Gateway response status is %s", status)
		}
		parsed.Done = true
	case "error":
		return gatewayEventError(event)
	}
	return nil
}

func appendGatewayText(parsed *GatewayEventResult, channel, delta string) error {
	if delta == "" {
		return nil
	}
	if strings.Contains(strings.ToLower(channel), "reason") ||
		strings.Contains(strings.ToLower(channel), "analysis") ||
		strings.Contains(strings.ToLower(channel), "thinking") {
		parsed.Reasoning += delta
		parsed.lastKind = EventThinking
		parsed.lastContent = delta
		return nil
	}
	parsed.Text += delta
	parsed.lastKind = EventText
	parsed.lastContent = delta
	return nil
}

func gatewayEventError(event map[string]any) error {
	raw := event["error"]
	if value, ok := raw.(map[string]any); ok {
		message, _ := value["message"].(string)
		if message == "" {
			message, _ = value["error"].(string)
		}
		if message == "" {
			message = "Grok Gateway stream error"
		}
		return platform.UpstreamError(message, gatewayErrorStatus(value), compactGatewayJSON(event))
	}
	if message, ok := raw.(string); ok && message != "" {
		return platform.UpstreamError(message, 502, compactGatewayJSON(event))
	}
	return platform.UpstreamError("Grok Gateway stream error", 502, compactGatewayJSON(event))
}

func gatewayErrorStatus(value map[string]any) int {
	code, _ := value["code"].(string)
	if strings.Contains(strings.ToLower(code), "rate") || strings.Contains(strings.ToLower(code), "limit") {
		return 429
	}
	return 502
}

// StreamGatewayChat connects to the current grok.com text-chat Gateway.
func (t *Transport) StreamGatewayChat(ctx context.Context, token, modelName, prompt string, attachments []string) <-chan GatewayEvent {
	out := make(chan GatewayEvent, 32)
	go func() {
		defer close(out)
		if err := t.runGatewayChat(ctx, token, modelName, prompt, attachments, out); err != nil {
			out <- GatewayEvent{Kind: EventSkip, Err: err}
		}
	}()
	return out
}

func (t *Transport) runGatewayChat(ctx context.Context, token, modelName, prompt string, attachments []string, out chan<- GatewayEvent) error {
	userID, err := t.resolveGatewayUserID(ctx, token)
	if err != nil {
		return err
	}
	endpoint, origin, err := gatewayEndpoint(Base, userID)
	if err != nil {
		return err
	}
	profile := resolveProxyProfile()
	client, err := t.ensureClient()
	if err != nil {
		return err
	}
	dialer := &websocket.Dialer{
		HandshakeTimeout:  gatewayHandshakeTimeout,
		NetDialContext:    client.GetDialer().DialContext,
		NetDialTLSContext: client.GetTLSDialer(),
	}
	conn, handshake, err := dialer.DialContext(ctx, endpoint, BuildGatewayHeaders(origin, userID, token, profile.UserAgent))
	if err != nil {
		body := ""
		status := 502
		if handshake != nil {
			status = handshake.StatusCode
			if handshake.Body != nil {
				data, _ := io.ReadAll(io.LimitReader(handshake.Body, 4096))
				_ = handshake.Body.Close()
				body = strings.TrimSpace(string(data))
			}
		}
		return platform.UpstreamError(gatewayHandshakeDiagnostic("Grok Gateway connect failed: "+err.Error(), status, handshakeStatus(handshake), body), status, body)
	}
	defer conn.Close()
	conn.SetReadLimit(gatewayMaxFrameBytes)

	var writeMu sync.Mutex
	write := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return conn.WriteJSON(value)
	}
	initialID := "evt_init_" + uuid.NewString()
	if err := write(map[string]any{
		"event": map[string]any{
			"type":     "session.create",
			"event_id": initialID,
			"session":  gatewaySession(modelName),
		},
	}); err != nil {
		return fmt.Errorf("send Gateway session.create: %w", err)
	}

	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go func() {
		ticker := time.NewTicker(gatewayHeartbeatPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ctx.Done():
				_ = conn.Close()
				return
			case now := <-ticker.C:
				if write(map[string]any{"event": map[string]any{
					"type": "ping", "event_id": fmt.Sprintf("evt_hb_%d", now.UnixMilli()),
				}}) != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	created, attached, turnSent := false, false, false
	var parsed GatewayEventResult
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read Grok Gateway: %w", err)
		}
		if messageType != websocket.TextMessage || len(data) > gatewayMaxFrameBytes {
			continue
		}
		var envelope map[string]any
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}
		if sessionID, _ := envelope["session_id"].(string); sessionID != "" {
			parsed.SessionID = sessionID
		}
		event := unwrapGatewayEvent(envelope)
		if err := ParseGatewayEvent(event, &parsed); err != nil {
			return err
		}
		emitGatewayDelta(out, &parsed)
		typeName, _ := event["type"].(string)
		switch typeName {
		case "session.created":
			clientID, _ := event["client_event_id"].(string)
			if clientID == "" {
				clientID, _ = event["event_id"].(string)
			}
			created = clientID == "" || clientID == initialID
		case "conversation.attached":
			attached = true
		case "response.done":
			out <- GatewayEvent{Kind: EventSoftStop, ConversationID: parsed.ConversationID, ResponseID: parsed.ResponseID}
			return nil
		case "session.ended":
			return fmt.Errorf("Grok Gateway session ended before response completion")
		}
		if created && attached && !turnSent {
			message := BuildGatewayTurnMessages(parsed.SessionID, prompt, attachments)
			if err := write(message); err != nil {
				return fmt.Errorf("send Gateway response.create: %w", err)
			}
			turnSent = true
		}
	}
}

func unwrapGatewayEvent(envelope map[string]any) map[string]any {
	if event, ok := envelope["event"].(map[string]any); ok {
		return event
	}
	return envelope
}

func emitGatewayDelta(out chan<- GatewayEvent, parsed *GatewayEventResult) {
	if parsed.lastContent == "" {
		return
	}
	out <- GatewayEvent{Kind: parsed.lastKind, Content: parsed.lastContent,
		ConversationID: parsed.ConversationID, ResponseID: parsed.ResponseID}
	parsed.lastContent = ""
}

func (t *Transport) resolveGatewayUserID(ctx context.Context, token string) (string, error) {
	if userID := extractUserID(token); userID != "" {
		if _, err := uuid.Parse(userID); err == nil {
			return userID, nil
		}
	}
	value, err := t.GetJSON(ctx, Base+"/api/auth/session", token, WithTimeout(15*time.Second))
	if err != nil {
		return "", fmt.Errorf("resolve Grok Gateway user id: %w", err)
	}
	userID := firstMapString(value,
		[]string{"session", "userId"}, []string{"user", "id"}, []string{"user", "userId"},
		[]string{"user", "sub"}, []string{"id"}, []string{"userId"}, []string{"sub"})
	if _, err := uuid.Parse(userID); err != nil {
		return "", fmt.Errorf("Grok Session returned invalid user id")
	}
	return userID, nil
}

func firstMapString(value map[string]any, paths ...[]string) string {
	for _, path := range paths {
		current := any(value)
		for _, key := range path {
			obj, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = obj[key]
		}
		if result, ok := current.(string); ok && strings.TrimSpace(result) != "" {
			return strings.TrimSpace(result)
		}
	}
	return ""
}

func gatewayEndpoint(baseURL, userID string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("invalid Grok Gateway base URL")
	}
	origin := (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", "", fmt.Errorf("invalid Grok Gateway base URL scheme")
	}
	parsed.Path = "/ws/mgw/"
	parsed.RawPath = ""
	parsed.RawQuery = url.Values{"uid": []string{userID}}.Encode()
	parsed.Fragment = ""
	return parsed.String(), origin, nil
}

func marshalGateway(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func compactGatewayJSON(value any) string {
	data, _ := json.Marshal(value)
	if len(data) > 4096 {
		data = data[:4096]
	}
	return string(data)
}

func handshakeStatus(resp *fhttp.Response) string {
	if resp == nil {
		return ""
	}
	return resp.Status
}

func gatewayHandshakeDiagnostic(errText string, status int, statusText, body string) string {
	parts := []string{errText}
	if status > 0 {
		label := strings.TrimSpace(statusText)
		prefix := fmt.Sprintf("%d ", status)
		if strings.HasPrefix(label, prefix) {
			label = strings.TrimSpace(strings.TrimPrefix(label, prefix))
		}
		if label == "" {
			label = http.StatusText(status)
		}
		parts = append(parts, strings.TrimSpace(fmt.Sprintf("HTTP %d %s", status, label)))
	}
	if body = strings.TrimSpace(body); body != "" {
		body = gatewaySensitiveValueRE.ReplaceAllString(body, "[redacted]")
		if len(body) > gatewayDiagnosticBytes {
			body = body[:gatewayDiagnosticBytes] + "..."
		}
		parts = append(parts, "body="+body)
	}
	return strings.Join(parts, "; ")
}

func httpStatusText(status int) string {
	return http.StatusText(status)
}

func (t *Transport) currentProxyURL() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.proxyURL
}
