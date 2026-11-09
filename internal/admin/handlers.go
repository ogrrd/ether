package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/datafs"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// ---- Setup status ----

// SetupStep is one wizard step's completion state.
type SetupStep struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Complete bool   `json:"complete"`
	Required bool   `json:"required"`
	Detail   string `json:"detail,omitempty"`
}

// SetupStatus derives wizard progress from the stored config.
func SetupStatus(cfg config.Config) (steps []SetupStep, complete bool) {
	plexDone := cfg.Plex.Claimed && cfg.Plex.Token != ""
	plexDetail := "not signed in"
	if plexDone {
		plexDetail = "claimed"
		if cfg.Plex.OwnerUsername != "" {
			plexDetail = "claimed by " + cfg.Plex.OwnerUsername
		}
	} else if cfg.Plex.Token != "" {
		plexDetail = "signed in, not claimed"
	}
	// Sources: a debrid account or a Usenet provider; either is enough.
	enabledProviders, enabledDebrid := 0, 0
	for _, p := range cfg.Usenet.Providers {
		if p.Enabled {
			enabledProviders++
		}
	}
	for _, p := range cfg.Debrid.Providers {
		if p.Enabled && strings.TrimSpace(p.APIKey) != "" {
			enabledDebrid++
		}
	}
	enabledIndexers, keylessSources := 0, 0
	for _, ix := range cfg.Indexers {
		if !ix.Enabled {
			continue
		}
		enabledIndexers++
		// A Stremio add-on is a source on its own: it needs no debrid or Usenet account.
		if ix.IsStremio() && strings.TrimSpace(ix.URL) != "" {
			keylessSources++
		}
	}
	cacheDone := strings.TrimSpace(cfg.Cache.Dir) != "" && cfg.Cache.BudgetBytes >= 0
	sourcesDone := enabledProviders+enabledDebrid+keylessSources > 0
	steps = []SetupStep{
		{ID: "plex", Title: "Sign in to plex.tv and claim", Complete: plexDone, Required: true, Detail: plexDetail},
		{ID: "sources", Title: "Debrid accounts or Usenet providers", Complete: sourcesDone, Required: true,
			Detail: fmt.Sprintf("%d debrid, %d Usenet, %d addon", enabledDebrid, enabledProviders, keylessSources)},
		{ID: "indexers", Title: "Indexers", Complete: enabledIndexers > 0, Required: true,
			Detail: fmt.Sprintf("%d enabled", enabledIndexers)},
		{ID: "cache", Title: "Cache folder and disk budget", Complete: cacheDone, Required: true, Detail: cfg.Cache.Dir},
		// Without TMDB there is no catalogue; a build may bundle a key.
		{ID: "catalog", Title: "TMDB API key", Complete: tmdb.ResolveKey(cfg.Catalog) != "", Required: true},
		{ID: "optional", Title: "Optional integrations", Complete: true, Required: false,
			Detail: "Trakt, OpenSubtitles, LLM, cam policy"},
	}
	complete = cfg.SetupComplete
	return steps, complete
}

// setupStatus uses the existing Plex requirement for legacy installs; once
// front ends are selected, a Plex-free household instead needs a local owner.
// A local owner also satisfies the step when Plex stays on but is connected later.
func (h *Handler) setupStatus(r *http.Request) ([]SetupStep, bool) {
	cfg := h.deps.Config.Get()
	steps, complete := SetupStatus(cfg)
	local := h.deps.LocalOwnerConfigured != nil && h.deps.LocalOwnerConfigured(r.Context())
	plexOff := cfg.Frontends != nil && !cfg.Frontends["plex"].Enabled
	for i := range steps {
		if steps[i].ID != "plex" {
			continue
		}
		if plexOff {
			steps[i].Title = "Create local owner"
			steps[i].Complete = local
			if local {
				steps[i].Detail = "created"
			} else {
				steps[i].Detail = "not created"
			}
		} else if local && !steps[i].Complete {
			steps[i].Complete = true
			steps[i].Detail = "connect Plex later"
		}
	}
	return steps, complete
}

func plainSetupGap(id string) string {
	switch id {
	case "sources":
		return "Add a debrid account, a Usenet provider, or a torrent addon before finishing, or finish now and add it later."
	case "indexers":
		return "Add an indexer before finishing, or finish now and add it later."
	case "catalog":
		return "Add a catalogue key before finishing, or finish now and add it later."
	case "cache":
		return "Choose a cache folder before finishing."
	default:
		return "Finish the remaining setup step, or finish now and add it later."
	}
}

