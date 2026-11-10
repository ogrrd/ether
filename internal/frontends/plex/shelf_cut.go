package plex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// The refresh interval selects a NEW generation only when building a screen.
// Continuations name the immutable generation, never the current wall clock.
const shelfGenerationParam = "etherShelf"
const shelfMaxBytes = 32 << 20
const shelfRetention = 7 * 24 * time.Hour

func init() {
	frontend.RegisterMigration("plex_shelf_generations", 1, `
CREATE TABLE plex_shelf_generations (
 generation TEXT PRIMARY KEY,
 scope TEXT NOT NULL,
 path TEXT NOT NULL,
 keys_json TEXT NOT NULL,
 expires_at BIGINT NOT NULL
);
CREATE INDEX plex_shelf_generations_expiry ON plex_shelf_generations(expires_at);`)
}

type shelfCut struct {
	at         time.Time
	generation string
	revision   string
}
type shelfGeneration struct {
	scope       string
	path        string
	keys        []frontend.RatingKey
	expires     int64
	items       []frontend.Title
	bytes       int
	serial      uint64
	persisted   bool
	lastPersist time.Time
}

func (s *Server) shelfScope(ctx context.Context, pr *Request) (string, string, bool) {
	user, _ := json.Marshal(pr.User)
	viewerRevision := ""
	var policy int64
	if rev, hasRev, err := s.exp().HomeRevision(ctx, pr.User); hasRev {
		if err != nil {
			return "", "", false
		}
		viewerRevision = rev.Viewer
		if s.viewerRestricted(pr.User) {
			policy = rev.Policy
		}
	}
	// Local config/layout counters and timestamp formatting are not cluster
	// identities. Authorization is the shared policy epoch and stable user data.
	data, _ := json.Marshal([]any{json.RawMessage(user), pr.User != nil && pr.User.Delegated, policy,
		pr.ClientID, pr.Language, pr.Product, pr.Version, pr.Platform, pr.PlatformVersion,
		pr.Device, pr.DeviceName, pr.Model, pr.ProfileExtra, pr.ClientCapabilities, pr.Remote})
	return fmt.Sprintf("%x", sha256.Sum256(data)), viewerRevision, true
}

func (s *Server) shelfSnapshot(ctx context.Context, pr *Request, h frontend.Hub, hc hubCtx, build func() ([]frontend.Title, []model.Metadata)) ([]frontend.Title, string) {
	titles, generation, _ := s.shelfSnapshotResult(ctx, pr, h, hc, func() ([]frontend.Title, []model.Metadata, error) {
		titles, metadata := build()
		return titles, metadata, nil
	})
	return titles, generation
}

