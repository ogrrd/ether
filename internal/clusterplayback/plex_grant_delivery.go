package clusterplayback

import (
	"github.com/braedonsaunders/ether/internal/streamgrant"
	"net/http"
	"net/url"
	"strings"
)

const plexReadGrant = "X-Ether-Read-Token"

var accountCredentialFields = []string{"Authorization", "X-Plex-Token", "api_key", "apikey", "X-Emby-Token", "X-MediaBrowser-Token", "access_token"}

func plexGrantEnvelope(q *http.Request) bool {
	if (q.Method != "GET" && q.Method != "HEAD") || q.URL.RawPath != "" || len(q.URL.RequestURI()) > 16384 || q.Header.Get(deliveryHop) != "" {
		return false
	}
	if _, ok := plexMediaSession(q.URL); !ok {
		return false
	}
	for name := range q.Header {
		for _, field := range append([]string{streamgrant.StreamGrantParam, plexReadGrant}, accountCredentialFields...) {
			if strings.EqualFold(name, field) {
				return false
			}
		}
	}
	values, e := url.ParseQuery(q.URL.RawQuery)
	if e != nil {
		return false
	}
	found := false
	hasStream, hasRead := false, false
	for name, items := range values {
		for _, field := range accountCredentialFields {
			if strings.EqualFold(name, field) {
				return false
			}
		}
		for _, field := range []string{streamgrant.StreamGrantParam, plexReadGrant} {
			if !strings.EqualFold(name, field) {
				continue
			}
			if name != field || len(items) != 1 || items[0] == "" || len(items[0]) > 512 || strings.TrimSpace(items[0]) != items[0] {
				return false
			}
			for _, c := range items[0] {
				if c < 33 || c > 126 {
					return false
				}
			}
			found = true
			if field == streamgrant.StreamGrantParam {
				hasStream = true
			} else {
				hasRead = true
			}
		}
	}
	if hasRead && !hasStream && !boundedPlexClient(q, values) {
		return false
	}
	return found
}

// ForwardGrantMedia is deliberately distinct from account-token forwarding.
// It transports the exact original supported grant envelope; only the owner's
// original authentication and current-policy handlers authorize any bytes.
func (r *Runtime) ForwardGrantMedia(w http.ResponseWriter, q *http.Request, id string) bool {
	if !plexGrantEnvelope(q) {
		return false
	}
	return r.ForwardMedia(w, q, id)
}

func boundedPlexClient(q *http.Request, values url.Values) bool {
	for _, field := range []string{"X-Plex-Client-Identifier", "X-Plex-Client-Id"} {
		candidate := q.Header.Get(field)
		if candidate == "" {
			candidate = values.Get(field)
		}
		if candidate == "" {
			for name, items := range values {
				if strings.EqualFold(name, field) && len(items) == 1 {
					candidate = items[0]
					break
				}
			}
		}
		if candidate != "" {
			if len(candidate) > 512 || strings.TrimSpace(candidate) != candidate {
				return false
			}
			for _, c := range candidate {
				if c < 33 || c > 126 {
					return false
				}
			}
			return true
		}
	}
	return false
}
