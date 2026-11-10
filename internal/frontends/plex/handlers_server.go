package plex

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

const (
	transcoderVideoBitrates    = "64,96,208,320,720,1500,2000,3000,4000,8000,10000,12000,20000"
	transcoderVideoQualities   = "0,1,2,3,4,5,6,7,8,9,10,11,12"
	transcoderVideoResolutions = "128,128,160,240,320,480,768,720,720,1080,1080,1080,1080"
)

// APIVersion is the PMS API version Ether implements (X-Plex-Pms-Api-Version).
const APIVersion = "1.2.3"

// serverInfoAttrs are the ServerConfiguration attributes of / (full) and
// /media/providers (providers=true, which omits a few, as PMS does).
// allowSync is the viewer's own download permission: Plex Web's
// showSyncDownload gate reads server.allowSync for library providers.
func (s *Server) serverInfoAttrs(providers bool, viewer *frontend.User) []model.Attr {
	id := s.identity()
	c := s.cfg()
	signIn, mapping := "none", "unknown"
	if c.Plex.Claimed || c.Plex.Token != "" {
		signIn, mapping = "ok", "mapped"
	}
	active := 0
	if s.d.Transcoder != nil && s.d.Playback != nil {
		for _, ps := range s.d.Playback.Sessions() {
			if len(s.transcodes.forClient(ps.UserID, ps.ClientID, ps.Key)) > 0 {
				active++
			}
		}
	}
	canTranscode := s.d.Transcoder != nil
	attrs := []model.Attr{
		model.A("allowCameraUpload", false),
		model.A("allowChannelAccess", false),
		model.A("allowMediaDeletion", false),
		model.A("allowSharing", true),
		model.A("allowSync", s.downloadsAllowed(viewer)),
		model.A("allowTuners", s.liveTVEnabled()),
		model.A("apiVersion", APIVersion),
		model.A("backgroundProcessing", false),
		model.A("certificate", c.Plex.CertFile != ""),
		model.A("companionProxy", true),
		model.A("countryCode", "usa"),
		model.A("diagnostics", ""),
		model.A("eventStream", true),
		model.A("friendlyName", id.FriendlyName),
	}
	if !providers {
		attrs = append(attrs, model.A("hubSearch", true), model.A("itemClusters", false))
	}
	if c.Plex.Claimed || c.Plex.Token != "" {
		attrs = append(attrs, model.A("offlineTranscode", 1))
	}
	attrs = append(attrs,
		model.A("livetv", s.liveTVAPIVersion()),
		model.A("machineIdentifier", id.MachineIdentifier),
	)
	if !providers {
		attrs = append(attrs, model.A("mediaProviders", true), model.A("multiuser", true))
	}
	attrs = append(attrs,
		model.A("musicAnalysis", 0),
		model.A("myPlex", c.Plex.Claimed || c.Plex.Token != ""),
		model.A("myPlexMappingState", mapping),
		model.A("myPlexSigninState", signIn),
		model.A("myPlexSubscription", id.MyPlexSubscription),
		model.A("myPlexUsername", c.Plex.OwnerUsername),
		model.A("ownerFeatures", strings.Join(id.OwnerFeatures, ",")),
		model.A("platform", id.Platform),
		model.A("platformVersion", id.PlatformVersion),
		model.A("pluginHost", false),
		model.A("pushNotifications", false),
		model.A("readOnlyLibraries", false),
		model.A("streamingBrainABRVersion", 3),
		model.A("streamingBrainVersion", 2),
		model.A("sync", s.downloadsEnabled()),
		model.A("transcoderActiveVideoSessions", active),
		model.A("transcoderAudio", canTranscode),
		model.A("transcoderLyrics", false),
	)
	if !providers {
		attrs = append(attrs, model.A("transcoderPhoto", true))
	}
	attrs = append(attrs,
		model.A("transcoderSubtitles", canTranscode),
		model.A("transcoderVideo", canTranscode),
		model.A("transcoderVideoBitrates", transcoderVideoBitrates),
		model.A("transcoderVideoQualities", transcoderVideoQualities),
		model.A("transcoderVideoResolutions", transcoderVideoResolutions),
		model.A("updatedAt", s.now().Unix()),
		model.A("updater", false),
		model.A("version", id.Version),
		model.A("voiceSearch", false),
	)
	return attrs
}

