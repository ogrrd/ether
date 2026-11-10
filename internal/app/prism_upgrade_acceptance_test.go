package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/libraries"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/store"
)

// Delivered v0.2.48 is commit 9503ece9. The fixture under testdata/prism-upgrade
// is that commit's registered SQLite migrations, extracted from source, not a
// database this binary migrated and then mislabeled. Fresh installs never
// apply auth/4 while auth_sessions already exists; this test does.
const delivered9503Commit = "9503ece9d544e5f111f2afa531e87a2c3ad0e6c1"

const (
	upgradeMachineID    = "prism-upgrade-machine-0123456789abcdef"
	upgradeMovieLibrary = "Basement Films"
	upgradeShowLibrary  = "Basement Series"
	upgradeProfileName  = "basement-direct"
	upgradeSourcePref   = "usenet"
	upgradeIndexerID    = "source-key-nzb"
	upgradeServerToken  = "fixture-server-token"
	upgradeAdminToken   = "ether-admin-fixture-session"
	upgradeOwnerID      = 7
	upgradeFriendID     = 8
	upgradeOwnerPlexID  = int64(4242)
	upgradeFriendPlexID = int64(4343)
	upgradeRatingKey    = int64(1001)
	upgradeViewOffset   = int64(123456789)
	upgradeRules        = `{"audio":"eng","profile":"basement-direct"}`
)

var upgradeClock = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

type deliveredStep struct {
	Component string
	Version   int
	SQL       string
}

// What a released installation can show through public APIs after the new
// binary opens it, versus what only the old database file can prove.
//
// Public, no old rows required: auth.Service.Authenticate rejects an
// ether-admin- token before plex.tv, and plex.Server calls that method for
// every non-public Plex route. OwnerSession is the admin API, not Plex.
//
// Requires the 9503 fixture: account ids, the server-token hash, the admin
// session hash, watch progress, track-preference profile text, and the
// config document (machine id, library names, profile name, source
// preference, indexer id, setupComplete) all survive migrations that did
// not exist in 9503, including auth/4's user-table rebuild.
//
// PostgreSQL is a separate path. SQLite commits each migration and keeps a
// pre-migrate snapshot that nothing restores automatically. PostgreSQL
// applies the pending set in one transaction, writes no snapshot, and
// auth/4 is only "drop NOT NULL on plex_id". A SQLite result is not
// PostgreSQL coverage.

