package plex

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
)

// Plex for Apple TV draws guide channel logos only from URLs that Plex's
// cloud image service can fetch: it never loads them from the server
// itself, so a logo on a LAN address never appears (#391, proven on the
// device: a public HTTPS logo rendered, the same channel's LAN URL did not,
// and the server saw no image request at all). Whatever is published there
// leaves the network, so it must never carry a viewer's token.

// signedLogoPrefix serves a channel's logo without a Plex token, to a URL
// signed by this server.
const signedLogoPrefix = "/livetv/logos/"

func cloudGuideLogoClient(pr *Request) bool {
	return pr != nil && (strings.EqualFold(pr.Platform, "tvOS") || strings.EqualFold(pr.Product, "Plex for Apple TV"))
}

// cloudGuideLogo is the logo URL for a client whose guide fetches logos
// through Plex's cloud: the channel's own logo when that is already public
// (as PMS publishes XMLTV icons), a shipped channel's logo from Ether's
// public repository at this build's release, else this server's signed,
// token-free logo URL on its public address when remote access publishes
// one, or on origin.
func (s *Server) cloudGuideLogo(ctx context.Context, c frontend.LiveTVChannel, origin string) string {
	if publicLogoURL(c.Logo) {
		return c.Logo
	}
	repoFile, _ := s.settings()["repoFileURL"].(func(string) string)
	if path, _, ok := s.d.ArtworkStores.ShowcaseLogo(c); ok && repoFile != nil {
		if u := repoFile(path); u != "" {
			return u
		}
	}
	if c.Logo == "" && s.d.LiveTV != nil {
		if guide := s.d.LiveTV.GuideChannelLogo(ctx, c); publicLogoURL(guide) {
			return guide
		}
	}
	base := origin
	if s.host.SurfaceURL != nil {
		if public, err := s.host.SurfaceURL(ctx, nil); err == nil {
			if public = strings.TrimRight(public, "/"); public != "" {
				base = public
			}
		}
	}
	return base + signedLogoPrefix + url.PathEscape(c.ID) + "/" + s.liveLogoSignature(c.ID) + ".png"
}

// publicLogoURL reports an http(s) image URL on a public host that Plex's
// cloud can fetch and draw as is (not SVG, which only this server rasterizes).
// Hosts are judged by name, without a DNS lookup on the request path.
func publicLogoURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Host == "" || strings.HasSuffix(strings.ToLower(u.Path), ".svg") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil {
		return clientnet.IsPublicIP(ip)
	}
	if ip, _, ok := clientnet.ParsePlexDirectHost(host); ok {
		return clientnet.IsPublicIP(ip)
	}
	if !strings.Contains(host, ".") {
		return false
	}
	for _, suffix := range []string{".local", ".lan", ".home", ".home.arpa", ".internal", ".localdomain", ".localhost", ".corp", ".intranet"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	return true
}

// liveLogoSignature authorizes signedLogoPrefix for one channel. The key is
// derived per node and survives restarts, so cached guide URLs stay valid.
func (s *Server) liveLogoSignature(id string) string {
	key := s.d.LiveLogoKey
	if len(key) == 0 {
		s.liveLogoKeyOnce.Do(func() {
			s.liveLogoKey = make([]byte, 32)
			_, _ = rand.Read(s.liveLogoKey)
		})
		key = s.liveLogoKey
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("ether-live-logo-v1\x00" + id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

// handleSignedLiveLogo serves a channel's logo to a signed URL, without a
// viewer: the signature was minted for a viewer allowed that channel, and a
// logo reveals nothing beyond what the guide that carried it did.
func (s *Server) handleSignedLiveLogo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channel")
	sig, ok := strings.CutSuffix(chi.URLParam(r, "signature"), ".png")
	if !ok || !s.liveTVEnabled() || !hmac.Equal([]byte(sig), []byte(s.liveLogoSignature(id))) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	c, err := s.d.LiveTV.Channel(r.Context(), id)
	if err != nil || !c.Enabled {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.writeChannelLogo(w, r, c, true, "public, max-age=86400")
}