func (h *Handler) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	steps, complete := h.setupStatus(r)
	requiredDone := true
	for _, s := range steps {
		if s.Required && !s.Complete {
			requiredDone = false
		}
	}
	cacheRestartRequired := h.deps.CacheRestartRequired != nil && h.deps.CacheRestartRequired()
	body := map[string]any{
		"setupComplete": complete, "requiredComplete": requiredDone, "steps": steps,
		"cacheRestartRequired": cacheRestartRequired,
	}
	if cacheVolumeMounted() {
		body["cacheVolume"] = "/cache"
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *Handler) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Force bool `json:"force"`
	}
	_ = decodeJSONLenient(r, &body) // empty body tolerated
	steps, _ := h.setupStatus(r)
	// Never even when forced: after setup only the server's Plex owner can
	// open the admin UI, so finishing unclaimed would lock everyone out.
	for _, s := range steps {
		if s.ID == "plex" && !s.Complete {
			writeError(w, http.StatusConflict, "plex_required",
				"create a local owner or claim Plex before completing setup")
			return
		}
	}
	if !body.Force {
		for _, s := range steps {
			if s.Required && !s.Complete {
				writeError(w, http.StatusConflict, "setup_incomplete", plainSetupGap(s.ID))
				return
			}
		}
	}
	if err := h.deps.Config.Update(func(c *config.Config) error { c.SetupComplete = true; return nil }); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", "could not save configuration")
		return
	}
	h.log.Info("admin: setup marked complete", "forced", body.Force)
	h.handleSetupStatus(w, r)
}

// handleSetupReopen lets the owner run the wizard again. Once setup is
// complete this route is reachable only with an owner credential, and the
// wizard works with that credential, so setup stays marked complete:
// clearing it would reopen the tokenless local-network access meant for a
// fresh install, letting any device on the LAN (or any client of a
// reverse proxy on this host) reconfigure the server, replace its plex.tv
// sign-in and claim it to another account.
func (h *Handler) handleSetupReopen(w http.ResponseWriter, r *http.Request) {
	h.handleSetupStatus(w, r)
}

// ---- Plex sign-in ----

func (h *Handler) handlePINStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.TestTimeout)
	defer cancel()
	id, code, authURL, err := h.deps.PlexAuth.StartPIN(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "plex_pin_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "code": code, "authUrl": authURL})
}

func (h *Handler) handlePINPoll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.TestTimeout)
	defer cancel()
	done, err := h.deps.PlexAuth.PollPIN(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "plex_pin_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": done})
}

func (h *Handler) handleClaim(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.TestTimeout)
	defer cancel()
	if err := h.deps.PlexAuth.Claim(ctx); err != nil {
		msg := err.Error()
		if errors.Is(err, plextv.ErrClaimExchangeFailed) {
			msg = "Plex couldn't link this server. Try again in a minute."
		}
		writeError(w, http.StatusBadGateway, "plex_claim_failed", msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleSyncPlexFriends(w http.ResponseWriter, r *http.Request) {
	cfg := h.deps.Config.Get()
	if !cfg.Plex.Claimed || cfg.Plex.Token == "" {
		writeError(w, http.StatusConflict, "plex_not_claimed", "Sign in and claim this server first.")
		return
	}
	syncer, ok := h.deps.PlexAuth.(PlexFriendSyncer)
	if !ok || syncer == nil {
		writeError(w, http.StatusServiceUnavailable, "plex_unavailable", "Plex friend sync is unavailable.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.TestTimeout)
	defer cancel()
	if err := syncer.SyncFriends(ctx); err != nil {
		writeError(w, http.StatusBadGateway, "plex_friends_sync_failed", err.Error())
		return
	}
	if h.deps.RefreshViewers != nil {
		_ = h.deps.RefreshViewers(ctx)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"detail": h.safeStatus(r, func(context.Context) any { return h.deps.PlexAuth.Status() }),
	})
}

func (h *Handler) handlePlexStatus(w http.ResponseWriter, r *http.Request) {
	cfg := h.deps.Config.Get()
	out := map[string]any{
		"claimed":       cfg.Plex.Claimed,
		"signedIn":      cfg.Plex.Token != "",
		"ownerUsername": cfg.Plex.OwnerUsername,
		"ownerPlexId":   cfg.Plex.OwnerPlexID,
		"detail":        h.safeStatus(r, func(context.Context) any { return h.deps.PlexAuth.Status() }),
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- Testers ----

type testResult struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

func (h *Handler) runTest(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context) error) {
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.TestTimeout)
	defer cancel()
	start := time.Now()
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("tester panicked: %v", rec)
			}
		}()
		return fn(ctx)
	}()
	res := testResult{OK: err == nil, DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
	}
	// A failed test is a successful API call: 200 with ok:false so the
	// wizard can show the provider's error inline.
	writeJSON(w, http.StatusOK, res)
}