var rootDirectories = []string{
	"actions", "activities", "butler", "channels", "clients", "devices", "diagnostics", "hubs", "library",
	"livetv", "media", "neighborhood", "playQueues", "playlists", "resources", "search", "server",
	"servers", "statistics", "system", "transcode", "updater", "user",
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	mc := &model.MediaContainer{Extra: s.serverInfoAttrs(false, FromContext(r.Context()).User)}
	for _, d := range rootDirectories {
		mc.Directory = append(mc.Directory, model.Directory{Count: 1, Key: d, Title: d})
	}
	mc.Size = len(mc.Directory)
	applyLegacyServerVersion(r, mc)
	s.write(w, r, mc)
}

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	id := s.identity()
	c := s.cfg()
	s.write(w, r, &model.MediaContainer{Extra: []model.Attr{
		model.A("apiVersion", APIVersion),
		model.A("claimed", c.Plex.Claimed),
		model.A("machineIdentifier", id.MachineIdentifier),
		model.A("version", id.Version),
	}})
}

// sectionPivots are the top-level navigation pivots of a section.
func sectionPivots(sec Section) []model.Pivot {
	id := strconv.Itoa(sec.ID)
	typeNum := "1"
	if sec.Type == frontend.TypeShow {
		typeNum = "2"
	}
	return []model.Pivot{
		{ID: "recommended", Key: "/hubs/sections/" + id, Type: "hub", Title: "Recommended", Context: "content.discover", Symbol: "star"},
		{ID: "library", Key: "/library/sections/" + id + "/all?type=" + typeNum, Type: "list", Title: "Library", Context: "content.library", Symbol: "library"},
		{ID: "collections", Key: "/library/sections/" + id + "/collections", Type: "list", Title: "Collections", Context: "content.collections", Symbol: "stack"},
		{ID: "playlists", Key: "/playlists?type=15&sectionID=" + id + "&playlistType=video", Type: "list", Title: "Playlists", Context: "content.playlists", Symbol: "playlist"},
		{ID: "categories", Key: "/library/sections/" + id + "/categories", Type: "list", Title: "Categories", Context: "content.categories", Symbol: "stack"},
	}
}

func agentFor(t frontend.MediaType) (agent, scanner string) {
	if t == frontend.TypeShow {
		return "tv.plex.agents.series", "Plex TV Series"
	}
	return "tv.plex.agents.movie", "Plex Movie"
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	u := FromContext(r.Context()).User
	now := s.now().Unix()
	dirs := []model.ProviderDirectory{{HubKey: "/hubs", Title: "Home"}} // discovery only: no browse key
	for _, sec := range s.userSections(u) {
		agent, scanner := agentFor(sec.Type)
		id := strconv.Itoa(sec.ID)
		dirs = append(dirs, model.ProviderDirectory{
			Agent:      agent,
			Language:   sec.Language,
			Refreshing: model.Ptr(false),
			Scanner:    scanner,
			UUID:       sec.UUID,
			ID:         id,
			Key:        "/library/sections/" + id,
			HubKey:     "/hubs/sections/" + id,
			Type:       string(sec.Type),
			Title:      sec.Title,
			UpdatedAt:  now,
			ScannedAt:  now,
			Pivot:      sectionPivots(sec),
		})
	}
	dirs = append(dirs, model.ProviderDirectory{
		ID: "playlists", Key: "/playlists", Type: "playlist", Title: "Playlists",
		Pivot: []model.Pivot{{ID: "playlists.video", Key: "/playlists?playlistType=video", Type: "list", Title: "Video", Context: "content.playlists.video", Symbol: "playlist"}},
	})
	if legacyRoku(FromContext(r.Context())) {
		for i := range dirs {
			dirs[i].Pivot = nil
			if dirs[i].ID != "playlists" {
				dirs[i].ID = ""
			}
		}
	}
	provider := model.MediaProvider{
		Identifier: model.LibraryIdentifier,
		Title:      "Library",
		Types:      "video",
		Protocols:  "stream,download",
		Feature: []model.Feature{
			{Key: "/library/sections", Type: "content", Directory: dirs},
			{Key: "/hubs/search", Type: "search"},
			{Key: "/library/matches", Type: "match"},
			{Key: "/library/metadata", Type: "metadata"},
			{Key: "/:/rate", Type: "rate"},
			{Key: "/photo/:/transcode", Type: "imagetranscoder"},
			{Key: "/hubs/promoted", Type: "promoted"},
			{Key: "/hubs/continueWatching", Type: "continuewatching"},
			{Key: "/actions", Type: "actions", Action: []model.Action{{ID: "removeFromContinueWatching", Key: "/actions/removeFromContinueWatching"}}},
			{Flavor: "universal", Key: "/playlists", Type: "playlist"},
			{Flavor: "universal", Key: "/playQueues", Type: "playqueue"},
			{Key: "/library/collections", Type: "collection"},
			{ScrobbleKey: "/:/scrobble", UnscrobbleKey: "/:/unscrobble", Key: "/:/timeline", Type: "timeline"},
			{Type: "queryParser"},
			{Key: "/library/search", Type: "universalsearch"},
		},
	}
	if s.downloadsAllowed(u) {
		provider.Feature = append(provider.Feature, downloadSubscribeFeature)
	}
	mc := &model.MediaContainer{Size: 1, Extra: s.serverInfoAttrs(true, u), MediaProvider: []model.MediaProvider{provider}}
	if p := s.liveTVProvider(r.Context(), FromContext(r.Context()).User); p != nil {
		mc.MediaProvider = append(mc.MediaProvider, *p)
		mc.Size++
	}
	applyLegacyServerVersion(r, mc)
	s.write(w, r, mc)
}