func TestPrismUpgradeFromDelivered9503(t *testing.T) {
	assertUpgradeBoundary(t)
	dir := t.TempDir()
	writeInstalledConfig(t, dir)
	buildDeliveredDB(t, dir, nil)
	original, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(original), `"setupComplete": true`) && !strings.Contains(string(original), `"setupComplete":true`) {
		t.Fatalf("fixture config lost setupComplete before open: %s", original)
	}
	snaps, err := preUpgradeFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertInstalledConfig(t, cfg.Get())

	ctx := context.Background()
	db, dialect, err := store.OpenWith(ctx, store.Options{DataDir: dir, SnapshotFiles: snaps})
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if dialect != store.SQLite {
		t.Fatalf("dialect %s", dialect)
	}
	assertInstalledConfig(t, cfg.Get())
	if cfg.Get().SetupComplete != true {
		t.Fatal("upgrade cleared setupComplete")
	}

	path := filepath.Join(dir, "ether.db")
	marker := path + store.PreMigrateSuffix + ".incomplete"
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("successful upgrade left the incomplete snapshot marker: %v", err)
	}
	if _, err := os.Stat(path + store.PreMigrateSuffix); err != nil {
		t.Fatalf("pre-migrate snapshot: %v", err)
	}
	snapCfg, err := os.ReadFile(filepath.Join(path+store.PreMigrateSuffix+store.PreMigrateFilesSuffix, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(snapCfg) != string(original) {
		t.Fatal("snapshot config is not the pre-upgrade file")
	}
	if !strings.Contains(string(snapCfg), upgradeMachineID) || !jsonBool(t, snapCfg, "setupComplete") {
		t.Fatalf("snapshot config lost machine id or setupComplete: %s", snapCfg)
	}

	snap := openSQLiteRO(t, path+store.PreMigrateSuffix)
	assertUpgradeTables(t, db, captureUpgradeTables(t, snap, store.SQLite))
	assertPrismCandidateArtifacts(t, db)
	if got := maxMigration(t, snap, "auth"); got != 3 {
		t.Fatalf("snapshot auth version %d, want the 9503 schema (3)", got)
	}
	if got := maxMigration(t, db, "auth"); got != 9 {
		t.Fatalf("upgraded auth version %d, want 9", got)
	}
	// afdcb3f7 rejected capability_headers because that slice's base had not
	// landed it. b7049abd is the reviewed inventory: capability_headers v3 is
	// required. Account sync has since landed as plexwatch v7,
	// state_plexwatch v6, and watchsend v2. Session checks below are unchanged.
	if store.KnownMigrations()["capability_headers"] != 3 {
		t.Fatalf("capability_headers version = %d, want 3", store.KnownMigrations()["capability_headers"])
	}
	if store.KnownMigrations()["plexwatch"] != 7 || store.KnownMigrations()["state_plexwatch"] != 6 || store.KnownMigrations()["watchsend"] != 2 {
		t.Fatalf("account sync inventory plexwatch=%d state_plexwatch=%d watchsend=%d", store.KnownMigrations()["plexwatch"], store.KnownMigrations()["state_plexwatch"], store.KnownMigrations()["watchsend"])
	}

	var liveSessions, snapSessions int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE token_hash=?`, auth.HashToken(upgradeAdminToken)).Scan(&liveSessions); err != nil {
		t.Fatal(err)
	}
	if err := snap.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE token_hash=?`, auth.HashToken(upgradeAdminToken)).Scan(&snapSessions); err != nil {
		t.Fatal(err)
	}
	if snapSessions != 1 {
		t.Fatalf("pre-migrate snapshot lost the admin session (%d)", snapSessions)
	}
	if liveSessions != 1 {
		t.Errorf("admin session rows after upgrade = %d; auth/4 copies auth_tokens and auth_delegations, then DROP TABLE auth_users while 9503's auth_sessions row references auth_users ON DELETE CASCADE. SQLite foreign_keys are on, so that drop deletes the session. The snapshot still has it and startup does not copy the snapshot back.", liveSessions)
	}

	var plexID int64
	var owner int
	if err := db.QueryRowContext(ctx, `SELECT plex_id, is_owner FROM auth_users WHERE id=?`, upgradeOwnerID).Scan(&plexID, &owner); err != nil {
		t.Fatal(err)
	}
	if plexID != upgradeOwnerPlexID || owner != 1 {
		t.Fatalf("owner row = plex %d owner %d", plexID, owner)
	}
	var friend int64
	if err := db.QueryRowContext(ctx, `SELECT plex_id FROM auth_users WHERE id=?`, upgradeFriendID).Scan(&friend); err != nil {
		t.Fatal(err)
	}
	if friend != upgradeFriendPlexID {
		t.Fatalf("friend plex id %d", friend)
	}
	var offset int64
	if err := db.QueryRowContext(ctx, `SELECT view_offset_ns FROM state_watch WHERE user_id=? AND rating_key=?`, upgradeOwnerID, upgradeRatingKey).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	if offset != upgradeViewOffset {
		t.Fatalf("watch offset %d", offset)
	}
	var rules string
	if err := db.QueryRowContext(ctx, `SELECT rules FROM trackprefs_rules WHERE user_id=?`, upgradeOwnerID).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if rules != upgradeRules {
		t.Fatalf("profile rules %q", rules)
	}

	validator := &upgradeValidator{}
	users, err := auth.New(ctx, db, auth.Options{Now: func() time.Time { return upgradeClock }, Validator: validator, Logger: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	ownerUser, err := users.Authenticate(ctx, upgradeServerToken)
	if err != nil || ownerUser.ID != upgradeOwnerID || !ownerUser.IsOwner {
		t.Fatalf("server token after upgrade: %+v %v", ownerUser, err)
	}
	calls := validator.calls
	if _, err := users.Authenticate(ctx, upgradeAdminToken); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("admin session accepted as a Plex credential: %v", err)
	}
	if validator.calls != calls {
		t.Fatal("admin session was sent to plex.tv")
	}
	sessionUser, err := users.OwnerSession(ctx, upgradeAdminToken)
	if liveSessions == 1 && (err != nil || sessionUser == nil || sessionUser.ID != upgradeOwnerID) {
		t.Fatalf("admin session did not remain an admin credential: %+v %v", sessionUser, err)
	}

	libSvc, err := libraries.New(ctx, db, libraries.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if movies, shows := cfg.LegacyLibraryNames(); movies != "" || shows != "" {
		if err := libSvc.MigrateConfigNames(ctx, movies, shows); err != nil {
			t.Fatal(err)
		}
	}
	upgraded := &App{cfg: cfg, libraries: libSvc}
	libs := upgraded.configuredLibraries()
	if len(libs) != 2 || libs[0].ID != 1 || libs[1].ID != 2 || libs[0].Name != upgradeMovieLibrary || libs[1].Name != upgradeShowLibrary {
		t.Fatalf("library identities %+v", libs)
	}
	assertPlexProtocol(t, db, cfg, users, validator, upgraded.libraryTitles)
}

func TestPrismUpgradeFailureDoesNotRestoreSnapshot(t *testing.T) {
	order := store.ComponentOrder()
	artwork, authAt := -1, -1
	for i, component := range order {
		switch component {
		case "artwork_metadata":
			artwork = i
		case "auth":
			authAt = i
		}
	}
	if artwork < 0 || authAt < 0 || artwork > authAt {
		t.Fatalf("failure injection needs artwork_metadata before auth, order %v", order)
	}
	dir := t.TempDir()
	writeInstalledConfig(t, dir)
	// auth/4 is the first auth migration 9503 lacks. Dropping the delegations
	// table makes that statement fail after artwork_metadata, which sorts
	// first and has no dependency, has already committed.
	buildDeliveredDB(t, dir, func(q sqlQuery) {
		if _, err := q.ExecContext(context.Background(), `DROP TABLE auth_delegations`); err != nil {
			t.Fatal(err)
		}
	})
	snaps, err := preUpgradeFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	opened, _, err := store.OpenWith(ctx, store.Options{DataDir: dir, SnapshotFiles: snaps})
	if err == nil {
		opened.Close()
		t.Fatal("sabotaged upgrade succeeded")
	}
	if !strings.Contains(err.Error(), "migration auth/4") {
		t.Fatalf("upgrade error %v, want auth/4 to fail after earlier migrations committed", err)
	}
	if cfg.Get().SetupComplete != true || cfg.Get().Server.MachineIdentifier != upgradeMachineID {
		t.Fatalf("failed upgrade rewrote identity: complete=%v machine=%s", cfg.Get().SetupComplete, cfg.Get().Server.MachineIdentifier)
	}
	path := filepath.Join(dir, "ether.db")
	if _, err := os.Stat(path + store.PreMigrateSuffix + ".incomplete"); err != nil {
		t.Fatalf("failed upgrade cleared the snapshot marker: %v", err)
	}
	live := openSQLiteRO(t, path)
	snap := openSQLiteRO(t, path+store.PreMigrateSuffix)
	if got := maxMigration(t, live, "auth"); got != 3 {
		t.Fatalf("rolled-back auth/4 left auth at %d", got)
	}
	if got := maxMigration(t, live, "artwork_metadata"); got != 2 {
		t.Fatalf("earlier committed migration artwork_metadata at %d, want 2", got)
	}
	if got := maxMigration(t, snap, "artwork_metadata"); got != 0 {
		t.Fatalf("snapshot was retaken after the failure and holds artwork %d", got)
	}
	var offset int64
	if err := live.QueryRow(`SELECT view_offset_ns FROM state_watch WHERE user_id=? AND rating_key=?`, upgradeOwnerID, upgradeRatingKey).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	if offset != upgradeViewOffset {
		t.Fatalf("live watch offset %d before the second start", offset)
	}
	live.Close()
	writable := openSQLiteRW(t, path)
	if _, err := writable.Exec(`UPDATE state_watch SET view_offset_ns=? WHERE user_id=? AND rating_key=?`, int64(999999999), upgradeOwnerID, upgradeRatingKey); err != nil {
		t.Fatal(err)
	}
	writable.Close()

	opened, _, err = store.OpenWith(ctx, store.Options{DataDir: dir, SnapshotFiles: snaps})
	if err == nil {
		opened.Close()
		t.Fatal("second start recovered the sabotaged migration")
	}
	live = openSQLiteRO(t, path)
	if err := live.QueryRow(`SELECT view_offset_ns FROM state_watch WHERE user_id=? AND rating_key=?`, upgradeOwnerID, upgradeRatingKey).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	if offset != 999999999 {
		t.Fatalf("second start restored the snapshot over ether.db (offset %d)", offset)
	}
	if err := snap.QueryRow(`SELECT view_offset_ns FROM state_watch WHERE user_id=? AND rating_key=?`, upgradeOwnerID, upgradeRatingKey).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	if offset != upgradeViewOffset {
		t.Fatalf("snapshot watch offset changed to %d", offset)
	}
	if cfg.Get().SetupComplete != true {
		t.Fatal("retry cleared setupComplete")
	}
}

func TestPrismUpgradePostgresPreservesDeliveredRows(t *testing.T) {
	dsn := os.Getenv(store.TestPostgresEnv)
	if dsn == "" {
		t.Skip("ETHER_TEST_POSTGRES is not set. SQLite's per-migration commit and pre-migrate snapshot do not cover PostgreSQL: pending migrations run in one transaction, no snapshot is written, and auth/4 only drops NOT NULL on plex_id.")
	}
	ctx := context.Background()
	t.Run("preserve", func(t *testing.T) {
		db := newPostgresSchema(t, dsn)
		applyDelivered9503(t, db, store.Postgres)
		seedDeliveredRows(t, db)
		before := captureUpgradeTables(t, db, store.Postgres)
		if got := maxMigration(t, db, "auth"); got != 3 {
			t.Fatalf("postgres fixture auth %d", got)
		}
		if err := store.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		assertUpgradeTables(t, db, before)
		assertPrismCandidateArtifacts(t, db)
		if got := maxMigration(t, db, "auth"); got != 9 {
			t.Fatalf("postgres auth %d", got)
		}
		var sessions int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE token_hash=?`, auth.HashToken(upgradeAdminToken)).Scan(&sessions); err != nil {
			t.Fatal(err)
		}
		if sessions != 1 {
			t.Fatalf("postgres admin sessions %d; auth/4's PostgreSQL override is not the SQLite rebuild", sessions)
		}
		var offset int64
		if err := db.QueryRowContext(ctx, `SELECT view_offset_ns FROM state_watch WHERE user_id=? AND rating_key=?`, upgradeOwnerID, upgradeRatingKey).Scan(&offset); err != nil || offset != upgradeViewOffset {
			t.Fatalf("postgres watch %d %v", offset, err)
		}
	})
	t.Run("late failure rolls back the whole set", func(t *testing.T) {
		db := newPostgresSchema(t, dsn)
		applyDelivered9503(t, db, store.Postgres)
		seedDeliveredRows(t, db)
		before := captureUpgradeTables(t, db, store.Postgres)
		applied, err := store.AppliedMigrations(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		// catalog/10 creates this index. It runs after auth/4. A collision
		// must undo every pending migration, not leave a partial schema.
		if _, err := db.ExecContext(ctx, `CREATE INDEX catalog_titles_episode_aired ON catalog_titles(key)`); err != nil {
			t.Fatal(err)
		}
		err = store.Migrate(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "catalog/10") {
			t.Fatalf("postgres migrate error %v", err)
		}
		assertUpgradeTables(t, db, before)
		if got := captureUpgradeTables(t, db, store.Postgres); !reflect.DeepEqual(got, before) {
			t.Fatal("failed PostgreSQL migration changed delivered tables or columns")
		}
		rolledBack, err := store.AppliedMigrations(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(applied, rolledBack) {
			t.Fatalf("rollback inventory got %v want %v", rolledBack, applied)
		}
		if got := maxMigration(t, db, "auth"); got != 3 {
			t.Fatalf("postgres failure left auth at %d; the single migration transaction did not roll back", got)
		}
		if got := maxMigration(t, db, "artwork_metadata"); got != 0 {
			t.Fatalf("postgres failure kept artwork_metadata %d", got)
		}
		var sessions int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE token_hash=?`, auth.HashToken(upgradeAdminToken)).Scan(&sessions); err != nil || sessions != 1 {
			t.Fatalf("postgres failure sessions %d %v", sessions, err)
		}
	})
}

func assertUpgradeBoundary(t *testing.T) {
	t.Helper()
	known := store.KnownMigrations()
	if known["playback"] != 4 {
		t.Fatalf("playback version=%d want 4", known["playback"])
	}
	if known["sessions_source_offers"] != 2 {
		t.Fatalf("sessions_source_offers version=%d, want2", known["sessions_source_offers"])
	}
	if known["capability_headers"] != 3 {
		t.Fatalf("capability_headers version = %d, want 3", known["capability_headers"])
	}
	if known["plexwatch"] != 7 || known["state_plexwatch"] != 6 || known["watchsend"] != 2 {
		t.Fatalf("account sync inventory plexwatch=%d state_plexwatch=%d watchsend=%d", known["plexwatch"], known["state_plexwatch"], known["watchsend"])
	}
	if known["auth"] != 9 || known["auth_sessions"] != 1 {
		t.Fatalf("landed auth inventory auth=%d auth_sessions=%d", known["auth"], known["auth_sessions"])
	}
}

func assertPlexProtocol(t *testing.T, db *sql.DB, cfg *config.Store, users *auth.Service, validator *upgradeValidator, titles func() (string, string)) {
	t.Helper()
	srv := plex.NewServer(plex.Deps{SessionStores: sessions.NewSessionStores(), Experience: experience.NewSelectors(),
		Auth:              users,
		Config:            cfg.Get,
		DB:                db,
		DisableEnrichment: true,
		LibraryTitles:     titles,
		Logger:            discardLog(),
		Now:               func() time.Time { return upgradeClock },
	})
	t.Cleanup(func() { srv.Close() })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/identity", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `machineIdentifier="`+upgradeMachineID+`"`) {
		t.Fatalf("identity %d %s", rec.Code, rec.Body.String())
	}

	calls := validator.calls
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("X-Plex-Token", upgradeAdminToken)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Plex protocol accepted the admin session: %d %s", rec.Code, rec.Body.String())
	}
	if validator.calls != calls {
		t.Fatal("Plex protocol forwarded the admin session to plex.tv")
	}
	if strings.Contains(rec.Body.String(), upgradeMovieLibrary) {
		t.Fatal("unauthorized library response included the library name")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("X-Plex-Token", upgradeServerToken)
	srv.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `key="1"`) || !strings.Contains(body, `key="2"`) || !strings.Contains(body, `title="`+upgradeMovieLibrary+`"`) || !strings.Contains(body, `title="`+upgradeShowLibrary+`"`) {
		t.Fatalf("library sections %d %s", rec.Code, body)
	}
}

