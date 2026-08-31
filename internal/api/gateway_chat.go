package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aurora-develop/grok2api/internal/account"
	"github.com/aurora-develop/grok2api/internal/grok"
	"github.com/aurora-develop/grok2api/internal/model"
)

func usesGatewayChat(modelName string) bool {
	return modelName == "grok-4.6"
}

func gatewayUpstreamModel(modelName string) string {
	if modelName == "grok-4.6" {
		return model.ModeExpert.ApiStr()
	}
	return modelName
}

func (s *Server) runGatewayChatOnce(w http.ResponseWriter, r *http.Request, lease *account.Lease, message string, fileInputs []string, emitThink, stream bool, modelName string) error {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	completionID := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	events := s.Transport.StreamGatewayChat(ctx, lease.Token, gatewayUpstreamModel(modelName), message, fileInputs)
	var text, reasoning strings.Builder
	var conversationID, responseID string

	if stream {
		sw := newSSEWriter(w)
		sw.writeComment("heartbeat")
		for ev := range events {
			if ev.Err != nil {
				sw.writeOpenAIError(ev.Err.Error(), "upstream_error", "", "")
				return nil
			}
			conversationID = ev.ConversationID
			responseID = ev.ResponseID
			switch ev.Kind {
			case grok.EventText:
				text.WriteString(ev.Content)
				sw.writeJSONData(makeStreamChunk(completionID, created, modelName, ev.Content, "", false))
			case grok.EventThinking:
				if emitThink {
					reasoning.WriteString(ev.Content)
					chunk := makeStreamChunk(completionID, created, modelName, "", ev.Content, false)
					chunk["choices"].([]any)[0].(map[string]any)["delta"] = map[string]any{"reasoning_content": ev.Content}
					sw.writeJSONData(chunk)
				}
			case grok.EventSoftStop:
				sw.writeJSONData(makeStreamChunk(completionID, created, modelName, "", "", true))
			}
		}
		sw.writeDone()
		if s.ConvTracker != nil && conversationID != "" && responseID != "" {
			s.ConvTracker.Set(lease.Token, conversationID, responseID)
		}
		return nil
	}

	for ev := range events {
		if ev.Err != nil {
			return ev.Err
		}
		conversationID = ev.ConversationID
		responseID = ev.ResponseID
		switch ev.Kind {
		case grok.EventText:
			text.WriteString(ev.Content)
		case grok.EventThinking:
			if emitThink {
				reasoning.WriteString(ev.Content)
			}
		}
	}
	resp := makeChatResponse(completionID, created, modelName, text.String(), reasoning.String(), emitThink)
	body, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	if s.ConvTracker != nil && conversationID != "" && responseID != "" {
		s.ConvTracker.Set(lease.Token, conversationID, responseID)
	}
	return nil
}
