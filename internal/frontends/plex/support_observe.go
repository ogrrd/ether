package plex

import (
	"context"
	"crypto/rand"
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
	"strings"
	"time"
)

func (s *Server) observeRequest(r *http.Request, sr *statusRecorder, elapsed time.Duration) {
	if s == nil || s.d.Support == nil || r == nil || sr == nil {
		return
	}
	pr := FromContext(r.Context())
	status := sr.status
	if status == 0 {
		status = http.StatusOK
		if r.Context().Err() != nil {
			// Observation only: the disconnected client received no response.
			status = 499
		}
	}
	kind := frontend.ClassifyDiagnosticRequest(r.URL.Path, status, sr.Header().Get("Content-Type"), sr.written)
	recording := s.d.Support.Recording()
	ev := s.supportEvent(pr, kind)
	ev.Method = r.Method
	ev.Route = r.URL.Path
	ev.Status = status
	ev.Bytes = sr.written
	ev.Range = supportRange(r.Header.Get("Range"))
	if recording {
		if kind != frontend.DiagnosticKindDevice {
			s.d.Support.Observe(ev)
		}
		ev.Kind = frontend.DiagnosticKindRequest
		ev.Client = s.supportClientFacts(pr, s.declaredCaps(pr), "request")
		ev.Client.Inputs = supportClientInputs(r)
		ev.Duration = elapsed
		ev.Cancelled = r.Context().Err() != nil
		ev.Query = frontend.SafeDiagnosticQuery(r.URL.Query())
		ev.DBQueries, ev.DBTime = s.exp().RequestDatabaseObservation(r.Context())
		if sr.sampling {
			ev.Response = summarizePlexResponse(sr.sample, sr.truncated, sr.Header().Get("Content-Encoding"))
		}
	} else if status >= 400 && kind == frontend.DiagnosticKindDevice && pr.User != nil {
		// ClassifyRequest maps most paths to a bare device sighting, which
		// the recorder never ring-buffers. A rejection on such a path (a
		// download, subscription or provider route, say) must still enter
		// the ring as a request, so a capture started for that failure
		// contains the 4xx in its pre-roll. While recording, the branch
		// above already re-kinds every event to a request.
		ev.Kind = frontend.DiagnosticKindRequest
	}
	s.d.Support.Observe(ev)
}

func (s *Server) observePlaybackReported(pr *Request) {
	if s == nil || s.d.Support == nil || pr == nil || pr.User == nil {
		return
	}
	s.d.Support.Observe(s.supportEvent(pr, frontend.DiagnosticKindPlaybackReported))
}

// observePlaybackDecision records a structured resolution for the support
// capture. Viewer identity follows the same explicit export flags as requests;
// the structured playback object itself never carries usernames or addresses.
func (s *Server) observePlaybackDecision(ctx context.Context, pr *Request, p *playRequest, ps *frontend.PlaySession) {
	if s == nil || s.d.Support == nil || s.d.DecisionTraces == nil || pr == nil || p == nil {
		return
	}
	tr, ok := s.d.DecisionTraces.LatestDecision(p.key)
	if !ok {
		return
	}
	requested := p.partReleaseID
	if requested == "" && p.mediaIndex > 0 {
		requested = s.resolveVariantSelection(ctx, pr, p)
	}
	pb := frontend.DiagnosticPlaybackDecision{
		TraceScope:         "latest-title-trace-not-request-bound",
		Profile:            tr.Profile,
		ProfileMaxHeight:   tr.ProfileMaxHeight,
		ClientMaxHeight:    tr.ClientMaxHeight,
		EffectiveMaxHeight: tr.EffectiveMaxHeight,
		AllowHDR:           tr.AllowHDR,
		AllowDolbyVision:   tr.AllowDolbyVision,
		Chooser:            tr.Chooser,
		ChosenRelease:      tr.ChosenVersion,
		RequestedPartID:    p.requestedPart,
		Variants:           s.playbackVariants(pr, p.key, tr, requested, p.requestedPart),
	}
	for _, c := range tr.Candidates {
		pb.Candidates = append(pb.Candidates, frontend.DiagnosticPlaybackCandidate{
			Name: c.Name, Height: c.Height, Size: c.Size, Score: c.Score,
			Rejected: c.Rejected, Reason: c.Reason, Healthy: c.Healthy, HealthChecked: c.HealthChecked,
		})
	}
	ev := s.supportEvent(pr, frontend.DiagnosticKindPlaybackDecision)
	ev.Playback = &pb
	ev.Client = s.supportClientFacts(pr, p.caps, "admission")
	if ev.SessionKey == "" && ps != nil {
		ev.SessionKey = ps.ID
	}
	s.d.Support.Observe(ev)
}

func (s *Server) playbackVariants(pr *Request, key frontend.RatingKey, tr frontend.DecisionTrace, requested string, requestedPart int64) []frontend.DiagnosticPlaybackVariant {
	if pr == nil || pr.User == nil || s.playIDs == nil {
		return nil
	}
	ids, ok := s.playIDs.releases(s.playIdentityFor(pr, key), s.now())
	if !ok || len(ids) == 0 {
		return nil
	}
	byID := make(map[string]frontend.DecisionCandidate, len(tr.Candidates))
	for _, c := range tr.Candidates {
		if c.ReleaseID != "" {
			byID[c.ReleaseID] = c
		}
	}
	out := make([]frontend.DiagnosticPlaybackVariant, 0, len(ids))
	for i, id := range ids {
		c := byID[id]
		auto := i == 0
		part := int64(key)
		if !auto && s.variantIDs != nil {
			part = s.variantIDs.idFor(key, id)
		}
		out = append(out, frontend.DiagnosticPlaybackVariant{
			PartID: part, Auto: auto, Height: c.Height, Release: c.Name,
			Requested: (requested != "" && id == requested) || (requestedPart != 0 && part == requestedPart),
		})
	}
	return out
}

