package watchlist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// plex uses only an account token supplied by the authenticated token source.
// A server share token is not an account token and a friend has no implicit grant.
func (s *Service) plex(ctx context.Context, token string) ([]core.Title, error) {
	out := []core.Title{}
	for start := 0; start < 2000; {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.o.PlexBaseURL, "/")+"/library/sections/watchlist/all?includeGuids=1&X-Plex-Container-Start="+strconv.Itoa(start)+"&X-Plex-Container-Size=100", nil)
		if err != nil {
			return nil, errors.New("Plex watchlist URL invalid")
		}
		req.Header.Set("X-Plex-Token", token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Plex-Product", "Ether")
		if s.o.ClientIdentifier != "" {
			req.Header.Set("X-Plex-Client-Identifier", s.o.ClientIdentifier)
		}
		resp, err := s.o.Client.Do(req)
		if err != nil {
			return nil, errors.New("Plex watchlist request failed")
		}
		data, e := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		resp.Body.Close()
		if e != nil || len(data) > 4<<20 {
			return nil, errors.New("Plex watchlist response unreadable or too large")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Plex watchlist: HTTP %d (viewer account authorization may be required)", resp.StatusCode)
		}
		var envelope struct {
			MediaContainer *struct {
				Size      int `json:"size"`
				TotalSize int `json:"totalSize"`
				Metadata  []struct {
					// Discover returns both guid (the Plex identity) and Guid (external IDs).
					// Declare both to prevent encoding/json's case-insensitive fallback
					// from decoding the lowercase string into the external-ID slice.
					PlexGUID string `json:"guid"`
					Type     string `json:"type"`
					Guid     []struct {
						ID string `json:"id"`
					} `json:"Guid"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(data, &envelope) != nil || envelope.MediaContainer == nil {
			return nil, errors.New("Plex watchlist response invalid")
		}
		for _, m := range envelope.MediaContainer.Metadata {
			for _, g := range m.Guid {
				var t *core.Title
				var e error
				switch {
				case strings.HasPrefix(g.ID, "tmdb://"):
					id, err := strconv.ParseInt(strings.TrimPrefix(g.ID, "tmdb://"), 10, 64)
					if err != nil || id <= 0 {
						continue
					}
					key := core.MovieKey(id)
					if m.Type == "show" {
						key = core.ShowKey(id)
					} else if m.Type != "movie" {
						continue
					}
					t, e = s.o.Catalog.Get(ctx, key)
				case strings.HasPrefix(g.ID, "imdb://"):
					t, e = s.o.Catalog.ByExternalID(ctx, "imdb", strings.TrimPrefix(g.ID, "imdb://"))
				default:
					continue
				}
				if e != nil && !errors.Is(e, core.ErrNotFound) {
					// A metadata outage cannot turn a real upstream entry into
					// an authoritative removal from the viewer's watchlist.
					return nil, fmt.Errorf("Plex watchlist metadata: %w", e)
				}
				if e == nil && t != nil {
					out = append(out, *t)
					break
				}
			}
		}
		n := len(envelope.MediaContainer.Metadata)
		total := envelope.MediaContainer.TotalSize
		if total > 0 {
			if start+n >= total {
				return out, nil
			}
			if n == 0 {
				return nil, errors.New("Plex watchlist ended before its reported total")
			}
		} else if n < 100 {
			return out, nil
		}
		start += n
	}
	return nil, errors.New("Plex watchlist exceeds 2000-item safety limit; previous snapshot preserved")
}
