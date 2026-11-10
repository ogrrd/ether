package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/openapi/spectest"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestJellyfinUserDataKeyIsDashed(t *testing.T) {
	id := ids.RatingKeyGUID(5)
	data := (&instance{protocol: "jellyfin"}).userDataDTO(id, frontend.WatchStatus{})
	if data["ItemId"] != id || data["Key"] != dashedGUID(id) || !strings.Contains(data["Key"].(string), "-") {
		t.Fatal(data)
	}
	if _, ok := (&instance{protocol: "emby"}).userDataDTO(id, frontend.WatchStatus{})["Key"]; ok {
		t.Fatal("emby user data gained a jellyfin key")
	}
}

func TestPre1970PremiereAndDateAdded(t *testing.T) {
	i := &instance{protocol: "jellyfin"}
	released := time.Date(1942, 11, 26, 0, 0, 0, 0, time.UTC)
	item := i.itemDTO(frontend.Title{Key: core.MovieKey(289), Type: "movie", Title: "Casablanca", Released: released}, frontend.WatchStatus{})
	if item["PremiereDate"] != wireTime(released) || item["DateCreated"] != wireTime(released) {
		t.Fatalf("movie premiere %v created %v", item["PremiereDate"], item["DateCreated"])
	}
	aired := time.Date(1960, 12, 9, 0, 0, 0, 0, time.UTC)
	episode := i.itemDTO(frontend.Title{Key: core.EpisodeKey(91759, 1, 1), Type: "episode", Season: 1, Episode: 1, Released: aired}, frontend.WatchStatus{})
	if episode["PremiereDate"] != wireTime(aired) || episode["DateCreated"] != wireTime(aired) {
		t.Fatalf("episode premiere %v created %v", episode["PremiereDate"], episode["DateCreated"])
	}
	unknown := i.itemDTO(frontend.Title{Key: core.MovieKey(1), Type: "movie", Title: "Undated"}, frontend.WatchStatus{})
	if _, ok := unknown["PremiereDate"]; ok || unknown["DateCreated"] != nil {
		t.Fatalf("unknown dates %#v", unknown)
	}
	theatrical := time.Date(1943, 1, 15, 0, 0, 0, 0, time.UTC)
	split := i.itemDTO(frontend.Title{Key: core.MovieKey(289), Type: "movie", Title: "Casablanca", Year: 1942, Released: theatrical}, frontend.WatchStatus{})
	if split["ProductionYear"] != 1942 || split["PremiereDate"] != wireTime(theatrical) {
		t.Fatalf("split year %v premiere %v", split["ProductionYear"], split["PremiereDate"])
	}
	future := time.Now().Add(48 * time.Hour).UTC()
	later := i.itemDTO(frontend.Title{Key: core.MovieKey(2), Type: "movie", Title: "Later", Released: future}, frontend.WatchStatus{})
	if later["PremiereDate"] != wireTime(future) || later["DateCreated"] != nil {
		t.Fatalf("future premiere %v created %v", later["PremiereDate"], later["DateCreated"])
	}
}

func TestJellyfinListShellMatchesRecordedMovieShape(t *testing.T) {
	released := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	title := frontend.Title{Key: 5, Type: "movie", Title: "Arrival", Year: 2016, Released: released, Summary: "A linguist.", Runtime: time.Hour, Genres: []string{"Drama"}, Rating: 8, Language: "en", ContentRating: "PG-13", Status: "Released"}
	exp := frontendtest.Experience{
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{Items: []frontend.Title{title}, Total: 1}, nil
		},
		UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			return map[frontend.RatingKey]frontend.WatchStatus{5: {IsFavorite: true, ViewCount: 2, ViewOffset: time.Minute, Duration: time.Hour, LastViewedAt: released}}, nil
		},
		NamedFacetIDsFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, string, []string) (map[string]string, error) {
			return map[string]string{"Drama": "genre-drama"}, nil
		},
	}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=Movie&Fields=DateCreated,Etag,Genres,Overview,MediaSources&Recursive=true", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var body struct {
		Items            []map[string]any
		StartIndex       int
		TotalRecordCount int
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 1 || body.TotalRecordCount != 1 {
		t.Fatal(w.Body.String())
	}
	item := body.Items[0]
	for _, key := range []string{"ChannelId", "BackdropImageTags", "ImageTags", "ImageBlurHashes", "VideoType", "Container", "Etag", "Overview", "Genres", "GenreItems", "DateCreated", "MediaSources", "UserData", "PremiereDate", "CommunityRating", "OriginalLanguage", "OfficialRating"} {
		if _, ok := item[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if item["ChannelId"] != nil || item["VideoType"] != "VideoFile" || item["Overview"] != "A linguist." {
		t.Fatal(item["ChannelId"], item["VideoType"], item["Overview"])
	}
	genres := item["GenreItems"].([]any)
	if len(genres) != 1 || genres[0].(map[string]any)["Name"] != "Drama" {
		t.Fatal(item["GenreItems"])
	}
	userData := item["UserData"].(map[string]any)
	if userData["IsFavorite"] != true || userData["ItemId"] == userData["Key"] {
		t.Fatal(userData)
	}
	sources := item["MediaSources"].([]any)
	source := sources[0].(map[string]any)
	if source["VideoType"] != "VideoFile" || source["UseMostCompatibleTranscodingProfile"] != false || source["ETag"] == "" {
		t.Fatal(source)
	}
	requireJellyfinSpec(t, http.MethodGet, "/Items", w)
}

func TestJellyfinSortNameListOmitsUnrequestedFacts(t *testing.T) {
	title := frontend.Title{Key: 5, Type: "movie", Title: "Arrival", SortTitle: "arrival", Year: 2016, Summary: strings.Repeat("overview ", 80), OriginalTitle: "Arrival Original", Tagline: "A tagline", Keywords: []string{"linguist", "alien"}, Countries: []string{"United States"}, Genres: []string{"Drama"}, Runtime: time.Hour}
	exp := frontendtest.Experience{LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
		return frontend.Page{Items: []frontend.Title{title}, Total: 1}, nil
	}}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=Movie&Fields=SortName&Recursive=true&SortBy=SortName&Limit=50", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var body struct{ Items []map[string]any }
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 1 {
		t.Fatal(w.Body.String())
	}
	item := body.Items[0]
	if item["SortName"] != "arrival" || item["Name"] != "Arrival" {
		t.Fatal(item["SortName"], item["Name"])
	}
	for _, key := range []string{"Overview", "Genres", "GenreItems", "Taglines", "Tags", "ProductionLocations", "OriginalTitle", "ProviderIds", "MediaSources"} {
		if _, ok := item[key]; ok {
			t.Errorf("list kept %s", key)
		}
	}
	if len(w.Body.Bytes()) > 2500 {
		t.Fatalf("sortname row is %d bytes", len(w.Body.Bytes()))
	}
}

