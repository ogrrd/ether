package plex

import (
	"context"
	"net/url"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type structuralProjectionKey struct{}

// applyHubDetailOptions projects requested structural blocks onto the rendered
// cards. Membership and paging have already been selected by the shelf engine.
func (s *Server) applyHubDetailOptions(ctx context.Context, mc *model.MediaContainer, q url.Values) {
	children, onDeck, related, extras := metadataOptionOn(q.Get("includeChildren")), metadataOptionOn(q.Get("includeOnDeck")), metadataOptionOn(q.Get("includeRelated")), metadataOptionOn(q.Get("includeExtras"))
	preferences := q.Get("includePreferences") == "1"
	if !children && !onDeck && !related && !extras && !preferences {
		return
	}
	ctx = context.WithValue(ctx, structuralProjectionKey{}, true)
	pr := FromContext(ctx)
	for hi := range mc.Hub {
		for i := range mc.Hub[hi].Metadata {
			row := &mc.Hub[hi].Metadata[i]
			n, err := strconv.ParseInt(row.RatingKey, 10, 64)
			if err != nil {
				continue
			}
			title := s.titles.get(frontend.RatingKey(n))
			if title == nil {
				// Cache eviction must not erase a requested block. Production
				// adapters use a local point read without remote discovery.
				if stored, ok := s.catalog().(frontend.StoredTitleLookup); ok {
					title, _ = stored.StoredTitle(ctx, frontend.RatingKey(n))
				} else {
					title, _ = s.catalog().Get(ctx, frontend.RatingKey(n))
				}
			}
			if title == nil {
				continue
			}
			full := s.detail(ctx, pr, title, children, onDeck, related, extras, q.Get("extraTypes"))
			row.Children, row.OnDeck, row.Related, row.Extras = full.Children, full.OnDeck, full.Related, full.Extras
			if preferences {
				row.Preferences = s.itemTrackPreferences(ctx, pr, title)
			}
			if extras {
				row.PrimaryExtraKey = full.PrimaryExtraKey
			}
		}
	}
}

// Inline OnDeck is a metadata object, so the client's element exclusions also
// apply to its children, including when it is nested in hub/detail structures.
func applyInlineOnDeckExclusions(mc *model.MediaContainer, excluded map[string]bool) {
	if len(excluded) == 0 {
		return
	}
	var visit func(*model.Metadata)
	visit = func(row *model.Metadata) {
		if row.OnDeck != nil {
			items := []model.Metadata{row.OnDeck.Metadata}
			applyExcludedElements(items, excluded)
			if excluded["Guid"] {
				items[0].GuidLead = nil
			}
			row.OnDeck.Metadata = items[0]
			visit(&row.OnDeck.Metadata)
		}
		if row.Children != nil {
			for i := range row.Children.Metadata {
				visit(&row.Children.Metadata[i])
			}
		}
		if row.Extras != nil {
			for i := range row.Extras.Metadata {
				visit(&row.Extras.Metadata[i])
			}
		}
		if row.Related != nil {
			for h := range row.Related.Hub {
				for i := range row.Related.Hub[h].Metadata {
					visit(&row.Related.Hub[h].Metadata[i])
				}
			}
		}
	}
	for i := range mc.Metadata {
		visit(&mc.Metadata[i])
	}
	for h := range mc.Hub {
		for i := range mc.Hub[h].Metadata {
			visit(&mc.Hub[h].Metadata[i])
		}
	}
}
