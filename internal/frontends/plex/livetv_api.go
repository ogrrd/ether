package plex

// Live TV wire shapes and route names follow Plex's official PMS OpenAPI 1.2.3
// (DVR, Devices, EPG and Live TV groups). In particular the grid attaches
// broadcast times to Media, not the Video item.

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/go-chi/chi/v5"
)

const liveProviderPath = "/" + frontend.LiveTVProvider

type liveChannel struct {
	ID       string `plex:"id"`
	GridKey  string `plex:"gridKey"`
	VCN      string `plex:"vcn"`
	IsHD     bool   `plex:"isHd"`
	Thumb    string `plex:"thumb"`
	Title    string `plex:"title"`
	CallSign string `plex:"callSign"`
	Language string `plex:"language"`
}
type liveMapping struct {
	ChannelKey       string `plex:"channelKey"`
	DeviceIdentifier string `plex:"deviceIdentifier"`
	Enabled          string `plex:"enabled"`
	LineupIdentifier string `plex:"lineupIdentifier"`
}
type liveDevice struct {
	Key            string        `plex:"key"`
	Make           string        `plex:"make"`
	Model          string        `plex:"model"`
	Protocol       string        `plex:"protocol"`
	Sources        string        `plex:"sources"`
	State          string        `plex:"state"`
	Status         string        `plex:"status"`
	Tuners         string        `plex:"tuners"`
	URI            string        `plex:"uri"`
	UUID           string        `plex:"uuid"`
	ChannelMapping []liveMapping `plex:"ChannelMapping"`
}
type liveDVR struct {
	EPGIdentifier string       `plex:"epgIdentifier"`
	Key           string       `plex:"key"`
	Language      string       `plex:"language"`
	Lineup        string       `plex:"lineup"`
	UUID          string       `plex:"uuid"`
	RefreshedAt   int64        `plex:"refreshedAt"`
	Device        []liveDevice `plex:"Device"`
}
type liveGrabber struct {
	Title      string `plex:"title"`
	Identifier string `plex:"identifier"`
	Protocol   string `plex:"protocol"`
}
type liveContainer struct {
	Size           int           `plex:"size"`
	Identifier     string        `plex:"identifier"`
	DVR            []liveDVR     `plex:"Dvr"`
	Device         []liveDevice  `plex:"Device"`
	Channel        []liveChannel `plex:"Channel"`
	ChannelMapping []liveMapping `plex:"ChannelMapping"`
	MediaGrabber   []liveGrabber `plex:"MediaGrabber"`
}

func (s *Server) liveTVEnabled() bool { return s.d.LiveTV != nil && s.identity().MyPlexSubscription }
func (s *Server) liveTVProvider(ctx context.Context, u *frontend.User) *model.MediaProvider {
	if !s.liveTVEnabled() || !s.d.LiveTV.VisibleTo(ctx, u) {
		return nil
	}
	return &model.MediaProvider{Extra: []model.Attr{model.A("id", 1), model.A("parentID", 1), model.A("providerIdentifier", "tv.plex.providers.epg.xmltv"), model.A("epgSource", "XMLTV")}, Identifier: frontend.LiveTVProvider, Title: "Live TV", Types: "video", Protocols: "livetv", Feature: []model.Feature{
		{Type: "content", Key: liveProviderPath, Directory: []model.ProviderDirectory{{ID: "livetv", Key: liveProviderPath + "/lineups/dvr/channels", HubKey: liveProviderPath + "/hubs", Title: "Live TV", Type: "livetv"}}},
		{Type: "universalsearch", Key: liveProviderPath + "/library/search"},
		{Type: "grid", Key: liveProviderPath + "/grid"}, {Type: "metadata", Key: liveProviderPath + "/metadata"}, {Type: "livetv", Key: "/livetv/dvrs"},
	}}
}