func TestJellyfinMovieAndEpisodeSourcesLookLikeFiles(t *testing.T) {
	movie := frontend.Title{Key: 5, Type: "movie", Title: "Arrival", Year: 2016}
	show := core.ShowKey(9)
	episode := frontend.Title{Key: core.EpisodeKey(9, 1, 1), Type: "episode", Title: "Episode 1", ShowTitle: "Coronation Street", ShowKey: show, Season: 1, Episode: 1}
	exp := frontendtest.Experience{
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{Items: []frontend.Title{movie}, Total: 1}, nil
		},
		ChildrenFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{Items: []frontend.Title{episode}, Total: 1}, nil
		},
	}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=Movie&Fields=MediaSources,Path&Recursive=true&Limit=50", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var listed struct{ Items []map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Items) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertJellyfinFile(t, listed.Items[0], "/Movies/Arrival (2016)/Arrival (2016).mkv")

	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/Shows/"+ids.RatingKeyGUID(show)+"/Episodes?Fields=MediaSources", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var episodes struct{ Items []map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &episodes) != nil || len(episodes.Items) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertJellyfinFile(t, episodes.Items[0], "/TV/Coronation Street/Season 01/Coronation Street - S01E01.mkv")
}

func assertJellyfinFile(t *testing.T, item map[string]any, path string) {
	t.Helper()
	sources, _ := item["MediaSources"].([]any)
	if item["Path"] != path || item["Container"] != "mkv" || len(sources) != 1 {
		t.Fatal(item["Path"], item["Container"], item["MediaSources"])
	}
	source := sources[0].(map[string]any)
	streams, _ := source["MediaStreams"].([]any)
	if source["Protocol"] != "File" || source["IsRemote"] != false || source["Path"] != path || source["Container"] != "mkv" || len(streams) < 1 {
		t.Fatal(source["Protocol"], source["IsRemote"], source["Path"], source["Container"], len(streams))
	}
}

func TestJellyfinViewsUseCollectionFolderShell(t *testing.T) {
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			t.Fatal("UserViews browsed the library")
			return frontend.Page{}, nil
		},
		LibraryCountFunc: func(_ context.Context, v *frontend.Viewer, id string) (int, error) {
			if v == nil || v.ID != 1 || id != "1" {
				t.Fatalf("library count viewer=%v id=%s", v, id)
			}
			return 6, nil
		},
	}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/UserViews", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var body struct{ Items []map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	view := body.Items[0]
	for _, key := range []string{"ChannelId", "ChildCount", "DateCreated", "DateLastMediaAdded", "Etag", "ImageTags", "LocationType", "LockData", "MediaType", "PlayAccess", "UserData"} {
		if key == "DateCreated" || key == "DateLastMediaAdded" {
			if _, ok := view[key]; ok {
				t.Fatalf("%s was invented: %v", key, view[key])
			}
			continue
		}
		if _, ok := view[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if view["MediaType"] != "Unknown" || view["PlayAccess"] != "Full" || view["LockData"] != false || view["ChannelId"] != nil || view["ChildCount"] != float64(6) {
		t.Fatal(view["MediaType"], view["PlayAccess"], view["LockData"], view["ChannelId"], view["ChildCount"])
	}
	userData := view["UserData"].(map[string]any)
	if _, ok := userData["PlayedPercentage"]; ok || userData["Played"] != false {
		t.Fatal(userData)
	}
	reference, err := conform.LoadFixture("../../../testdata/conform/jellyfin/findroid/findroid_jsonhome_user views_GET__Users_${user_id}_Views.json")
	if err != nil {
		t.Fatal(err)
	}
	report := conform.Compare(reference, conform.Response{Status: w.Code, Headers: w.Header(), Body: w.Body.Bytes()})
	for _, diff := range report.Differences {
		if diff.Blocking && diff.Kind == conform.KindMissing {
			switch diff.Path {
			case "$/Items[]/DateCreated", "$/Items[]/DateLastMediaAdded", "$/Items[]/ParentId", "$/Items[]/Path", "$/Items[]/PrimaryImageAspectRatio", "$/Items[]/ImageTags/Primary", "$/Items[]/ImageBlurHashes/Primary":
			default:
				t.Errorf("view shell missing %s", diff.Path)
			}
		}
	}
	requireJellyfinSpec(t, http.MethodGet, "/UserViews", w)
}

func TestJellyfinItemDetailServesCollectionFolder(t *testing.T) {
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
		},
		LibraryCountFunc: func(context.Context, *frontend.Viewer, string) (int, error) {
			return 6, nil
		},
	}
	inst := mountJellyfin(t, exp)
	viewID, err := ids.GUID(ids.View, 1)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items/"+viewID, nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var item map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if item["Id"] != viewID || item["Type"] != "CollectionFolder" || item["CollectionType"] != "movies" || item["Name"] != "Movies" || item["ChildCount"] != float64(6) {
		t.Fatal(item["Id"], item["Type"], item["CollectionType"], item["Name"], item["ChildCount"])
	}
	missing := httptest.NewRecorder()
	other, err := ids.GUID(ids.View, 9)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/Items/"+other, nil)
	req.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(missing, req)
	if missing.Code != http.StatusNotFound {
		t.Fatal(missing.Code, missing.Body.String())
	}
}

