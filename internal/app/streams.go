package app

import (
	"context"
	"io"
	"math"
	"strings"

	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/langfeat"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/subtitles"
)

// subtitleUser is u with the subtitle languages of the viewer's stream
// preferences (Viewers > Defaults or their override), which win over the
// account's own list.
func (a *App) subtitleUser(ctx context.Context, u *core.User) *core.User {
	if u == nil {
		return nil
	}
	copy := *u
	if a.trackprefs != nil {
		rules, err := a.trackprefs.GetRules(ctx, u.ID)
		if err == nil && len(rules.SubtitleLanguages) > 0 {
			copy.SubtitleLanguages = rules.SubtitleLanguages
		}
	}
	return &copy
}

// subtitlePrefs is the viewer's subtitle mode/style, used when listing
// sidecars already on disk. It does not cause a download.
func (a *App) subtitlePrefs(ctx context.Context, u *core.User) subtitles.Preferences {
	if u == nil || a.trackprefs == nil {
		return subtitles.Preferences{}
	}
	rules, err := a.trackprefs.GetRules(ctx, u.ID)
	if err != nil {
		return subtitles.Preferences{}
	}
	return subtitles.Preferences{Mode: rules.SubtitleMode, Style: rules.SubtitleStyle}
}

func (a *App) enrichSubtitles(ctx context.Context, t *core.Title, u *core.User, res *core.Resolution, src core.MediaSource, info *core.MediaInfo) {
	a.subtitles.Enrich(ctx, t, a.subtitleUser(ctx, u), res, src, info, a.subtitlePrefs(ctx, u))
}

var (
	_ frontend.SubtitleSidecarSource  = subtitleSurface{}
	_ frontend.SubtitleSidecarCatalog = subtitleSurface{}
	_ frontend.SubtitleSidecarSearch  = subtitleSurface{}
	_ frontend.SubtitleSidecarUpload  = subtitleSurface{}
)

func (s subtitleSurface) OpenFormat(id, format string) (io.ReadCloser, string, error) {
	return s.app.subtitles.OpenFormat(id, format)
}

func (s subtitleSurface) OpenFormatFrom(id, format, source string) (io.ReadCloser, string, error) {
	return s.app.subtitles.OpenFormatFrom(id, format, source)
}

func (s subtitleSurface) CachedSidecars(ctx context.Context, t *core.Title, u *core.User, embedded []core.StreamInfo) []core.StreamInfo {
	out := s.app.subtitles.CachedSidecars(t, s.app.subtitleUser(ctx, u), embedded, s.app.subtitlePrefs(ctx, u))
	if !s.app.subtitles.Enabled() {
		out = append(out, s.app.subtitles.ManualSidecars(t, embedded)...)
	}
	return out
}

func (s subtitleSurface) SearchSubtitles(ctx context.Context, t *core.Title, _ *core.User, lang, release string) ([]frontend.SubtitleSearchOffer, error) {
	asked := lang
	cands, searchErr := s.app.subtitles.SearchOnDemand(ctx, t, lang, release)
	if searchErr != nil {
		cands = nil
	}
	out := make([]frontend.SubtitleSearchOffer, 0, len(cands))
	for _, c := range cands {
		if c.Language != "" {
			lang = c.Language
		}
		title := c.Release
		if title == "" {
			title = strings.TrimSuffix(c.FileName, ".srt")
		}
		provider := c.Provider
		if provider == "" {
			provider = "OpenSubtitles"
		}
		out = append(out, frontend.SubtitleSearchOffer{
			ID: c.FileID, Language: lang, Title: title, Provider: provider,
			Score: int(math.Round(c.Score)), HearingImpaired: c.HearingImp, Forced: c.Forced,
			PerfectMatch: c.HashMatch || (release != "" && strings.EqualFold(c.Release, release)),
		})
	}
	if s.app.lang != nil {
		for _, o := range s.app.lang.Offers(t, asked) {
			out = append(out, frontend.SubtitleSearchOffer{
				ID: o.ID, Language: o.Language, Title: o.Title, Provider: o.Provider,
			})
		}
	}
	if searchErr != nil && len(out) == 0 {
		return nil, searchErr
	}
	return out, nil
}

