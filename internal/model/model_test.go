package model

import "testing"

func TestResolveGrok46UsesExpertWebMode(t *testing.T) {
	spec, ok := Resolve("grok-4.6")
	if !ok {
		t.Fatal("grok-4.6 should be enabled")
	}
	if spec.ModeId != ModeExpert {
		t.Fatalf("mode = %v, want %v", spec.ModeId, ModeExpert)
	}
	if spec.Tier != TierSuper {
		t.Fatalf("tier = %v, want %v", spec.Tier, TierSuper)
	}
	if !spec.IsChat() {
		t.Fatal("grok-4.6 should expose chat capability")
	}
}