// Testers take unsaved settings. A secret the UI sent back as the
// placeholder is replaced by the stored one only while it is bound to the
// same entry and destination (UnredactConfig); otherwise the request is
// refused, so a test can never send a stored secret somewhere new. Single
// settings objects are decoded onto the redacted stored value, so leaving
// a secret out behaves like sending the placeholder.

// decodeTestBody decodes optional tester settings onto dst: an empty body
// keeps dst, a malformed one is an error (so a half-parsed request never
// runs a test with stored secrets).
func decodeTestBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		writeError(w, http.StatusBadRequest, "bad_json", "could not read request body")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(dst); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		return false
	}
	return true
}

func writeSecretRequired(w http.ResponseWriter, errs []FieldError) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": map[string]any{
			"code": "secret_required", "message": "enter the secret again to test changed connection settings", "fields": errs,
		},
	})
}

func (h *Handler) handleTestUsenet(w http.ResponseWriter, r *http.Request) {
	var p config.UsenetProvider
	if err := decodeJSONLenient(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	var perrs placeholderErrors
	p = perrs.unredactProvider("password", p, h.deps.Config.Get().Usenet.Providers)
	if len(perrs) > 0 {
		writeSecretRequired(w, perrs)
		return
	}
	if errs := ValidateConfig(withProvider(p)); len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.TestUsenet(ctx, p) })
}

func withProvider(p config.UsenetProvider) config.Config {
	c := config.Defaults(os.TempDir())
	c.Usenet.Providers = []config.UsenetProvider{p}
	return c
}

func (h *Handler) handleTestIndexer(w http.ResponseWriter, r *http.Request) {
	var ix config.Indexer
	if err := decodeJSONLenient(r, &ix); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	prev := h.deps.Config.Get()
	var perrs placeholderErrors
	ix = perrs.unredactIndexer("indexer", ix, prev.IndexerSites.FlareSolverrURL, prev)
	if len(perrs) > 0 {
		writeSecretRequired(w, perrs)
		return
	}
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.TestIndexer(ctx, ix) })
}

func (h *Handler) handleTestDebrid(w http.ResponseWriter, r *http.Request) {
	var p config.DebridProvider
	if err := decodeJSONLenient(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	var perrs placeholderErrors
	p = perrs.unredactDebrid("apiKey", p, h.deps.Config.Get().Debrid.Providers)
	if len(perrs) > 0 {
		writeSecretRequired(w, perrs)
		return
	}
	c := config.Defaults(os.TempDir())
	c.Debrid.Providers = []config.DebridProvider{p}
	if errs := ValidateConfig(c); len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.TestDebrid(ctx, p) })
}

func (h *Handler) handleTestIntroDB(w http.ResponseWriter, r *http.Request) {
	if h.deps.TestIntroDB == nil {
		writeError(w, http.StatusNotImplemented, "not_available", "TheIntroDB test is not available")
		return
	}
	var body struct {
		APIKey string `json:"apiKey"`
	}
	if !decodeTestBody(w, r, &body) {
		return
	}
	key := body.APIKey
	if isRedacted(key) || key == "" {
		key = h.deps.Config.Get().Markers.APIKey
	}
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.TestIntroDB(ctx, key) })
}

func (h *Handler) handleTestTMDB(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if !decodeTestBody(w, r, &body) {
		return
	}
	key := body.Key
	// TMDB is a fixed destination: an empty or placeholder key tests the
	// stored (or bundled) key.
	if isRedacted(key) || key == "" {
		key = h.deps.Config.Get().Catalog.TMDBKey
	}
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.TestTMDB(ctx, key) })
}

