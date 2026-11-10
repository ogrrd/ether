package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/remote"
)

var portalDockerBridge = netip.MustParsePrefix("172.16.0.0/12")

func (r *frontendRuntime) surfaceURL(ctx context.Context, id frontend.Protocol, req *http.Request) (string, error) {
	if req == nil {
		if id == "plex" && r.app != nil && r.app.registrar != nil {
			if origin := publicPlexOrigin(r.app.registrar.Status().Connections); origin != "" {
				return origin, nil
			}
		}
		return "", errors.New("client address unavailable")
	}
	ip, ok := r.app.frontendClientIP(req)
	if !ok {
		return "", errors.New("client address unavailable")
	}
	r.control.listeners.mu.Lock()
	run := r.control.listeners.running[id]
	var addresses, prefixes, shared []string
	if run != nil {
		for _, listener := range run.listeners {
			addresses = append(addresses, listener.Addr().String())
		}
		prefixes = append(prefixes, run.prefixes...)
		shared = append(shared, run.sharedPrefixes...)
	}
	r.control.listeners.mu.Unlock()
	if run == nil {
		return "", errors.New("frontend unavailable")
	}
	var cfg config.Config
	var trusted []netip.Prefix
	if r.app != nil && r.app.cfg != nil {
		cfg = r.app.cfg.Get()
		trusted = r.app.trustedProxyPrefixes()
	}
	// An app with its own public address is reached at that address as-is,
	// not at the Plex custom URL with this app's prefix appended.
	if base, ok := frontendPublicOrigin(cfg, string(id)); ok {
		return strings.TrimRight(base, "/"), nil
	}
	if base, ok := configuredOrigin(cfg, r.mainPort()); ok {
		if len(shared) > 0 {
			if resolved, err := surfaceSharedAddress(strings.TrimRight(base, "/")+"/ether/me", shared); err == nil {
				return resolved, nil
			}
		}
		if len(prefixes) > 0 && prefixes[0] != "" {
			return strings.TrimRight(strings.TrimRight(base, "/")+prefixes[0], "/"), nil
		}
		return strings.TrimRight(base, "/"), nil
	}
	local, _ := req.Context().Value(http.LocalAddrContextKey).(net.Addr)
	var interfaces []remote.InterfaceAddr
	if identity.LAN(ip) {
		interfaces, _ = remote.LANAddresses(remote.LANOptions{IncludeIPv6: true})
	}
	if base, ok := surfaceAddress(req, ip, local, addresses, prefixes, trusted, interfaces); ok {
		return base, nil
	}
	portal, err := r.viewerPortalURL(ctx, req)
	if err != nil {
		return "", err
	}
	return surfaceSharedAddress(portal, shared)
}

func (r *frontendRuntime) mainPort() string {
	r.mu.Lock()
	addr := r.mainAddr
	r.mu.Unlock()
	if addr == nil {
		return ""
	}
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	return port
}

func surfaceSharedAddress(portal string, shared []string) (string, error) {
	u, err := url.Parse(portal)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/ether/me") {
		return "", errors.New("portal address unavailable")
	}
	for _, prefix := range shared {
		if prefix != "" {
			u.Path = strings.TrimSuffix(u.Path, "/ether/me") + prefix
			return strings.TrimRight(u.String(), "/"), nil
		}
	}
	return "", errors.New("no reachable frontend address")
}

func surfaceAddress(req *http.Request, peer netip.Addr, local net.Addr, addresses, prefixes []string, trusted []netip.Prefix, interfaces []remote.InterfaceAddr) (string, bool) {
	localPort := ""
	if local != nil {
		var err error
		_, localPort, err = net.SplitHostPort(local.String())
		if err != nil {
			return "", false
		}
	}
	for index, address := range addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			continue
		}
		if local != nil && port != localPort {
			continue
		}
		if base, ok := requestOriginWithPort(req, host, port, trusted); ok {
			u, err := url.Parse(base)
			if err == nil && u != nil {
				if index < len(prefixes) && prefixes[index] != "" {
					u.Path = strings.TrimRight(u.Path, "/") + prefixes[index]
				}
				return strings.TrimRight(u.String(), "/"), true
			}
		}
		if local == nil {
			break
		}
	}
	return surfaceLANAddress(peer, local, addresses, prefixes, interfaces)
}

