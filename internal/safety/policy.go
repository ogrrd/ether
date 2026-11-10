// Package safety persists owner policy and enforces it at Plex request boundaries.
package safety

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // Owner time policies work on hosts without a zoneinfo database.

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

func init() {
	store.Register("safety", 1, `CREATE TABLE safety_policies(user_id INTEGER PRIMARY KEY, policy TEXT NOT NULL);`)
}

// Policy is the owner's per-viewer content and playback policy. A viewer's
// stream limit and content rating limit are not here: they live on the user
// record (core.User.StreamLimit, core.User.MaxContentRating), which every
// protocol already reads.
type Policy struct {
	BlockedGenres   []string `json:"blockedGenres"`
	BlockedKeywords []string `json:"blockedKeywords"`
	RemoteDisabled  bool     `json:"remoteDisabled"`
	MaxBitrateKbps  int      `json:"maxBitrateKbps"`
	// AllowedHours is empty for any time, otherwise hours 0..23 in Timezone.
	AllowedHours []int  `json:"allowedHours"`
	Timezone     string `json:"timezone"`
	// AllowDownloads is an owner override of plex.tv allowSync. Nil means
	// the plex.tv default (and the server AllowFriends setting) still apply.
	AllowDownloads *bool `json:"allowDownloads,omitempty"`
}

type Service struct {
	catalogBrowseAllowed func(*core.User, *core.Title) bool
	catalogAllowed       func(*core.User, *core.Title) bool // installed once before request serving
	db                   *sql.DB
	mu                   sync.RWMutex
	policies             map[int64]Policy

	invalMu sync.Mutex
	inval   *clusterbus.Invalidator // nil on a standalone server
	gen     atomic.Uint64

	plexShare        func(*core.User) PlexShare
	friendsDownloads func() bool
	shareCache       map[int64]cachedShare

	playbackMu    sync.Mutex
	playbackEpoch map[int64]playbackConfirm
}

// Generation changes when a viewer's safety policy is saved.
func (s *Service) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

// playbackConfirm is an unrestricted viewer's fenced snapshot at one epoch.
// A later play re-reads the epoch and skips the write when it has not moved.
type playbackConfirm struct {
	epoch  int64
	user   core.User
	policy Policy
}

func New(ctx context.Context, db *sql.DB) (*Service, error) {
	s := &Service{db: db, policies: map[int64]Policy{}}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// reload replaces the cached policies with the stored ones.
func (s *Service) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id,policy FROM safety_policies`)
	if err != nil {
		return err
	}
	defer rows.Close()
	policies := map[int64]Policy{}
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		var p Policy
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return err
		}
		policies[id] = p
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.policies = policies
	s.mu.Unlock()
	return nil
}

// SetBus connects the cluster bus (nil disconnects): policy changes made on
// this node reach the others' caches and vice versa.
func (s *Service) SetBus(bus clusterbus.Bus) {
	iv := clusterbus.NewInvalidator(bus, "safety", nil, func(ctx context.Context, _ []string, _ bool) {
		_ = s.reload(ctx) // on error keep the last policies; the next change retries
	})
	s.invalMu.Lock()
	old := s.inval
	s.inval = iv
	s.invalMu.Unlock()
	old.Close()
}

// Close disconnects the cluster bus.
func (s *Service) Close() { s.SetBus(nil) }
func (s *Service) Get(id int64) Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.policies[id]
	p.AllowedHours = append([]int(nil), p.AllowedHours...)
	p.BlockedGenres = append([]string(nil), p.BlockedGenres...)
	p.BlockedKeywords = append([]string(nil), p.BlockedKeywords...)
	return p
}

// Validate reports why p cannot be stored, if it cannot.
func (p Policy) Validate() error {
	if p.MaxBitrateKbps < 0 {
		return fmt.Errorf("invalid viewer or bitrate")
	}
	for _, h := range p.AllowedHours {
		if h < 0 || h > 23 {
			return fmt.Errorf("hours must be 0..23")
		}
	}
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			return fmt.Errorf("invalid timezone")
		}
	}
	return nil
}

func (s *Service) Put(ctx context.Context, id int64, p Policy) error {
	// Cache an independent snapshot, just as Get returns one to callers.
	p.AllowedHours = append([]int(nil), p.AllowedHours...)
	p.BlockedGenres = append([]string(nil), p.BlockedGenres...)
	p.BlockedKeywords = append([]string(nil), p.BlockedKeywords...)
	if id == 0 {
		return fmt.Errorf("invalid viewer or bitrate")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO safety_policies(user_id,policy) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET policy=excluded.policy`, id, string(raw)); err != nil {
		return err
	}
	if err = viewerpolicy.AddInputs(ctx, s.db, tx, viewerpolicy.SourceSafety, PolicyInputs(p)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.policies[id] = p
	s.gen.Add(1)
	s.mu.Unlock()
	s.invalMu.Lock()
	iv := s.inval
	s.invalMu.Unlock()
	iv.Notify(strconv.FormatInt(id, 10))
	return nil
}