func assertInstalledConfig(t *testing.T, cfg config.Config) {
	t.Helper()
	if !cfg.SetupComplete || cfg.Server.MachineIdentifier != upgradeMachineID {
		t.Fatalf("config complete=%v machine=%s", cfg.SetupComplete, cfg.Server.MachineIdentifier)
	}
	// The installed profile served movies and shows; the upgrade splits it
	// into a movie and a TV profile of the same name.
	if cfg.Quality.Default != upgradeProfileName || len(cfg.Quality.Profiles) != 2 ||
		cfg.Quality.Profiles[0].Name != upgradeProfileName || cfg.Quality.Profiles[0].MediaType != core.MediaMovie ||
		cfg.Quality.Profiles[1].Name != upgradeProfileName || cfg.Quality.Profiles[1].MediaType != core.MediaTV {
		t.Fatalf("profile key %+v default %q", cfg.Quality.Profiles, cfg.Quality.Default)
	}
	if cfg.Quality.Profiles[0].CustomFormatScores[core.SourceFormat(core.SourceBluRay)] != 4000 || len(cfg.Quality.Profiles[0].QualityOrder) != 1 {
		t.Fatalf("profile ranking %+v", cfg.Quality.Profiles[0])
	}
	if cfg.Resolver.SourcePreference != upgradeSourcePref {
		t.Fatalf("source preference %q", cfg.Resolver.SourcePreference)
	}
	if len(cfg.Indexers) != 1 || cfg.Indexers[0].ID != upgradeIndexerID {
		t.Fatalf("indexer ids %+v", cfg.Indexers)
	}
}