func surfaceLANAddress(peer netip.Addr, local net.Addr, addresses, prefixes []string, interfaces []remote.InterfaceAddr) (string, bool) {
	if local == nil || !identity.LAN(peer) {
		return "", false
	}
	_, localPort, err := net.SplitHostPort(local.String())
	if err != nil {
		return "", false
	}
	for index, address := range addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != localPort {
			continue
		}
		if peer.IsLoopback() {
			bound := net.ParseIP(host)
			if bound != nil && (bound.IsUnspecified() || bound.IsLoopback()) {
				u := url.URL{Scheme: "http", Host: net.JoinHostPort(peer.String(), port)}
				if index < len(prefixes) {
					u.Path = prefixes[index]
				}
				return strings.TrimRight(u.String(), "/"), true
			}
		}
		portal, err := portalLANAddress(peer, host, port, interfaces)
		if err == nil {
			u, _ := url.Parse(portal)
			u.Path = ""
			if index < len(prefixes) {
				u.Path = prefixes[index]
			}
			return strings.TrimRight(u.String(), "/"), true
		}
	}
	return "", false
}

// viewerPortalURL chooses a reachable portal URL in order:
// 1. the owner's configured public or custom URL, if any;
// 2. the request Host (with the app's port) or X-Forwarded-Host / X-Forwarded-Proto from a trusted proxy;
// 3. the matching LAN interface or published remote connection.
func (r *frontendRuntime) viewerPortalURL(ctx context.Context, req *http.Request) (string, error) {
	ip, ok := r.app.frontendClientIP(req)
	if !ok {
		return "", errors.New("client address unavailable")
	}
	r.mu.Lock()
	addr := r.mainAddr
	r.mu.Unlock()
	if addr == nil {
		return "", errors.New("main listener unavailable")
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", err
	}
	var cfg config.Config
	var trusted []netip.Prefix
	if r.app != nil && r.app.cfg != nil {
		cfg = r.app.cfg.Get()
		trusted = r.app.trustedProxyPrefixes()
	}
	var interfaces []remote.InterfaceAddr
	if identity.LAN(ip) && !ip.IsLoopback() {
		interfaces, _ = remote.LANAddresses(remote.LANOptions{IncludeIPv6: true})
	}
	var connections []remote.Connection
	for _, raw := range cfg.Plex.CustomURLs {
		connections = append(connections, remote.Connection{URI: raw, Custom: true})
	}
	if r.app != nil && r.app.registrar != nil {
		connections = append(connections, r.app.registrar.Status().Connections...)
	}
	return portalAddress(ctx, req, ip, host, port, cfg, trusted, interfaces, connections)
}

func portalAddress(ctx context.Context, req *http.Request, ip netip.Addr, host, port string, cfg config.Config, trusted []netip.Prefix, interfaces []remote.InterfaceAddr, connections []remote.Connection) (string, error) {
	if base, ok := configuredOrigin(cfg, port); ok {
		return strings.TrimRight(base, "/") + "/ether/me", nil
	}
	if base, ok := requestOriginWithPort(req, host, port, trusted); ok {
		return strings.TrimRight(base, "/") + "/ether/me", nil
	}
	// A loopback client talking to a loopback or wildcard listener already
	// knows the answer. Enumerating interfaces is the slow first call.
	if ip.IsLoopback() {
		if bound := net.ParseIP(host); bound != nil && (bound.IsUnspecified() || bound.IsLoopback()) {
			return (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), port), Path: "/ether/me"}).String(), nil
		}
	}
	if identity.LAN(ip) {
		if lan, err := portalLANAddress(ip, host, port, interfaces); err == nil {
			return lan, nil
		}
	}
	return portalPublicAddress(ctx, connections)
}

// viewerPortalSecureURL selects a reachable HTTPS address for clients, such as
// Stremio, that reject non-loopback HTTP addon manifests.
func (r *frontendRuntime) viewerPortalSecureURL(ctx context.Context, req *http.Request) (string, error) {
	ip, ok := r.app.frontendClientIP(req)
	if !ok {
		return "", errors.New("client address unavailable")
	}
	r.mu.Lock()
	addr := r.mainAddr
	r.mu.Unlock()
	if addr == nil {
		return "", errors.New("main listener unavailable")
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", err
	}
	var cfg config.Config
	var trusted []netip.Prefix
	if r.app != nil && r.app.cfg != nil {
		cfg = r.app.cfg.Get()
		trusted = r.app.trustedProxyPrefixes()
	}
	if base, ok := configuredOrigin(cfg, port); ok && strings.HasPrefix(base, "https://") {
		return strings.TrimRight(base, "/") + "/ether/me", nil
	}
	if base, ok := requestOriginWithPort(req, host, port, trusted); ok && strings.HasPrefix(base, "https://") {
		return strings.TrimRight(base, "/") + "/ether/me", nil
	}
	var interfaces []remote.InterfaceAddr
	if identity.LAN(ip) {
		interfaces, _ = remote.LANAddresses(remote.LANOptions{IncludeIPv6: true})
	}
	var connections []remote.Connection
	if r.app != nil && r.app.registrar != nil {
		connections = append(connections, r.app.registrar.Status().Connections...)
	}
	for _, raw := range cfg.Plex.CustomURLs {
		connections = append(connections, remote.Connection{URI: raw, Custom: true})
	}
	return portalSecureAddress(ctx, ip, host, port, interfaces, connections)
}