func TestJellyfinBoxSetsAndPlaylists(t *testing.T) {
	var got frontend.CollectionQuery
	exp := frontendtest.Experience{CollectionsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, q frontend.CollectionQuery) (frontend.CollectionPage, error) {
		if v == nil || v.ID != 1 {
			t.Fatal(v)
		}
		got = q
		return frontend.CollectionPage{Items: []frontend.Collection{{Key: 9, Title: "Before", Summary: "A trilogy", ChildCount: 3}}, Total: 1}, nil
	}}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=BoxSet&StartIndex=2&Limit=5", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || got.Offset != 2 || got.Limit != 5 || !strings.Contains(w.Body.String(), `"Type":"BoxSet"`) || !strings.Contains(w.Body.String(), `"Name":"Before"`) || !strings.Contains(w.Body.String(), `"ChildCount":3`) {
		t.Fatal(w.Code, got, w.Body.String())
	}
	requireJellyfinSpec(t, http.MethodGet, "/Items", w)
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=Playlist&Limit=10", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Items":[]`) || !strings.Contains(w.Body.String(), `"TotalRecordCount":0`) {
		t.Fatal(w.Code, w.Body.String())
	}
	requireJellyfinSpec(t, http.MethodGet, "/Items", w)
}

func TestJellyfinBrowseFiltersReachTheLibrary(t *testing.T) {
	var got frontend.ItemQuery
	exp := frontendtest.Experience{LibraryItemsFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, _ string, q frontend.ItemQuery) (frontend.Page, error) {
		got = q
		return frontend.Page{}, nil
	}}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items?Recursive=true&MinCommunityRating=7.5&OfficialRatings=PG-13,DE-16&Studios=A24&MinRuntimeTicks=600000000&SortBy=PremiereDate&SortOrder=Descending", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || got.Filter.MinRating != 7.5 || len(got.Filter.ContentRatings) != 2 || got.Filter.ContentRatings[0] != "PG-13" || got.Filter.ContentRatings[1] != "de/16" || got.Filter.Studio != "A24" || got.Filter.MinRuntime != 60*time.Second || got.Sort != core.SortReleased || !got.Desc {
		t.Fatal(w.Code, got)
	}
}

// /Items?searchTerm= is a search: it goes to the title index with the
// request's other filters, sort and paging, never to a filtered library walk.
func TestJellyfinItemsSearchTermUsesSearch(t *testing.T) {
	var got frontend.ItemQuery
	var term string
	exp := frontendtest.Experience{
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			t.Fatal("a search walked the library")
			return frontend.Page{}, nil
		},
		SearchFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, s string, q frontend.ItemQuery) (frontend.SearchPage, error) {
			term, got = s, q
			return frontend.SearchPage{}, nil
		},
	}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items?Recursive=true&IncludeItemTypes=Movie,Series&MinCommunityRating=7.5&Studios=A24&SearchTerm=Arrival&StartIndex=24&Limit=24&SortBy=PremiereDate&SortOrder=Descending", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || term != "Arrival" || got.Filter.Query != "" || got.Filter.MinRating != 7.5 || got.Filter.Studio != "A24" || got.Offset != 24 || got.Limit != 24 || len(got.Types) != 2 || got.Sort != core.SortReleased || !got.Desc {
		t.Fatal(w.Code, term, got)
	}
}

func TestJellyfinNextUpUsesContinueEpisodes(t *testing.T) {
	episode := frontend.Title{Key: core.EpisodeKey(5, 1, 2), Type: "episode", Title: "Two", Season: 1, Episode: 2}
	movie := frontend.Title{Key: 3, Type: "movie", Title: "Film"}
	exp := frontendtest.Experience{RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
		return []frontend.Row{{Purpose: frontend.RowContinue, Items: []frontend.Title{movie, episode}}}, nil
	}, ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Key == core.ShowKey(5) {
			return frontend.ItemDetail{Title: frontend.Title{Key: req.Key, Type: "show", Title: "The Scene"}}, nil
		}
		if req.Key != episode.Key {
			t.Fatalf("next up item %v", req.Key)
		}
		full := episode
		full.ShowTitle = "The Scene"
		return frontend.ItemDetail{Title: full}, nil
	}, UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return map[frontend.RatingKey]frontend.WatchStatus{}, nil
	}}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Shows/NextUp", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var body struct{ Items []map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 1 || body.Items[0]["Name"] != "Two" || body.Items[0]["SeriesName"] != "The Scene" {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestJellyfinEpisodeShellKeepsParentImageSlots(t *testing.T) {
	title := frontend.Title{Key: 8, Type: "episode", Title: "Pilot", ShowTitle: "Show", Season: 1, Episode: 1, Year: 2016, ShowKey: 4}
	exp := frontendtest.Experience{RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
		return []frontend.Row{{Purpose: frontend.RowNextUp, Items: []frontend.Title{title}}}, nil
	}, UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return map[frontend.RatingKey]frontend.WatchStatus{}, nil
	}}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Shows/NextUp", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var body struct{ Items []map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	item := body.Items[0]
	for _, key := range []string{"ParentPrimaryImageTag", "ParentLogoImageTag", "SeriesPrimaryImageTag", "ParentBackdropImageTags", "VideoType", "Container", "ChannelId"} {
		if _, ok := item[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if _, ok := item["Etag"]; ok || item["VideoType"] != "VideoFile" {
		t.Fatal("etag without a fields request, or video type", item["Etag"], item["VideoType"])
	}
}

func TestJellyfinUnplayedCountUsesEpisodeWatchState(t *testing.T) {
	show := core.ShowKey(42)
	season := core.SeasonKey(42, 1)
	watched := core.EpisodeKey(42, 1, 1)
	fresh := core.EpisodeKey(42, 1, 2)
	missing := core.EpisodeKey(42, 1, 3)
	episodes := []frontend.Title{
		{Key: watched, Type: "episode"},
		{Key: fresh, Type: "episode"},
		{Key: missing, Type: "episode"},
	}
	children := func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, key frontend.RatingKey, q frontend.ItemQuery) (core.Page, error) {
		if key == show && q.Recursive {
			return core.Page{Items: episodes, Total: len(episodes)}, nil
		}
		if key == show {
			return core.Page{Items: []frontend.Title{{Key: season, Type: "season", ShowKey: show, Season: 1, Title: "Season 1"}}, Total: 1}, nil
		}
		if key == season {
			return core.Page{Items: episodes, Total: len(episodes)}, nil
		}
		return core.Page{}, errors.New("unavailable")
	}
	userData := func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return map[frontend.RatingKey]frontend.WatchStatus{watched: {ViewCount: 1}, fresh: {ViewCount: 0}}, nil
	}
	inst := mountJellyfin(t, frontendtest.Experience{
		ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{Key: show, Type: "show", Title: "Show"}}, nil
		},
		ChildrenFunc: children,
		UserDataFunc: userData,
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items/"+ids.RatingKeyGUID(show), nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var item map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	data, _ := item["UserData"].(map[string]any)
	if data["UnplayedItemCount"] != float64(2) {
		t.Fatal(data)
	}
	requireJellyfinSpec(t, "GET", r.URL.Path, w)

	seasonList := mountJellyfin(t, frontendtest.Experience{
		ChildrenFunc: children,
		UserDataFunc: userData,
	})
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/Shows/"+ids.RatingKeyGUID(show)+"/Seasons", nil)
	r.Header.Set("X-Emby-Token", "grant")
	seasonList.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var page struct {
		Items []map[string]any
	}
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
		t.Fatal(w.Body.String())
	}
	seasonData, _ := page.Items[0]["UserData"].(map[string]any)
	if seasonData["UnplayedItemCount"] != float64(2) {
		t.Fatal(seasonData)
	}

	failed := mountJellyfin(t, frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: show, Type: "show", Title: "Show"}}, nil
	}, ChildrenFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (core.Page, error) {
		return core.Page{}, errors.New("unavailable")
	}})
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/Items/"+ids.RatingKeyGUID(show), nil)
	r.Header.Set("X-Emby-Token", "grant")
	failed.Handler().ServeHTTP(w, r)
	var hidden map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &hidden) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	hiddenData, _ := hidden["UserData"].(map[string]any)
	if _, ok := hiddenData["UnplayedItemCount"]; ok {
		t.Fatal(hiddenData)
	}
}

func TestJellyfinEpisodeParentsComeFromTheTitle(t *testing.T) {
	i := &instance{protocol: "jellyfin"}
	encoded := i.itemDTO(frontend.Title{Key: core.EpisodeKey(42, 1, 3), Type: "episode", Title: "Pilot"}, frontend.WatchStatus{})
	showID := ids.RatingKeyGUID(core.ShowKey(42))
	seasonID := ids.RatingKeyGUID(core.SeasonKey(42, 1))
	if encoded["SeriesId"] != showID || encoded["SeasonId"] != seasonID || encoded["ParentId"] != seasonID || encoded["SeasonName"] != "Season 1" {
		t.Fatal(encoded["SeriesId"], encoded["SeasonId"], encoded["ParentId"], encoded["SeasonName"])
	}
	named := i.itemDTO(frontend.Title{Key: 8, Type: "episode", ShowKey: core.ShowKey(42), ShowTitle: "Show", SeasonTitle: "The Beginning", Season: 1, Episode: 3}, frontend.WatchStatus{})
	if named["SeriesName"] != "Show" || named["SeasonName"] != "The Beginning" || named["SeasonId"] != seasonID {
		t.Fatal(named["SeriesName"], named["SeasonName"], named["SeasonId"])
	}
	movie := i.itemDTO(frontend.Title{Key: core.MovieKey(3), Type: "movie", Title: "Film"}, frontend.WatchStatus{})
	if _, ok := movie["SeriesId"]; ok {
		t.Fatal(movie["SeriesId"])
	}
	if _, ok := movie["SeasonId"]; ok {
		t.Fatal(movie["SeasonId"])
	}
	show := i.itemDTO(frontend.Title{Key: core.ShowKey(42), Type: "show", Title: "Show", Studios: []string{"A24"}}, frontend.WatchStatus{})
	i.shapeJellyfinItem(nil, nil, show, frontend.Title{Type: "show", Studios: []string{"A24"}})
	data, _ := show["UserData"].(map[string]any)
	if data["PlayedPercentage"] != float64(0) {
		t.Fatal(data)
	}
}

func TestJellyfinSeasonListOmitsUnrequestedFields(t *testing.T) {
	season := frontend.Title{Key: core.SeasonKey(42, 1), Type: "season", ShowKey: core.ShowKey(42), ShowTitle: "Show", Season: 1, Title: "Season 1", Summary: "secret", Studios: []string{"A24"}}
	inst := mountJellyfin(t, frontendtest.Experience{ChildrenFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (core.Page, error) {
		return core.Page{Items: []frontend.Title{season}, Total: 1}, nil
	}})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Shows/"+ids.RatingKeyGUID(core.ShowKey(42))+"/Seasons?Fields=Genres,ParentId", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var page struct {
		Items []map[string]any
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
		t.Fatal(err, w.Body.String())
	}
	item := page.Items[0]
	if item["SeriesName"] != "Show" || item["ParentId"] != ids.RatingKeyGUID(core.ShowKey(42)) || item["Name"] != "Season 1" || item["MediaType"] != "Unknown" {
		t.Fatal(item["SeriesName"], item["ParentId"], item["Name"], item["MediaType"])
	}
	if _, ok := item["SeasonName"]; ok {
		t.Fatal(item["SeasonName"])
	}
	for _, key := range []string{"CanDelete", "CanDownload", "Overview", "SortName", "ProviderIds", "RunTimeTicks", "DateCreated"} {
		if _, ok := item[key]; ok {
			t.Fatalf("unrequested %s", key)
		}
	}
	requireJellyfinSpec(t, "GET", r.URL.Path, w)
}

func TestJellyfinSeasonListUsesParentSeriesName(t *testing.T) {
	show := core.ShowKey(42)
	season := frontend.Title{Key: core.SeasonKey(42, 1), Type: "season", Season: 1, Title: "Season 1"}
	inst := mountJellyfin(t, frontendtest.Experience{
		ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{Key: show, Type: "show", Title: "The Scene"}}, nil
		},
		ChildrenFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (core.Page, error) {
			return core.Page{Items: []frontend.Title{season}, Total: 1}, nil
		},
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Shows/"+ids.RatingKeyGUID(show)+"/Seasons?Fields=ParentId", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var page struct {
		Items []map[string]any
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0]["SeriesName"] != "The Scene" || page.Items[0]["Name"] != "Season 1" {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, ok := page.Items[0]["SeasonName"]; ok {
		t.Fatal(page.Items[0]["SeasonName"])
	}
}

func TestJellyfinDetailShellAndKnownVersionFacts(t *testing.T) {
	inst := mountJellyfin(t, frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: core.EpisodeKey(42, 1, 1), Type: "episode", Title: "Arrival", ShowKey: core.ShowKey(42), ShowTitle: "Show", Season: 1, Episode: 1, Studios: []string{"A24"}}, Versions: []frontend.Version{{
			Media: core.MediaInfo{Container: "mkv", Size: 40, BitrateKbps: 8000, Chapters: []core.Chapter{{Title: "Start", Start: time.Second}}, Streams: []core.StreamInfo{{Type: "video", Width: 1920, Height: 800}}},
		}}}, nil
	}})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items/"+ids.RatingKeyGUID(core.EpisodeKey(42, 1, 1)), nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var item map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Chapters", "ExternalUrls", "LockData", "LockedFields", "People", "RemoteTrailers", "Taglines", "Tags", "LocalTrailerCount", "SpecialFeatureCount", "Etag", "Container"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	for _, key := range []string{"Width", "Height"} {
		if _, ok := item[key]; ok {
			t.Fatalf("%s belongs to the playback slice", key)
		}
	}
	for _, key := range []string{"FileName", "Bitrate", "Size", "DateModified"} {
		if _, ok := item[key]; ok {
			t.Fatalf("%s is not on BaseItemDto", key)
		}
	}
	if item["Container"] != "mkv" || item["SeriesStudio"] != "A24" {
		t.Fatal(item["Container"], item["SeriesStudio"])
	}
	if item["DisplayPreferencesId"] != item["Id"] {
		t.Fatal(item["DisplayPreferencesId"], item["Id"])
	}
	chapters, _ := item["Chapters"].([]any)
	if len(chapters) != 1 {
		t.Fatal(item["Chapters"])
	}
	requireJellyfinSpec(t, "GET", "/Items/"+ids.RatingKeyGUID(core.EpisodeKey(42, 1, 1)), w)

	estimated := mountJellyfin(t, frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: 6, Type: "movie"}, Versions: []frontend.Version{{
			Media: core.MediaInfo{Estimated: true, Container: "mkv", Size: 9, Streams: []core.StreamInfo{{Type: "video", Width: 640, Height: 480}}},
		}}}, nil
	}})
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/Items/"+ids.RatingKeyGUID(6), nil)
	r.Header.Set("X-Emby-Token", "grant")
	estimated.Handler().ServeHTTP(w, r)
	var guess map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &guess) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, key := range []string{"Width", "Height", "Size", "Bitrate"} {
		if _, ok := guess[key]; ok {
			t.Fatalf("estimated %s exposed", key)
		}
	}
	if _, ok := guess["Chapters"]; !ok {
		t.Fatal("detail shell dropped chapters key")
	}
}

func TestJellyfinNamedPeopleProviderLinksAndRuntime(t *testing.T) {
	show := core.ShowKey(42)
	episode := frontend.Title{Key: core.EpisodeKey(42, 1, 1), Type: "episode", Runtime: 30 * time.Minute}
	inst := mountJellyfin(t, frontendtest.Experience{
		ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{
				Key: show, Type: "show", Title: "Show", IDs: core.IDs{TMDB: 42, IMDB: "tt0000001"},
				Cast: []core.Person{{Name: "Actor", Role: "Lead"}},
			}}, nil
		},
		ChildrenFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (core.Page, error) {
			return core.Page{Items: []frontend.Title{episode}, Total: 1}, nil
		},
		UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			return map[frontend.RatingKey]frontend.WatchStatus{}, nil
		},
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items/"+ids.RatingKeyGUID(show), nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var item map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	people, _ := item["People"].([]any)
	if len(people) != 1 || people[0].(map[string]any)["Name"] != "Actor" || people[0].(map[string]any)["Id"] != nil {
		t.Fatal(item["People"])
	}
	urls, _ := item["ExternalUrls"].([]any)
	if len(urls) != 2 || item["CumulativeRunTimeTicks"] != float64(30*time.Minute/100) {
		t.Fatal(item["ExternalUrls"], item["CumulativeRunTimeTicks"])
	}
	requireJellyfinSpec(t, http.MethodGet, "/Items/"+ids.RatingKeyGUID(show), w)
}

func TestJellyfinUserRootParentsEveryView(t *testing.T) {
	inst := mountJellyfin(t, frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}, {ID: "2", Name: "Shows", Type: "show"}}, nil
	}})
	call := func(path string) map[string]any {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatal(path, w.Code, w.Body.String())
		}
		return body
	}
	root := call("/Items/Root")
	if root["Type"] != "UserRootFolder" || root["Name"] != "Media Folders" || root["Id"] != jellyfinRootID() || root["ChildCount"] != float64(2) || root["ParentId"] != nil {
		t.Fatal(root["Type"], root["Name"], root["Id"], root["ChildCount"], root["ParentId"])
	}
	if _, ok := root["Path"]; ok {
		t.Fatal(root["Path"])
	}
	userID, err := ids.GUID(ids.User, 1)
	if err != nil {
		t.Fatal(err)
	}
	again := call("/Users/" + userID + "/Items/Root")
	if again["Id"] != root["Id"] {
		t.Fatal(again["Id"])
	}
	byID := call("/Items/" + jellyfinRootID())
	if byID["Id"] != root["Id"] || byID["Type"] != "UserRootFolder" {
		t.Fatal(byID["Id"], byID["Type"])
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/UserViews", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var page struct{ Items []map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.Items[0]["ParentId"] != root["Id"] || page.Items[1]["ParentId"] != root["Id"] {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestJellyfinVirtualFolders(t *testing.T) {
	views := frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}, {ID: "2", Name: "Shows", Type: "show"}}, nil
	}}
	denied := mountJellyfin(t, views)
	dw := httptest.NewRecorder()
	dr := httptest.NewRequest(http.MethodGet, "/Library/VirtualFolders", nil)
	dr.Header.Set("X-Emby-Token", "grant")
	denied.Handler().ServeHTTP(dw, dr)
	if dw.Code != http.StatusForbidden || dw.Body.Len() != 0 {
		t.Fatalf("non-admin folders: %d %s", dw.Code, dw.Body.String())
	}
	inst := mountJellyfinViewer(t, views, frontend.Viewer{ID: 1, IsOwner: true})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Library/VirtualFolders", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var folders []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &folders); err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 || folders[0]["CollectionType"] != "movies" || folders[1]["CollectionType"] != "tvshows" || folders[0]["ItemId"] == "" || folders[0]["PrimaryImageItemId"] != folders[0]["ItemId"] {
		t.Fatal(folders)
	}
	if _, ok := folders[0]["Locations"]; ok {
		t.Fatal("invented library path")
	}
	options, _ := folders[0]["LibraryOptions"].(map[string]any)
	if _, ok := options["PreferredMetadataLanguage"]; ok || options["MetadataCountryCode"] != nil || options["EnableChapterImageExtraction"] != false || options["EnableTrickplayImageExtraction"] != false || options["EnableInternetProviders"] != false || options["SaveSubtitlesWithMedia"] != false {
		t.Fatal(options)
	}
	if paths, ok := options["PathInfos"].([]any); !ok || len(paths) != 0 {
		t.Fatal(options["PathInfos"])
	}
	if folders[0]["RefreshStatus"] != "Idle" {
		t.Fatal(folders[0]["RefreshStatus"])
	}
	requireJellyfinSpec(t, "GET", "/Library/VirtualFolders", w)
}

func TestJellyfinExternalURLsUseTheLiveTMDBShape(t *testing.T) {
	movie := jellyfinExternalURLs(frontend.Title{Type: "movie", IDs: core.IDs{TMDB: 10378, IMDB: "tt1254207"}})
	episode := jellyfinExternalURLs(frontend.Title{Type: "episode", Season: 1, Episode: 2, ShowIDs: core.IDs{TMDB: 91461}})
	if len(movie) != 2 || movie[0].(map[string]string)["Url"] != "https://www.imdb.com/title/tt1254207" || movie[1].(map[string]string)["Name"] != "TMDB" || movie[1].(map[string]string)["Url"] != "https://www.themoviedb.org/movie/10378" {
		t.Fatal(movie)
	}
	if len(episode) != 1 || episode[0].(map[string]string)["Url"] != "https://www.themoviedb.org/tv/91461/season/1/episode/2" {
		t.Fatal(episode)
	}
}

func TestJellyfinVirtualLibraryHasEmptyLocations(t *testing.T) {
	inst := mountJellyfinViewer(t, frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie", Virtual: true}, {ID: "2", Name: "Shows", Type: "show"}}, nil
	}}, frontend.Viewer{ID: 1, IsOwner: true})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Library/VirtualFolders", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var folders []map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &folders) != nil || len(folders) != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	locations, ok := folders[0]["Locations"].([]any)
	if !ok || len(locations) != 0 {
		t.Fatal(folders[0]["Locations"])
	}
	if _, ok := folders[1]["Locations"]; ok {
		t.Fatal("non-virtual library gained locations")
	}
	options, _ := folders[0]["LibraryOptions"].(map[string]any)
	if paths, ok := options["PathInfos"].([]any); !ok || len(paths) != 0 || folders[0]["RefreshStatus"] != "Idle" {
		t.Fatal(folders[0]["LibraryOptions"], folders[0]["RefreshStatus"])
	}
	requireJellyfinSpec(t, "GET", "/Library/VirtualFolders", w)
}

func TestJellyfinShowParentIsTheVisibleLibrary(t *testing.T) {
	show := core.ShowKey(42)
	parent, err := ids.GUID(ids.View, 2)
	if err != nil {
		t.Fatal(err)
	}
	views := func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}, {ID: "2", Name: "Shows", Type: "show"}}, nil
	}
	inst := mountJellyfin(t, frontendtest.Experience{
		ViewsFunc: views,
		ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{Key: show, Type: "show", Title: "Show"}}, nil
		},
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items/"+ids.RatingKeyGUID(show), nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var item map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil || item["ParentId"] != parent {
		t.Fatal(w.Code, item["ParentId"])
	}
	if _, ok := item["DateCreated"]; ok {
		t.Fatal(item["DateCreated"])
	}

	ambiguous := mountJellyfin(t, frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "2", Type: "show"}, {ID: "4", Type: "show"}}, nil
		},
		ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{Key: show, Type: "show", Title: "Show"}}, nil
		},
	})
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/Items/"+ids.RatingKeyGUID(show), nil)
	r.Header.Set("X-Emby-Token", "grant")
	ambiguous.Handler().ServeHTTP(w, r)
	item = nil
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, ok := item["ParentId"]; ok {
		t.Fatal(item["ParentId"])
	}

	movies, err := ids.GUID(ids.View, 1)
	if err != nil {
		t.Fatal(err)
	}
	movie := mountJellyfin(t, frontendtest.Experience{
		ViewsFunc: views,
		ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{Key: core.MovieKey(3), Type: "movie", Title: "Film"}}, nil
		},
	})
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/Items/"+ids.RatingKeyGUID(core.MovieKey(3)), nil)
	r.Header.Set("X-Emby-Token", "grant")
	movie.Handler().ServeHTTP(w, r)
	item = nil
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil || item["ParentId"] != movies {
		t.Fatal(w.Code, item["ParentId"])
	}
}

func requireJellyfinSpec(t *testing.T, method, path string, w *httptest.ResponseRecorder) {
	t.Helper()
	doc := spectest.Document(t, "jellyfin")
	if doc == nil {
		return
	}
	if err := doc.Validate(method, path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
}

type staticEditions struct{ versions []frontend.Version }

func (s staticEditions) Editions(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) ([]frontend.Version, error) {
	return s.versions, nil
}
func (staticEditions) Variants(context.Context, frontend.ResolveRequest, int) ([]frontend.ReleaseVariant, error) {
	return nil, nil
}
func (staticEditions) RefillSearchCache(*frontend.Title) <-chan struct{} { return nil }
func (staticEditions) Available(context.Context, frontend.RatingKey) (bool, bool) {
	return false, false
}

type issuedHandles struct{ n int }

func (h *issuedHandles) Issue(context.Context, frontend.VersionScope, frontend.RatingKey, string) (string, error) {
	h.n++
	return fmt.Sprintf("edition-handle-%d", h.n), nil
}
func (h *issuedHandles) Resolve(context.Context, frontend.VersionScope, string) (frontend.VersionTarget, error) {
	return frontend.VersionTarget{}, frontend.ErrNotFound
}
func (h *issuedHandles) MatchSession(context.Context, frontend.VersionScope, string, string) error {
	return frontend.ErrNotFound
}

func TestJellyfinListSplitsProbedEditionsBySize(t *testing.T) {
	exp := frontendtest.Experience{LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
		return frontend.Page{Items: []frontend.Title{{Key: core.MovieKey(10378), Type: "movie", Title: "Big Buck Bunny", IDs: core.IDs{TMDB: 10378}}}, Total: 1}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		return &frontend.Viewer{ID: 1}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity: ident, Experience: exp, Capabilities: frontendtest.Capabilities{},
		Editions: staticEditions{versions: []frontend.Version{
			{ID: "small", Media: core.MediaInfo{Size: 10, Container: "mkv", Streams: []core.StreamInfo{{Type: "video", Index: 0, Codec: "h264"}}}},
			{ID: "large", Media: core.MediaInfo{Size: 20, Container: "mkv", Streams: []core.StreamInfo{{Type: "video", Index: 0, Codec: "hevc"}, {Type: "subtitle", Index: 1, Codec: "subrip"}}}},
		}},
		VersionHandles: &issuedHandles{},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items?Recursive=true&Fields=MediaSources,ProviderIds", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var body struct {
		Items []struct {
			Id           string
			HasSubtitles bool
			MediaSources []struct{ Size int64 }
		}
		TotalRecordCount int
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 2 || body.TotalRecordCount != 2 {
		t.Fatal(w.Body.String())
	}
	if body.Items[0].Id == body.Items[1].Id || body.Items[0].MediaSources[0].Size == body.Items[1].MediaSources[0].Size {
		t.Fatal(body.Items)
	}
	if !body.Items[1].HasSubtitles {
		t.Fatal("subtitle edition lost HasSubtitles")
	}
}

func mountJellyfin(t *testing.T, exp frontendtest.Experience) frontend.Instance {
	t.Helper()
	return mountJellyfinViewer(t, exp, frontend.Viewer{ID: 1})
}

func mountJellyfinViewer(t *testing.T, exp frontendtest.Experience, viewer frontend.Viewer) frontend.Instance {
	t.Helper()
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		v := viewer
		return &v, nil
	}}
	if exp.ViewsFunc == nil && exp.LibraryItemsFunc == nil && exp.CollectionsFunc == nil && exp.RowsFunc == nil && exp.ItemFunc == nil && exp.ChildrenFunc == nil {
		t.Fatal("empty experience")
	}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

type showArtFake struct {
	imageGrantsFake
	calls int
}

func (f *showArtFake) IssueBatch(context.Context, frontend.ImageBatchRequest) ([]frontend.ImageBatchItem, error) {
	f.calls++
	return []frontend.ImageBatchItem{{
		Item:          core.ShowKey(42),
		Authoritative: true,
		Images: []frontend.GrantedImage{
			{Image: frontend.Image{Kind: frontend.ImagePoster}, Secret: "show-primary"},
			{Image: frontend.Image{Kind: frontend.ImageLogo}, Secret: "show-logo"},
			{Image: frontend.Image{Kind: frontend.ImageBackdrop}, Secret: "show-backdrop"},
		},
	}}, nil
}

func TestJellyfinEpisodeInheritsShowArtwork(t *testing.T) {
	grants := &showArtFake{}
	i := &instance{protocol: "jellyfin", host: frontend.Host{ImageGrants: grants}}
	show := core.ShowKey(42)
	r := withSeriesStudioCache(httptest.NewRequest(http.MethodGet, "/Items", nil))
	r.Header.Set("X-Emby-Token", "grant")
	item := map[string]any{}
	i.shapeJellyfinItem(r, &frontend.Viewer{ID: 1}, item, frontend.Title{Key: core.EpisodeKey(42, 1, 3), Type: "episode", Season: 1, Episode: 3})
	seriesID := ids.RatingKeyGUID(show)
	backs, _ := item["ParentBackdropImageTags"].([]string)
	if item["ParentLogoImageTag"] != "show-logo" || item["ParentLogoItemId"] != seriesID || item["ParentPrimaryImageTag"] != "show-primary" || item["SeriesPrimaryImageTag"] != "show-primary" || len(backs) != 1 || backs[0] != "show-backdrop" || item["ParentBackdropItemId"] != seriesID {
		t.Fatal(item)
	}
	i.shapeJellyfinItem(r, &frontend.Viewer{ID: 1}, map[string]any{}, frontend.Title{Key: core.EpisodeKey(42, 1, 4), Type: "episode", Season: 1, Episode: 4})
	if grants.calls != 1 {
		t.Fatal(grants.calls)
	}
}

type collageGrantFake struct{ calls int }

func (f *collageGrantFake) Issue(_ context.Context, r frontend.ImageGrantRequest) (string, error) {
	if r.Credential != "grant" || r.Item == 0 || !strings.HasPrefix(r.Image.Path, frontend.LibraryCollagePrefix) || r.Image.Path != r.Image.Tag {
		return "", errors.New("denied")
	}
	f.calls++
	return "collage-secret", nil
}
func (*collageGrantFake) Revoke(context.Context, string, string) error { return nil }
func (*collageGrantFake) Serve(http.ResponseWriter, *http.Request, string) {
}

type collageExperience struct {
	frontendtest.Experience
	image frontend.Image
	calls int
}

func (c *collageExperience) LibraryCollage(context.Context, *frontend.Viewer, frontend.Device, string) (frontend.Image, error) {
	c.calls++
	return c.image, nil
}

func TestJellyfinLibraryCollagePublishesPrimaryAndParentTag(t *testing.T) {
	viewID, err := ids.GUID(ids.View, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref := frontend.LibraryCollagePrefix + strings.Repeat("ab", 32)
	exp := &collageExperience{
		Experience: frontendtest.Experience{
			ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
				return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
			},
			LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
				return frontend.Page{Total: 2}, nil
			},
		},
		image: frontend.Image{Kind: frontend.ImagePoster, Path: ref, Tag: ref, Subject: viewID, Width: 640, Height: 640, BlurHash: "LGF5"},
	}
	grants := &collageGrantFake{}
	i := &instance{protocol: "jellyfin", host: frontend.Host{Experience: exp, ImageGrants: grants}}
	r := withSeriesStudioCache(httptest.NewRequest(http.MethodGet, "/UserViews", nil))
	r.Header.Set("X-Emby-Token", "grant")
	folder, ok := i.collectionFolder(r, &frontend.Viewer{ID: 1}, frontend.View{ID: "1", Name: "Movies", Type: "movie"})
	if !ok {
		t.Fatal("folder")
	}
	tags := folder["ImageTags"].(map[string]string)
	hashes := folder["ImageBlurHashes"].(map[string]any)["Primary"].(map[string]string)
	if tags["Primary"] != "collage-secret" || hashes["collage-secret"] != "LGF5" || folder["PrimaryImageAspectRatio"] != float64(1) {
		t.Fatal(folder["ImageTags"], folder["ImageBlurHashes"], folder["PrimaryImageAspectRatio"])
	}
	movie := map[string]any{}
	i.shapeJellyfinItem(r, &frontend.Viewer{ID: 1}, movie, frontend.Title{Key: core.MovieKey(5), Type: "movie", Title: "Movie"})
	if movie["ParentId"] != viewID || movie["ParentPrimaryImageTag"] != "collage-secret" || movie["ParentPrimaryImageItemId"] != viewID {
		t.Fatal(movie["ParentId"], movie["ParentPrimaryImageTag"], movie["ParentPrimaryImageItemId"])
	}
	if grants.calls != 1 || exp.calls != 1 {
		t.Fatalf("grants %d collages %d", grants.calls, exp.calls)
	}
}

func TestJellyfinEpisodeDetailLoadsTheShowStudio(t *testing.T) {
	show := core.ShowKey(42)
	episode := core.EpisodeKey(42, 1, 5)
	inst := mountJellyfin(t, frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Key == show {
			return frontend.ItemDetail{Title: frontend.Title{Key: show, Type: "show", Title: "Show", Studios: []string{"Jun Group Productions"}}}, nil
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: episode, Type: "episode", Title: "Five", ShowKey: show, ShowTitle: "Show", Season: 1, Episode: 5}}, nil
	}})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items/"+ids.RatingKeyGUID(episode), nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var item map[string]any
	if json.Unmarshal(w.Body.Bytes(), &item) != nil || item["SeriesStudio"] != "Jun Group Productions" {
		t.Fatal(item["SeriesStudio"])
	}
}

// Other countries' ratings read the way Jellyfin and Emby write them.
func TestOfficialRatingUsesCountryPrefix(t *testing.T) {
	exp := frontendtest.Experience{LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
		return frontend.Page{Items: []frontend.Title{
			{Key: core.MovieKey(1), Type: "movie", Title: "US", ContentRating: "TV-PG"},
			{Key: core.MovieKey(2), Type: "movie", Title: "Germany", ContentRating: "de/6"},
		}, Total: 2}, nil
	}}
	inst := mountJellyfin(t, exp)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Items?Recursive=true", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"OfficialRating":"TV-PG"`) || !strings.Contains(w.Body.String(), `"OfficialRating":"DE-6"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// peopleExperience adds the people reader the host's experience provides.
type peopleExperience struct {
	frontendtest.Experience
	people func(frontend.PeopleQuery) []frontend.PersonDetail
}

func (p peopleExperience) People(_ context.Context, _ *frontend.Viewer, _ frontend.Device, q frontend.PeopleQuery) ([]frontend.PersonDetail, error) {
	return p.people(q), nil
}
func (peopleExperience) Person(context.Context, *frontend.Viewer, frontend.Device, string, frontend.PersonReadOptions) (frontend.PersonDetail, error) {
	return frontend.PersonDetail{}, frontend.ErrNotFound
}
func (peopleExperience) PersonCredit(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, int64) (frontend.PersonDetail, error) {
	return frontend.PersonDetail{}, frontend.ErrNotFound
}

// /Persons?searchTerm= returns the people whose names match, from the
// people search, never every person in the library.
func TestJellyfinPersonsHonourSearchTerm(t *testing.T) {
	var asked frontend.PeopleQuery
	exp := peopleExperience{
		Experience: frontendtest.Experience{FacetsFunc: func(context.Context, *core.Viewer, core.Device, string, string) ([]core.FacetValue, error) {
			t.Fatal("a people search listed every person")
			return nil, nil
		}},
		people: func(q frontend.PeopleQuery) []frontend.PersonDetail {
			asked = q
			return []frontend.PersonDetail{{ID: "a1", Name: "Ringo Starr", TMDB: 1}, {ID: "b2", Name: "Star Person", TMDB: 2}}
		},
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		return &frontend.Viewer{ID: 1}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Persons?searchTerm=star&limit=24&recursive=true", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != 200 || asked.Query != "star" || asked.Limit != 24 || !strings.Contains(body, `"Name":"Ringo Starr"`) || !strings.Contains(body, `"Type":"Person"`) || !strings.Contains(body, `"TotalRecordCount":2`) {
		t.Fatal(w.Code, asked, body)
	}
}