func (s *Server) supportEvent(pr *Request, kind frontend.DiagnosticKind) frontend.DiagnosticEvent {
	ev := frontend.DiagnosticEvent{Time: s.now(), Kind: kind, Protocol: "plex"}
	if pr == nil {
		return ev
	}
	ev.DeviceKey = pr.ClientID
	ev.SessionKey = pr.SessionID
	ev.AttemptKey = pr.supportAttempt
	ev.DeviceName = pr.DeviceName
	ev.Device = pr.Device
	if ev.DeviceName == "" {
		ev.DeviceName = pr.Device
	}
	ev.Product = pr.Product
	ev.Model = pr.Model
	ev.Platform = pr.Platform
	ev.AppVersion = pr.Version
	if pr.RemoteIP.IsValid() {
		ev.Peer = pr.RemoteIP.String()
	}
	if pr.User != nil {
		ev.Username = pr.User.Username
		ev.ViewerID = pr.User.ID
	}
	return ev
}

// Capture the authenticated receipt before a long media handler can block.
// A start has no response status and does not imply successful delivery.
func (s *Server) observePlaybackRequestStart(r *http.Request, pr *Request) {
	if s.d.Support == nil || !s.d.Support.Recording() || pr == nil || pr.User == nil || !frontend.DiagnosticPlaybackRequestPath(r.URL.Path) {
		return
	}
	pr.supportAttempt = rand.Text()
	ev := s.supportEvent(pr, frontend.DiagnosticKindRequestStarted)
	ev.Method, ev.Route = r.Method, r.URL.Path
	ev.Query = frontend.SafeDiagnosticQuery(r.URL.Query())
	ev.Range = supportRange(r.Header.Get("Range"))
	ev.Client = s.supportClientFacts(pr, s.declaredCaps(pr), "request")
	ev.Client.Inputs = supportClientInputs(r)
	s.d.Support.Observe(ev)
}

func (s *Server) observeClientAdmission(pr *Request, caps frontend.ClientCaps) {
	if s.d.Support == nil || !s.d.Support.Recording() || pr == nil || pr.User == nil {
		return
	}
	ev := s.supportEvent(pr, frontend.DiagnosticKindClientCapabilities)
	ev.Client = s.supportClientFacts(pr, caps, "admission")
	s.d.Support.Observe(ev)
}

func (s *Server) supportClientFacts(pr *Request, effective frontend.ClientCaps, stage string) *frontend.DiagnosticClientFacts {
	if pr == nil {
		return nil
	}
	base := frontend.PlaybackDefaultsFor(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, nil)
	owner := frontend.PlaybackDefaultsFor(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, s.cfg().Clients.Overrides)
	declared := s.declaredCaps(pr)
	return &frontend.DiagnosticClientFacts{
		Stage: stage, Registry: base.ID, RegistryMaxHeight: base.MaxHeight, OwnerMaxHeight: owner.MaxHeight,
		DeclaredMaxHeight: declared.MaxHeight, MaxHeight: effective.MaxHeight, ExplicitMaxHeight: effective.ExplicitMaxHeight,
		MaxBitrateKbps: effective.MaxBitrateKbps, DefaultRemoteBitrate: effective.DefaultRemoteBitrate, Remote: effective.Remote,
		HDR10: effective.HDR10, DolbyVision: effective.DolbyVision,
		VideoCodecs: effective.VideoCodecs, AudioCodecs: effective.AudioCodecs, Containers: effective.Containers,
	}
}

func supportClientInputs(r *http.Request) []frontend.DiagnosticClientInput {
	q := r.URL.Query()
	var out []frontend.DiagnosticClientInput
	for _, name := range []string{"X-Plex-Client-Identifier", "X-Plex-Product", "X-Plex-Platform", "X-Plex-Device", "X-Plex-Model", "X-Plex-Version", "X-Plex-Client-Profile-Name", "X-Plex-Client-Profile-Extra", "X-Plex-Client-Capabilities"} {
		in := frontend.DiagnosticClientInput{Name: name}
		for k, vs := range r.Header {
			if strings.EqualFold(k, name) {
				in.Header = true
				in.HeaderEmpty = len(vs) == 0 || strings.TrimSpace(vs[0]) == ""
				break
			}
		}
		for k, vs := range q {
			if strings.EqualFold(k, name) {
				in.Query = true
				in.QueryEmpty = len(vs) == 0 || strings.TrimSpace(vs[0]) == ""
				break
			}
		}
		out = append(out, in)
	}
	return out
}

// Range is a protocol value, never free-form header text.
func supportRange(v string) string {
	if v == "" {
		return ""
	}
	if len(v) > 80 || !strings.HasPrefix(v, "bytes=") {
		return "omitted"
	}
	parts := strings.Split(strings.TrimPrefix(v, "bytes="), "-")
	if len(parts) != 2 || (parts[0] == "" && parts[1] == "") {
		return "omitted"
	}
	for _, part := range parts {
		if len(part) > 19 {
			return "omitted"
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return "omitted"
			}
		}
	}
	return v
}