// frontendPublicOrigin returns the app's own public address when the owner
// set a valid one. Plex never uses it: Plex apps use Plex.CustomURLs.
func frontendPublicOrigin(cfg config.Config, id string) (string, bool) {
	if id == "plex" {
		return "", false
	}
	return parseConfiguredURL(cfg.Frontends[id].PublicURL)
}

func configuredOrigin(cfg config.Config, defaultPort string) (string, bool) {
	for _, raw := range cfg.Plex.CustomURLs {
		if base, ok := parseConfiguredURL(raw); ok {
			return base, true
		}
	}
	if host := strings.TrimSpace(cfg.Plex.PublicHost); host != "" {
		if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
			if base, ok := parseConfiguredURL(host); ok {
				return base, true
			}
		} else {
			var h, p string
			if sh, sp, err := net.SplitHostPort(host); err == nil {
				h, p = strings.Trim(sh, "[]"), sp
			} else {
				h = strings.Trim(host, "[]")
				if cfg.Plex.PublicPort > 0 {
					p = strconv.Itoa(cfg.Plex.PublicPort)
				} else if cfg.Server.Port > 0 {
					p = strconv.Itoa(cfg.Server.Port)
				} else {
					p = defaultPort
				}
			}
			scheme := "http"
			if p == "443" || cfg.Plex.SecureConnections == remote.SecureRequired {
				scheme = "https"
			}
			addr := h
			if p != "" && !((scheme == "http" && p == "80") || (scheme == "https" && p == "443")) {
				addr = net.JoinHostPort(h, p)
			}
			if base, ok := parseConfiguredURL(scheme + "://" + addr); ok {
				return base, true
			}
		}
	}
	return "", false
}

func parseConfiguredURL(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	origin := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	c, err := remote.ParseCustomURL(origin)
	if err != nil || !usableRequestHost(c.Address, true) {
		return "", false
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), true
}

func requestOriginWithPort(req *http.Request, boundHost, appPort string, trusted []netip.Prefix) (string, bool) {
	if req == nil || appPort == "" {
		return "", false
	}
	trustedProxy := portalProxyTrusted(req, trusted)
	if trustedProxy {
		if fHost := portalForwardedHost(req); fHost != "" {
			scheme := "http"
			if req.TLS != nil {
				scheme = "https"
			}
			if fProto := portalForwardedProto(req); fProto == "http" || fProto == "https" {
				scheme = fProto
			}
			h, p, err := net.SplitHostPort(fHost)
			if err != nil {
				h = strings.Trim(fHost, "[]")
				p = ""
			} else {
				h = strings.Trim(h, "[]")
			}
			if usableRequestHost(h, true) {
				hostPort := h
				if p != "" {
					hostPort = net.JoinHostPort(h, p)
				}
				candidate := (&url.URL{Scheme: scheme, Host: hostPort}).String()
				if _, err := remote.ParseCustomURL(candidate); err == nil {
					return candidate, true
				}
			}
		}
	}
	if bound := net.ParseIP(boundHost); bound != nil && bound.IsLoopback() {
		return "", false
	}
	rawHost := portalSanitizeHost(strings.TrimSpace(req.Host))
	if rawHost == "" {
		return "", false
	}
	h, reqPort, err := net.SplitHostPort(rawHost)
	if err != nil {
		h = strings.Trim(rawHost, "[]")
		reqPort = ""
	} else {
		h = strings.Trim(h, "[]")
	}
	if !usableRequestHost(h, false) {
		return "", false
	}
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	fProto := ""
	if trustedProxy {
		fProto = portalForwardedProto(req)
		if fProto == "http" || fProto == "https" {
			scheme = fProto
		}
	}
	hostPort := net.JoinHostPort(h, appPort)
	if trustedProxy && (fProto == "http" || fProto == "https") && reqPort == "" {
		hostPort = h
	}
	candidate := (&url.URL{Scheme: scheme, Host: hostPort}).String()
	if _, err := remote.ParseCustomURL(candidate); err != nil {
		return "", false
	}
	return candidate, true
}

func usableRequestHost(host string, fromProxyOrConfig bool) bool {
	h := strings.TrimSpace(strings.Trim(host, "[]"))
	if h == "" || strings.EqualFold(h, "localhost") {
		return false
	}
	if !fromProxyOrConfig && strings.EqualFold(h, "example.com") {
		return false
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		ip = ip.Unmap()
		if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || portalDockerBridge.Contains(ip) {
			return false
		}
	}
	return true
}

func portalProxyTrusted(r *http.Request, trusted []netip.Prefix) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	peer = peer.Unmap()
	if peer.IsLoopback() {
		return true
	}
	for _, p := range trusted {
		if p.Contains(peer) {
			return true
		}
	}
	return false
}

