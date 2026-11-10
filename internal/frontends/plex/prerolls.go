package plex

import (
	"context"
	"net/http"
	"os"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Owner pre-rolls (#298) play like Cinema Trailers: they are extras in the
// play queue, direct-play MP4s that Ether converted itself, and they mask
// the feature's startup by pre-buffering it while they play. With no
// pre-roll configured, nothing here changes a response.

// prerollSource marks a frontend.ExtraClip built from a pre-roll.
const prerollSource = "preroll"

// prerollExtra returns a ready pre-roll as an extra.
func (s *Server) prerollExtra(ctx context.Context, key frontend.RatingKey) (frontend.ExtraClip, frontend.PrerollClip, bool) {
	if s.d.Prerolls == nil || !key.IsPreroll() {
		return frontend.ExtraClip{}, frontend.PrerollClip{}, false
	}
	clip, ok := s.d.Prerolls.Clip(ctx, key)
	if !ok {
		return frontend.ExtraClip{}, frontend.PrerollClip{}, false
	}
	return frontend.ExtraClip{
		Key: clip.Key, Title: clip.Name, Subtype: "trailer", ExtraType: 1,
		Duration: clip.Duration, Size: clip.Height, Source: prerollSource,
		Official: true, PublishedAt: clip.UpdatedAt,
	}, clip, true
}

// prerollInfo is the converted clip's media info (H.264 High, AAC stereo,
// faststart MP4).
func prerollInfo(clip frontend.PrerollClip) frontend.MediaInfo {
	return frontend.MediaInfo{
		Container: "mp4", Duration: clip.Duration, Size: clip.Size, BitrateKbps: clip.BitrateKbps,
		Streams: []frontend.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Profile: "high", Width: clip.Width, Height: clip.Height, PixelFormat: "yuv420p", Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Profile: "lc", Channels: 2, SampleRate: 48000, Default: true},
		},
	}
}

func prerollStamp(clip frontend.PrerollClip) string {
	return strconv.FormatInt(clip.UpdatedAt.Unix(), 10)
}

// prerollMetadata renders a pre-roll as a trailer clip with its own
// thumbnail; it has no parent.
func (s *Server) prerollMetadata(e frontend.ExtraClip, clip frontend.PrerollClip, withMedia bool) model.Metadata {
	key := e.Key.String()
	thumb := "/library/metadata/" + key + "/thumb/" + prerollStamp(clip)
	md := model.Metadata{
		RatingKey: key,
		Key:       "/library/metadata/" + key,
		GUID:      guidFor(&frontend.Title{Key: e.Key}),
		Type:      "clip",
		Subtype:   e.Subtype,
		Title:     e.Title,
		Thumb:     thumb,
		Art:       thumb,
		Duration:  ms(e.Duration),
		Extra:     []model.Attr{model.A("extraType", e.ExtraType)},
	}
	if withMedia {
		syn := &frontend.Title{Key: e.Key, Type: frontend.TypeMovie, Title: e.Title, Runtime: e.Duration}
		md.Media = []model.Media{buildMedia(syn, prerollInfo(clip), 1)}
	}
	return md
}

// prerollPlayback serves the universal decision/start endpoints for a
// pre-roll key.
func (s *Server) prerollPlayback(w http.ResponseWriter, r *http.Request, key frontend.RatingKey) {
	ctx := r.Context()
	pr := FromContext(ctx)
	e, _, ok := s.prerollExtra(ctx, key)
	if !ok || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, pr.Remote, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	s.serveExtraPlayback(w, r, pr, key, e, &frontend.Title{Type: frontend.TypeMovie})
}

// prerollPart direct-plays the converted MP4 with byte ranges.
func (s *Server) prerollPart(w http.ResponseWriter, r *http.Request, key frontend.RatingKey) {
	ctx := r.Context()
	pr := FromContext(ctx)
	e, clip, ok := s.prerollExtra(ctx, key)
	if !ok || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, pr.Remote, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	f, err := os.Open(clip.Path)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.prewarmFeature(ctx, pr, s.featureForExtra(pr, e), s.clientCaps(pr))
	if r.Method != http.MethodHead {
		s.prerollStarted(ctx, pr, e)
	}
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// prerollStarted records the play against the feature it precedes.
func (s *Server) prerollStarted(ctx context.Context, pr *Request, e frontend.ExtraClip) {
	if e.Source != prerollSource || s.d.Prerolls == nil || pr.User == nil {
		return
	}
	play := frontend.PrerollPlay{Viewer: pr.User, Key: e.Key, Client: pr.Product}
	if feature := s.featureForExtra(pr, e); feature != 0 {
		play.Feature = feature
		if t, err := s.metadataTitle(ctx, pr, feature); err == nil && t != nil {
			play.FeatureTitle = prerollFeatureTitle(t)
		}
	}
	s.d.Prerolls.Started(context.WithoutCancel(ctx), play)
}

func prerollFeatureTitle(t *frontend.Title) string {
	if t.Type == frontend.TypeEpisode && t.ShowTitle != "" {
		return t.ShowTitle + " · S" + strconv.Itoa(t.Season) + "E" + strconv.Itoa(t.Episode) + " · " + t.Title
	}
	return t.Title
}

// prerollThumb serves a pre-roll's thumbnail for /library/metadata images.
func (s *Server) prerollThumb(w http.ResponseWriter, r *http.Request, key frontend.RatingKey) bool {
	if s.d.Prerolls == nil || !key.IsPreroll() {
		return false
	}
	path, ok := s.d.Prerolls.Thumb(r.Context(), key)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, "", st.ModTime(), f)
	return true
}

// prerollKey picks the pre-roll for a fresh movie or episode play, or 0.
// A client that asked for Cinema Trailers (extrasPrefixCount) declared a
// fresh play; otherwise a saved resume point means a resume.
func (s *Server) prerollKey(ctx context.Context, pr *Request, feature frontend.RatingKey, cinema bool) (frontend.RatingKey, string) {
	if s.d.Prerolls == nil || pr.User == nil || (feature.Kind() != frontend.KindMovie && feature.Kind() != frontend.KindEpisode) || !s.d.Prerolls.Active(ctx) {
		return 0, ""
	}
	t, err := s.metadataTitle(ctx, pr, feature)
	if err != nil || t == nil || !s.visibleTitle(ctx, pr.User, t) {
		return 0, ""
	}
	resume := false
	if !cinema && s.d.State != nil {
		if st, err := s.d.State.Get(ctx, pr.User.ID, feature); err == nil && st.ViewOffset > 0 {
			resume = true
		}
	}
	clip, ok := s.d.Prerolls.Pick(ctx, frontend.PrerollRequest{
		Viewer: pr.User, Device: frontend.ClassifyDevice(pr.Product, pr.Platform, pr.Device), Away: pr.Remote,
		Feature: t, Resume: resume, Client: pr.Product, At: s.now(),
	})
	if !ok {
		return 0, ""
	}
	return clip.Key, clip.Order
}
