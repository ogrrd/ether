package plex

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// parseKeys parses a {key} path parameter that may be a comma list. Lists
// stop at maxExpand keys: every key costs a catalogue lookup (possibly a
// TMDB fetch), and hub keys never list more than a hub's 200 items.
func parseKeys(s string) ([]frontend.RatingKey, error) {
	var out []frontend.RatingKey
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(out) == maxExpand {
			break
		}
		k, err := frontend.ParseRatingKey(p)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, errors.New("no keys")
	}
	return out, nil
}

func keyParam(r *http.Request) (frontend.RatingKey, error) {
	return frontend.ParseRatingKey(chi.URLParam(r, "key"))
}

// itemContainer is the container for a single-section item response.
func (s *Server) itemContainer(t frontend.MediaType) *model.MediaContainer {
	mc := libraryContainer()
	if sec, ok := s.sectionFor(t); ok {
		mc.LibrarySectionID = model.Ptr(sec.ID)
		mc.LibrarySectionTitle = sec.Title
		mc.LibrarySectionUUID = sec.UUID
	}
	return mc
}

// titleVisible reports whether the user may see the title's section.
func (s *Server) titleVisible(u *frontend.User, t *frontend.Title) bool {
	return s.visibility().TitleAllowed(u, t) && s.plexMetaRatingAllowed(u, t)
}

// metadataKeys parses a detail key. Numeric rating keys stay as they are.
// A plex:// guid (More Ways to Watch keeps the discover identity and asks
// this server for it) resolves through the local mapping. A variant part id
// resolves to the title that owns that part, so a re-read of the part the
// client just selected still shows the subtitle.
func (s *Server) metadataKeys(ctx context.Context, raw string) ([]frontend.RatingKey, error) {
	raw = strings.TrimSpace(raw)
	if decoded, err := url.PathUnescape(raw); err == nil && decoded != "" {
		raw = decoded
	}
	if strings.Contains(raw, "://") {
		if s.d.LibraryGUIDs == nil {
			return nil, errors.New("no keys")
		}
		// A list of plex.tv identities (Infuse resolves its Watchlist row
		// this way) names the library titles that have one; the others
		// are skipped, as unknown keys in a key list are.
		guids := strings.Split(raw, ",")
		var keys []frontend.RatingKey
		var unknown []string
		for _, guid := range guids {
			guid = strings.TrimSpace(guid)
			k, err := s.d.LibraryGUIDs.ExactKey(ctx, guid)
			if err != nil {
				if len(guids) == 1 {
					return nil, err
				}
				if errors.Is(err, frontend.ErrNotFound) {
					unknown = append(unknown, guid)
				}
				continue
			}
			keys = append(keys, k)
		}
		s.resolveGUIDsLater(unknown)
		if len(keys) == 0 {
			return nil, frontend.ErrNotFound
		}
		return keys, nil
	}
	keys, err := parseKeys(raw)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		if mapped, rel, ok := s.partTitle(int64(k)); ok && rel != "" {
			keys[i] = mapped
		}
	}
	return keys, nil
}

