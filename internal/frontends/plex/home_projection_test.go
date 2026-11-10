package plex

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestPlaylistHomeProjectionPreservesHub(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	viewer := &core.User{ID: 1, IsOwner: true}
	if _, err := f.srv.playlists().Create(context.Background(), viewer.ID, "Test", []core.RatingKey{core.MovieKey(1)}); err != nil {
		t.Fatal(err)
	}
	pr := &Request{User: viewer}
	h, row, ok := f.srv.playlistsHubProjection(context.Background(), pr, 10)
	legacy, ok2 := f.srv.playlistsHub(context.Background(), pr, 10)
	if !ok || !ok2 || !reflect.DeepEqual(h, legacy) || len(row.Entries) != 1 || row.Entries[0].Kind != frontend.RowEntryPlaylist || row.Entries[0].Playlist.Title != "Test" || row.Entries[0].Playlist.Items != nil {
		t.Fatalf("projection mismatch: %+v %+v", h, row)
	}
}

func TestLiveHomeProjectionPreservesHub(t *testing.T) {
	f, _, channel := liveFixture(t)
	r := httptest.NewRequest("GET", "/hubs", nil)
	r = r.WithContext(withRequest(r.Context(), &Request{User: &core.User{ID: 1, IsOwner: true}}))
	h, row := f.srv.liveTVHubProjection(r)
	legacy := f.srv.liveTVHub(r)
	if h == nil || !reflect.DeepEqual(h, legacy) || len(row.Entries) != 1 {
		t.Fatalf("projection mismatch: %+v %+v", h, row)
	}
	c := row.Entries[0].LiveChannel
	if c == nil || c.ID != channel.ID || c.Current == nil || c.Current.Title != "Owner programme" || c.Current.Start.IsZero() || c.Current.End.IsZero() {
		t.Fatalf("bad live projection: %+v", c)
	}
}