func (s *Server) registerLiveTVRoutes(r chi.Router) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !s.liveTVEnabled() || !s.d.LiveTV.VisibleTo(r.Context(), FromContext(r.Context()).User) {
				writeHTTPError(w, 404)
				return
			}
			if c := chi.URLParam(r, "consumer"); c != "" && c != "0" {
				writeHTTPError(w, 404)
				return
			}
			pr := FromContext(r.Context())
			if p, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !p.CanPlay(pr.User, pr.Remote, s.now()) {
				writeHTTPError(w, 403)
				return
			}
			h(w, r)
		}
	}
	get := func(path string, h http.HandlerFunc) { r.Get(path, guard(h)); r.Head(path, guard(h)) }
	get("/media/subscriptions", s.handleLiveSubscriptions)
	get("/media/subscriptions/{subscription}", s.handleLiveSubscription)
	r.Delete("/media/subscriptions/{subscription}", guard(s.handleLiveSubscriptionStop))
	get("/media/grabbers/operations/{operation}", s.handleLiveOperation)
	r.Delete("/media/grabbers/operations/{operation}", guard(s.handleLiveOperationStop))
	get("/livetv/dvrs", s.handleLiveDVRs)
	get("/livetv/dvrs/{dvr}", s.handleLiveDVRs)
	r.Post("/livetv/dvrs/{dvr}/channels/{channel}/tune", guard(s.handleLiveTune))
	get("/media/grabbers", s.handleLiveGrabbers)
	get("/media/grabbers/devices", s.handleLiveDevices)
	get("/media/grabbers/devices/{device}", s.handleLiveDevices)
	get("/media/grabbers/devices/{device}/channels", s.handleLiveChannels)
	get("/media/grabbers/devices/{device}/channelmap", s.handleLiveChannels)
	for _, path := range []string{"/livetv/epg/channels", "/livetv/epg/lineupchannels", "/livetv/epg/channelmap", liveProviderPath + "/lineups/dvr/channels"} {
		get(path, s.handleLiveChannels)
	}
	get(liveProviderPath, s.handleLiveRoot)
	get(liveProviderPath+"/grid", s.handleLiveGuide)
	get(liveProviderPath+"/metadata/{programme}", s.handleLiveGuide)
	get(liveProviderPath+"/channels/{channel}", s.handleLiveChannel)
	get(liveProviderPath+"/hubs", s.handleLiveHubs)
	get(liveProviderPath+"/library/search", s.handleLiveChannelSearch)
	get(liveProviderPath+"/watchnow", s.handleLiveWatchNow)
	get(liveProviderPath+"/watchnow/all", s.handleLiveGuide)
	get(liveProviderPath+"/sections/{section}/all", s.handleLiveGuide)
	get("/livetv/guide.xml", func(w http.ResponseWriter, r *http.Request) {
		s.d.LiveTV.ServeXMLTV(w, r, FromContext(r.Context()).User)
	})
	get("/livetv/channels/{channel}/logo.svg", s.handleLiveLogo)
	get("/livetv/channels/{channel}/logo.png", s.handleLiveRasterLogo)
	// No viewer guard: Plex's cloud fetches it without credentials; the
	// signature is the authorization.
	r.Get(signedLogoPrefix+"{channel}/{signature}", s.handleSignedLiveLogo)
	r.Head(signedLogoPrefix+"{channel}/{signature}", s.handleSignedLiveLogo)
	get("/livetv/sessions", s.handleLiveSessions)
	get("/livetv/sessions/{session}", s.handleLiveSession)
	r.Delete("/livetv/sessions/{session}", guard(func(w http.ResponseWriter, r *http.Request) {
		err := s.d.LiveTV.Stop(r.Context(), chi.URLParam(r, "session"), FromContext(r.Context()).User)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.writeEmpty(w, r)
	}))
	get("/livetv/sessions/{session}/{consumer}/dash/{rep}/{part}", func(w http.ResponseWriter, r *http.Request) {
		rep, err := strconv.Atoi(chi.URLParam(r, "rep"))
		part := chi.URLParam(r, "part")
		init := part == "header"
		n, e := strconv.Atoi(strings.TrimSuffix(part, ".m4s"))
		if err != nil || rep < 0 || (!init && (e != nil || n < 0 || !strings.HasSuffix(part, ".m4s"))) {
			writeHTTPError(w, 404)
			return
		}
		s.d.LiveTV.ServeDASHPart(w, r, chi.URLParam(r, "session"), FromContext(r.Context()).User, rep, n, init)
	})
	get("/livetv/sessions/{session}/{consumer}/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		if chi.URLParam(r, "consumer") != "0" {
			writeHTTPError(w, 404)
			return
		}
		s.d.LiveTV.ServePlaylist(w, r, chi.URLParam(r, "session"), FromContext(r.Context()).User)
	})
	get("/livetv/sessions/{session}/{consumer}/relay/{segment}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := frontend.ParseSegmentName(chi.URLParam(r, "segment"))
		if !ok {
			writeHTTPError(w, 404)
			return
		}
		cw := s.newCountWriter(w, r)
		s.d.LiveTV.ServeRelaySegment(cw, r, chi.URLParam(r, "session"), n, FromContext(r.Context()).User)
		cw.finish()
	})

	get("/livetv/sessions/{session}/{consumer}/resource/{resource}", func(w http.ResponseWriter, r *http.Request) {
		s.d.LiveTV.ServeResource(w, r, chi.URLParam(r, "session"), chi.URLParam(r, "resource"), FromContext(r.Context()).User)
	})

}
func (s *Server) liveDevices(r *http.Request) ([]liveDevice, error) {
	cs, err := s.d.LiveTV.AllowedChannels(r.Context(), FromContext(r.Context()).User)
	if err != nil {
		return nil, err
	}
	maps := []liveMapping{}
	for _, c := range cs {
		maps = append(maps, liveMapping{c.ID, c.Number, "1", c.GuideID})
	}
	return []liveDevice{{Key: "1", Make: "Ether", Model: "Owner channels", Protocol: "livetv", Sources: "IPTV,Virtual", State: "1", Status: "1", Tuners: "4", URI: "device://tv.plex.grabbers.ether/1", UUID: s.identity().MachineIdentifier + "-livetv", ChannelMapping: maps}}, nil
}
func (s *Server) handleLiveDVRs(w http.ResponseWriter, r *http.Request) {
	if id := chi.URLParam(r, "dvr"); id != "" && id != "1" {
		writeHTTPError(w, 404)
		return
	}
	devices, err := s.liveDevices(r)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.write(w, r, &liveContainer{Size: 1, Identifier: frontend.LiveTVProvider, DVR: []liveDVR{{EPGIdentifier: frontend.LiveTVProvider, Key: "1", Language: "eng", Lineup: "lineup://" + frontend.LiveTVProvider + "/dvr", UUID: s.identity().MachineIdentifier + "-livetv", RefreshedAt: s.liveLineupRefreshedAtFor(FromContext(r.Context()).User), Device: devices}}})
}

func (s *Server) liveLineupRefreshedAtFor(u *frontend.User) int64 {
	if s.d.LiveTV != nil {
		if at := s.d.LiveTV.LineupChangedFor(u); !at.IsZero() {
			return at.Unix()
		}
	}
	return s.now().Unix()
}

// NotifyLiveLineup tells connected Plex clients to reload the DVR lineup and
// guide. Channel create and delete call it through LiveTV.SetOnChange.
func (s *Server) NotifyLiveLineup() {
	s.NotifyLiveLineupFor(0)
}

