package mediabrowser

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type activityFake struct {
	frontendtest.Experience
	viewer      *frontend.Viewer
	latestViews []string
	played      []bool
	unfiltered  int
	resumeCalls int
	resumeItems []frontend.Title
	lowerBound  bool
	lastExact   bool
}

func (f *activityFake) LatestAdded(ctx context.Context, v *frontend.Viewer, d frontend.Device, view string, limit int) ([]frontend.LatestItem, error) {
	if played, ok := frontend.LatestPlayedFilter(ctx); ok {
		f.played = append(f.played, played)
	} else {
		f.unfiltered++
	}
	if v != f.viewer || d.ID != "tv" || limit != 20 {
		return nil, frontend.ErrNotFound
	}
	if view != "1" && view != "2" {
		return nil, frontend.ErrNotFound
	}
	f.latestViews = append(f.latestViews, view)
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	title := frontend.Title{Key: 5, Type: "movie", Title: "Latest movie"}
	if view == "2" {
		title = frontend.Title{Key: 6, Type: "show", Title: "Latest series"}
	}
	return []frontend.LatestItem{{Title: title, AddedAt: &at}}, nil
}
func (f *activityFake) Resume(_ context.Context, v *frontend.Viewer, d frontend.Device, view string, types []frontend.MediaType, offset, limit int, requireExact bool) (frontend.ActivityPage, error) {
	if v != f.viewer || d.ID != "tv" || view != "" || len(types) != 2 || offset != 0 || limit != 20 {
		return frontend.ActivityPage{}, frontend.ErrNotFound
	}
	f.resumeCalls++
	f.lastExact = requireExact
	items := f.resumeItems
	if items == nil {
		items = []frontend.Title{{Key: 7, Type: "episode", Title: "Resume"}}
	}
	return frontend.ActivityPage{Items: items, Total: len(items), TotalExact: !f.lowerBound, More: f.lowerBound}, nil
}

func TestSwiftfinLatestAndResumeIgnoreHomeRows(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		exp := &activityFake{viewer: viewer, Experience: frontendtest.Experience{RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			t.Fatal("Home rows consulted")
			return nil, nil
		}}}
		identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp})
		if err != nil {
			t.Fatal(err)
		}
		for _, lib := range []struct {
			id   int64
			name string
		}{{1, "Latest movie"}, {2, "Latest series"}} {
			view, _ := ids.GUID(ids.View, lib.id)
			path := "/Items/Latest?enableUserData=true&fields=MediaStreams&fields=Genres&fields=Studios&limit=20&parentId=" + view
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), lib.name) || !strings.Contains(w.Body.String(), `"DateCreated":"2024-01-02T03:04:05.0000000Z"`) {
				t.Fatal(tc.name, w.Code, w.Body.String())
			}
		}
		r := httptest.NewRequest("GET", "/UserItems/Resume?limit=20&mediaTypes=Video&startIndex=0", nil)
		r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Resume") || exp.resumeCalls != 1 || !exp.lastExact || len(exp.latestViews) != 2 {
			t.Fatal(tc.name, w.Code, w.Body.String(), exp)
		}
		if tc.name == "jellyfin" && (strings.Contains(w.Body.String(), `"CanDelete"`) || strings.Contains(w.Body.String(), `"Overview"`) || strings.Contains(w.Body.String(), `"ProviderIds"`) || strings.Contains(w.Body.String(), `"OriginalTitle"`)) {
			t.Fatal(w.Body.String())
		}
		exp.lowerBound = true
		r = httptest.NewRequest("GET", "/UserItems/Resume?limit=20&mediaTypes=Video&startIndex=0&enableTotalRecordCount=false", nil)
		r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
		w = httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || exp.lastExact || strings.Contains(w.Body.String(), "TotalRecordCount") {
			t.Fatal(tc.name, w.Code, w.Body.String())
		}
	}
}

type groupedActivityFake struct {
	*activityFake
	groupCalls []bool
}

func (f *groupedActivityFake) LatestAddedGrouped(ctx context.Context, v *frontend.Viewer, d frontend.Device, view string, limit int, group bool) ([]frontend.LatestItem, error) {
	f.groupCalls = append(f.groupCalls, group)
	rows, err := f.LatestAdded(ctx, v, d, view, limit)
	if err == nil && !group && view == "2" {
		rows[0].Title.Type = "episode"
		rows[0].Title.Title = "Latest episode"
	}
	return rows, err
}

type listedEdition struct{ staticEditions }

func (listedEdition) Editions(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) ([]frontend.Version, error) {
	return []frontend.Version{{
		ID: "file",
		Media: frontend.MediaInfo{
			Container: "mkv", Size: 100, BitrateKbps: 1000, Duration: 2 * time.Second,
			Chapters: []core.Chapter{{Title: "Start", Start: time.Second}},
			Streams: []frontend.StreamInfo{
				{Type: "video", Index: 0, Codec: "h264"},
				{Type: "audio", Index: 1, Codec: "aac"},
				{Type: "subtitle", Index: 2, Codec: "subrip", Language: "eng"},
			},
		},
	}}, nil
}

