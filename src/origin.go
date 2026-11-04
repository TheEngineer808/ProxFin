package src

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// crossSiteRequest reports whether r was sent by a browser on behalf of
// another website: a page the user happens to open, anywhere on the
// internet, can make the browser send requests to ProxFin on the local
// network. With web authentication off (the default) that page could
// otherwise change any setting through the web interface's websocket,
// including the FFmpeg path and options, i.e. run commands on the server.
//
// Requests that change something (the websocket, the API, the PPV
// endpoints, logins) are refused when this returns true. Requests without
// the browser headers (Plex, Emby, Jellyfin, curl, scripts) are not
// affected: browsers always send them and pages can't remove them.
func crossSiteRequest(r *http.Request) bool {

	// Fetch metadata, sent by current browsers on every request including
	// websocket handshakes. It is computed from the address of the page
	// that made the request, so it is right behind reverse proxies too.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none": // "none": typed in, bookmark
		return false
	case "cross-site":
		return true
	}

	// "same-site" (another port or subdomain) and older browsers: compare
	// the origin of the page with this server.
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false // not sent by a browser for another page
	}

	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true // "null": sandboxed frames, local files
	}

	return !isOwnHost(normalizeHost(u.Host, u.Scheme), r)
}

// isOwnHost reports whether host (normalized) is an address this server
// is reached at: the request's Host, the host a reverse proxy forwarded,
// or a domain configured in the settings.
func isOwnHost(host string, r *http.Request) bool {

	var scheme = "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}

	var candidates = []string{r.Host}

	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		candidates = append(candidates, strings.TrimSpace(strings.Split(fwd, ",")[0]))
	}

	for _, domain := range []string{Settings.HttpThreadfinDomain, Settings.HttpsThreadfinDomain} {
		if domain != "" {
			candidates = append(candidates, domain)
		}
	}

	for _, c := range candidates {
		if c == "" {
			continue
		}
		if normalizeHost(c, scheme) == host {
			return true
		}
		// A configured domain without a port matches that host on any port.
		if !strings.Contains(c, ":") && hostOnly(host) == strings.ToLower(c) {
			return true
		}
	}

	return false
}

// normalizeHost lower-cases host and removes the default port of scheme.
func normalizeHost(host, scheme string) string {

	host = strings.ToLower(strings.TrimSuffix(host, "."))

	if h, port, err := net.SplitHostPort(host); err == nil {
		if (scheme == "http" || scheme == "ws") && port == "80" || (scheme == "https" || scheme == "wss") && port == "443" {
			return h
		}
	}

	return host
}

// hostOnly returns host without its port.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// refuseCrossSite answers a cross-site request with 403 and reports
// whether it did.
func refuseCrossSite(w http.ResponseWriter, r *http.Request) bool {
	if !crossSiteRequest(r) {
		return false
	}
	showWarning(1103)
	httpStatusError(w, r, http.StatusForbidden)
	return true
}

// usersForBrowser returns the users without their password hashes and
// salts: the web interface only uses the "data" part, and the hashes must
// not leave the server.
func usersForBrowser(users map[string]interface{}) map[string]interface{} {

	var out = make(map[string]interface{}, len(users))

	for id, u := range users {
		user, ok := u.(map[string]interface{})
		if !ok {
			continue
		}
		var clean = make(map[string]interface{}, len(user))
		for k, v := range user {
			switch k {
			case "_password", "_salt", "_username":
				continue
			}
			clean[k] = v
		}
		out[id] = clean
	}

	return out
}