// Older Roku clients cannot consume provider pivots. PMS compares only the
// numeric components supplied by the client, so 6.4 satisfies 6.4.2.
func legacyRoku(pr *Request) bool {
	if pr.Platform != "Roku" {
		return false
	}
	parts := strings.Split(pr.Version, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return true
	}
	values := make([]int, len(parts))
	for i, part := range parts {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return true
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return true
		}
		values[i] = n
	}
	minimum := []int{6, 4, 2}
	for i, n := range values {
		if n != minimum[i] {
			return n < minimum[i]
		}
	}
	return false
}

func applyLegacyServerVersion(r *http.Request, mc *model.MediaContainer) {
	if FromContext(r.Context()).Product != "Plex Home Theater" {
		return
	}
	for i := range mc.Extra {
		if mc.Extra[i].Name != "version" {
			continue
		}
		version, ok := mc.Extra[i].Value.(string)
		if !ok {
			continue
		}
		number, build, hasBuild := strings.Cut(version, "-")
		if strings.Count(number, ".") == 3 {
			number += ".0"
			if hasBuild {
				number += "-" + build
			}
			mc.Extra[i].Value = number
		}
	}
}

func (s *Server) handleMyPlexAccount(w http.ResponseWriter, r *http.Request) {
	c := s.cfg()
	acct := model.MyPlex{
		Username:          c.Plex.OwnerUsername,
		MappingState:      "unknown",
		SignInState:       "none",
		SubscriptionState: "Unknown",
	}
	if c.Plex.Claimed || c.Plex.Token != "" {
		acct.MappingState = "mapped"
		acct.SignInState = "ok"
	}
	id := s.identity()
	acct.SubscriptionActive = id.MyPlexSubscription
	if id.MyPlexSubscription {
		acct.SubscriptionState = "Active"
		acct.SubscriptionFeatures = strings.Join(id.OwnerFeatures, ",")
	}
	if c.Plex.PublicHost != "" {
		acct.PublicAddress = c.Plex.PublicHost
		acct.PublicPort = c.Plex.PublicPort
		if acct.PublicPort == 0 {
			acct.PublicPort = c.Server.Port
		}
	}
	if host, _, err := net.SplitHostPort(r.Host); err == nil && host != "" {
		acct.PrivateAddress = host
		acct.PrivatePort = c.Server.Port
	}
	s.write(w, r, &acct)
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	id := s.identity()
	host, portS, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	port, _ := strconv.Atoi(portS)
	if port == 0 {
		port = s.cfg().Server.Port
	}
	s.write(w, r, &model.MediaContainer{Size: 1, Server: []model.Server{{
		Name: id.FriendlyName, Host: host, Address: host, Port: port,
		MachineIdentifier: id.MachineIdentifier, Version: id.Version,
	}}})
}