type upgradeValidator struct{ calls int }

func (v *upgradeValidator) Account(context.Context, string) (*plextv.Account, error) {
	v.calls++
	return nil, core.ErrUnauthorized
}

func (v *upgradeValidator) HasServer(context.Context, string) (bool, error) {
	v.calls++
	return false, nil
}

func writeInstalledConfig(t *testing.T, dir string) {
	t.Helper()
	cfg := config.Defaults(dir)
	cfg.SetupComplete = true
	cfg.Server.MachineIdentifier = upgradeMachineID
	cfg.Server.FriendlyName = "Prism Upgrade Fixture"
	cfg.Server.Port = 32400
	cfg.Plex.Claimed = true
	cfg.Plex.OwnerPlexID = upgradeOwnerPlexID
	cfg.Plex.OwnerUsername = "fixture-owner"
	cfg.Plex.Token = upgradeServerToken
	cfg.Plex.ClientIdentifier = upgradeMachineID
	cfg.Quality.Default = upgradeProfileName
	cfg.Quality.Profiles = []core.QualityProfile{{
		Name:               upgradeProfileName,
		MaxHeight:          1080,
		QualityOrder:       []core.QualityItem{{Name: "1080p", Qualities: []string{"bluray-1080", "webdl-1080"}}},
		CustomFormatScores: map[string]int{core.SourceFormat(core.SourceBluRay): 4000, core.CodecFormat("hevc"): 6},
		PreferredLanguages: []string{"eng"},
	}}
	cfg.Resolver.SourcePreference = upgradeSourcePref
	cfg.Indexers = []config.Indexer{{
		ID:      upgradeIndexerID,
		Name:    "Fixture NZB",
		URL:     "https://indexer.fixture.invalid",
		APIKey:  "fixture-indexer-key",
		Enabled: true,
	}}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// An installed pre-v0.3.8 server kept its library names in config.json;
	// the upgrade moves them into the libraries table.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	server, _ := doc["server"].(map[string]any)
	server["moviesLibraryName"] = upgradeMovieLibrary
	server["tvShowsLibraryName"] = upgradeShowLibrary
	if raw, err = json.MarshalIndent(doc, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sqlQuery is the subset of *sql.DB and *sql.Tx the fixture builder uses.
// Building the 9503 file is one transaction so WSL does not fsync every
// statement; the upgrade under test still opens that file through store.OpenWith.
type sqlQuery interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func buildDeliveredDB(t *testing.T, dir string, sabotage func(sqlQuery)) {
	t.Helper()
	db := openSQLiteRW(t, filepath.Join(dir, "ether.db"))
	if _, err := db.Exec(`PRAGMA synchronous=OFF`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	applyDelivered9503(t, tx, store.SQLite)
	if got := maxMigration(t, tx, "auth"); got != 3 {
		t.Fatalf("fixture auth version %d, want 9503's 3 (not this binary's schema)", got)
	}
	if got := maxMigration(t, tx, "auth_sessions"); got != 1 {
		t.Fatalf("fixture auth_sessions %d", got)
	}
	var prism int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('artwork_image_metadata','identity_grants','frontend_tickets')`).Scan(&prism); err != nil || prism != 0 {
		t.Fatalf("fixture already contains prism tables (%d, %v)", prism, err)
	}
	seedDeliveredRows(t, tx)
	if sabotage != nil {
		sabotage(tx)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedDeliveredRows(t *testing.T, db sqlQuery) {
	t.Helper()
	ctx := context.Background()
	created := upgradeClock.Add(-30 * 24 * time.Hour).Unix()
	expires := upgradeClock.Add(20 * 24 * time.Hour).Unix()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO auth_users(id,plex_id,username,title,is_owner,active,origin,created_at,updated_at) VALUES(?,?,?,?,1,1,'friend',?,?)`,
		upgradeOwnerID, upgradeOwnerPlexID, "fixture-owner", "Fixture Owner", created, created)
	exec(`INSERT INTO auth_users(id,plex_id,username,title,is_owner,active,origin,created_at,updated_at) VALUES(?,?,?,?,0,1,'friend',?,?)`,
		upgradeFriendID, upgradeFriendPlexID, "fixture-friend", "Fixture Friend", created, created)
	exec(`INSERT INTO auth_tokens(token_hash,user_id,source,created_at) VALUES(?,?, 'server', ?)`,
		auth.HashToken(upgradeServerToken), upgradeOwnerID, created)
	exec(`INSERT INTO auth_sessions(token_hash,user_id,created_at,expires_at) VALUES(?,?,?,?)`,
		auth.HashToken(upgradeAdminToken), upgradeOwnerID, created, expires)
	exec(`INSERT INTO state_watch(user_id,rating_key,view_offset_ns,duration_ns,view_count,counted,last_viewed_at,user_rating,updated_at) VALUES(?,?,?,5000000000,2,1,?,8.5,?)`,
		upgradeOwnerID, upgradeRatingKey, upgradeViewOffset, upgradeClock.Unix(), upgradeClock.Unix())
	exec(`INSERT INTO auth_delegations(token_hash,user_id,expires_at) VALUES('fixture-delegation',?,?)`, upgradeFriendID, expires)
	exec(`INSERT INTO state_history(id,user_id,rating_key,viewed_at,completed,imported) VALUES(61,?,?,?,1,1)`, upgradeOwnerID, upgradeRatingKey, created)
	exec(`INSERT INTO state_playlists(id,user_id,title,smart,updated_at) VALUES(62,?,'Fixture playlist',0,?)`, upgradeFriendID, created)
	exec(`INSERT INTO state_playlist_items(playlist_id,position,rating_key) VALUES(62,0,?)`, upgradeRatingKey)
	exec(`INSERT INTO cluster_nodes(id,name,version,started_at,last_seen,incarnation,migrations) VALUES('old-node','Delivered','v0.2.48',?,?,'old-process','{"auth":3,"cluster":2}')`, created, created)
	exec(`INSERT INTO cluster_leader(id,epoch,node_id,acquired_at) VALUES(1,17,'old-node',?)`, created)
	exec(`INSERT INTO cluster_update_lease(id,node_id,version,acquired_at,expires_at,failed_version) VALUES(1,'old-node','v0.2.48',?,?,'failed-fixture')`, created, expires)
	exec(`INSERT INTO plex_sessions(id,node_id,user_id,session_key,data,updated_at) VALUES('fixture-playback','old-node',?,91,'{"fixture":"session"}',?)`, upgradeFriendID, created)
	exec(`INSERT INTO plex_playqueues(id,user_id,rev,data,updated_at) VALUES(92,?,4,'{"fixture":"queue"}',?)`, upgradeFriendID, created)
	exec(`INSERT INTO availability_releases(id,rating_key,source_id,generation,payload,title,name,protocol,first_seen,last_seen) VALUES('fixture-release',?,'fixture-source','generation','{"name":"preserved"}','Fixture title','Fixture release','usenet',?,?)`, upgradeRatingKey, created, created)
	exec(`INSERT INTO availability_feed_items(id,source_id,generation,match_title,payload,seen_at) VALUES('fixture-feed','fixture-source','generation','Fixture title','{"name":"preserved feed"}',?)`, created)
	exec(`INSERT INTO trackprefs_rules(user_id,rules) VALUES(?,?)`, upgradeOwnerID, upgradeRules)
	exec(`INSERT INTO trackprefs_memory(user_id,show_id,kind,choice,updated_at) VALUES(?,55,'audio','eng',?)`, upgradeOwnerID, upgradeClock.Unix())
}

func applyDelivered9503(t *testing.T, db sqlQuery, dialect store.Dialect) {
	t.Helper()
	steps := loadDelivered9503Dialect(t, dialect)
	ctx := context.Background()
	create := `CREATE TABLE schema_migrations (component TEXT NOT NULL, version INTEGER NOT NULL, PRIMARY KEY (component, version))`
	if dialect == store.Postgres {
		create = store.TranslateDDL(create)
	}
	if _, err := db.ExecContext(ctx, create); err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		sqlText := step.SQL

		if _, err := db.ExecContext(ctx, sqlText); err != nil {
			t.Fatalf("apply %s/%d: %v", step.Component, step.Version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(component, version) VALUES (?, ?)`, step.Component, step.Version); err != nil {
			t.Fatal(err)
		}
	}
}

func loadDelivered9503(t *testing.T) []deliveredStep {
	return loadDelivered9503Dialect(t, store.SQLite)
}

func loadDelivered9503Dialect(t *testing.T, dialect store.Dialect) []deliveredStep {
	t.Helper()
	dir := filepath.Join("testdata", "prism-upgrade")
	filename := "schema.sql"
	if dialect == store.Postgres {
		filename = "schema-postgres.sql"
	}
	schema, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(schema)
	var manifest struct {
		Commit         string `json:"commit"`
		SHA256         string `json:"sha256"`
		PostgresSHA256 string `json:"postgres_sha256"`
		Migrations     []struct {
			Component string `json:"component"`
			Version   int    `json:"version"`
		} `json:"migrations"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	expected := manifest.SHA256
	if dialect == store.Postgres {
		expected = manifest.PostgresSHA256
	}
	if manifest.Commit != delivered9503Commit || expected != hex.EncodeToString(sum[:]) {
		t.Fatalf("fixture provenance commit %s sha %s", manifest.Commit, manifest.SHA256)
	}
	parts := strings.Split(string(schema), "\n-- prism-upgrade-migration-end\n")
	var steps []deliveredStep
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "-- migration ") {
			continue
		}
		line, sqlText, ok := strings.Cut(part, "\n")
		for ok && strings.HasPrefix(strings.TrimSpace(line), "--") && !strings.HasPrefix(strings.TrimSpace(line), "-- migration ") {
			line, sqlText, ok = strings.Cut(sqlText, "\n")
		}
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "-- migration "))
		if len(fields) != 2 {
			t.Fatalf("migration header %q", line)
		}
		var version int
		if _, err := parseVersion(fields[1], &version); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, deliveredStep{Component: fields[0], Version: version, SQL: strings.TrimSpace(sqlText)})
	}
	if len(steps) != len(manifest.Migrations) {
		t.Fatalf("schema has %d migrations, manifest %d", len(steps), len(manifest.Migrations))
	}
	for i, step := range steps {
		if step.Component != manifest.Migrations[i].Component || step.Version != manifest.Migrations[i].Version {
			t.Fatalf("migration %d is %s/%d, manifest %s/%d", i, step.Component, step.Version, manifest.Migrations[i].Component, manifest.Migrations[i].Version)
		}
	}
	return steps
}