// NotifyLiveLineupFor confines personal guide invalidation to its viewer.
func (s *Server) NotifyLiveLineupFor(viewer int64) {
	now := s.liveLineupRefreshedAtFor(&frontend.User{ID: viewer})
	s.publishNotification(model.NotificationContainer{
		Type: "activity",
		Size: 1,
		ActivityNotification: []model.ActivityNotification{{
			Event: "ended",
			UUID:  "ether-livetv-lineup",
			Activity: &model.Activity{
				UUID:     "ether-livetv-lineup",
				Type:     "livetv.guide",
				Title:    "Live TV lineup",
				Progress: 100,
			},
		}},
	}, viewer)
	s.publishNotification(model.NotificationContainer{
		Type: "timeline",
		Size: 1,
		TimelineEntry: []model.TimelineEntry{{
			Identifier: frontend.LiveTVProvider,
			SectionID:  "1",
			ItemID:     "1",
			Type:       1,
			Title:      "Live TV",
			State:      5,
			UpdatedAt:  now,
		}},
	}, viewer)
}
func (s *Server) handleLiveGrabbers(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &liveContainer{Size: 1, Identifier: frontend.LiveTVProvider, MediaGrabber: []liveGrabber{{Title: "Ether owner channels", Identifier: "tv.plex.grabbers.ether", Protocol: "livetv"}}})
}
func (s *Server) handleLiveDevices(w http.ResponseWriter, r *http.Request) {
	if id := chi.URLParam(r, "device"); id != "" && id != "1" {
		writeHTTPError(w, 404)
		return
	}
	devices, err := s.liveDevices(r)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.write(w, r, &liveContainer{Size: len(devices), Identifier: frontend.LiveTVProvider, Device: devices})
}
func liveLogo(c frontend.LiveTVChannel) string {
	return "/livetv/channels/" + url.PathEscape(c.ID) + "/logo.png"
}

func isStandaloneGuideLogoClient(pr *Request) bool {
	if pr == nil {
		return false
	}
	return strings.EqualFold(pr.Platform, "tvOS") ||
		strings.EqualFold(pr.Product, "Plex for Apple TV") ||
		strings.EqualFold(pr.Product, "Plex Web") ||
		strings.EqualFold(pr.Platform, "Android") ||
		plexAndroidClient(pr)
}

func (s *Server) liveGuideLogo(r *http.Request, c frontend.LiveTVChannel) string {
	path := liveLogo(c)
	pr := FromContext(r.Context())
	if !isStandaloneGuideLogoClient(pr) || r.Host == "" {
		return path
	}
	scheme, host := s.requestOrigin(r)
	if cloudGuideLogoClient(pr) {
		return s.cloudGuideLogo(r.Context(), c, scheme+"://"+host)
	}
	// These clients load guide thumbnails as standalone URLs without auth headers.
	u := url.URL{Scheme: scheme, Host: host, Path: "/livetv/channels/" + c.ID + "/logo.png", RawPath: path}
	q := url.Values{}
	q.Set("X-Plex-Token", pr.Token)
	u.RawQuery = q.Encode()
	return u.String()
}

// requestOrigin is the scheme and host this request reached the server at,
// honouring forwarding headers only from trusted proxies.
func (s *Server) requestOrigin(r *http.Request) (string, string) {
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	peerHost, _, _ := net.SplitHostPort(r.RemoteAddr)
	peer, _ := netip.ParseAddr(peerHost)
	trusted := peer.IsLoopback()
	for _, prefix := range clientnet.ProxyPrefixes(s.cfg().Server.TrustedProxyCIDRs) {
		trusted = trusted || prefix.Contains(peer.Unmap())
	}
	if trusted {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto == "http" || proto == "https" {
			scheme = proto
		}
		if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" && !strings.ContainsAny(forwarded, "/@?#, ") {
			host = forwarded
		}
	}
	return scheme, host
}

