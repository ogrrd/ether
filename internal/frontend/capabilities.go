package frontend

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

// CapabilityRequest describes client declarations, not a claim about playback.
// Viewer must come from the authenticated session when user rules are desired.
// Build requires WithClientIP using Host.ClientIP on the current request; the
// host derives network locality and ignores the caller's Remote claim.
type CapabilityRequest struct {
	Viewer       *core.Viewer
	Device       caps.DeviceInfo
	Profile      caps.DeviceProfile
	ProtocolInfo []string
	UserAgent    string
}

// CapabilityResult pairs client playback capabilities with the selected owner
// quality profile. The profile does not grant additional hardware capabilities.
type CapabilityResult struct {
	Client  core.ClientCaps
	Quality core.QualityProfile
}

// Capabilities applies client declarations, device quirks and owner rules.
type Capabilities interface {
	Build(context.Context, CapabilityRequest) (CapabilityResult, error)
	ApplyClientRules(HostQualityConfig, core.ClientCaps, *core.Viewer, string) core.ClientCaps
}

type ruleCapabilities struct {
	apply func(HostQualityConfig, core.ClientCaps, *core.Viewer, string) core.ClientCaps
}

func (ruleCapabilities) Build(context.Context, CapabilityRequest) (CapabilityResult, error) {
	return CapabilityResult{}, core.ErrUnauthorized
}

func (r ruleCapabilities) ApplyClientRules(q HostQualityConfig, c core.ClientCaps, v *core.Viewer, media string) core.ClientCaps {
	if r.apply == nil {
		return c
	}
	return r.apply(q, c, v, media)
}

// BindClientRules adapts a client-rule function into a Capabilities service.
func BindClientRules(fn func(HostQualityConfig, core.ClientCaps, *core.Viewer, string) core.ClientCaps) Capabilities {
	return ruleCapabilities{apply: fn}
}