// shelfSnapshotResult never publishes an incomplete generation after a source
// read fails. Existing hub builders have already handled their source errors.
func (s *Server) shelfSnapshotResult(ctx context.Context, pr *Request, h frontend.Hub, hc hubCtx, build func() ([]frontend.Title, []model.Metadata, error)) ([]frontend.Title, string, error) {
	scope, viewerRevision, ok := s.shelfScope(ctx, pr)
	if !ok {
		// Without the authorization revision a retained row cannot be served.
		return nil, "", frontend.ErrNotAvailable
	}
	identity := fmt.Sprintf("%s|%d|%d|%s", scope, hc.section, hc.related, h.ID)
	s.shelfMu.Lock()
	now := s.now()
	if cut, ok := s.shelfCuts[identity]; ok && cut.revision == viewerRevision && now.Sub(cut.at) < 5*time.Minute && !now.Before(cut.at) {
		if gen, ok := s.shelfGenerations[cut.generation]; ok {
			s.shelfMu.Unlock()
			return gen.items, cut.generation, nil
		}
	}
	s.shelfMu.Unlock()
	titles, items, err := build()
	if err != nil {
		return nil, "", err
	}
	// Wire identities, including duplicates from a producer, are canonicalized
	// BEFORE calculating counts or cutting any page.
	seen := map[string]bool{}
	byKey := map[string]frontend.Title{}
	for _, title := range titles {
		if _, ok := byKey[title.Key.String()]; !ok {
			byKey[title.Key.String()] = title
		}
	}
	unique := make([]frontend.Title, 0, len(items))
	for _, item := range items {
		if item.RatingKey != "" && !seen[item.RatingKey] {
			seen[item.RatingKey] = true
			unique = append(unique, byKey[item.RatingKey])
		}
	}
	keys := make([]frontend.RatingKey, 0, len(unique))
	for _, title := range unique {
		keys = append(keys, title.Key)
	}
	data, _ := json.Marshal(keys)
	footprint, _ := json.Marshal(unique)
	generation := fmt.Sprintf("%x", sha256.Sum256(append([]byte(identity+"|"), data...)))
	s.shelfMu.Lock()
	// Concurrent screen builds must publish the same winning generation.
	if cut, ok := s.shelfCuts[identity]; ok && cut.revision == viewerRevision && now.Sub(cut.at) < 5*time.Minute && !now.Before(cut.at) {
		if gen, ok := s.shelfGenerations[cut.generation]; ok {
			s.shelfMu.Unlock()
			return gen.items, cut.generation, nil
		}
	}
	if s.shelfCuts == nil {
		s.shelfCuts = map[string]shelfCut{}
	}
	if s.shelfGenerations == nil {
		s.shelfGenerations = map[string]shelfGeneration{}
	}
	if _, ok := s.shelfGenerations[generation]; !ok {
		s.shelfSerial++
		s.shelfGenerations[generation] = shelfGeneration{scope: scope, path: shelfPath(hubItemsKey(h, hc)), keys: keys, expires: now.Add(shelfRetention).Unix(), items: unique, bytes: len(footprint), serial: s.shelfSerial}
		s.shelfBytes += len(footprint)
	}
	s.shelfCuts[identity] = shelfCut{now, generation, viewerRevision}
	// The memory cache is an acceleration only; shared keys survive its eviction.
	for (s.shelfBytes > shelfMaxBytes || len(s.shelfGenerations) > 512) && len(s.shelfGenerations) > 1 {
		oldest := generation
		for k, g := range s.shelfGenerations {
			if g.serial < s.shelfGenerations[oldest].serial {
				oldest = k
			}
		}
		s.shelfBytes -= s.shelfGenerations[oldest].bytes
		delete(s.shelfGenerations, oldest)
		for k, cut := range s.shelfCuts {
			if cut.generation == oldest {
				delete(s.shelfCuts, k)
			}
		}
	}
	g := s.shelfGenerations[generation]
	persist := s.d.DB != nil && (!g.persisted || (g.expires-now.Unix() < int64((24*time.Hour)/time.Second) && now.Sub(g.lastPersist) >= time.Hour))
	if persist && g.persisted {
		g.lastPersist = now
		s.shelfGenerations[generation] = g
	}
	s.shelfMu.Unlock()
	if persist {
		s.stageShelf(ctx, shelfWrite{generation: generation, scope: g.scope, path: g.path, keys: g.keys, expires: now.Add(shelfRetention).Unix()})
	}

	return g.items, generation, nil
}

func (s *Server) shelfURL(raw, generation string) string {
	u, err := url.Parse(raw)
	if err != nil || generation == "" {
		return raw
	}
	q := u.Query()
	q.Set(shelfGenerationParam, generation)
	u.RawQuery = q.Encode()
	return u.String()
}

// shelfToken performs one query lookup without constructing url.Values. The
// overwhelmingly common no-token route (including other queries) allocates nothing.
func shelfToken(raw string) string {
	for raw != "" {
		pair, rest, _ := strings.Cut(raw, "&")
		raw = rest
		key, value, _ := strings.Cut(pair, "=")
		if key == shelfGenerationParam {
			v, _ := url.QueryUnescape(value)
			return v
		}
	}
	return ""
}

func shelfPath(raw string) string { u, _ := url.Parse(raw); return u.Path }

func (s *Server) recoverShelf(ctx context.Context, generation string) (shelfGeneration, bool) {
	s.shelfMu.Lock()
	g, ok := s.shelfGenerations[generation]
	s.shelfMu.Unlock()
	if ok {
		return g, true
	}
	if s.d.DB == nil {
		return shelfGeneration{}, false
	}
	var data string
	err := s.d.DB.QueryRowContext(ctx, `SELECT scope,path,keys_json,expires_at FROM plex_shelf_generations WHERE generation=? AND expires_at>=?`, generation, s.now().Unix()).Scan(&g.scope, &g.path, &data, &g.expires)
	if err != nil || json.Unmarshal([]byte(data), &g.keys) != nil {
		return shelfGeneration{}, false
	}
	return g, true
}