func (s *Server) handleLiveChannels(w http.ResponseWriter, r *http.Request) {
	cs, err := s.d.LiveTV.AllowedChannels(r.Context(), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	out := []liveChannel{}
	maps := []liveMapping{}
	for _, c := range cs {
		out = append(out, liveChannel{ID: c.ID, GridKey: c.ID, VCN: c.Number, Thumb: s.liveGuideLogo(r, c), Title: c.Name, CallSign: c.Name, Language: "eng"})
		maps = append(maps, liveMapping{c.ID, c.Number, "1", c.GuideID})
	}
	s.write(w, r, &liveContainer{Size: len(out), Identifier: frontend.LiveTVProvider, Channel: out, ChannelMapping: maps})
}
func liveProgrammeMetadata(c frontend.LiveTVChannel, p frontend.LiveTVProgramme) model.Metadata {
	dur := p.End.Sub(p.Start).Milliseconds()
	rating := p.Item.Rating
	if rating == "" {
		rating = c.Rating
	}
	// EPG entries are movie/episode metadata; clip + live is the tune session
	// shape. Use a standalone programme when the guide supplies no series type.
	mediaType := p.Item.MediaType
	if mediaType != "episode" && mediaType != "movie" {
		mediaType = "movie"
	}
	md := model.Metadata{RatingKey: p.ID, Key: liveProviderPath + "/metadata/" + url.PathEscape(p.ID), GUID: "ether://livetv/" + p.ID, Type: mediaType, Title: p.Item.Title, Summary: p.Item.Summary, ContentRating: rating, Thumb: p.Item.Thumb, Duration: dur, Media: []model.Media{{ID: p.Start.Unix(), Duration: dur, Protocol: "livetv", Extra: []model.Attr{model.A("beginsAt", p.Start.Unix()), model.A("endsAt", p.End.Unix()), model.A("channelIdentifier", c.ID), model.A("channelID", c.ID), model.A("channelTitle", c.Name), model.A("channelCallSign", c.Name), model.A("channelVcn", c.Number), model.A("channelThumb", liveLogo(c)), model.A("gridKey", c.ID)}}}}
	if p.Lens != "" {
		md.Summary = p.Lens + "\n\n" + md.Summary
	}
	if strings.HasPrefix(p.Item.GUID, "plex://") {
		md.GUID = p.Item.GUID
		if p.Item.EpisodeTitle != "" {
			md.Title = p.Item.EpisodeTitle
		}
		md.GrandparentTitle = p.Item.ShowTitle
		md.GrandparentGUID = p.Item.ShowGUID
		md.Art = p.Item.Art
		md.GrandparentThumb = p.Item.Thumb
		md.GrandparentArt = p.Item.Art
		if p.Item.Season > 0 {
			season := p.Item.Season
			md.ParentIndex = &season
		}
		if p.Item.Episode > 0 {
			episode := p.Item.Episode
			md.Index = &episode
		}
		if p.Item.ReleasedMS > 0 {
			md.OriginallyAvailableAt = time.UnixMilli(p.Item.ReleasedMS).UTC().Format("2006-01-02")
		}
		for _, g := range p.Item.Genres {
			md.Genre = append(md.Genre, model.Tag{Tag: g})
		}
		md.Media[0].Extra = append(md.Media[0].Extra, model.A("premiere", p.Item.Premiere), model.A("live", p.Item.Live))
	}
	return md
}

// errLiveBadRequest marks a guide query the client got wrong (400, not 500).
var errLiveBadRequest = errors.New("invalid guide request")

func (s *Server) liveGuideMetadata(r *http.Request) ([]model.Metadata, error) {
	return s.liveGuideMetadataCapture(r, nil)
}

func (s *Server) liveGuideMetadataCapture(r *http.Request, captured *[]frontend.LiveTVProgramme) ([]model.Metadata, error) {
	from := s.now()
	until := from.Add(time.Second)
	q := r.URL.Query()
	if date := q.Get("date"); date != "" {
		var err error
		from, err = time.Parse("2006-01-02", date)
		if err != nil {
			return nil, fmt.Errorf("%w: date", errLiveBadRequest)
		}
		until = from.Add(24 * time.Hour)
	}
	if raw := q.Get("beginsAt"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: beginsAt", errLiveBadRequest)
		}
		from = time.Unix(n, 0)
		until = from.Add(24 * time.Hour)
	}
	if raw := q.Get("endsAt"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: endsAt", errLiveBadRequest)
		}
		until = time.Unix(n, 0)
	}
	gridKey := q.Get("channelGridKey")
	if id := chi.URLParam(r, "programme"); id != "" {
		dash := strings.LastIndexByte(id, '-')
		if dash <= 0 {
			return nil, frontend.ErrNotFound
		}
		ms, err := strconv.ParseInt(id[dash+1:], 10, 64)
		if err != nil {
			return nil, frontend.ErrNotFound
		}
		// Programme keys are channelID-startMS. Detail requests must stay
		// channel-scoped too, even when a client omits channelGridKey.
		gridKey = id[:dash]
		from = time.UnixMilli(ms)
		until = from.Add(time.Second)
	}
	if !until.After(from) || until.Sub(from) > 7*24*time.Hour {
		return nil, fmt.Errorf("%w: window", errLiveBadRequest)
	}
	// Plex asks for one grid channel at a time. Converting the whole lineup's
	// window (179k rows) to keep one channel made the guide CPU-bound.
	var ps []frontend.LiveTVProgramme
	var err error
	if gridKey != "" {
		ps, err = s.d.LiveTV.GuideForChannels(r.Context(), FromContext(r.Context()).User, from, until, []string{gridKey})
	} else {
		ps, err = s.d.LiveTV.Guide(r.Context(), FromContext(r.Context()).User, from, until)
	}
	if err != nil {
		return nil, err
	}
	byID := map[string]frontend.LiveTVChannel{}
	if gridKey != "" {
		c, err := s.d.LiveTV.ChannelForViewer(r.Context(), FromContext(r.Context()).User, gridKey)
		if errors.Is(err, frontend.ErrNotFound) {
			return []model.Metadata{}, nil
		}
		if err != nil {
			return nil, err
		}
		byID[c.ID] = c
	} else {
		cs, err := s.d.LiveTV.Channels(r.Context())
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if c.Personal {
				c, err = s.d.LiveTV.ChannelForViewer(r.Context(), FromContext(r.Context()).User, c.ID)
				if err != nil {
					continue
				}
			}
			byID[c.ID] = c
		}
	}
	out := []model.Metadata{}
	for _, p := range ps {
		if id := chi.URLParam(r, "programme"); id != "" && p.ID != id {
			continue
		}
		md := liveProgrammeMetadata(byID[p.ChannelID], p)
		if c, ok := byID[p.ChannelID]; ok && cloudGuideLogoClient(FromContext(r.Context())) {
			// The Apple TV guide draws airings' channelThumb too: give it the
			// same fetchable logo as the lineup.
			for i, a := range md.Media[0].Extra {
				if a.Name == "channelThumb" {
					md.Media[0].Extra[i].Value = s.liveGuideLogo(r, c)
				}
			}
		}
		out = append(out, md)
		if captured != nil {
			*captured = append(*captured, p)
		}
	}
	return out, nil
}
func (s *Server) handleLiveGuide(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	md, err := s.liveGuideMetadata(r)
	pr := FromContext(r.Context())
	attrs := []any{"client", pr.ClientID, "product", pr.Product, "detail", chi.URLParam(r, "programme") != "", "items", len(md), "elapsed", time.Since(started)}
	for _, name := range []string{"beginsAt", "endsAt", "type", "includeDetails"} {
		if raw := r.URL.Query().Get(name); raw != "" {
			if n, e := strconv.ParseInt(raw, 10, 64); e == nil {
				attrs = append(attrs, name, n)
			} else {
				attrs = append(attrs, name, "invalid")
			}
		}
	}
	summaries, clips, episodes, movies := 0, 0, 0, 0
	for _, m := range md {
		switch m.Type {
		case "clip":
			clips++
		case "episode":
			episodes++
		case "movie":
			movies++
		}
		if m.Summary != "" {
			summaries++
		}
	}
	attrs = append(attrs, "with_summary", summaries, "clips", clips, "episodes", episodes, "movies", movies, "format", int(pr.Format))
	if err != nil {
		s.log.Warn("live guide request failed", append(attrs, "failure_code", s.d.ArtworkStores.DiagnoseLiveTV("guide", err).Code)...)
	} else if chi.URLParam(r, "programme") != "" && len(md) == 0 {
		s.log.Warn("live guide detail missing", attrs...)
	} else {
		s.log.Debug("live guide response", attrs...)
	}
	if errors.Is(err, frontend.ErrNotAvailable) {
		w.Header().Set("Retry-After", "1")
		writeHTTPError(w, 503)
		return
	}
	if errors.Is(err, errLiveBadRequest) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if chi.URLParam(r, "programme") != "" && len(md) == 0 {
		writeHTTPError(w, 404)
		return
	}
	s.write(w, r, &model.MediaContainer{Size: len(md), Identifier: frontend.LiveTVProvider, Metadata: md})
}
func (s *Server) handleLiveRoot(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &model.MediaContainer{Size: 2, Identifier: frontend.LiveTVProvider, Directory: []model.Directory{{Key: liveProviderPath + "/lineups/dvr/channels", Title: "Guide", Type: "livetv"}, {Key: liveProviderPath + "/watchnow/all", Title: "On Now", Type: "mixed"}}})
}
func (s *Server) handleLiveWatchNow(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &model.MediaContainer{Size: 1, Identifier: frontend.LiveTVProvider, Type: []model.MetaType{{Active: true, Key: liveProviderPath + "/watchnow/all", Title: "All Channels", Type: "mixed"}}})
}
func (s *Server) liveTVHub(r *http.Request) *model.Hub {
	h, _ := s.liveTVHubProjection(r)
	return h
}