func portalForwardedHost(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); h != "" {
		return portalSanitizeHost(strings.TrimSpace(strings.Split(h, ",")[0]))
	}
	return ""
}

func portalForwardedProto(r *http.Request) string {
	p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	return strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
}

func portalSanitizeHost(host string) string {
	if host == "" || strings.ContainsAny(host, " \t\r\n/@") || len(host) > 255 {
		return ""
	}
	return host
}

func portalSecureAddress(ctx context.Context, peer netip.Addr, host, port string, interfaces []remote.InterfaceAddr, connections []remote.Connection) (string, error) {
	// A loopback admin session has no LAN address of its own. The certificate
	// is published on the server's LAN interfaces, which is the address Stremio
	// on this machine can still open.
	if peer.Unmap().IsLoopback() {
		if secure, ok := loopbackPlexDirect(host, port, interfaces, connections); ok {
			return secure, nil
		}
	}
	if lan, err := portalLANAddress(peer, host, port, interfaces); err == nil {
		local, _ := url.Parse(lan)
		localIP := net.ParseIP(local.Hostname())
		for _, candidate := range connections {
			u, err := url.Parse(candidate.URI)
			if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != port {
				continue
			}
			ip, _, ok := remote.ParsePlexDirectHost(u.Hostname())
			if !candidate.Local || !ok || !ip.Equal(localIP) {
				continue
			}
			u.Path = strings.TrimRight(u.Path, "/") + "/ether/me"
			return u.String(), nil
		}
	}
	secure := make([]remote.Connection, 0, len(connections))
	for _, candidate := range connections {
		u, err := url.Parse(candidate.URI)
		if err == nil && u.Scheme == "https" {
			secure = append(secure, candidate)
		}
	}
	return portalPublicAddress(ctx, secure)
}

// loopbackPlexDirect returns the server's own LAN plex.direct HTTPS address
// when this listener can accept it. A loopback-only bind is not that listener.
func loopbackPlexDirect(host, port string, interfaces []remote.InterfaceAddr, connections []remote.Connection) (string, bool) {
	bound := net.ParseIP(host)
	if bound != nil && bound.IsLoopback() {
		return "", false
	}
	for _, iface := range interfaces {
		if iface.IP == nil || iface.IP.IsLoopback() {
			continue
		}
		if bound != nil && !bound.IsUnspecified() && !bound.Equal(iface.IP) {
			continue
		}
		for _, candidate := range connections {
			u, err := url.Parse(candidate.URI)
			if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != port {
				continue
			}
			ip, _, ok := remote.ParsePlexDirectHost(u.Hostname())
			if !candidate.Local || !ok || !ip.Equal(iface.IP) {
				continue
			}
			u.Path = strings.TrimRight(u.Path, "/") + "/ether/me"
			return u.String(), true
		}
	}
	return "", false
}

func portalLANAddress(ip netip.Addr, host, port string, interfaces []remote.InterfaceAddr) (string, error) {
	if ip.IsLoopback() {
		bound := net.ParseIP(host)
		if bound != nil && (bound.IsUnspecified() || bound.IsLoopback()) {
			return (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), port), Path: "/ether/me"}).String(), nil
		}
	}

	for _, candidate := range interfaces {
		if candidate.Net == nil || !candidate.Net.Contains(net.IP(ip.AsSlice())) {
			continue
		}
		bound := net.ParseIP(host)
		if bound != nil && !bound.IsUnspecified() && !bound.Equal(candidate.IP) {
			continue
		}
		u := url.URL{Scheme: "http", Host: net.JoinHostPort(candidate.IP.String(), port), Path: "/ether/me"}
		return u.String(), nil
	}
	return "", errors.New("no reachable LAN address")
}

func portalPublicAddress(ctx context.Context, connections []remote.Connection) (string, error) {
	for _, c := range connections {
		if c.Local || c.Relay {
			continue
		}
		u, err := url.Parse(c.URI)
		if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
			continue
		}
		hostname := u.Hostname()
		if p := net.ParseIP(hostname); p != nil {
			if !remote.IsPublicIP(p) {
				continue
			}
		} else if p, _, ok := remote.ParsePlexDirectHost(hostname); ok {
			if !remote.IsPublicIP(p) {
				continue
			}
		} else {
			ips, e := net.DefaultResolver.LookupIPAddr(ctx, hostname)
			if e != nil || len(ips) == 0 {
				continue
			}
			valid := true
			for _, a := range ips {
				if !remote.IsPublicIP(a.IP) {
					valid = false
					break
				}
			}
			if !valid {
				continue
			}
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/ether/me"
		u.RawPath = ""
		return u.String(), nil
	}
	return "", errors.New("public address unavailable")
}