func parseVersion(s string, dst *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("bad version " + s)
		}
		n = n*10 + int(c-'0')
	}
	*dst = n
	return n, nil
}

func maxMigration(t *testing.T, db sqlQuery, component string) int {
	t.Helper()
	var v int
	err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations WHERE component=?`, component).Scan(&v)
	if err != nil {
		t.Fatalf("version %s: %v", component, err)
	}
	return v
}

func openSQLiteRW(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func openSQLiteRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func newPostgresSchema(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatal(err)
	}
	schema := "ether_prism_up_" + hex.EncodeToString(rnd[:])
	admin, err := sql.Open(store.PostgresDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	loc, err := postgresSearchPath(dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(store.PostgresDriverName, loc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var got string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&got); err != nil || got != schema {
		t.Fatalf("search_path %q (%v); refusing to touch another schema", got, err)
	}
	return db
}

func postgresSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return "", errors.New("ETHER_TEST_POSTGRES must be a postgres URL so the upgrade test can set search_path")
	}
	q := u.Query()
	q.Set("options", "-csearch_path="+schema)
	// This fixture reads the same tables before and after DDL on one pool.
	// Re-describe each query so pgx does not reuse a pre-migration row shape.
	q.Set("default_query_exec_mode", "describe_exec")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func jsonBool(t *testing.T, raw []byte, key string) bool {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	v, ok := doc[key].(bool)
	return ok && v
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestPrismPlaybackPayloadMigrationFailureRepair(t *testing.T) {
	for _, sabotage := range []string{"malformed-parent", "incompatible-index"} {
		t.Run(sabotage, func(t *testing.T) {
			ctx := context.Background()
			var db *sql.DB
			if dsn := os.Getenv(store.TestPostgresEnv); dsn != "" {
				db = newPostgresSchema(t, dsn)
				applyDelivered9503(t, db, store.Postgres)
				seedDeliveredRows(t, db)
			} else {
				dir := t.TempDir()
				buildDeliveredDB(t, dir, nil)
				db = openSQLiteRW(t, filepath.Join(dir, "ether.db"))
				t.Cleanup(func() { db.Close() })
			}
			before := captureUpgradeTables(t, db, store.DialectOf(db))
			applied, err := store.AppliedMigrations(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if sabotage == "malformed-parent" {
				if _, err := db.ExecContext(ctx, `CREATE TABLE playback_payload_incarnations(collision INTEGER)`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.ExecContext(ctx, `CREATE TABLE payload_index_collision(collision INTEGER); CREATE INDEX playback_payload_incarnation_slot ON payload_index_collision(collision)`); err != nil {
					t.Fatal(err)
				}
			}
			err = store.Migrate(ctx, db)
			if err == nil || !strings.Contains(err.Error(), "playback/4") {
				t.Fatalf("payload migration failure=%v", err)
			}
			assertUpgradeTables(t, db, before)
			// SQLite commits playback/3 (released original_language) before
			// playback/4 fails. PostgreSQL applies the pending set in one
			// transaction, so the failure leaves the delivered playback head.
			wantPlayback := 2
			if store.DialectOf(db) != store.Postgres {
				wantPlayback = 3
			}
			if got := maxMigration(t, db, "playback"); got != wantPlayback {
				t.Fatalf("failed payload version recorded=%d want %d", got, wantPlayback)
			}
			if store.DialectOf(db) == store.Postgres {
				rolledBack, err := store.AppliedMigrations(ctx, db)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(applied, rolledBack) {
					t.Fatalf("failed PG pending set not rolled back: %v want %v", rolledBack, applied)
				}
			}
			if sabotage == "malformed-parent" {
				if _, err := db.ExecContext(ctx, `DROP TABLE playback_payload_incarnations`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.ExecContext(ctx, `DROP INDEX playback_payload_incarnation_slot; DROP TABLE payload_index_collision`); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Migrate(ctx, db); err != nil {
				t.Fatal(err)
			}
			assertUpgradeTables(t, db, before)
			assertUpgradeInventory(t, db)
			if got := maxMigration(t, db, "playback"); got != 4 {
				t.Fatalf("repaired payload version=%d", got)
			}
			assertPayloadWitnessUniqueness(t, db)
			if err := store.Migrate(ctx, db); err != nil {
				t.Fatalf("valid existing schema rerun=%v", err)
			}
		})
	}
}

func assertPayloadWitnessUniqueness(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"playback_payload_incarnations", "playback_payload_owners", "playback_payload_deleting"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("fresh payload table %s count=%d err=%v", table, n, err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO playback_payload_incarnations(domain,incarnation,witness_slot) VALUES('constraint-domain','first',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO playback_payload_incarnations(domain,incarnation,witness_slot) VALUES('constraint-domain','second',0)`); err == nil {
		t.Fatal("duplicate domain/witness slot accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO playback_payload_incarnations(domain,incarnation,witness_slot) VALUES('constraint-domain','first',1)`); err == nil {
		t.Fatal("duplicate incarnation accepted")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM playback_payload_incarnations WHERE domain='constraint-domain'`); err != nil {
		t.Fatal(err)
	}
}
func TestPrismPlaybackPayloadFreshConstraints(t *testing.T) {
	db := store.OpenTest(t)
	if got := maxMigration(t, db, "playback"); got != 4 {
		t.Fatalf("fresh playback version=%d", got)
	}
	assertPayloadWitnessUniqueness(t, db)
}

func TestSourceOfferMigrationFailureRepair(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, db *sql.DB) {
		t.Helper()
		if got := maxMigration(t, db, "sessions_source_offers"); got != 2 {
			t.Fatalf("source offer migration %d", got)
		}
		var id int64
		// Raw orphan payloads must fail the shared-header foreign key; the
		// successful path uses the common transactional admission authority.
		err := db.QueryRow(`INSERT INTO sessions_source_offers(namespace,digest,item_id,kind,reference,revision) VALUES('sessions.source-offer','missing',1,'version','reference','') RETURNING id`).Scan(&id)
		if err == nil {
			t.Fatal("orphan source offer accepted")
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		digest := strings.Repeat("a", 64)
		if err = capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{{HeaderScope: capability.HeaderScope{Namespace: "sessions.source-offer", ViewerID: 1, Surface: "silo", DeviceID: "tv"}, Digest: digest, ExpiresMillis: 9999999999999}}, capability.Limits{PerScope: 256, PerShard: 4096}); err != nil {
			t.Fatal(err)
		}
		if err = tx.QueryRow(`INSERT INTO sessions_source_offers(namespace,digest,item_id,kind,reference,revision) VALUES('sessions.source-offer',?,1,'version','reference','') RETURNING id`, digest).Scan(&id); err != nil || id <= 2147483647 || id > 9007199254740991 {
			t.Fatalf("identity range %d %v", id, err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`DELETE FROM capability_headers WHERE namespace='sessions.source-offer' AND digest=?`, digest); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sessions_source_offers`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("header revocation failed cascade %d %v", count, err)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		writeInstalledConfig(t, dir)
		buildDeliveredDB(t, dir, func(db sqlQuery) {
			if _, err := db.ExecContext(ctx, `CREATE TABLE sessions_source_offers(poison TEXT);INSERT INTO sessions_source_offers VALUES('retained')`); err != nil {
				t.Fatal(err)
			}
		})
		db, _, err := store.OpenWith(ctx, store.Options{DataDir: dir})
		if err == nil {
			db.Close()
			t.Fatal("source migration collision accepted")
		}
		if !strings.Contains(err.Error(), "sessions_source_offers/1") {
			t.Fatal(err)
		}
		repair := openSQLiteRW(t, filepath.Join(dir, "ether.db"))
		if got := maxMigration(t, repair, "sessions_source_offers"); got != 0 {
			t.Fatal("failed migration version committed")
		}
		var value string
		if err = repair.QueryRow(`SELECT poison FROM sessions_source_offers`).Scan(&value); err != nil || value != "retained" {
			t.Fatalf("collision changed %q %v", value, err)
		}
		var offset int64
		if err = repair.QueryRow(`SELECT view_offset_ns FROM state_watch WHERE user_id=? AND rating_key=?`, upgradeOwnerID, upgradeRatingKey).Scan(&offset); err != nil || offset != upgradeViewOffset {
			t.Fatalf("upgrade changed progress %d %v", offset, err)
		}
		if _, err = repair.Exec(`DROP TABLE sessions_source_offers`); err != nil {
			t.Fatal(err)
		}
		repair.Close()
		db, _, err = store.OpenWith(ctx, store.Options{DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		check(t, db)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv(store.TestPostgresEnv)
		if dsn == "" {
			t.Skip("ETHER_TEST_POSTGRES is not configured")
		}
		db := newPostgresSchema(t, dsn)
		applyDelivered9503(t, db, store.Postgres)
		seedDeliveredRows(t, db)
		if _, err := db.Exec(`CREATE TABLE sessions_source_offers(poison TEXT);INSERT INTO sessions_source_offers VALUES('retained')`); err != nil {
			t.Fatal(err)
		}
		before, err := store.AppliedMigrations(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		err = store.Migrate(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "sessions_source_offers/1") {
			t.Fatalf("collision %v", err)
		}
		after, err := store.AppliedMigrations(ctx, db)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("failed set did not roll back %v", err)
		}
		if _, err = db.Exec(`DROP TABLE sessions_source_offers`); err != nil {
			t.Fatal(err)
		}
		if err = store.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		check(t, db)
		assertUpgradeInventory(t, db)
	})
}