func (s *Server) shelfTitles(ctx context.Context, pr *Request, g shelfGeneration, keys []frontend.RatingKey) ([]frontend.Title, bool) {
	titles := g.items
	if len(titles) == 0 && len(keys) > 0 {
		if cached, ok := s.catalog().(frontend.CachedTitleSource); ok {
			var err error
			titles, err = cached.CachedTitles(ctx, keys)
			if err != nil {
				return nil, false
			}
		} else {
			for _, key := range keys {
				t, err := s.metadataTitle(ctx, pr, key)
				if err != nil {
					return nil, false
				}
				titles = append(titles, *t)
			}
		}
	}
	byKey := map[frontend.RatingKey]frontend.Title{}
	for _, t := range titles {
		byKey[t.Key] = t
	}
	ordered := make([]frontend.Title, 0, len(keys))
	for _, key := range keys {
		t, ok := byKey[key]
		if !ok {
			return nil, false
		}
		ordered = append(ordered, t)
	}
	return ordered, true
}

type shelfMembershipKey struct{}

func shelfMember(ctx context.Context, key frontend.RatingKey) (frontend.Title, bool) {
	titles, _ := ctx.Value(shelfMembershipKey{}).(map[frontend.RatingKey]frontend.Title)
	title, ok := titles[key]
	return title, ok
}
func shelfContainsAll(ctx context.Context, titles []frontend.Title) bool {
	if len(titles) == 0 {
		return false
	}
	for _, t := range titles {
		if _, ok := shelfMember(ctx, t.Key); !ok {
			return false
		}
	}
	return true
}

// Registered after authentication. Missing shared generations fall through to
// the current route's build; scope violations still fail current authorization.
func (s *Server) serveShelfGeneration(w http.ResponseWriter, r *http.Request) bool {
	generation := shelfToken(r.URL.RawQuery)
	if generation == "" {
		return false
	}
	g, ok := s.recoverShelf(r.Context(), generation)
	if !ok {
		return false
	}
	pr := FromContext(r.Context())
	scope, _, valid := s.shelfScope(r.Context(), pr)
	if !valid || g.scope != scope {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	keys := g.keys
	metadata := strings.HasPrefix(r.URL.Path, "/library/metadata/")
	if metadata {
		requested, err := parseKeys(strings.TrimPrefix(r.URL.Path, "/library/metadata/"))
		if err != nil || len(requested) > len(keys) {
			writeHTTPError(w, http.StatusNotFound)
			return true
		}
		for i, key := range requested {
			if keys[i] != key {
				writeHTTPError(w, http.StatusNotFound)
				return true
			}
		}
		keys = requested
	} else if r.URL.Path != g.path {
		writeHTTPError(w, http.StatusNotFound)
		return true
	}
	total := len(keys)
	start, end := pr.window(total)
	if metadata {
		start, end = 0, total
	} // PMS hubKey names a detail subset, not a listing.
	titles, ok := s.shelfTitles(r.Context(), pr, g, keys[start:end])
	if !ok {
		return false
	}
	if metadata {
		members := map[frontend.RatingKey]frontend.Title{}
		for _, t := range titles {
			members[t.Key] = t
		}
		*r = *r.WithContext(context.WithValue(r.Context(), shelfMembershipKey{}, members))
		return false // retain the existing full-detail metadata renderer and container
	}
	mc := hubContainer()
	if strings.HasPrefix(r.URL.Path, "/library/collections/") {
		key, err := keyParam(r)
		if err != nil {
			return false
		}
		c, err := s.collectionRaw(r.Context(), pr.User, key)
		if err != nil {
			return false
		}
		mc = s.collectionItemsContainer(c, key)
	}
	mc.Metadata = s.renderTitles(r.Context(), pr, titles, renderOpts{media: true, availabilityInSQL: true})
	mc.Size = len(mc.Metadata)
	mc.SetPage(start, total)
	s.write(w, r, mc)
	return true
}