func (s *Server) handlePrefs(w http.ResponseWriter, r *http.Request) {
	c := s.cfg()
	id := s.identity()
	secure := map[string]int{"required": 0, "preferred": 1, "disabled": 2}[c.Plex.SecureConnections]
	if c.Plex.SecureConnections == "" {
		secure = 1
	}
	port := c.Plex.PublicPort
	if port == 0 {
		port = c.Server.Port
	}
	// Values are typed (string, int, bool) as in PMS JSON; XML renders bools
	// as 1/0.
	settings := []model.Setting{
		{ID: "FriendlyName", Label: "Friendly name", Summary: "This name will be used to identify this media server to other computers on your network.", Type: "text", Default: "", Value: id.FriendlyName, Group: "general"},
		// PMS lists these after its general settings, before MachineIdentifier.
		{ID: "OnDeckWindow", Label: "Weeks to consider for Continue Watching", Summary: "Media that has not been watched in this many weeks will not appear in Continue Watching.", Type: "int", Default: 16, Value: 16, Group: "library", Advanced: true},
		{ID: "OnDeckLimit", Label: "Maximum number of Continue Watching items which will appear.", Summary: "Limits the number of shows which will appear Continue Watching. Setting it too high can affect performance.", Type: "int", Default: plexOnDeckLimit, Value: plexOnDeckLimit, Group: "library", Advanced: true},
		{ID: "OnDeckIncludePremieres", Label: "Include season premieres in Continue Watching", Summary: "New season premieres will always appear no matter how many weeks have passed since watching.", Type: "bool", Default: true, Value: true, Group: "library", Advanced: true},
		{ID: "MachineIdentifier", Label: "", Summary: "", Type: "text", Default: "", Value: id.MachineIdentifier, Hidden: true, Group: ""},
		{ID: "PublishServerOnPlexOnlineKey", Label: "Publish server on Plex Online", Summary: "", Type: "bool", Default: false, Value: c.Plex.RemoteAccess, Group: "network"},
		{ID: "ManualPortMappingMode", Label: "Manually specify public port", Summary: "", Type: "bool", Default: false, Value: c.Plex.PublicPort != 0, Group: "network"},
		{ID: "ManualPortMappingPort", Label: "", Summary: "", Type: "int", Default: 32400, Value: port, Hidden: true, Group: ""},
		{ID: "secureConnections", Label: "Secure connections", Summary: "", Type: "int", Default: 1, Value: secure, Advanced: true, Group: "network", EnumValues: "1:Preferred|0:Required"},
		{ID: "customConnections", Label: "Custom server access URLs", Summary: "", Type: "text", Default: "", Value: strings.Join(c.Plex.CustomURLs, ","), Group: "network", Advanced: true},
		{ID: "WanPerStreamMaxUploadRate", Label: "Limit remote stream bitrate", Summary: "", Type: "int", Default: 0, Value: c.Transcode.RemoteMaxBitrateKbps, Group: "network"},
		{ID: "TranscoderTempDirectory", Label: "Transcoder temporary directory", Summary: "", Type: "text", Default: "", Value: c.Transcode.TempDir, Group: "transcoder", Advanced: true},
		{ID: "HardwareAcceleratedCodecs", Label: "Use hardware acceleration when available", Summary: "", Type: "bool", Default: true, Value: c.Transcode.HWAccel != "none", Group: "transcoder"},
		{ID: "TranscodeCountLimit", Label: "Maximum simultaneous video transcode", Summary: "", Type: "int", Default: 0, Value: c.Transcode.MaxTranscodes, Group: "transcoder", Advanced: true},
		{ID: "sendCrashReports", Label: "Send crash reports to Plex", Summary: "", Type: "bool", Default: true, Value: false, Group: "general"},
		{ID: "logDebug", Label: "Enable Plex Media Server debug logging", Summary: "", Type: "bool", Default: true, Value: c.Server.LogLevel == "debug", Group: "general", Advanced: true},
	}
	if id := r.URL.Query().Get("id"); id != "" {
		var only []model.Setting
		for _, st := range settings {
			if st.ID == id {
				only = append(only, st)
			}
		}
		settings = only
	}
	s.write(w, r, &model.MediaContainer{Size: len(settings), Setting: settings})
}

