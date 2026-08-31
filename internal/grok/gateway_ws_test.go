package grok

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGatewaySessionCreateMessage(t *testing.T) {
	msg := BuildGatewaySessionCreateMessage("build", "evt_init_1")
	var got map[string]any
	if err := json.Unmarshal(msg, &got); err != nil {
		t.Fatal(err)
	}
	if got["event"].(map[string]any)["type"] != "session.create" {
		t.Fatalf("event type = %#v", got["event"])
	}
	session := got["event"].(map[string]any)["session"].(map[string]any)
	if session["model"] != "build" {
		t.Fatalf("model = %#v", session["model"])
	}
}

func TestGatewayTurnMessages(t *testing.T) {
	message := BuildGatewayTurnMessages("session-1", "hello", []string{"file-1"})
	if message["session_id"] != "session-1" {
		t.Fatalf("session id = %#v", message["session_id"])
	}
	event := message["event"].(map[string]any)
	if event["type"] != "response.create" {
		t.Fatalf("event type = %#v", event["type"])
	}
	if event["item"].(map[string]any)["type"] != "message" {
		t.Fatalf("item = %#v", event["item"])
	}
	chunks := event["item"].(map[string]any)["x_grok"].(map[string]any)["input_chunks"].([]any)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestBuildGatewayHeaders(t *testing.T) {
	h := BuildGatewayHeaders("https://grok.com", "user-1", "token-1", "Mozilla/5.0")
	for key, want := range map[string]string{
		"Origin":          "https://grok.com",
		"Accept-Language": "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7",
		"Cache-Control":   "no-cache",
		"Pragma":          "no-cache",
		"User-Agent":      "Mozilla/5.0",
	} {
		if got := h.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if !strings.Contains(h.Get("Cookie"), "x-userid=user-1") {
		t.Fatalf("cookie = %q", h.Get("Cookie"))
	}
}

func TestParseGatewayEvent(t *testing.T) {
	var parsed GatewayEventResult
	for _, raw := range []string{
		`{"type":"conversation.attached","conversation":{"id":"conv-1"}}`,
		`{"type":"response.chunk","chunk":{"text":{"channel":"CHANNEL_ASSISTANT_RESPONSE","text":"hi"}}}`,
		`{"type":"response.chunk","chunk":{"text":{"channel":"CHANNEL_REASONING","text":"think"}}}`,
		`{"type":"response.done","response":{"id":"resp-1","status":"completed"}}`,
	} {
		var event map[string]any
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		if err := ParseGatewayEvent(event, &parsed); err != nil {
			t.Fatal(err)
		}
	}
	if parsed.ConversationID != "conv-1" || parsed.ResponseID != "resp-1" {
		t.Fatalf("parsed = %#v", parsed)
	}
	if parsed.Text != "hi" || parsed.Reasoning != "think" || !parsed.Done {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestParseGatewayError(t *testing.T) {
	var parsed GatewayEventResult
	var event map[string]any
	_ = json.Unmarshal([]byte(`{"type":"error","error":{"message":"rate limited","code":"rate_limit"}}`), &event)
	err := ParseGatewayEvent(event, &parsed)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error = %v", err)
	}
}

func TestGatewayHandshakeDiagnosticIncludesResponseAndRedactsSecrets(t *testing.T) {
	body := `<html>denied sso=secret-token; authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature</html>`
	got := gatewayHandshakeDiagnostic("websocket: bad handshake", 403, "403 Forbidden", body)
	for _, want := range []string{"websocket: bad handshake", "HTTP 403 Forbidden", "<html>denied"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic %q does not contain %q", got, want)
		}
	}
	for _, secret := range []string{"secret-token", "eyJhbGciOiJIUzI1NiJ9.payload.signature"} {
		if strings.Contains(got, secret) {
			t.Fatalf("diagnostic leaked secret %q: %q", secret, got)
		}
	}
}

func TestGatewayHandshakeDiagnosticTruncatesBody(t *testing.T) {
	got := gatewayHandshakeDiagnostic("websocket: bad handshake", 403, "403 Forbidden", strings.Repeat("x", 2048))
	if len(got) > 900 {
		t.Fatalf("diagnostic length = %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("diagnostic = %q", got)
	}
}