func (h *Handler) handleTestOpenSubtitles(w http.ResponseWriter, r *http.Request) {
	prev := h.deps.Config.Get().OpenSubtitles
	cfg := redactOpenSubtitles(prev)
	if !decodeTestBody(w, r, &cfg) {
		return
	}
	var perrs placeholderErrors
	cfg = perrs.unredactOpenSubtitles(cfg, prev)
	if len(perrs) > 0 {
		writeSecretRequired(w, perrs)
		return
	}
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.TestOpenSubtitles(ctx, cfg) })
}

// llmSettings decodes posted (unsaved) language-model settings onto the
// stored ones and restores a bound API key.
func (h *Handler) llmSettings(w http.ResponseWriter, r *http.Request) (config.LLM, bool) {
	prev := h.deps.Config.Get().LLM
	cfg := redactLLM(prev)
	if !decodeTestBody(w, r, &cfg) {
		return cfg, false
	}
	var perrs placeholderErrors
	cfg = perrs.unredactLLM(cfg, prev)
	if len(perrs) > 0 {
		writeSecretRequired(w, perrs)
		return cfg, false
	}
	return cfg, true
}

func (h *Handler) handleTestLLM(w http.ResponseWriter, r *http.Request) {
	cfg, ok := h.llmSettings(w, r)
	if !ok {
		return
	}
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.TestLLM(ctx, cfg) })
}

// LLMModel is one model a language-model provider offers.
type LLMModel struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

func (h *Handler) handleLLMProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": config.LLMProviders})
}

// handleLLMModels lists models for the posted (unsaved) settings. Like the
// testers, a provider failure is a successful call carrying the provider's
// message, so the model picker can show it inline.
func (h *Handler) handleLLMModels(w http.ResponseWriter, r *http.Request) {
	if h.deps.LLMModels == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "model listing is not available")
		return
	}
	cfg, ok := h.llmSettings(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.TestTimeout)
	defer cancel()
	models, err := h.deps.LLMModels(ctx, cfg)
	res := struct {
		Models []LLMModel `json:"models"`
		Error  string     `json:"error,omitempty"`
	}{Models: models}
	if res.Models == nil {
		res.Models = []LLMModel{}
	}
	if err != nil {
		res.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) handleTestTrakt(w http.ResponseWriter, r *http.Request) {
	prev := h.deps.Config.Get().Trakt
	cfg := redactTrakt(prev)
	if !decodeTestBody(w, r, &cfg) {
		return
	}
	var perrs placeholderErrors
	cfg = perrs.unredactTrakt(cfg, prev)
	if len(perrs) > 0 {
		writeSecretRequired(w, perrs)
		return
	}
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.TestTrakt(ctx, cfg) })
}

// ---- Dashboard / status / traces ----

func (h *Handler) statusProviders() map[string]StatusFunc {
	out := map[string]StatusFunc{}
	add := func(name string, fn StatusFunc) {
		if fn != nil {
			out[name] = fn
		}
	}
	add("sessions", h.deps.SessionsStatus)
	add("cache", h.deps.CacheStatus)
	add("providers", h.deps.ProvidersStatus)
	add("indexers", h.deps.IndexersStatus)
	add("catalog", h.deps.CatalogStatus)
	add("remote", h.deps.RemoteStatus)
	add("hwaccel", h.deps.HWAccelStatus)
	for name, fn := range h.deps.ExtraStatus {
		add(name, fn)
	}
	return out
}

// safeStatus calls a provider with a timeout and panic recovery, in the
// request goroutine (providers are expected to be quick snapshots).
func (h *Handler) safeStatus(r *http.Request, fn StatusFunc) any {
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.StatusTimeout)
	defer cancel()
	var out any
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				h.log.Error("admin: status provider panicked", "panic", rec)
				out = map[string]any{"error": fmt.Sprintf("status provider failed: %v", rec)}
			}
		}()
		out = fn(ctx)
	}()
	if ctx.Err() == context.DeadlineExceeded {
		return map[string]any{"error": "status provider timed out"}
	}
	return out
}

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.dashboardBody(r))
}

func (h *Handler) handleSystemLoad(w http.ResponseWriter, r *http.Request) {
	if h.deps.SystemLoad == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "system load is not available")
		return
	}
	writeJSON(w, http.StatusOK, h.safeStatus(r, h.deps.SystemLoad))
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	fn, ok := h.statusProviders()[name]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_status", "no status provider named "+strconv.Quote(name))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "value": h.safeStatus(r, fn)})
}

