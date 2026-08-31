package api

import "testing"

func TestGrok46UsesGatewayChat(t *testing.T) {
	if !usesGatewayChat("grok-4.6") {
		t.Fatal("grok-4.6 should use the WebSocket Gateway")
	}
	if usesGatewayChat("grok-4.20-auto") {
		t.Fatal("legacy grok model should keep the HTTP chat path")
	}
}

func TestGatewayUpstreamModel(t *testing.T) {
	if got := gatewayUpstreamModel("grok-4.6"); got != "expert" {
		t.Fatalf("upstream model = %q", got)
	}
}