// handlePrefsSet accepts and ignores PMS settings writes; Ether is
// configured through its own admin UI.
func (s *Server) handlePrefsSet(w http.ResponseWriter, r *http.Request) {
	s.log.Info("ignoring PMS prefs write; use the Ether admin UI", "query", redactQuery(r.URL.RawQuery))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents := []model.Agent{
		{Identifier: "tv.plex.agents.movie", Primary: true, Name: "Plex Movie", MediaType: []model.MediaType{{MediaType: 1, Name: "movie"}}},
		{Identifier: "tv.plex.agents.series", Primary: true, Name: "Plex Series", MediaType: []model.MediaType{{MediaType: 2, Name: "show"}}},
		{Identifier: "com.plexapp.agents.none", Name: "Personal Media", MediaType: []model.MediaType{{MediaType: 1, Name: "movie"}, {MediaType: 2, Name: "show"}}},
	}
	s.write(w, r, &model.MediaContainer{Size: len(agents), Agent: agents})
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	var users []frontend.User
	if s.host.Identity != nil {
		if us, err := s.listUsers(r.Context()); err == nil {
			users = us
		} else {
			s.log.Warn("list users", "err", err)
		}
	}
	mc := &model.MediaContainer{Identifier: "com.plexapp.system.accounts"}
	for _, u := range users {
		a := model.Account{ID: u.ID, Key: "/accounts/" + strconv.FormatInt(u.ID, 10), Name: u.Username, AutoSelectAudio: true, Thumb: u.Thumb}
		if len(u.AudioLanguages) > 0 {
			a.DefaultAudioLanguage = u.AudioLanguages[0]
		}
		if len(u.SubtitleLanguages) > 0 {
			a.DefaultSubtitleLang = u.SubtitleLanguages[0]
		}
		if s.d.TrackPreferences != nil && viewerStoredSubtitlePreference(r.Context(), s.d.TrackPreferences, u.ID) {
			if rules, err := s.d.TrackPreferences.GetRules(r.Context(), u.ID); err == nil {
				// Plex account subtitleMode: 0 manual, 1 with foreign audio, 2 always.
				// Unset and off stay 0, which is the value already written for every account.
				// Shared defaults (user 0) stay off this document so an untouched account
				// keeps the bytes it had before this viewer chose a preference.
				a.SubtitleMode = plexSubtitleMode(rules.SubtitleMode)
				if len(rules.SubtitleLanguages) > 0 {
					a.DefaultSubtitleLang = rules.SubtitleLanguages[0]
				}
			}
		}
		mc.Account = append(mc.Account, a)
	}
	mc.Size = len(mc.Account)
	s.write(w, r, mc)
}

// plexSubtitleMode matches Plex's account attribute: 0 is manual, 1 is shown
// with foreign audio, 2 is always. Forced-only has no Plex value; 1 tells the
// client to enable subtitles while Ether's selector still picks a forced track.
// Off stays 0, the same attribute every unchanged account already carries.
// viewerStoredSubtitlePreference is true when this account has its own
// trackprefs row. Stores that cannot tell own from inherited still report
// the GetRules result, which tests and older fakes rely on.
func viewerStoredSubtitlePreference(ctx context.Context, prefs TrackPreferences, user int64) bool {
	type own interface {
		HasViewerRules(context.Context, int64) (bool, error)
	}
	checker, ok := prefs.(own)
	if !ok {
		return true
	}
	has, err := checker.HasViewerRules(ctx, user)
	return err == nil && has
}

func plexSubtitleMode(mode string) int {
	switch mode {
	case "always":
		return 2
	case "foreign-only", "forced-only":
		return 1
	default:
		return 0
	}
}

func (s *Server) handleSecurityToken(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	if s.host.Identity == nil || pr.User == nil || pr.User.Delegated {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	token, err := s.host.Identity.IssueDelegation(r.Context(), pr.User.ID)
	if errors.Is(err, frontend.ErrUnauthorized) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.write(w, r, &model.MediaContainer{Extra: []model.Attr{model.A("token", token)}})
}

func (s *Server) handleUpdaterStatus(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &model.MediaContainer{Extra: []model.Attr{
		model.A("autoUpdateVersion", 1),
		model.A("canInstall", false),
		model.A("checkedAt", 0),
		model.A("downloadURL", ""),
		model.A("status", 0),
	}})
}

// handleUnknown answers endpoints Ether doesn't implement the way PMS
// answers paths it does not know: 404 with its HTML error page. An empty
// 200 container here would make a mistyped or unsupported request look
// like an empty result; routes Ether knowingly stubs answer with
// writeEmpty explicitly in routes().
func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	s.log.Debug("unhandled endpoint", "method", r.Method, "path", r.URL.Path, "query", redactQuery(r.URL.RawQuery))
	writeHTTPError(w, http.StatusNotFound)
}

func (s *Server) handleOK(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// redactQuery strips tokens from a logged query string.
func redactQuery(q string) string {
	parts := strings.Split(q, "&")
	for i, p := range parts {
		k, _, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		// Authentication URL-decodes parameter names. Apply the same decoding
		// before classifying secrets, while preserving unrelated diagnostics.
		name, err := url.QueryUnescape(k)
		if err == nil && (strings.EqualFold(name, "X-Plex-Token") || strings.EqualFold(name, streamGrantParam) || strings.EqualFold(name, readCapParam)) {
			parts[i] = k + "=REDACTED"
		}
	}
	return strings.Join(parts, "&")
}