func (s subtitleSurface) DownloadSubtitle(ctx context.Context, t *core.Title, user *core.User, id int64) (core.StreamInfo, error) {
	if s.app.lang != nil && langfeat.IsReserved(id) {
		st, err := s.app.lang.Fulfill(ctx, user, t, id)
		if err == nil || !errors.Is(err, core.ErrNotFound) {
			return st, err
		}
	}
	st, err := s.app.subtitles.DownloadOnDemand(ctx, t, id)
	if err != nil {
		return st, err
	}
	clientID := ""
	if pr := plex.FromContext(ctx); pr != nil {
		clientID = pr.ClientID
	}
	s.app.startSubtitleCorrection(ctx, t, st.ExternalURL, s.app.playSession(user, t, clientID))
	return st, nil
}

func (a *App) playSession(user *core.User, title *core.Title, clientID string) *core.PlaySession {
	if user == nil || title == nil {
		return nil
	}
	return a.playSessionOn("", user.ID, clientID, title.Key)
}

type playSessionFinder interface {
	SessionFor(int64, string, core.RatingKey) (*core.PlaySession, bool)
	SessionForSurfaceClient(string, int64, string, core.RatingKey) (*core.PlaySession, bool)
}

func (a *App) playSessionOn(surface string, userID int64, clientID string, key core.RatingKey) *core.PlaySession {
	if a == nil || a.playback == nil {
		return nil
	}
	return lookupPlaySession(a.playback, surface, userID, clientID, key)
}

// lookupPlaySession finds the live session for this pick. Plex identities
// have an empty surface; Jellyfin/Emby register with surface=protocol and
// a variant, so SessionFor (no surface, no variant) always misses them.
func lookupPlaySession(p playSessionFinder, surface string, userID int64, clientID string, key core.RatingKey) *core.PlaySession {
	if p == nil || clientID == "" {
		return nil
	}
	if surface != "" && surface != "plex" {
		if sess, ok := p.SessionForSurfaceClient(surface, userID, clientID, key); ok {
			return sess
		}
		return nil
	}
	if sess, ok := p.SessionFor(userID, clientID, key); ok {
		return sess
	}
	return nil
}

func (a *App) startSubtitleCorrection(ctx context.Context, t *core.Title, id string, sess *core.PlaySession) {
	if a.subtitles == nil {
		return
	}
	if sess != nil {
		if sctx := a.playback.SessionContext(sess.ID); sctx != nil {
			ctx = sctx
		}
	}
	a.subtitles.StartCorrection(ctx, t, id, a.liveReleaseAccess(sess))
}

func (a *App) liveReleaseAccess(sess *core.PlaySession) subtitles.ReleaseAccess {
	if sess == nil || a.playback == nil {
		return subtitles.ReleaseAccess{}
	}
	src := a.playback.SourceKey(sess.ID)
	if src == "" && sess.Resolution != nil {
		src = sess.Resolution.Release.ID
	}
	if src == "" {
		return subtitles.ReleaseAccess{}
	}
	id := sess.ID
	access := subtitles.ReleaseAccess{
		URL:      a.playback.InputURL(id),
		Duration: sess.Info.Duration,
		Info:     &sess.Info,
		Source:   src,
		Hold: func() (io.ReaderAt, int64, func(), bool) {
			return a.playback.SourceReader(id)
		},
	}
	if a.transcoder != nil {
		access.FFmpeg = a.transcoder.FFmpegPath()
	}
	return access
}

func (s subtitleSurface) UploadSubtitle(_ context.Context, t *core.Title, _ *core.User, filename, format, language string, body []byte) (core.StreamInfo, error) {
	return s.app.subtitles.StoreUpload(t, filename, format, language, body)
}

// markerDialogue is markers.Options.Dialogue: cue times from subtitle text
// the system already holds for the played release (cached sidecars), for
// stinger detection. It never fetches video or touches the network.
func (a *App) markerDialogue(_ context.Context, _ core.RatingKey, info core.MediaInfo) ([]markers.DialogueCue, bool) {
	cues, ok := a.subtitles.CuesForInfo(info)
	if !ok {
		return nil, false
	}
	out := make([]markers.DialogueCue, len(cues))
	for i, c := range cues {
		out[i] = markers.DialogueCue{Start: c.Start, End: c.End}
	}
	return out, true
}