// PolicyInputs lists the catalogue facts p reads. An episode inherits its
// show's keywords while it has none of its own, so the set's emptiness
// counts as well as each blocked value (Inputs.Field records both).
func PolicyInputs(p Policy) viewerpolicy.Inputs {
	in := viewerpolicy.Inputs{}
	if len(p.BlockedGenres) > 0 {
		in.Field("genre", p.BlockedGenres...)
	}
	if len(p.BlockedKeywords) > 0 {
		in.Field("keyword", p.BlockedKeywords...)
	}
	return in
}

// ReadPolicyInputs is the safety source of a viewer-policy inputs rebuild.
func ReadPolicyInputs(ctx context.Context, tx *sql.Tx) (viewerpolicy.Inputs, error) {
	rows, err := tx.QueryContext(ctx, `SELECT policy FROM safety_policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	in := viewerpolicy.Inputs{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var p Policy
		if json.Unmarshal([]byte(raw), &p) != nil {
			// Unreadable: whatever it blocks, treat both facets as read.
			in.Field("genre", "\x00")
			in.Field("keyword", "\x00")
			continue
		}
		for k := range PolicyInputs(p) {
			in[k] = struct{}{}
		}
	}
	return in, rows.Err()
}

// SetCatalogPolicy installs viewer-aware direct and discovery catalogue policies during construction.
func (s *Service) SetCatalogPolicy(direct, browse func(*core.User, *core.Title) bool) {
	s.catalogAllowed = direct
	s.catalogBrowseAllowed = browse
}

// TitleFacts is what Allowed reads of a title beyond its row for u: the
// genres and keywords u's policy blocks. Ratings and collections are on the
// row. The catalogue policy installed by SetCatalogPolicy is the
// catalogue's to state (core.SearchCandidates).
func (s *Service) TitleFacts(u *core.User) core.TitleFacts {
	if u == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.policies[u.ID]
	var facts core.TitleFacts
	if len(p.BlockedGenres) > 0 {
		facts |= core.FactGenres
	}
	if len(p.BlockedKeywords) > 0 {
		facts |= core.FactKeywords
	}
	return facts
}

func (s *Service) Allowed(u *core.User, t *core.Title) bool {
	if s.catalogAllowed != nil && !s.catalogAllowed(u, t) {
		return false
	}
	return s.PolicyAllowed(u, t)
}

// SetMaxRating is a no-op denial on a standalone safety service; host-side
// rating mutation is authorized and persisted through the app's ViewerPolicy.
func (s *Service) SetMaxRating(context.Context, *core.User, string) error {
	return core.ErrUnauthorized
}

// PolicyAllowed evaluates mandatory safety and share restrictions for u without
// consulting catalogue visibility rules.
func (s *Service) PolicyAllowed(u *core.User, t *core.Title) bool {
	if t == nil || t.Adult {
		return false
	}
	if u == nil {
		return true
	}
	if !AllowedWithPolicy(u, t, s.ForUser(u)) {
		return false
	}
	compiled := shareFilterFor(t, s.compiledFor(u))
	if ownerSetsRating(u) {
		compiled.allowRatings = nil
		compiled.denyRatings = nil
		compiled.maxRating = ""
		compiled.minRating = ""
	}
	return sharePassesCompiled(t, compiled)
}

// AllowedWithPolicy evaluates caller-owned policy facts. The caller must
// separately enforce the catalog visibility gate and durable policy fence.
func AllowedWithPolicy(u *core.User, t *core.Title, p Policy) bool {
	if t == nil || t.Adult {
		return false
	}
	if u == nil {
		return true
	}
	if contentrating.Exceeds(t.ContentRating, u.MaxContentRating) {
		return false
	}
	blocked := func(values, bans []string) bool {
		for _, v := range values {
			for _, b := range bans {
				if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(b)) {
					return true
				}
			}
		}
		return false
	}
	return !blocked(t.Genres, p.BlockedGenres) && !blocked(t.Keywords, p.BlockedKeywords)
}
func (s *Service) CanPlay(u *core.User, remote bool, now time.Time) bool {
	return CanPlayWithPolicy(u, s.ForUser(u), remote, now)
}

// CanPlayWithPolicy evaluates the same playback rules against an owned policy
// snapshot. It does not establish freshness or an authorization interval.
func CanPlayWithPolicy(u *core.User, p Policy, remote bool, now time.Time) bool {
	if u == nil {
		return false
	}
	if remote && p.RemoteDisabled {
		return false
	}
	if len(p.AllowedHours) == 0 {
		return true
	}
	loc := time.UTC
	if p.Timezone != "" {
		var err error
		loc, err = time.LoadLocation(p.Timezone)
		if err != nil {
			return false
		}
	}
	for _, h := range p.AllowedHours {
		if now.In(loc).Hour() == h {
			return true
		}
	}
	return false
}

// PolicyInTx reads the durable policy row. A missing row is unrestricted.
func (s *Service) PolicyInTx(ctx context.Context, tx *sql.Tx, viewerID int64) (Policy, error) {
	if s == nil || tx == nil || viewerID <= 0 {
		return Policy{}, fmt.Errorf("safety: invalid viewer")
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT policy FROM safety_policies WHERE user_id=?`, viewerID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Policy{}, nil
	}
	if err != nil {
		return Policy{}, err
	}
	var policy Policy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return Policy{}, err
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// Preset is a parental-controls starting point. MaxContentRating belongs on
// the viewer's user record; Policy is stored here. JSON flattens both.
type Preset struct {
	MaxContentRating string `json:"maxContentRating"`
	Policy
}

func KidsPreset(age string) Preset {
	rating := "PG"
	switch age {
	case "preschool":
		rating = "TV-Y"
	case "teens":
		rating = "PG-13"
	}
	return Preset{MaxContentRating: rating, Policy: Policy{BlockedKeywords: []string{"pornography", "erotic movie"}}}
}
func (s *Service) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid viewer", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Get(id))
	})
	m.HandleFunc("PUT /{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var p Policy
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		d.DisallowUnknownFields()
		if id <= 0 {
			http.Error(w, "invalid viewer", 400)
			return
		}
		if err := d.Decode(&p); err != nil {
			http.Error(w, "invalid policy", 400)
			return
		}
		if err := s.Put(r.Context(), id, p); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	})
	m.HandleFunc("GET /presets/kids", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(KidsPreset(r.URL.Query().Get("age")))
	})
	return m
}

