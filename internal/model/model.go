// Package model defines the Grok model registry: mode ids, account tiers,
// capability flags and the master list of supported models.
package model

import "strings"

// ModeId is the Grok conversation mode used in chat payloads.
type ModeId int

const (
	ModeAuto    ModeId = 0
	ModeFast    ModeId = 1
	ModeExpert  ModeId = 2
	ModeHeavy   ModeId = 3
	ModeGrok43  ModeId = 4
	ModeConsole ModeId = 5
)

// ApiStr returns the modeId string used in the grok.com chat payload.
func (m ModeId) ApiStr() string {
	switch m {
	case ModeAuto:
		return "auto"
	case ModeFast:
		return "fast"
	case ModeExpert:
		return "expert"
	case ModeHeavy:
		return "heavy"
	case ModeGrok43:
		return "grok-420-computer-use-sa"
	case ModeConsole:
		return "console"
	}
	return "auto"
}

// Tier is the account pool tier.
type Tier int

const (
	TierBasic Tier = 0
	TierSuper Tier = 1
	TierHeavy Tier = 2
)

func (t Tier) Name() string {
	switch t {
	case TierBasic:
		return "basic"
	case TierSuper:
		return "super"
	case TierHeavy:
		return "heavy"
	}
	return "basic"
}

// TierFromName parses a pool name.
func TierFromName(name string) (Tier, bool) {
	switch strings.ToLower(name) {
	case "basic", "":
		return TierBasic, true
	case "super":
		return TierSuper, true
	case "heavy":
		return TierHeavy, true
	}
	return TierBasic, false
}

// Capability is a bitmask of model abilities.
type Capability int

const (
	CapChat        Capability = 1
	CapImage       Capability = 2
	CapImageEdit   Capability = 4
	CapVideo       Capability = 8
	CapVoice       Capability = 16
	CapAsset       Capability = 32
	CapConsoleChat Capability = 64
)

// Spec describes a single model entry.
type Spec struct {
	ModelName  string
	ModeId     ModeId
	Tier       Tier
	Capability Capability
	Enabled    bool
	PublicName string
	PreferBest bool
}

func (s *Spec) IsChat() bool        { return s.Capability&CapChat != 0 }
func (s *Spec) IsImage() bool       { return s.Capability&CapImage != 0 }
func (s *Spec) IsImageEdit() bool   { return s.Capability&CapImageEdit != 0 }
func (s *Spec) IsVideo() bool       { return s.Capability&CapVideo != 0 }
func (s *Spec) IsVoice() bool       { return s.Capability&CapVoice != 0 }
func (s *Spec) IsConsoleChat() bool { return s.Capability&CapConsoleChat != 0 }

func (s *Spec) PoolID() int { return int(s.Tier) }

// PoolCandidates returns the priority-ordered pool ids used for account
// reservation. When PreferBest is set, higher tiers are tried first.
func (s *Spec) PoolCandidates() []int {
	switch s.Tier {
	case TierBasic:
		if s.PreferBest {
			return []int{2, 1, 0}
		}
		return []int{0, 1, 2}
	case TierSuper:
		if s.PreferBest {
			return []int{2, 1}
		}
		return []int{1, 2}
	case TierHeavy:
		return []int{2}
	}
	return []int{0}
}

// PoolName returns the primary tier name.
func (s *Spec) PoolName() string { return s.Tier.Name() }

var registry = newRegistry()

type registryT struct {
	specs  []*Spec
	byName map[string]*Spec
}

func newRegistry() *registryT {
	r := &registryT{byName: map[string]*Spec{}}
	for _, s := range allModels {
		cp := s
		r.specs = append(r.specs, &cp)
		r.byName[s.ModelName] = &cp
	}
	return r
}

// Get returns the spec for a model name (nil if unknown).
func Get(name string) *Spec {
	if s, ok := registry.byName[name]; ok {
		return s
	}
	return nil
}

// Resolve returns the spec or false if unknown/disabled.
func Resolve(name string) (*Spec, bool) {
	s := Get(name)
	if s == nil || !s.Enabled {
		return nil, false
	}
	return s, true
}