func (h *Handler) handleTraces(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.StatusTimeout)
	defer cancel()
	v, err := h.deps.ResolverTraces(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "traces_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"traces": v})
}

// ---- Disk ----

func (h *Handler) handleDisk(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = h.deps.Config.Get().Cache.Dir
	}
	// Statfs needs an existing path: walk up to the nearest existing parent.
	probe := path
	for probe != "" {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	usage := datafs.Usage
	if h.deps.DiskUsage != nil {
		usage = h.deps.DiskUsage
	}
	total, free, err := usage(probe)
	if err != nil {
		writeError(w, http.StatusBadRequest, "disk_failed", "could not read free space for "+path+": "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "probedPath": probe,
		"totalBytes": total, "freeBytes": free, "usedBytes": total - free,
		"cacheBudgetBytes": h.deps.Config.Get().Cache.BudgetBytes,
	})
}

// ---- Users ----

func (h *Handler) handleListUsers(w http.ResponseWriter, r *http.Request) {
	records, err := h.storedViewers(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "users_failed", err.Error())
		return
	}
	users := make([]userWithLifecycle, 0, len(records))
	for _, u := range records {
		users = append(users, toUserWithLifecycle(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (h *Handler) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "user id must be a number")
		return
	}
	// Merge onto the stored user so PATCH-style partial bodies work and
	// identity fields the client omitted are not blanked.
	existing, err := h.viewerDirectory(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "users_failed", err.Error())
		return
	}
	var u *core.User
	for i := range existing {
		if existing[i].ID == id {
			uu := existing[i]
			u = &uu
			break
		}
	}
	if u == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	// Languages belong to the viewer's Audio & subtitles rules, which mirror
	// them onto the user record; this endpoint does not write them.
	subtitles, audio := slices.Clone(u.SubtitleLanguages), slices.Clone(u.AudioLanguages)
	if err := decodeJSONLenient(r, u); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	u.ID = id // path wins over body
	u.SubtitleLanguages, u.AudioLanguages = subtitles, audio
	u.MaxContentRating = strings.TrimSpace(u.MaxContentRating)
	var invalid []FieldError
	if u.StreamLimit < 0 {
		invalid = append(invalid, FieldError{Field: "streamLimit", Message: "must be >= 0"})
	}
	if u.MaxContentRating != "" && contentrating.LevelOf(u.MaxContentRating) == contentrating.Unrated {
		invalid = append(invalid, FieldError{Field: "maxContentRating", Message: "unrecognized rating"})
	}
	if len(invalid) > 0 {
		writeValidationError(w, invalid)
		return
	}
	if err := h.deps.Users.UpdateUser(r.Context(), *u); err != nil {
		writeError(w, http.StatusBadGateway, "users_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// ---- Logs ----

func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			if n > 5000 {
				n = 5000
			}
			limit = n
		}
	}
	entries := h.logs.Entries(limit, r.URL.Query().Get("level"), r.URL.Query().Get("q"))
	if entries == nil {
		entries = []LogEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": h.logs.Len()})
}

func (h *Handler) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	h.logs.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DiskFree reports available bytes on the filesystem containing path.
func DiskFree(path string) (int64, error) { _, free, err := datafs.Usage(path); return free, err }

// DiskUsage reports the size and free bytes of the volume holding path.
func DiskUsage(path string) (total, free int64, err error) { return datafs.Usage(path) }

// handleDatabaseCompact reports (GET) or starts (POST) the owner's one-time
// database compaction. POST returns at once; the compaction runs in the
// background and GET reports it.
func (h *Handler) handleDatabaseCompact(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner {
		writeError(w, http.StatusForbidden, "owner_required", "sign in as the owner to compact the database")
		return
	}
	fn := h.deps.DatabaseCompactStatus
	if r.Method == http.MethodPost {
		fn = h.deps.DatabaseCompact
	}
	if fn == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "database compaction is not available")
		return
	}
	value, err := fn(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, "database_compact_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": value})
}

func (h *Handler) handleDatabaseCheck(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner {
		writeError(w, http.StatusForbidden, "owner_required", "sign in as the owner to check the database")
		return
	}
	if h.deps.DatabaseCheck == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "database checks are not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	value, err := h.deps.DatabaseCheck(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_check_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": value})
}
