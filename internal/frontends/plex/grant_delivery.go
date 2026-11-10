package plex

import (
	"net/http"
	"net/url"
	"strings"
)

// grantPlexMediaID recognizes an opaque credential envelope, not its authority.
// Only the exact owner can authenticate these values against its real entry.
func grantPlexMediaID(r *http.Request, pr *Request) (string, bool) {
	id, ok := missingPlexMediaID(r)
	if !ok || pr == nil || pr.Token != "" || r.Header.Get("Authorization") != "" {
		return "", false
	}
	if len(r.Header.Values("X-Plex-Token")) > 0 {
		return "", false
	}
	if len(r.Header.Values(streamGrantParam)) > 0 || len(r.Header.Values(readCapParam)) > 0 || len(r.Header.Values("Authorization")) > 0 {
		return "", false
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", false
	}
	credentials := 0
	hasStream, hasRead := false, false
	for key, values := range query {
		if strings.EqualFold(key, "X-Plex-Token") || strings.EqualFold(key, "Authorization") {
			return "", false
		}
		if !strings.EqualFold(key, streamGrantParam) && !strings.EqualFold(key, readCapParam) {
			continue
		}
		// Existing owner checks use canonical keys; aliases and duplicate values
		// cannot be allowed to change precedence across two independent parsers.
		if key != streamGrantParam && key != readCapParam {
			return "", false
		}
		if len(values) != 1 || !boundedPlexGrant(values[0]) {
			return "", false
		}
		credentials++
		if key == streamGrantParam {
			hasStream = true
		}
		if key == readCapParam {
			hasRead = true
		}
	}
	if credentials < 1 || credentials > 2 || (hasRead && !hasStream && (pr.ClientID == "" || len(pr.ClientID) > 256)) {
		return "", false
	}
	return id, true
}
func boundedPlexGrant(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, c := range value {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func (s *Server) forwardMissingPlexGrant(w http.ResponseWriter, r *http.Request, pr *Request) bool {
	id, eligible := grantPlexMediaID(r, pr)
	if !eligible {
		return false
	}
	if _, local := s.transcodes.get(id); local {
		return false
	}
	forward, ok := s.currentPlexEncoder().(interface {
		ForwardGrantMedia(http.ResponseWriter, *http.Request, string) bool
	})
	if !ok {
		return false
	}
	// This is a terminal credential transfer. Never set pr.User/Capability,
	// install a local entry, or resume the sender's authenticated handler chain.
	return forward.ForwardGrantMedia(w, r, id)
}