// ListEnabled returns all enabled specs in registration order.
func ListEnabled() []*Spec {
	out := make([]*Spec, 0, len(registry.specs))
	for _, s := range registry.specs {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

// ListByCapability returns enabled specs matching the capability.
func ListByCapability(cap Capability) []*Spec {
	out := make([]*Spec, 0)
	for _, s := range registry.specs {
		if s.Enabled && s.Capability&cap != 0 {
			out = append(out, s)
		}
	}
	return out
}

var allModels = []Spec{
	// --- grok.com chat ---
	{ModelName: "grok-4.6", ModeId: ModeExpert, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.6"},
	{ModelName: "grok-4.20-0309-non-reasoning", ModeId: ModeFast, Tier: TierBasic, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning"},
	{ModelName: "grok-4.20-0309", ModeId: ModeAuto, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309"},
	{ModelName: "grok-4.20-0309-reasoning", ModeId: ModeExpert, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning"},
	{ModelName: "grok-4.20-0309-non-reasoning-super", ModeId: ModeFast, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning Super"},
	{ModelName: "grok-4.20-0309-super", ModeId: ModeAuto, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Super"},
	{ModelName: "grok-4.20-0309-reasoning-super", ModeId: ModeExpert, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning Super"},
	{ModelName: "grok-4.20-0309-non-reasoning-heavy", ModeId: ModeFast, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning Heavy"},
	{ModelName: "grok-4.20-0309-heavy", ModeId: ModeAuto, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Heavy"},
	{ModelName: "grok-4.20-0309-reasoning-heavy", ModeId: ModeExpert, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning Heavy"},
	{ModelName: "grok-4.20-multi-agent-0309", ModeId: ModeHeavy, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent 0309"},
	{ModelName: "grok-4.20-fast", ModeId: ModeFast, Tier: TierBasic, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Fast", PreferBest: true},
	{ModelName: "grok-4.3-fast", ModeId: ModeFast, Tier: TierBasic, Capability: CapChat, Enabled: true, PublicName: "Grok 4.3 Fast", PreferBest: true},
	{ModelName: "grok-4.20-auto", ModeId: ModeAuto, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Auto", PreferBest: true},
	{ModelName: "grok-4.20-expert", ModeId: ModeExpert, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Expert", PreferBest: true},
	{ModelName: "grok-4.20-heavy", ModeId: ModeHeavy, Tier: TierHeavy, Capability: CapChat, Enabled: true, PublicName: "Grok 4.20 Heavy", PreferBest: true},
	{ModelName: "grok-4.3-beta", ModeId: ModeGrok43, Tier: TierSuper, Capability: CapChat, Enabled: true, PublicName: "Grok 4.3 Beta"},
	// --- media (grok.com) ---
	{ModelName: "grok-imagine-image-lite", ModeId: ModeFast, Tier: TierBasic, Capability: CapImage, Enabled: true, PublicName: "Grok Imagine Image Lite"},
	{ModelName: "grok-imagine-image", ModeId: ModeAuto, Tier: TierSuper, Capability: CapImage, Enabled: true, PublicName: "Grok Imagine Image"},
	{ModelName: "grok-imagine-image-pro", ModeId: ModeAuto, Tier: TierSuper, Capability: CapImage, Enabled: true, PublicName: "Grok Imagine Image Pro"},
	{ModelName: "grok-imagine-image-edit", ModeId: ModeAuto, Tier: TierSuper, Capability: CapImageEdit, Enabled: true, PublicName: "Grok Imagine Image Edit"},
	{ModelName: "grok-imagine-video", ModeId: ModeAuto, Tier: TierSuper, Capability: CapVideo, Enabled: true, PublicName: "Grok Imagine Video"},
	// --- console.x.ai media (basic SSO, DPoP) ---
	{ModelName: "grok-imagine-image-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapImage | CapImageEdit, Enabled: true, PublicName: "Grok Imagine Image (Console)"},
	{ModelName: "grok-imagine-image-quality-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapImage | CapImageEdit, Enabled: true, PublicName: "Grok Imagine Image Quality (Console)"},
	{ModelName: "grok-imagine-video-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapVideo, Enabled: true, PublicName: "Grok Imagine Video (Console)"},
	// --- console.x.ai chat (basic, free) ---
	{ModelName: "grok-4.5-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 (Console)"},
	{ModelName: "grok-4.5-low", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 Low Thinking"},
	{ModelName: "grok-4.5-medium", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 Medium Thinking"},
	{ModelName: "grok-4.5-high", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 High Thinking"},
	{ModelName: "grok-4.5-xhigh", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.5 XHigh Thinking"},
	{ModelName: "grok-4.3-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.3 (Console)"},
	{ModelName: "grok-4.3-low", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.3 Low Thinking"},
	{ModelName: "grok-4.3-medium", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.3 Medium Thinking"},
	{ModelName: "grok-4.3-high", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.3 High Thinking"},
	{ModelName: "grok-4.20-0309-reasoning-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 0309 Reasoning (Console)"},
	{ModelName: "grok-4.20-0309-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 0309 (Console)"},
	{ModelName: "grok-4.20-multi-agent-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent (Console)"},
	{ModelName: "grok-4.20-multi-agent-low", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent Low"},
	{ModelName: "grok-4.20-multi-agent-medium", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent Medium"},
	{ModelName: "grok-4.20-multi-agent-high", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent High"},
	{ModelName: "grok-4.20-multi-agent-xhigh", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 Multi-Agent XHigh"},
	{ModelName: "grok-4.20-0309-non-reasoning-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok 4.20 0309 Non-Reasoning (Console)"},
	{ModelName: "grok-build-console", ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Grok Build 0.1 (Console)"},
}
