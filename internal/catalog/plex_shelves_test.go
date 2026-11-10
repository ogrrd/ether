package catalog

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	_ "github.com/braedonsaunders/ether/internal/state"
	"math/rand/v2"
	"testing"
	"time"
)

func TestPlexShelfDeepPageAndCount(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Now().UTC().Truncate(time.Second)
	c.SetNow(func() time.Time { return now })
	for i := 0; i < 250; i++ {
		title := core.Title{Key: core.MovieKey(int64(1000 + i)), Type: core.TypeMovie, Title: "Shelf", Released: now.Add(-time.Duration(i+1) * time.Hour)}
		if err := c.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
		count := 0
		if i < 230 {
			count = 1
		}
		if _, err := c.db.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,view_offset_ns,view_count,last_viewed_at,updated_at) VALUES(?,?,?,?,?,?)`, 1, int64(title.Key), time.Minute.Nanoseconds(), count, now.Add(-time.Duration(i)*time.Second).UnixNano(), now.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := c.RecentPage(ctx, core.BrowseQuery{Filter: core.Filter{KnownAddedDate: true}, Offset: 220, Limit: 7}, []core.MediaType{core.TypeMovie})
	if err != nil || recent.Total != 250 || len(recent.Items) != 7 || recent.Items[0].Key != core.MovieKey(1220) {
		t.Fatalf("recent deep page %+v %v", recent, err)
	}
	deck, err := c.PlexOnDeckPage(ctx, 1, 220, 7, core.Filter{}, false)
	if err != nil || deck.Total != 250 || len(deck.Items) != 7 || deck.Items[0].Key != core.MovieKey(1220) {
		t.Fatalf("onDeck deep page %+v %v", deck, err)
	}
	for i, item := range deck.Items {
		if item.Key != core.MovieKey(int64(1220+i)) {
			t.Fatalf("onDeck page order: item %d is %v", i, item.Key)
		}
	}
	// Past the end no row carries the total.
	deck, err = c.PlexOnDeckPage(ctx, 1, 300, 7, core.Filter{}, false)
	if err != nil || deck.Total != 250 || len(deck.Items) != 0 {
		t.Fatalf("onDeck past the end %+v %v", deck, err)
	}
	unwatched := true
	recent, err = c.RecentPage(ctx, core.BrowseQuery{Filter: core.Filter{KnownAddedDate: true, Group: &core.FilterGroup{Unwatched: &unwatched, UserID: 1}}, Offset: 0, Limit: 3}, []core.MediaType{core.TypeMovie})
	if err != nil || recent.Total != 20 || len(recent.Items) != 3 || recent.Items[0].Key != core.MovieKey(1230) {
		t.Fatalf("filter before page %+v %v", recent, err)
	}
	deck, err = c.PlexOnDeckPage(ctx, 2, 0, 7, core.Filter{}, false)
	if err != nil || deck.Total != 0 || len(deck.Items) != 0 {
		t.Fatal("viewer leak", deck, err)
	}
}

func TestPlexOnDeckCurrentShowAnchorAndHide(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Now().UTC().Truncate(time.Second)
	c.SetNow(func() time.Time { return now })
	show := core.ShowKey(700)
	title := core.Title{Key: show, Type: core.TypeShow, Title: "Show", Released: now.Add(-time.Hour)}
	if err := c.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		ep := core.Title{Key: core.EpisodeKey(700, 1, i), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: i, Title: "Episode", Released: now.Add(-time.Hour)}
		if err := c.UpsertTitle(ctx, ep, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []int{4, 1} {
		at := now
		if i == 4 {
			at = now.Add(-time.Minute)
		}
		if _, err := c.db.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,view_count,last_viewed_at) VALUES(?,?,1,?)`, 1, int64(core.EpisodeKey(700, 1, i)), at.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.PlexOnDeckPage(ctx, 1, 0, 10, core.Filter{}, false)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != core.EpisodeKey(700, 1, 2) {
		t.Fatalf("show anchor %+v %v", page, err)
	}
	visible := core.Filter{ExcludeKeys: []core.RatingKey{core.EpisodeKey(700, 1, 2)}}
	page, err = c.PlexOnDeckPage(ctx, 1, 0, 10, visible, false)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != core.EpisodeKey(700, 1, 3) {
		t.Fatal("blocked next episode was not skipped", page, err)
	}
	visible.ExcludeKeys = []core.RatingKey{core.EpisodeKey(700, 1, 1), core.EpisodeKey(700, 1, 4)}
	page, err = c.PlexOnDeckPage(ctx, 1, 0, 10, visible, false)
	if err != nil || page.Total != 0 {
		t.Fatal("blocked episode established watch anchor", page, err)
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO state_continue_hidden(user_id,item_key) VALUES(1,?)`, int64(show)); err != nil {
		t.Fatal(err)
	}
	page, err = c.PlexOnDeckPage(ctx, 1, 0, 10, core.Filter{}, false)
	if err != nil || page.Total != 0 {
		t.Fatal("hidden show", page, err)
	}
}

// The On Deck window dates a show by its latest view of any episode, keeps
// a later season's premiere that aired inside the window, and drops the
// rest of what was last watched before it.
func TestPlexOnDeckWindow(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Now().UTC().Truncate(time.Second)
	c.SetNow(func() time.Time { return now })
	since := now.Add(-16 * 7 * 24 * time.Hour)
	old := since.Add(-24 * time.Hour)
	upsert := func(title core.Title) {
		t.Helper()
		if err := c.UpsertTitle(ctx, title, title.Type != core.TypeEpisode); err != nil {
			t.Fatal(err)
		}
	}
	watch := func(key core.RatingKey, offset time.Duration, count int, at time.Time) {
		t.Helper()
		if _, err := c.db.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,view_offset_ns,view_count,last_viewed_at) VALUES(1,?,?,?,?)`, int64(key), offset.Nanoseconds(), count, at.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	// 801: an old in-progress episode, but another episode watched today.
	// 802: last watched before the window, next up is S1E3 (out).
	// 803: last watched before the window, next up is a fresh S2E1 (in).
	// 804: as 803, but the S2 premiere aired before the window (out).
	for id := int64(801); id <= 804; id++ {
		upsert(core.Title{Key: core.ShowKey(id), Type: core.TypeShow, Title: "Show", Released: old.Add(-time.Hour * 24 * 400)})
	}
	ep := func(show int64, season, episode int, aired time.Time) core.RatingKey {
		key := core.EpisodeKey(show, season, episode)
		upsert(core.Title{Key: key, Type: core.TypeEpisode, ShowKey: core.ShowKey(show), Season: season, Episode: episode, Title: "Episode", Released: aired})
		return key
	}
	aired := old.Add(-time.Hour * 24 * 300)
	watch(ep(801, 1, 1, aired), time.Minute, 0, old)
	watch(ep(801, 1, 2, aired), 0, 1, now)
	ep(801, 1, 3, aired)
	watch(ep(802, 1, 2, aired), 0, 1, old)
	ep(802, 1, 3, aired)
	watch(ep(803, 1, 2, aired), 0, 1, old)
	ep(803, 2, 1, now.Add(-time.Hour))
	watch(ep(804, 1, 2, aired), 0, 1, old)
	ep(804, 2, 1, old)
	// Movies: one watched inside the window, one before it.
	upsert(core.Title{Key: core.MovieKey(901), Type: core.TypeMovie, Title: "In", Released: aired})
	upsert(core.Title{Key: core.MovieKey(902), Type: core.TypeMovie, Title: "Out", Released: aired})
	watch(core.MovieKey(901), time.Minute, 0, now)
	watch(core.MovieKey(902), time.Minute, 0, old)

	all, err := c.PlexOnDeckPage(ctx, 1, 0, 50, core.Filter{}, false)
	if err != nil || all.Total != 6 {
		t.Fatalf("unwindowed deck %+v %v", all, err)
	}
	page, err := c.OnDeckPageSince(ctx, 1, 0, 50, core.Filter{}, false, since)
	if err != nil {
		t.Fatal(err)
	}
	got := map[core.RatingKey]bool{}
	for _, item := range page.Items {
		got[item.Key] = true
	}
	want := []core.RatingKey{core.EpisodeKey(801, 1, 1), core.EpisodeKey(803, 2, 1), core.MovieKey(901)}
	if page.Total != len(want) || len(page.Items) != len(want) {
		t.Fatalf("windowed deck %v (total %d), want %v", page.Items, page.Total, want)
	}
	for _, key := range want {
		if !got[key] {
			t.Fatalf("windowed deck %v lacks %v", page.Items, key)
		}
	}
}

// The windowed deck reads only the candidates the window can admit; it must
// answer exactly the whole-history deck with the window applied to it: a
// movie or show kept when its latest view of any row is inside the window
// or undated, or when its deck row is a later season's premiere aired
// inside it.
func TestPlexOnDeckWindowMatchesWholeHistory(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Now().UTC().Truncate(time.Second)
	c.SetNow(func() time.Time { return now })
	since := now.Add(-16 * 7 * 24 * time.Hour)
	rnd := rand.New(rand.NewPCG(7, 36))
	when := func() int64 {
		switch rnd.IntN(5) {
		case 0:
			return 0 // undated (an import)
		case 1, 2:
			return since.Add(time.Duration(rnd.Int64N(int64(now.Sub(since))))).UnixNano()
		default:
			return since.Add(-time.Duration(rnd.Int64N(int64(400 * 24 * time.Hour)))).UnixNano()
		}
	}
	watch := func(key core.RatingKey) {
		t.Helper()
		offset, count := int64(0), 1
		if rnd.IntN(4) == 0 {
			offset, count = time.Minute.Nanoseconds()*int64(1+rnd.IntN(30)), 0
		}
		if _, err := c.db.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,view_offset_ns,view_count,last_viewed_at) VALUES(1,?,?,?,?)`, int64(key), offset, count, when()); err != nil {
			t.Fatal(err)
		}
	}
	aired := since.Add(-500 * 24 * time.Hour)
	for show := int64(1); show <= 60; show++ {
		if err := c.UpsertTitle(ctx, core.Title{Key: core.ShowKey(show), Type: core.TypeShow, Title: "Show", Released: aired}, true); err != nil {
			t.Fatal(err)
		}
		seasons := 1 + rnd.IntN(3)
		for season := 1; season <= seasons; season++ {
			released := aired
			if season > 1 && rnd.IntN(2) == 0 {
				released = now.Add(-time.Duration(rnd.Int64N(int64(30 * 24 * time.Hour)))) // a fresh season
			}
			for ep := 1; ep <= 4; ep++ {
				key := core.EpisodeKey(show, season, ep)
				if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeEpisode, ShowKey: core.ShowKey(show), Season: season, Episode: ep, Title: "Episode", Released: released}, false); err != nil {
					t.Fatal(err)
				}
				if season == 1 && rnd.IntN(2) == 0 {
					watch(key)
				}
			}
		}
	}
	for movie := int64(1); movie <= 80; movie++ {
		key := core.MovieKey(1000 + movie)
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, Title: "Movie", Released: aired}, true); err != nil {
			t.Fatal(err)
		}
		if rnd.IntN(2) == 0 {
			watch(key)
		}
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO state_continue_hidden(user_id,item_key) VALUES(1,?)`, int64(core.ShowKey(3))); err != nil {
		t.Fatal(err)
	}
	latest := map[core.RatingKey]int64{}
	rows, err := c.db.QueryContext(ctx, `SELECT rating_key,last_viewed_at FROM state_watch WHERE user_id=1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k core.RatingKey
		var at int64
		if err := rows.Scan(&k, &at); err != nil {
			t.Fatal(err)
		}
		entity := k
		if k.Kind() == core.KindEpisode {
			entity = core.ShowKey(k.TMDB())
		}
		latest[entity] = max(latest[entity], at)
	}
	rows.Close()
	whole, err := c.PlexOnDeckPage(ctx, 1, 0, 1000, core.Filter{}, false)
	if err != nil {
		t.Fatal(err)
	}
	var want []core.RatingKey
	for _, item := range whole.Items {
		entity := item.Key
		if item.Type == core.TypeEpisode {
			entity = core.ShowKey(item.Key.TMDB())
		}
		premiere := item.Type == core.TypeEpisode && item.Episode == 1 && item.Season > 1 && !item.Released.Before(since)
		if at := latest[entity]; at >= since.UnixNano() || at <= 0 || premiere {
			want = append(want, item.Key)
		}
	}
	got, err := c.OnDeckPageSince(ctx, 1, 0, 1000, core.Filter{}, false, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) < 10 || len(want) == len(whole.Items) {
		t.Fatalf("fixture too weak: %d of %d deck rows in the window", len(want), len(whole.Items))
	}
	if got.Total != len(want) || len(got.Items) != len(want) {
		t.Fatalf("windowed deck has %d rows (total %d), want %d", len(got.Items), got.Total, len(want))
	}
	for i := range want {
		if got.Items[i].Key != want[i] {
			t.Fatalf("row %d: %v, want %v", i, got.Items[i].Key, want[i])
		}
	}
}