func TestLatestEditionFactsAndPath(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	exp := &activityFake{viewer: viewer}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	paths := frontendtest.VirtualPaths{PathsFunc: func(context.Context, *frontend.Viewer, frontend.Device, []frontend.RatingKey) (map[frontend.RatingKey]string, error) {
		return map[frontend.RatingKey]string{5: "Movies/Film.mkv"}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp, Editions: listedEdition{}, VirtualPaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	view, _ := ids.GUID(ids.View, 1)
	r := httptest.NewRequest("GET", "/Items/Latest?limit=20&ParentId="+view+"&Fields=MediaSources", nil)
	r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	var items []map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &items) != nil || len(items) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if items[0]["Path"] != "/Movies/Film.mkv" || items[0]["HasSubtitles"] != true || items[0]["Container"] != "mkv" {
		t.Fatal(items[0]["Path"], items[0]["HasSubtitles"], items[0]["Container"])
	}
	sources, _ := items[0]["MediaSources"].([]any)
	if len(sources) != 1 {
		t.Fatal(items[0]["MediaSources"])
	}
	source := sources[0].(map[string]any)
	streams, _ := source["MediaStreams"].([]any)
	if source["Container"] != "mkv" || source["Path"] != "/Movies/Film.mkv" || source["Bitrate"] != float64(1000000) || len(streams) != 3 {
		t.Fatal(source["Container"], source["Path"], source["Bitrate"], len(streams))
	}
	if _, ok := source["Chapters"]; ok {
		t.Fatal(source["Chapters"])
	}
	subtitle := streams[2].(map[string]any)
	if subtitle["LocalizedForced"] != "Forced" || subtitle["LocalizedLanguage"] != "English" {
		t.Fatal(subtitle["LocalizedForced"], subtitle["LocalizedLanguage"])
	}
}

func TestLatestGroupingAndItemAddedDate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		viewer := &frontend.Viewer{ID: 1}
		exp := &groupedActivityFake{activityFake: &activityFake{viewer: viewer}}
		identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp})
		if err != nil {
			t.Fatal(err)
		}
		view, _ := ids.GUID(ids.View, 2)
		for _, v := range []struct{ query, name string }{{"", "Latest series"}, {"&GroupItems=false", "Latest episode"}} {
			r := httptest.NewRequest("GET", "/Items/Latest?limit=20&ParentId="+view+v.query, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), v.name) {
				t.Fatal(tc.name, w.Code, w.Body.String())
			}
		}
		if len(exp.groupCalls) != 2 || !exp.groupCalls[0] || exp.groupCalls[1] {
			t.Fatal(tc.name, exp.groupCalls)
		}
		i := inst.(*instance)
		at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
		if dto := i.itemDTO(frontend.Title{Key: 5, Type: "movie", Released: at, AddedAt: at}, frontend.WatchStatus{}); dto["DateCreated"] != wireTime(at) {
			t.Fatal(tc.name, dto)
		}
		if dto := i.itemDTO(frontend.Title{Key: 5, Type: "movie"}, frontend.WatchStatus{}); dto["DateCreated"] != nil {
			t.Fatal(tc.name, dto)
		}
	}
}

func TestLatestDefaultsHidePlayedOnBothFrontends(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		exp := &activityFake{viewer: viewer}
		identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp})
		if err != nil {
			t.Fatal(err)
		}
		view, _ := ids.GUID(ids.View, 1)
		for _, query := range []struct {
			raw    string
			played bool
		}{{"", false}, {"&IsPlayed=false", false}, {"&IsPlayed=true", true}} {
			r := httptest.NewRequest("GET", "/Items/Latest?limit=20&ParentId="+view+query.raw, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatal(tc.name, query.raw, w.Code, w.Body.String())
			}
		}
		if exp.unfiltered != 0 || len(exp.played) != 3 || exp.played[0] || exp.played[1] || !exp.played[2] {
			t.Fatalf("%s played=%v unfiltered=%d", tc.name, exp.played, exp.unfiltered)
		}
		r := httptest.NewRequest("GET", "/Items/Latest?limit=20&ParentId="+view+"&IsPlayed=maybe", nil)
		r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(tc.name, w.Code)
		}
	}
}

func TestJellyfinResumeStaysOnPlayableItems(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	show := core.ShowKey(91461)
	episode := core.EpisodeKey(91461, 1, 2)
	exp := &activityFake{
		viewer: viewer,
		resumeItems: []frontend.Title{{
			Key: episode, Type: "episode", Title: "Two", ShowKey: show, Season: 1, Episode: 2,
		}},
		Experience: frontendtest.Experience{
			ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
				if req.Key != show {
					t.Fatal(req.Key)
				}
				return frontend.ItemDetail{Title: frontend.Title{Key: show, Type: "show", Title: "Show"}}, nil
			},
		},
	}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	for _, front := range []frontend.Frontend{Jellyfin{MachineID: "test"}, Emby{MachineID: "test"}} {
		inst, err := front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp})
		if err != nil {
			t.Fatal(err)
		}
		userID, err := ids.GUID(ids.User, viewer.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/UserItems/Resume?limit=20&mediaTypes=Video&startIndex=0", "/Users/" + userID + "/Items/Resume?Limit=20"} {
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatal(front.Describe().ID, path, w.Code, w.Body.String())
			}
			var page struct {
				Items            []map[string]any
				TotalRecordCount float64
			}
			if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.TotalRecordCount != 1 {
				t.Fatal(front.Describe().ID, path, w.Body.String())
			}
			if page.Items[0]["Type"] != "Episode" || page.Items[0]["Name"] != "Two" {
				t.Fatal(page.Items[0]["Type"], page.Items[0]["Name"])
			}
		}
	}
}