// handleMetadataGUIDPath answers /library/metadata/plex://{type}/{id}[,...]:
// the identities' slashes keep the path off the {key} route.
func (s *Server) handleMetadataGUIDPath(w http.ResponseWriter, r *http.Request) {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	rctx.URLParams.Add("key", strings.TrimPrefix(r.URL.Path, "/library/metadata/"))
	s.handleMetadata(w, r)
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	startDetailPageWait(pr)
	q := r.URL.Query()
	// Validate before any catalogue lookup or playback warm-up: these flags
	// have a boolean integer grammar even when their work is asynchronous.
	for _, name := range []string{"checkFiles", "skipRefresh", "asyncCheckFiles", "checkFileAvailability", "asyncAugmentMetadata", "asyncRefreshLocalMediaAgent", "asyncRefreshAnalysis"} {
		if values, present := q[name]; present {
			n, err := strconv.Atoi(values[0])
			if err != nil || (n != 0 && n != 1) {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
		}
	}
	keys, err := s.metadataKeys(ctx, chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if len(keys) == 1 && pr.User != nil && isPlayableKey(keys[0]) {
		// A subtitle pick for this title may still be downloading; the
		// re-read that selects the new track must already list it (#389).
		s.subtitlePicks.wait(ctx, pr.User.ID, keys[0])
		if ctx.Err() != nil {
			return
		}
	}
	var mc *model.MediaContainer
	for _, k := range keys {
		var md model.Metadata
		switch k.Kind() {
		case frontend.KindCollection, frontend.KindSmart:
			c, err := s.collection(ctx, pr.User, k)
			if err != nil {
				if len(keys) == 1 {
					s.writeErr(w, r, err)
					return
				}
				continue
			}
			md = s.collectionMetadata(c, ctx)
			if mc == nil {
				mc = s.itemContainer(c.Type)
			}
		case frontend.KindPlaylist:
			pl, err := s.playlist(ctx, pr.User, k)
			if err != nil {
				if len(keys) == 1 {
					s.writeErr(w, r, err)
					return
				}
				continue
			}
			md = s.playlistListingMetadata(ctx, pl)
			if pl.SmartURI != "" {
				md.Content = "library://x/directory/" + url.QueryEscape(pl.SmartURI)
			}
			if mc == nil {
				mc = libraryContainer()
			}
		case frontend.KindExtra:
			emd, ok := s.extraMetadataByKey(ctx, pr, k)
			if !ok {
				if len(keys) == 1 {
					writeHTTPError(w, http.StatusNotFound)
					return
				}
				continue
			}
			md = emd
			if mc == nil {
				mc = libraryContainer()
			}
		default:
			t, err := s.metadataTitle(ctx, pr, k)
			if err == nil && !s.visibleTitle(ctx, pr.User, t) {
				err = frontend.ErrNotFound
			}
			// Policy/storage reads can fail closed on cancellation. Do not turn
			// that interrupted lookup into a claim that the title disappeared.
			if ctx.Err() != nil {
				return
			}
			if err != nil && len(keys) == 1 && q.Get("collection") == "" {
				if future, e := s.scheduledEpisode(ctx, pr, k); e == nil {
					md = s.scheduledMetadata(ctx, pr, future)
					mc = s.itemContainer(future.Type)
					mc.Metadata = append(mc.Metadata, md)
					continue
				}
			}
			if err != nil && len(keys) == 1 {
				if t, ck, e := s.lookupPlaceholder(ctx, pr, k, q.Get("collection")); e == nil {
					md = s.placeholderMetadata(ctx, pr, t, ck)
					mc = s.itemContainer(t.Type)
					mc.Metadata = append(mc.Metadata, md)
					continue
				}
			}
			if err != nil {
				if len(keys) == 1 {
					s.writeErr(w, r, err)
					return
				}
				continue
			}
			if len(keys) == 1 && isPlayable(t.Type) {
				// Version discovery belongs to the open detail page, not to
				// admission of the independent playback warm-up. Start it first
				// so a warm-up that needs a search can join the same work.
				if pr.User != nil && s.detailSearchAllowed(pr) {
					if refiller, ok := s.d.Variants.(frontend.SearchCacheRefiller); ok {
						// Repeated metadata fetches while the same page is being
						// watched must not retry a failed search on every fetch.
						watchKey := fmt.Sprintf("%d|%s|%d", pr.User.ID, pr.ClientID, t.Key)
						s.variantWatch.mu.Lock()
						watching := s.variantWatch.pending[watchKey]
						s.variantWatch.mu.Unlock()
						if !watching {
							pr.detailRefillRequested = true
							pr.detailRefill = refiller.RefillSearchCache(t)
						}
					}
				}
				s.warmDetail(ctx, pr, t)
			}
			if len(keys) == 1 {
				// Live session intent (#32): a single-item detail open.
				s.recordDetailIntent(ctx, pr, t)
			}
			// renderTitles uses verified cached GUIDs and queues missing identities.
			// Remote identity enrichment must not hold detail or Play requests.

			if len(keys) == 1 {
				pr.versionItemKey = t.Key
			}
			if len(keys) == 1 && (metadataOptionOn(q.Get("checkFiles")) || metadataOptionOn(q.Get("asyncCheckFiles")) || metadataOptionOn(q.Get("checkFileAvailability"))) {
				pr.versionFlowKey = t.Key
			}
			md = s.detail(ctx, pr, t, metadataOptionOn(q.Get("includeChildren")), metadataOptionOn(q.Get("includeOnDeck")), metadataOptionOn(q.Get("includeRelated")), metadataOptionOn(q.Get("includeExtras")), q.Get("extraTypes"))
			if q.Get("includePreferences") == "1" {
				md.Preferences = s.itemTrackPreferences(ctx, pr, t)
			}
			if raw, present := q["includeRelatedCount"]; present && md.Related != nil {
				if count, err := strconv.Atoi(raw[0]); err == nil && count >= 0 {
					for i := range md.Related.Hub {
						h := &md.Related.Hub[i]
						if len(h.Metadata) > count {
							h.Metadata, h.More = h.Metadata[:count], true
						}
						if len(h.Directory) > count {
							h.Directory, h.More = h.Directory[:count], true
						}
						h.Size = len(h.Metadata) + len(h.Directory)
					}
				}
			}
			if mc == nil {
				mc = s.itemContainer(t.Type)
			}
		}
		mc.Metadata = append(mc.Metadata, md)
	}
	if mc == nil {
		mc = libraryContainer()
	}
	s.projectFileAvailability(r, mc)
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func metadataOptionOn(raw string) bool {
	n, err := strconv.Atoi(raw)
	return err == nil && n == 1
}

// detail renders a full item page.
func (s *Server) detail(ctx context.Context, pr *Request, t *frontend.Title, children, onDeck, related, extras bool, extraTypes string) model.Metadata {
	s.rememberTitle(t)
	memo := showEpisodes{}
	ts := []frontend.Title{*t}
	items := s.renderTitles(ctx, pr, ts, renderOpts{detail: true, media: true})
	s.applyLeafCounts(ctx, pr, items, ts, memo)
	md := items[0]
	// Per-viewer decision header (#31); detail pages only, never lists.
	s.decorateDetail(ctx, pr, t, &md)
	if children && t.Type == frontend.TypeShow {
		if kids, err := s.metadataChildren(ctx, pr, t.Key); err == nil {
			rendered := s.renderTitles(ctx, pr, kids, renderOpts{media: true, inSection: true, skipLeafCounts: t.Type == frontend.TypeShow})
			if t.Type == frontend.TypeShow {
				s.applyLeafCounts(ctx, pr, rendered, kids, memo)
			}
			md.Children = &model.Children{Size: len(rendered), Metadata: rendered}
		}
	}
	if onDeck && (t.Type == frontend.TypeShow || t.Type == frontend.TypeSeason) {
		show := t.Key
		if t.Type == frontend.TypeSeason {
			show = frontend.ShowKey(t.Key.TMDB())
		}
		if ep, st := s.showOnDeck(ctx, pr, show, memo); ep != nil && (t.Type != frontend.TypeSeason || ep.Season == t.Season) {
			// The show page's Play button plays this episode: resolve,
			// open and probe it so OnDeck and the following Play have its tracks.
			if noWarm, _ := ctx.Value(structuralProjectionKey{}).(bool); !noWarm {
				s.warmDetail(ctx, pr, ep)
			}
			if items := s.renderTitles(ctx, pr, []frontend.Title{*ep}, renderOpts{media: true, state: st}); len(items) > 0 {
				if extras {
					items[0].Extras = &model.Extras{}
					for _, e := range s.extrasFor(ctx, pr, ep, extraTypes) {
						items[0].Extras.Metadata = append(items[0].Extras.Metadata, s.extraMetadata(e, ep, true))
					}
					items[0].Extras.Size = len(items[0].Extras.Metadata)
				}
				md.OnDeck = &model.OnDeck{Metadata: items[0]}
			}
		}
	}
	if related && s.d.Recommender != nil {
		if hubs, err := s.detailRelated(ctx, pr).Rows(ctx, pr.User, t.Key, 12); err == nil {
			md.Related = &model.Related{Hub: s.renderHubs(ctx, pr, hubs, hubCtx{related: t.Key})}
		} else {
			s.log.Debug("related hubs", "key", t.Key, "err", err)
		}
	}
	if extras {
		list := s.extrasFor(ctx, pr, t, extraTypes)
		ex := &model.Extras{Size: len(list)}
		for _, e := range list {
			ex.Metadata = append(ex.Metadata, s.extraMetadata(e, t, true))
		}
		md.Extras = ex
		if len(list) > 0 {
			md.PrimaryExtraKey = "/library/metadata/" + list[0].Key.String()
		}
	}
	return md
}

// sameDayUnavailable is a local visibility lookup. Merely browsing must never
// spend search quota trying to discover an upload.
func (s *Server) sameDayUnavailable(ctx context.Context, pr *Request, t *frontend.Title) bool {
	var source frontend.AvailabilitySource
	source, _ = s.d.Variants.(frontend.AvailabilitySource)
	return s.d.Experience.SameDayUnavailable(ctx, t, s.now(), s.d.FilterAvailable, source)
}

// showOnDeck picks the episode to resume or start next.
func (s *Server) showOnDeck(ctx context.Context, pr *Request, show frontend.RatingKey, memo showEpisodes) (*frontend.Title, map[frontend.RatingKey]frontend.WatchStatus) {
	eps, err := s.episodesOfShow(ctx, pr, memo, show)
	if err != nil || len(eps) == 0 {
		return nil, nil
	}
	st := s.stateFor(ctx, pr, eps)
	return s.d.Experience.ShowOnDeck(eps, st, func(t *frontend.Title) bool { return s.sameDayUnavailable(ctx, pr, t) }), st
}

const detailWarmWaitBudget = 4 * time.Second

// Apple TV can refresh detail metadata and obtains the admitted source's
// tracks again on Play. Keep speculative preparation behind its browse
// pages; share a short allowance across the probe and version discovery.
// Android and iOS Bedrock players populate their subtitle picker from the
// pre-play detail/queue item and do not re-fetch metadata once playback starts.
func startDetailPageWait(pr *Request) {
	if plexMobileOrTVBedrockClient(pr) {
		pr.detailWaitUntil = time.Now().Add(detailWarmWaitBudget)
	} else if pr != nil && (strings.EqualFold(pr.Platform, "tvOS") || strings.EqualFold(pr.Product, "Plex for Apple TV")) {
		pr.detailWaitUntil = time.Now().Add(250 * time.Millisecond)
	}
}

// warmDetail schedules a detail-page warm-up for t and waits up to a bounded
// budget for its probe when no session, resumed probe or warm probe is ready
// yet, so the first detail/season/show open renders the file's real audio and
// subtitle tracks instead of an estimate.
func (s *Server) warmDetail(ctx context.Context, pr *Request, t *frontend.Title) {
	if t == nil || !isPlayable(t.Type) || s.d.Playback == nil {
		return
	}
	if plexMobileOrTVBedrockClient(pr) {
		if _, ok := s.d.Playback.(frontend.MetadataProber); ok {
			// A single wait: do not add speculative warmDetail's four seconds
			// after the metadata probe's own bounded wait. Errors/timeouts
			// preserve the normal detail response and background preparation.
			_ = s.probeDetailTracks(ctx, pr, t)
			return
		}
	}
	caps := s.clientCapsForKey(pr, t.Key)
	if warm, ok := s.d.Playback.(frontend.Prewarmer); ok {
		// Plex builds these caps from the device's own profile: a held
		// detail page may warm one cached add-on stream (PlayIntent).
		warm.Prewarm(ctx, pr.User, t.Key, caps, frontend.PrewarmOptions{Reason: frontend.PrewarmDetail, PlayIntent: true})
	}
	if pr == nil || pr.User == nil {
		return
	}
	if ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, t.Key); ok && ps != nil {
		return
	}
	if _, _, ok := s.resumedTracks(ctx, pr, t); ok {
		return
	}
	if wp, ok := s.d.Playback.(frontend.WarmProber); ok {
		if _, warm := wp.WarmProbeInfo(pr.User, caps, t.Key); warm {
			return
		}
	}
	if waiter, ok := s.d.Playback.(frontend.WarmProbeWaiter); ok {
		budget := detailWarmWaitBudget
		if !pr.detailWaitUntil.IsZero() {
			budget = min(budget, time.Until(pr.detailWaitUntil))
		}
		waiter.WaitWarmProbe(ctx, pr.User, caps, t.Key, budget)
	}
}

// seasonNextEpisode picks the episode a viewer opening a season page is most
// likely to play first: an in-progress episode, else the episode after the
// last watched one, else the first aired episode.
func (s *Server) seasonNextEpisode(ctx context.Context, pr *Request, eps []frontend.Title) *frontend.Title {
	if len(eps) == 0 {
		return nil
	}
	now := s.now()
	return s.d.Experience.SeasonNextEpisode(eps, s.stateFor(ctx, pr, eps), now, func(t *frontend.Title) bool { return s.sameDayUnavailable(ctx, pr, t) })
}

func (s *Server) handleChildren(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	startDetailPageWait(pr)
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	switch k.Kind() {
	case frontend.KindCollection, frontend.KindSmart:
		s.collectionItems(w, r, k)
		return
	case frontend.KindPlaylist:
		s.handlePlaylistItems(w, r)
		return
	}
	parent, err := s.metadataTitle(ctx, pr, k)
	if err == nil && !s.visibleTitle(ctx, pr.User, parent) {
		err = frontend.ErrNotFound
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if parent.Type == frontend.TypeMovie || parent.Type == frontend.TypeEpisode {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	s.rememberTitle(parent) // seasons render the show's studio/year/logo
	kids, err := s.metadataChildren(ctx, pr, k)
	if err != nil && !errors.Is(err, frontend.ErrNotFound) {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(parent.Type)
	mc.Key = parent.Key.String()
	mc.Nocache = model.Ptr(true)
	mc.Thumb = s.thumbURL(parent)
	mc.Art = artURL(parent)
	var allLeaves *model.Directory
	switch parent.Type {
	case frontend.TypeShow:
		mc.Summary = parent.Summary
		mc.ParentIndex = model.Ptr(1)
		mc.ParentTitle = parent.Title
		mc.ParentYear = parent.Year
		mc.Title1 = mc.LibrarySectionTitle
		mc.Title2 = parent.Title
		mc.ViewGroup = "season"
		mc.ViewMode = 65593
		if r.URL.Query().Get("excludeAllLeaves") != "1" {
			allLeaves = &model.Directory{LeafCount: model.Ptr(parent.LeafCount), Thumb: s.thumbURL(parent), ViewedLeafCount: model.Ptr(0),
				Key: "/library/metadata/" + parent.Key.String() + "/allLeaves", Title: "All episodes"}
		}
	case frontend.TypeSeason:
		show := parent.ShowKey
		if show == 0 {
			show = parent.Key.Parent()
		}
		mc.ParentIndex = model.Ptr(parent.Season)
		mc.ParentTitle = ""
		// PMS always sends parentTitle here, empty.
		mc.Extra = append(mc.Extra, model.A("parentTitle", ""))
		if st, err := s.d.Catalog.Get(ctx, show); err == nil {
			s.rememberTitle(st)
			if st.ContentRating != "" {
				mc.Extra = append(mc.Extra, model.A("grandparentContentRating", st.ContentRating))
			}
			if len(st.Studios) > 0 {
				mc.Extra = append(mc.Extra, model.A("grandparentStudio", st.Studios[0]))
			}
		}
		mc.GrandparentRatingKey = int64(show)
		mc.GrandparentTitle = parent.ShowTitle
		mc.GrandparentThumb = "/library/metadata/" + show.String() + "/thumb/" + imageStamp(parent)
		if mc.Art == "" {
			mc.Art = "/library/metadata/" + show.String() + "/art/" + imageStamp(parent)
		}
		mc.Title1 = parent.ShowTitle
		mc.Title2 = seasonTitle(parent)
		mc.ViewGroup = "episode"
		mc.ViewMode = viewModeList
	default:
		mc.ViewMode = viewModeList
	}
	kids = s.visibleTitles(pr.User, kids)
	if parent.Type == frontend.TypeShow && r.URL.Query().Has("unwatched") {
		if r.URL.Query().Get("unwatched") != "1" {
			kids = nil
		} else {
			eps, _ := s.episodesOfShow(ctx, pr, nil, k)
			states := s.stateFor(ctx, pr, eps)
			hasUnplayed := map[int]bool{}
			for _, ep := range eps {
				if states[ep.Key].ViewCount == 0 {
					hasUnplayed[ep.Season] = true
				}
			}
			filtered := kids[:0]
			for _, season := range kids {
				if hasUnplayed[season.Season] {
					filtered = append(filtered, season)
				}
			}
			kids = filtered
		}
	}

	childQ := r.URL.Query()
	if parent.Type == frontend.TypeShow {
		for name := range childQ {
			if strings.TrimRight(name, "!<>=") == "unwatched" {
				childQ.Del(name)
			}
		}
	}
	kids, err = s.childQuery(ctx, pr, kids, childQ)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	future := s.scheduledEpisodes(ctx, pr, k)
	kids = appendScheduled(kids, future)
	lo, hi := pr.window(len(kids))
	page := kids[lo:hi]
	if parent.Type == frontend.TypeSeason {
		if ep := s.seasonNextEpisode(ctx, pr, page); ep != nil {
			s.warmDetail(ctx, pr, ep)
		}
	}
	if len(future) == 0 {
		mc.Metadata = s.renderTitles(ctx, pr, page, renderOpts{media: true, inSection: true, skipLeafCounts: parent.Type == frontend.TypeShow})
	} else {
		// Batch ordinary rendering without ever passing placeholders to media
		// generation, then restore the canonical episode order.
		var aired []frontend.Title
		for _, child := range page {
			if !child.Released.After(s.now()) {
				aired = append(aired, child)
			}
		}
		rendered := s.renderTitles(ctx, pr, aired, renderOpts{media: true, inSection: true, skipLeafCounts: parent.Type == frontend.TypeShow})
		byKey := make(map[string]model.Metadata, len(rendered))
		for _, md := range rendered {
			byKey[md.RatingKey] = md
		}
		for _, child := range page {
			if child.Released.After(s.now()) {
				mc.Metadata = append(mc.Metadata, s.scheduledMetadata(ctx, pr, &child))
			} else if md, ok := byKey[child.Key.String()]; ok {
				mc.Metadata = append(mc.Metadata, md)
			}
		}
	}
	if parent.Type == frontend.TypeShow {
		s.applyLeafCounts(ctx, pr, mc.Metadata, page, nil)
		if allLeaves != nil {
			viewed, leaves := 0, 0
			for _, m := range mc.Metadata {
				if m.ViewedLeafCount != nil {
					viewed += *m.ViewedLeafCount
				}
				if m.LeafCount != nil {
					leaves += *m.LeafCount
				}
			}
			allLeaves.ViewedLeafCount = model.Ptr(viewed)
			if lo == 0 && hi == len(kids) {
				// Every season is on this page: total the same per-season
				// counts the viewed total comes from.
				allLeaves.LeafCount = model.Ptr(leaves)
			}
			mc.Directory = []model.Directory{*allLeaves}
		}
	}
	mc.Size = len(mc.Metadata) + len(mc.Directory)
	if pr.HasPage {
		mc.SetPage(lo, len(kids))
	}
	s.write(w, r, mc)
}

func (s *Server) handleAllLeaves(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	startDetailPageWait(pr)
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	parent, err := s.metadataTitle(ctx, pr, k)
	if err == nil && !s.visibleTitle(ctx, pr.User, parent) {
		err = frontend.ErrNotFound
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	var eps []frontend.Title
	switch parent.Type {
	case frontend.TypeShow:
		eps, err = s.metadataGrandchildren(ctx, pr, k)
	case frontend.TypeSeason:
		eps, err = s.metadataChildren(ctx, pr, k)
	default:
		eps = []frontend.Title{*parent}
	}
	if err != nil && !errors.Is(err, frontend.ErrNotFound) {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(parent.Type)
	mc.Key = parent.Key.String()
	mc.Title1 = parent.Title
	mc.Title2 = "All Episodes"
	mc.ViewGroup = "episode"
	if parent.Type == frontend.TypeMovie {
		mc.ViewGroup = "movie"
	}
	mc.ViewMode = viewModeList
	mc.Mixed = model.Ptr(true)
	// Filter before paging, as handleChildren does, so the window and
	// totalSize count only episodes the viewer may see.
	eps = s.visibleTitles(pr.User, eps)
	if r.URL.Query().Has("unwatched") {
		n, err := strconv.ParseInt(r.URL.Query().Get("unwatched"), 10, 32)
		if err != nil || n < 0 || n > 1 || strings.HasPrefix(r.URL.Query().Get("unwatched"), "-") {
			writeHTTPError(w, 400)
			return
		}
		if n == 1 {
			states := s.stateFor(ctx, pr, eps)
			filtered := eps[:0]
			for _, ep := range eps {
				if states[ep.Key].ViewCount == 0 {
					filtered = append(filtered, ep)
				}
			}
			eps = filtered
		}
	}
	leafQ := r.URL.Query()
	leafQ.Del("unwatched")
	eps, err = s.childQuery(ctx, pr, eps, leafQ)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	lo, hi := pr.window(len(eps))
	mc.Metadata = s.renderTitles(ctx, pr, eps[lo:hi], renderOpts{media: true, inSection: true})
	mc.Size = len(mc.Metadata)
	if pr.HasPage {
		mc.SetPage(lo, len(eps))
	}
	s.write(w, r, mc)
}

func (s *Server) handleParent(w http.ResponseWriter, r *http.Request) {
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	up := k.Parent()
	if s.d.EpisodeOrder != nil && k.Kind() == frontend.KindEpisode {
		if n, _, _, ok := s.d.EpisodeOrder.Position(r.Context(), FromContext(r.Context()).User, k); ok {
			up = frontend.SeasonKey(k.TMDB(), n)
		}
	}
	if strings.HasSuffix(r.URL.Path, "/grandparent") {
		up = up.Parent()
	}
	if up == 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.renderSingle(w, r, up)
}

// renderSingle writes one item as a detail container.
func (s *Server) renderSingle(w http.ResponseWriter, r *http.Request, k frontend.RatingKey) {
	ctx := r.Context()
	pr := FromContext(ctx)
	t, err := s.metadataTitle(ctx, pr, k)
	if err == nil && !s.visibleTitle(ctx, pr.User, t) {
		err = frontend.ErrNotFound
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(t.Type)
	mc.Metadata = []model.Metadata{s.detail(ctx, pr, t, false, false, false, false, "")}
	mc.Size = 1
	s.write(w, r, mc)
}

func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := keyParam(r)
	if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	limit := 20
	if n, _ := strconv.Atoi(r.URL.Query().Get("count")); n > 0 {
		limit = min(n, 100)
	}
	items, err := s.recommendationCandidates(ctx, pr.User, limit, func(ctx context.Context, fetch int) ([]frontend.Title, error) {
		return s.d.Catalog.Similar(ctx, k, fetch)
	})
	if err != nil && !errors.Is(err, frontend.ErrNotFound) {
		s.writeErr(w, r, err)
		return
	}
	mc := libraryContainer()
	mc.Metadata = s.renderTitles(ctx, pr, s.filterAllowed(pr.User, items), renderOpts{media: true})
	lo, hi := pr.window(len(mc.Metadata))
	mc.SetPage(lo, len(mc.Metadata))
	mc.Metadata = mc.Metadata[lo:hi]
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// handleItemOnDeck is /library/metadata/{show}/onDeck.
func (s *Server) handleItemOnDeck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	mc := libraryContainer()
	if k.Kind() == frontend.KindShow {
		show, err := s.metadataTitle(ctx, pr, k)
		if err == nil && !s.visibleTitle(ctx, pr.User, show) {
			err = frontend.ErrNotFound
		}
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		if ep, st := s.showOnDeck(ctx, pr, k, nil); ep != nil {
			s.warmDetail(ctx, pr, ep)
			mc.Metadata = s.renderTitles(ctx, pr, []frontend.Title{*ep}, renderOpts{media: true, state: st})
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Collections

func (s *Server) collection(ctx context.Context, u *frontend.User, k frontend.RatingKey) (*frontend.Collection, error) {
	return s.collectionSelection().Get(ctx, u, k, collectionDevice(ctx), true)
}

func (s *Server) collectionRaw(ctx context.Context, u *frontend.User, k frontend.RatingKey) (*frontend.Collection, error) {
	return s.collectionSelection().Get(ctx, u, k, "", false)
}

func collectionDevice(ctx context.Context) string {
	pr := FromContext(ctx)
	return frontend.ClassifyDevice(pr.Product, pr.Platform, pr.Device)
}

func (s *Server) collectionSelection() frontend.ScopedCollection {
	return s.d.Experience.NewScopedCollection(frontend.ScopedCollectionOptions{
		Collections:  s.d.Collections,
		Visibility:   s.visibility(),
		Now:          s.now,
		Unrestricted: func(viewer *frontend.User) bool { return !s.viewerRestricted(viewer) },
		TitleAllowed: s.titleAllowed,
		Openable: func(ctx context.Context, viewer *frontend.User, items []frontend.Title) []frontend.Title {
			return s.openableTitles(withStoredPolicy(ctx), viewer, items, false)
		},
	})
}

func (s *Server) lookupPlaceholder(ctx context.Context, pr *Request, key frontend.RatingKey, raw string) (*frontend.Title, frontend.RatingKey, error) {
	var requested *frontend.RatingKey
	if raw != "" {
		collection, err := frontend.ParseRatingKey(raw)
		if err != nil {
			return nil, 0, frontend.ErrNotFound
		}
		requested = &collection
	}
	return s.collectionSelection().Placeholder(ctx, pr.User, key, requested, frontend.ClassifyDevice(pr.Product, pr.Platform, pr.Device))
}

func (s *Server) scopedCollectionCounts(ctx context.Context, u *frontend.User, cs []frontend.Collection) []frontend.Collection {
	return s.collectionSelection().Counts(ctx, u, cs, collectionDevice(ctx))
}

func (s *Server) collectionMetadata(c *frontend.Collection, contexts ...context.Context) model.Metadata {
	rk := c.Key.String()
	m := model.Metadata{
		RatingKey: rk,
		Key:       "/library/collections/" + rk + "/children",
		GUID:      "collection://" + rk,
		Type:      "collection",
		Title:     c.Title,
		Subtype:   string(c.Type),
		Summary:   c.Summary,
		Index:     model.Ptr(0),
		Thumb:     "/library/metadata/" + rk + "/thumb/" + collectionStamp(c),
		AddedAt:   s.now().Unix(),
		UpdatedAt: s.now().Unix(),
	}
	// PMS omits childCount for an empty collection on detail and listing
	// responses, while retaining the collection itself.
	if c.ChildCount > 0 {
		m.ChildCount = model.Ptr(c.ChildCount)
	}
	if c.BackdropPath != "" {
		m.Art = "/library/metadata/" + rk + "/art/" + collectionStamp(c)
	}
	// PMS reports a collection's smart flag as the string "1"/"0".
	smart := "0"
	if c.Kind != "user" && c.Kind != "franchise" {
		smart = "1"
	}
	m.Extra = []model.Attr{model.A("smart", smart)}
	if sec, ok := s.sectionFor(c.Type); ok {
		m.LibrarySectionID = sec.ID
		m.LibrarySectionKey = "/library/sections/" + strconv.Itoa(sec.ID)
		m.LibrarySectionTitle = sec.Title
	}
	m.Image = []model.Image{{Alt: c.Title, Type: "coverPoster", URL: m.Thumb}}
	if m.Art != "" {
		m.Image = append(m.Image, model.Image{Alt: c.Title, Type: "background", URL: m.Art})
	}
	m.UltraBlurColors = s.ultraBlurFor(c.Key, c.BackdropPath)
	if len(contexts) > 0 {
		s.applyListWatch(contexts[0], &m, c.Key)
	}
	return m
}

// collectionRows renders a listing's collections with their watch state
// read in one batch, not one read per collection.
func (s *Server) collectionRows(ctx context.Context, cs []frontend.Collection) []model.Metadata {
	if len(cs) == 0 {
		return nil
	}
	var state map[frontend.RatingKey]frontend.WatchStatus
	if pr := FromContext(ctx); pr != nil && pr.User != nil && s.d.State != nil {
		keys := make([]frontend.RatingKey, len(cs))
		for i := range cs {
			keys[i] = cs[i].Key
		}
		var err error
		if state, err = s.d.State.GetMany(ctx, pr.User.ID, keys); err != nil {
			state = nil
		}
	}
	out := make([]model.Metadata, 0, len(cs))
	for i := range cs {
		m := s.collectionMetadata(&cs[i])
		if state != nil {
			st := state[cs[i].Key]
			m.UserRating = st.UserRating
			m.ViewOffset = ms(st.ViewOffset)
			m.ViewCount = st.ViewCount
			m.LastViewedAt = unixOrZero(st.LastViewedAt)
			m.LastRatedAt = unixOrZero(st.LastRatedAt)
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) sectionCollections(w http.ResponseWriter, r *http.Request, sec Section, sub string) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := s.sectionContainer(sec)
	_, mc.Title2 = sectionTypeTitle(sec)
	mc.ViewGroup = string(sec.Type)
	var all []frontend.Collection
	if s.d.Collections != nil {
		cs, err := s.d.Collections.List(ctx, pr.User, sec.Type)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		all = cs
	}
	all = s.scopedCollectionCounts(ctx, pr.User, all)
	all = filterCollectionTitles(all, r.URL.Query())
	lo, hi := pr.window(len(all))
	mc.Metadata = append(mc.Metadata, s.collectionRows(ctx, all[lo:hi])...)
	mc.Size = len(mc.Metadata)
	mc.SetPage(lo, len(all))
	s.write(w, r, mc)
}

// handleCollections is /library/collections (all sections).
func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := libraryContainer()
	if s.d.Collections != nil {
		for _, sec := range s.userSections(pr.User) {
			cs, err := s.d.Collections.List(ctx, pr.User, sec.Type)
			if err != nil {
				s.writeErr(w, r, err)
				return
			}
			cs = s.scopedCollectionCounts(ctx, pr.User, cs)
			mc.Metadata = append(mc.Metadata, s.collectionRows(ctx, cs)...)
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func (s *Server) handleCollection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	c, err := s.collection(ctx, FromContext(ctx).User, k)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(c.Type)
	mc.Metadata = []model.Metadata{s.collectionMetadata(c, ctx)}
	mc.Size = 1
	s.write(w, r, mc)
}

func (s *Server) handleCollectionItems(w http.ResponseWriter, r *http.Request) {
	if s.serveCollectionShelf(w, r) {
		return
	}
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.collectionItems(w, r, k)
}

func (s *Server) collectionItems(w http.ResponseWriter, r *http.Request, k frontend.RatingKey) {
	ctx := r.Context()
	pr := FromContext(ctx)
	c, err := s.collectionRaw(ctx, pr.User, k)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if !c.Upcoming && !k.IsEphemeral() {
		s.collectionMemberPage(w, r, c, k)
		return
	}
	offset, size := pr.pageOf(0)
	mc := s.collectionItemsContainer(c, k)
	if strings.HasPrefix(r.URL.Path, "/library/metadata/") {
		mc.SetSyncProjection(true)
	}
	if !c.Upcoming {
		sqlAvail, _ := s.d.Catalog.(interface{ BrowseAvailabilityInSQL() bool })
		if sqlAvail != nil && sqlAvail.BrowseAvailabilityInSQL() && !s.viewerRestricted(pr.User) && shelfToken(r.URL.RawQuery) == "" && !s.plexMetaOn() {
			page, err := s.collectionSelection().Page(ctx, pr.User, c, k, collectionDevice(ctx), offset, size)
			if err != nil {
				s.writeErr(w, r, err)
				return
			}
			mc.Metadata = s.renderTitles(ctx, pr, page.Items, renderOpts{media: true, availabilityInSQL: true})
			total := page.Total
			if len(mc.Metadata) < len(page.Items) && offset+len(page.Items) >= total {
				total = max(offset+len(mc.Metadata), 0)
			}
			mc.Size = len(mc.Metadata)
			mc.SetPage(min(offset, total), total)
			s.write(w, r, mc)
			return
		}
		all, err := s.collectionSelection().Page(ctx, pr.User, c, k, collectionDevice(ctx), 0, 5000)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		members := s.openableTitles(withStoredPolicy(ctx), pr.User, all.Items, false)
		seen := make(map[frontend.RatingKey]bool, len(members))
		uniq := members[:0]
		for _, item := range members {
			if !seen[item.Key] {
				seen[item.Key] = true
				uniq = append(uniq, item)
			}
		}
		total := len(uniq)
		offset = min(offset, total)
		end := offset + min(size, total-offset)
		mc.Metadata = s.renderTitles(ctx, pr, uniq[offset:end], renderOpts{media: true, availabilityInSQL: true})
		mc.Size = len(mc.Metadata)
		mc.SetPage(offset, total)
		s.write(w, r, mc)
		return
	}
	page, err := s.collectionSelection().Page(ctx, pr.User, c, k, collectionDevice(ctx), offset, size)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	now := s.now()
	var released []frontend.Title
	for i := range page.Items {
		if !page.Items[i].IsFutureRelease(now) {
			released = append(released, page.Items[i])
		}
	}
	var rendered []model.Metadata
	if len(released) > 0 {
		rendered = s.renderTitles(ctx, pr, released, renderOpts{media: true, availabilityInSQL: true})
	}
	renderedByKey := make(map[string]model.Metadata, len(rendered))
	for _, md := range rendered {
		renderedByKey[md.RatingKey] = md
	}
	mc.Metadata = make([]model.Metadata, 0, len(page.Items))
	for i := range page.Items {
		t := &page.Items[i]
		if t.IsFutureRelease(now) {
			mc.Metadata = append(mc.Metadata, s.placeholderMetadata(ctx, pr, t, k))
		} else if md, ok := renderedByKey[t.Key.String()]; ok {
			mc.Metadata = append(mc.Metadata, md)
		}
	}
	offset = min(offset, page.Total)
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, page.Total)
	s.write(w, r, mc)
}

// Stored collection grids use the same scoped membership generations as shelf rows.
// Ephemeral search collections have a separately paged catalogue source.
// Admission and duplicate removal happen before offsets; decoration is limited
// to the requested page. A missing title must never shorten a raw source page
// while leaving the client to advance through a different offset space.
func (s *Server) collectionMemberPage(w http.ResponseWriter, r *http.Request, c *frontend.Collection, k frontend.RatingKey) {
	ctx := withStoredPolicy(r.Context())
	pr := FromContext(ctx)
	selected, _, err := s.shelfSnapshotResult(ctx, pr, frontend.Hub{ID: "collection.grid." + k.String(), Key: k}, hubCtx{}, func() ([]frontend.Title, []model.Metadata, error) {
		page, err := s.collectionSelection().Page(ctx, pr.User, c, k, collectionDevice(ctx), 0, int(^uint(0)>>1))
		if err != nil {
			return nil, nil, err
		}
		members := s.openableTitles(ctx, pr.User, page.Items, false)
		identities := make([]model.Metadata, 0, len(members))
		for _, title := range members {
			identities = append(identities, model.Metadata{RatingKey: title.Key.String()})
		}
		return members, identities, nil
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	// Retain the discovery cut, but recheck current authorization before paging.
	selected = s.openableTitles(ctx, pr.User, selected, true)
	offset, size := pr.pageOf(0)
	total := len(selected)
	offset = min(offset, total)
	end := offset + min(size, total-offset)
	mc := s.collectionItemsContainer(c, k)
	if strings.HasPrefix(r.URL.Path, "/library/metadata/") {
		mc.SetSyncProjection(true)
	}
	mc.Metadata = s.renderTitles(ctx, pr, selected[offset:end], renderOpts{media: true, availabilityInSQL: true})
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, total)
	s.write(w, r, mc)
}

func (s *Server) collectionItemsContainer(c *frontend.Collection, k frontend.RatingKey) *model.MediaContainer {
	mc := s.itemContainer(c.Type)
	mc.Key = k.String()
	mc.Title1 = c.Title
	mc.Title2 = c.Title
	mc.Summary = c.Summary
	mc.Thumb = "/library/metadata/" + k.String() + "/thumb/" + collectionStamp(c)
	mc.ViewGroup = string(c.Type)
	mc.ViewMode = viewModeList
	return mc
}

func (s *Server) placeholderMetadata(ctx context.Context, pr *Request, t *frontend.Title, collection frontend.RatingKey) model.Metadata {
	md := s.metadata(ctx, pr, t, renderOpts{})
	md.Key = "/library/metadata/" + t.Key.String() + "?collection=" + collection.String()
	// Future placeholders use base TMDB artwork through the collection-scoped
	// image handler; overlays and custom poster providers require released titles.
	md.Thumb = s.thumbURL(t)
	md.Art = artURL(t)
	if md.Thumb != "" {
		md.Thumb += "?collection=" + collection.String()
	}
	if md.Art != "" {
		md.Art += "?collection=" + collection.String()
	}
	md.Image = nil
	md.Summary = "Scheduled release: " + t.Released.Format("2006-01-02") + ". " + md.Summary
	return md
}

// Prefer exact facts for the TV picker, without making a slow source fail
// the detail page. Completed background probes are visible on later reads.
func (s *Server) probeDetailTracks(ctx context.Context, pr *Request, t *frontend.Title) error {
	if pr == nil || pr.User == nil || s.d.Playback == nil {
		return nil
	}
	if _, _, exact := s.mediaInfoWithSource(ctx, pr, t); exact {
		return nil
	}
	if prober, ok := s.d.Playback.(frontend.MetadataProber); ok {
		if !pr.detailWaitUntil.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, pr.detailWaitUntil)
			defer cancel()
		}
		return prober.ProbeMetadata(ctx, pr.User, t.Key, s.clientCapsForKey(pr, t.Key))
	}
	return nil
}

// Disabled warming on Plex Android/iOS permits cached metadata reads only: neither
// the required probe nor version discovery may start a fresh indexer search.
// Other clients retain their existing discovery policy.
func (s *Server) detailSearchAllowed(pr *Request) bool {
	if !plexMobileOrTVBedrockClient(pr) {
		return true
	}
	c := s.cfg().Cache
	return !c.DisableSpeculation && c.Mode != "off" && c.DetailWarmup != "off"
}