func (s *Server) liveTVHubProjection(r *http.Request) (*model.Hub, frontend.Row) {
	if !s.liveTVEnabled() {
		return nil, frontend.Row{}
	}
	var programmes []frontend.LiveTVProgramme
	md, err := s.liveGuideMetadataCapture(r, &programmes)
	if err != nil {
		return nil, frontend.Row{}
	}
	cs, err := s.d.LiveTV.AllowedChannels(r.Context(), FromContext(r.Context()).User)
	if err != nil {
		return nil, frontend.Row{}
	}
	found := map[string]bool{}
	for _, item := range md {
		for _, media := range item.Media {
			for _, a := range media.Extra {
				if a.Name == "channelIdentifier" {
					if id, ok := a.Value.(string); ok {
						found[id] = true
					}
				}
			}
		}
	}
	for _, c := range cs {
		if !found[c.ID] {
			md = append(md, s.liveChannelMetadata(r, c))
		}
	}
	if len(md) == 0 {
		return nil, frontend.Row{}
	}
	if len(md) > 24 {
		md = md[:24]
	}
	h := &model.Hub{Key: liveProviderPath + "/watchnow/all", Title: "On Now", Type: "mixed", HubIdentifier: "livetv.onnow", Context: "hub.livetv.onnow", Size: len(md), Style: "shelf", Promoted: model.Ptr(true), Metadata: md}
	row := frontend.Row{ID: "livetv.onnow", Title: "On Now", Purpose: frontend.RowLive, Style: "shelf", Promoted: true}
	channels := make(map[string]frontend.LiveTVChannel, len(cs))
	for _, c := range cs {
		channels[c.ID] = c
	}
	for _, item := range md {
		var id string
		for _, media := range item.Media {
			for _, attr := range media.Extra {
				if attr.Name == "channelIdentifier" {
					id, _ = attr.Value.(string)
				}
			}
		}
		c, ok := channels[id]
		if !ok {
			continue
		}
		entry := &frontend.RowLiveChannel{ID: c.ID, Number: c.Number, Name: c.Name, Logo: liveLogo(c), GuideReference: c.GuideID}
		now := s.now()
		for _, p := range programmes {
			if p.ChannelID != id || !p.End.After(now) {
				continue
			}
			programme := &frontend.RowProgramme{ID: p.ID, Title: p.Item.Title, Start: p.Start, End: p.End, Item: p.Item.Key}
			if !p.Start.After(now) {
				if entry.Current == nil || p.Start.Before(entry.Current.Start) {
					entry.Current = programme
				}
			} else if entry.Next == nil || p.Start.Before(entry.Next.Start) {
				entry.Next = programme
			}
		}
		row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryLiveChannel, LiveChannel: entry})
	}
	return h, row
}
func (s *Server) liveChannelMetadata(r *http.Request, c frontend.LiveTVChannel) model.Metadata {
	return model.Metadata{RatingKey: "channel-" + c.ID, Key: liveProviderPath + "/channels/" + url.PathEscape(c.ID), Type: "clip", Title: c.Name, Summary: "Live channel. No programme guide supplied.", Thumb: s.liveGuideLogo(r, c), ContentRating: c.Rating, Extra: []model.Attr{model.A("live", true), model.A("dvrChannel", true)}, Media: []model.Media{{Protocol: "livetv", Extra: []model.Attr{model.A("origin", "livetv"), model.A("channelIdentifier", c.ID), model.A("channelTitle", c.Name), model.A("channelVcn", c.Number), model.A("onAir", true)}}}}
}
func (s *Server) handleLiveChannel(w http.ResponseWriter, r *http.Request) {
	c, err := s.d.LiveTV.AllowedChannel(r.Context(), FromContext(r.Context()).User, chi.URLParam(r, "channel"))
	if errors.Is(err, frontend.ErrNotFound) {
		writeHTTPError(w, 404)
		return
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.write(w, r, &model.MediaContainer{Size: 1, Identifier: frontend.LiveTVProvider, Metadata: []model.Metadata{s.liveChannelMetadata(r, c)}})
}

func (s *Server) handleLiveHubs(w http.ResponseWriter, r *http.Request) {
	mc := &model.MediaContainer{Identifier: frontend.LiveTVProvider}
	if h := s.liveTVHub(r); h != nil {
		mc.Hub = []model.Hub{*h}
		mc.Size = 1
	}
	s.write(w, r, mc)
}
func (s *Server) liveSessionMetadata(info frontend.LiveTVSessionInfo) model.Metadata {
	md := model.Metadata{RatingKey: info.ID, Key: "/livetv/sessions/" + info.ID, GUID: "plex://livetv/session/" + info.ID, Type: "clip", Title: info.Channel.Name, Summary: "", Extra: []model.Attr{model.A("live", true), model.A("genuineMediaAnalysis", false)}}
	if info.Programme != nil {
		md.Title = info.Programme.Item.Title
		md.Summary = info.Programme.Item.Summary
		md.Thumb = info.Programme.Item.Thumb
	}
	md.Media = []model.Media{{Protocol: "hls", Container: "mpegts", Extra: []model.Attr{model.A("uuid", info.ID), model.A("origin", "livetv"), model.A("beginsAt", info.StartedAt.Unix()), model.A("channelIdentifier", info.Channel.ID), model.A("channelTitle", info.Channel.Name), model.A("channelCallSign", info.Channel.Name), model.A("channelVcn", info.Channel.Number), model.A("onAir", true)}, Part: []model.Part{{Key: "/livetv/sessions/" + info.ID + "/0/index.m3u8", Container: "mpegts", Protocol: "hls", Decision: "directplay"}}}}
	if info.MediaInfo != nil && info.MediaInfo.Video() != nil {
		md.Extra = []model.Attr{model.A("live", true), model.A("genuineMediaAnalysis", true)}
		live := md.Media[0]
		m := buildMedia(&frontend.Title{Title: info.Channel.Name}, *info.MediaInfo, info.StartedAt.Unix())
		m.Extra = live.Extra
		m.Protocol = "hls"
		m.Container = "mpegts"
		m.ID = tagID("live", info.ID) + 1
		m.Duration = 0
		m.Part[0] = live.Part[0]
		m.Part[0].ID = m.ID
		m.Part[0].Stream = s.buildStreams(0, *info.MediaInfo, selection{}, nil)
		md.Media = []model.Media{m}
	}
	return md
}
func (s *Server) handleLiveTune(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "dvr") != "1" {
		writeHTTPError(w, 404)
		return
	}
	pr := FromContext(r.Context())
	c, err := s.d.LiveTV.AllowedChannel(r.Context(), pr.User, chi.URLParam(r, "channel"))
	if errors.Is(err, frontend.ErrNotFound) {
		writeHTTPError(w, 404)
		return
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	id := c.ID
	info, err := s.d.LiveTV.TuneWithSession(r.Context(), pr.User, id, s.clientCaps(pr), pr.SessionID)
	if err != nil {
		s.log.Warn("live tune response failed", append([]any{"channel", id, "client", pr.ClientID, "product", pr.Product}, s.d.ArtworkStores.DiagnoseLiveTV("tune", err).LogAttrs...)...)
		s.writeLiveTuneError(w, r, err)
		return
	}
	s.log.Info("live tune ready", "channel", info.Channel.Name, "session", info.ID, "client", pr.ClientID, "product", pr.Product, "analyzed", info.MediaInfo != nil)
	s.writeLiveSubscription(w, r, info)
}
func (s *Server) handleLiveSessions(w http.ResponseWriter, r *http.Request) {
	infos := s.d.LiveTV.Sessions(r.Context(), FromContext(r.Context()).User)
	md := []model.Metadata{}
	for _, info := range infos {
		md = append(md, s.liveSessionMetadata(info))
	}
	s.write(w, r, &model.MediaContainer{Size: len(md), Identifier: frontend.LiveTVProvider, Metadata: md})
}
func (s *Server) handleLiveSession(w http.ResponseWriter, r *http.Request) {
	info, err := s.d.LiveTV.Session(r.Context(), chi.URLParam(r, "session"), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.write(w, r, &model.MediaContainer{Size: 1, Identifier: frontend.LiveTVProvider, Metadata: []model.Metadata{s.liveSessionMetadata(info)}})
}

// handleLiveTVPlayback hooks the universal decision/start endpoints before the
// catalogue parser. Plex passes the tune response's /livetv/sessions key here.
func (s *Server) handleLiveTVPlayback(w http.ResponseWriter, r *http.Request) bool {
	if s.refuseCapability(w, r) {
		return true
	}
	path := r.URL.Query().Get("path")
	if !strings.HasPrefix(path, "/livetv/sessions/") {
		return false
	}
	if !s.liveTVEnabled() {
		writeHTTPError(w, 404)
		return true
	}
	pr := FromContext(r.Context())
	if p, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !p.CanPlay(pr.User, pr.Remote, s.now()) {
		writeHTTPError(w, 403)
		return true
	}
	id := strings.TrimPrefix(path, "/livetv/sessions/")
	if strings.Contains(id, "/") {
		writeHTTPError(w, 404)
		return true
	}
	info, err := s.d.LiveTV.Session(r.Context(), id, FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return true
	}
	if strings.HasSuffix(r.URL.Path, "/decision") {
		s.write(w, r, &model.MediaContainer{Size: 1, Identifier: frontend.LiveTVProvider, Extra: []model.Attr{model.A("generalDecisionCode", 1001), model.A("generalDecisionText", "Live TV stream available."), model.A("transcodeDecisionCode", 1001), model.A("transcodeDecisionText", "Live TV HLS streaming.")}, Metadata: []model.Metadata{s.liveSessionMetadata(info)}})
		return true
	}
	if strings.HasSuffix(r.URL.Path, "/start.mpd") {
		if err := s.d.LiveTV.ServeDASH(w, r, id, pr.User); err != nil {
			s.writeLiveDASHError(w, r, info, err)
		}
		return true
	}
	if strings.HasSuffix(r.URL.Path, "/start") && r.URL.Query().Get("protocol") == frontend.ProtocolHTTP {
		if err := s.d.LiveTV.ServeHTTP(w, r, id, pr.User); err != nil {
			s.writeErr(w, r, err)
		}
		return true
	}
	// Native players can retry this endpoint without following a redirect.
	// The Live TV playlist already uses rooted, authenticated child URLs.
	s.d.LiveTV.ServePlaylist(w, r, id, pr.User)
	return true
}

// writeLiveDASHError is the Plex decision document for a virtual channel that
// cannot produce DASH. Plex Web treats a bare 503 as s1001 (Network) and never
// shows the reason. Decision codes at or above 2000 are the message it displays.
func (s *Server) writeLiveDASHError(w http.ResponseWriter, r *http.Request, info frontend.LiveTVSessionInfo, err error) {
	dashMsg, dashCause, ok := s.d.ArtworkStores.VirtualDASHError(err)
	if !ok {
		s.writeErr(w, r, err)
		return
	}
	msg := "This live channel cannot be played as DASH."
	if dashMsg != "" {
		msg = dashMsg
	} else if dashCause != nil && !errors.Is(dashCause, frontend.ErrNotAvailable) {
		msg = playbackMessage(dashCause)
	}
	s.write(w, r, &model.MediaContainer{
		Size: 1, Identifier: frontend.LiveTVProvider,
		Extra: []model.Attr{
			model.A("directPlayDecisionCode", codeDirectPlayError),
			model.A("directPlayDecisionText", msg),
			model.A("generalDecisionCode", codeGeneralError),
			model.A("generalDecisionText", msg),
			model.A("transcodeDecisionCode", codeTranscodeError),
			model.A("transcodeDecisionText", msg),
		},
		Metadata: []model.Metadata{s.liveSessionMetadata(info)},
	})
}
func (s *Server) handleLiveLogo(w http.ResponseWriter, r *http.Request) {
	s.serveLiveLogo(w, r, chi.URLParam(r, "channel"), false)
}

func (s *Server) handleLiveRasterLogo(w http.ResponseWriter, r *http.Request) {
	s.serveLiveLogo(w, r, chi.URLParam(r, "channel"), true)
}

func (s *Server) serveLiveLogo(w http.ResponseWriter, r *http.Request, id string, raster bool) {
	pr := FromContext(r.Context())
	if !s.liveTVEnabled() || !s.d.LiveTV.VisibleTo(r.Context(), pr.User) {
		writeHTTPError(w, 404)
		return
	}
	if p, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !p.CanPlay(pr.User, pr.Remote, s.now()) {
		writeHTTPError(w, 403)
		return
	}
	c, err := s.d.LiveTV.AllowedChannel(r.Context(), pr.User, id)
	if errors.Is(err, frontend.ErrNotFound) {
		writeHTTPError(w, 404)
		return
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeChannelLogo(w, r, c, raster, "private, max-age=300")
}

// writeChannelLogo writes channel c's logo: its source image (or matched
// guide logo), rasterized when asked, else a generated card.
func (s *Server) writeChannelLogo(w http.ResponseWriter, r *http.Request, c frontend.LiveTVChannel, raster bool, cacheControl string) {
	var err error
	format := androidPhotoOutputFormat(r)
	rasterFormat := format
	if rasterFormat == "" {
		rasterFormat = "png"
	}
	write := func(data []byte, ct string) {
		setArtworkHeaders(w.Header(), ct)
		w.Header().Set("Cache-Control", cacheControl)
		w.Header().Add("Vary", "X-Plex-Token")
		w.Header().Add("Vary", "X-Plex-Product")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
	}
	src := c.Logo
	var (
		data []byte
		ct   string
	)
	if src != "" && s.channelLogos != nil {
		data, ct, err = s.channelLogos.GetUntrustedURL(r.Context(), src)
	}
	if (src == "" || err != nil || len(data) == 0) && s.d.LiveTV != nil && s.channelLogos != nil && c.GuideID != "" {
		if guideLogo := s.d.LiveTV.GuideChannelLogo(r.Context(), c); guideLogo != "" && guideLogo != c.Logo {
			src = guideLogo
			data, ct, err = s.channelLogos.GetUntrustedURL(r.Context(), src)
		}
	}
	if err == nil && len(data) > 0 {
		if raster && strings.HasPrefix(ct, "image/svg+xml") {
			data, ct, err = s.channelRaster.Rasterize(r.Context(), src, data, frontend.RasterOptions{Format: rasterFormat})
		}
		if err == nil && raster && format != "" {
			data, ct, err = s.images.RasterFormat(r.Context(), data, ct, format, s.now())
		}
		if err == nil && len(data) > 0 {
			write(data, ct)
			return
		}
	}
	if _, shipped, ok := s.d.ArtworkStores.ShowcaseLogo(c); ok {
		// A shipped channel's own artwork, the same file Apple TV fetches
		// from Ether's public repository.
		data, ct, err = shipped, "image/png", nil
		if raster && format != "" {
			data, ct, err = s.images.RasterFormat(r.Context(), data, ct, format, s.now())
		}
		if err == nil && len(data) > 0 {
			write(data, ct)
			return
		}
	}
	if raster {
		data, ct, err := s.channelRaster.ChannelLogo(r.Context(), c.ID, c.Number, c.Name, frontend.RasterOptions{Format: rasterFormat})
		if err != nil {
			writeHTTPError(w, 404)
			return
		}
		write(data, ct)
		return
	}
	setArtworkHeaders(w.Header(), "image/svg+xml")
	w.Header().Set("Cache-Control", cacheControl)
	if r.Method != "HEAD" {
		fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360"><rect width="640" height="360" rx="24" fill="#182d45"/><text x="40" y="125" fill="#64dfc7" font-size="68" font-family="sans-serif">%s</text><text x="40" y="220" fill="white" font-size="30" font-family="sans-serif">%s</text></svg>`, html.EscapeString(c.Number), html.EscapeString(c.Name))
	}
}

// Plex Web 4.160's LiveTVSessionModel consumes the active subscription and
// grab operation in addition to the OpenAPI's top-level Metadata sample. The
// activity ID is the real Ether live session's identity, not a Plex entitlement.
type liveOperation struct {
	Key          string         `plex:"key"`
	ID           string         `plex:"id"`
	Subscription string         `plex:"mediaSubscriptionID"`
	Provider     string         `plex:"provider"`
	MediaIndex   int            `plex:"mediaIndex"`
	Status       string         `plex:"status"`
	Rolling      bool           `plex:"rolling"`
	Metadata     model.Metadata `plex:"Metadata,object"`
}
type liveTimeline struct {
	Metadata []model.Metadata `plex:"Metadata"`
}
type liveSubscription struct {
	Key       string        `plex:"key"`
	Type      int           `plex:"type"`
	Title     string        `plex:"title"`
	Operation liveOperation `plex:"MediaGrabOperation,object"`
	Timeline  liveTimeline  `plex:"Timeline,object"`
}
type liveSubscriptionContainer struct {
	Size         int                `plex:"size"`
	Identifier   string             `plex:"identifier"`
	Metadata     []model.Metadata   `plex:"Metadata"`
	Subscription []liveSubscription `plex:"MediaSubscription"`
	Operation    *liveOperation     `plex:"MediaGrabOperation,object"`
	Extra        []model.Attr
}

func (s *Server) liveSubscriptionFor(info frontend.LiveTVSessionInfo) liveSubscription {
	md := s.liveSessionMetadata(info)
	timeline := md
	if info.Programme != nil {
		timeline = liveProgrammeMetadata(info.Channel, *info.Programme)
		timeline.Media[0].Extra = append(timeline.Media[0].Extra, model.A("onAir", true))
	}
	op := liveOperation{Key: "/media/grabbers/operations/" + info.ID, ID: info.ID, Subscription: info.ID, Provider: frontend.LiveTVProvider, Status: "inprogress", Rolling: true, Metadata: md}
	return liveSubscription{Key: info.ID, Type: 12, Title: info.Channel.Name, Operation: op, Timeline: liveTimeline{Metadata: []model.Metadata{timeline}}}
}

func (s *Server) writeLiveSubscription(w http.ResponseWriter, r *http.Request, info frontend.LiveTVSessionInfo) {
	if nativeLiveRequest(r) {
		s.writeNativeLiveTune(w, r, info)
		return
	}
	w.Header().Set("X-Plex-Activity", "livetv-"+info.ID)
	s.write(w, r, &liveSubscriptionContainer{Size: 1, Identifier: frontend.LiveTVProvider, Metadata: []model.Metadata{s.liveSessionMetadata(info)}, Subscription: []liveSubscription{s.liveSubscriptionFor(info)}})
}
func (s *Server) handleLiveSubscriptions(w http.ResponseWriter, r *http.Request) {
	infos := s.d.LiveTV.Sessions(r.Context(), FromContext(r.Context()).User)
	if nativeLiveRequest(r) {
		out := []nativeLiveSubscription{}
		for _, info := range infos {
			out = append(out, s.nativeLiveSubscriptionFor(info))
		}
		s.write(w, r, &nativeLiveContainer{Size: len(out), Subscriptions: out})
		return
	}
	out := []liveSubscription{}
	for _, info := range infos {
		out = append(out, s.liveSubscriptionFor(info))
	}
	s.write(w, r, &liveSubscriptionContainer{Size: len(out), Identifier: frontend.LiveTVProvider, Subscription: out})
}
func (s *Server) handleLiveSubscription(w http.ResponseWriter, r *http.Request) {
	info, err := s.liveSubscriptionSession(r, chi.URLParam(r, "subscription"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeLiveSubscription(w, r, info)
}
func (s *Server) handleLiveSubscriptionStop(w http.ResponseWriter, r *http.Request) {
	info, err := s.liveSubscriptionSession(r, chi.URLParam(r, "subscription"))
	if err == nil {
		err = s.d.LiveTV.Stop(r.Context(), info.ID, FromContext(r.Context()).User)
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeEmpty(w, r)
}
func (s *Server) handleLiveOperation(w http.ResponseWriter, r *http.Request) {
	info, err := s.d.LiveTV.Session(r.Context(), chi.URLParam(r, "operation"), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if nativeLiveRequest(r) {
		s.write(w, r, &nativeLiveContainer{Size: 1, Grabs: s.nativeLiveSubscriptionFor(info).Grab})
		return
	}
	op := s.liveSubscriptionFor(info).Operation
	s.write(w, r, &liveSubscriptionContainer{Size: 1, Identifier: frontend.LiveTVProvider, Operation: &op})
}
func (s *Server) handleLiveOperationStop(w http.ResponseWriter, r *http.Request) {
	err := s.d.LiveTV.Stop(r.Context(), chi.URLParam(r, "operation"), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeEmpty(w, r)
}