// BrowseBlocks is the part of a viewer's policy that library SQL can apply
// before pagination: blocked genres and blocked keywords. The rating limit
// is on the user record, which browse filters already read.
func (s *Service) BrowseBlocks(u *core.User) (genres, keywords []string) {
	if s == nil || u == nil {
		return nil, nil
	}
	p := s.ForUser(u)
	return append([]string(nil), p.BlockedGenres...), append([]string(nil), p.BlockedKeywords...)
}

// BrowseShareFilter is the parsed plex.tv share predicate for one library
// type, ready to push into catalog SQL. Owner rating caps drop plex.tv
// rating clauses and keep label clauses.
func (s *Service) BrowseShareFilter(u *core.User, typ core.MediaType) core.Filter {
	if s == nil || u == nil {
		return core.Filter{}
	}
	c := s.compiledFor(u)
	f := c.movies
	if typ == core.TypeShow || typ == core.TypeSeason || typ == core.TypeEpisode {
		f = c.tv
	}
	return f.forType(typ, ownerSetsRating(u))
}

// ForUser is u's policy.
func (s *Service) ForUser(u *core.User) Policy {
	if u == nil {
		return Policy{}
	}
	return s.Get(u.ID)
}

// UserPolicy implements core.UserPolicySource: the viewer as policy-aware
// code should see them. The stream and rating limits are already on the user
// record, so this is an independent copy callers may modify.
func (s *Service) UserPolicy(u *core.User) *core.User {
	if u == nil {
		return nil
	}
	out := *u
	out.AllowedLibraries = append([]core.LibraryID(nil), u.AllowedLibraries...)
	return &out
}
func (s *Service) BitrateLimit(u *core.User) int { return s.ForUser(u).MaxBitrateKbps }

func (s *Service) Restricted(u *core.User) bool {
	if u == nil {
		return true
	}
	p := s.ForUser(u)
	share := s.shareFor(u)
	return u.MaxContentRating != "" || len(u.AllowedLibraries) > 0 || len(p.BlockedGenres) > 0 || len(p.BlockedKeywords) > 0 ||
		share.FilterMovies != "" || share.FilterTelevision != ""
}

// DiscoveryAllowed adds browsing preferences without weakening content safety.
func (s *Service) DiscoveryAllowed(u *core.User, t *core.Title) bool {
	return s.Allowed(u, t) && (s.catalogBrowseAllowed == nil || s.catalogBrowseAllowed(u, t))
}
