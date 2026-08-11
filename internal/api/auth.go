package api

import (
	"crypto/subtle"
	"net/http"
)

// The portal has two audiences.
//
// Reading — the host list, the specs, the change feed — is open to anyone who
// can reach the portal, which in practice means anyone on the corporate
// network. That is the whole point of a portal: people should not need an
// account to look up an API.
//
// Everything that changes state or acts on someone else's service is behind a
// login: editing a service, supplying a spec URL, triggering a probe, the
// settings page. Those are the operations that made this API dangerous when it
// gained write access — a spec URL a stranger can set is a request the portal
// makes from inside the cluster.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAdmin(w, r) {
			return
		}
		next(w, r)
	}
}

func (s *Server) requireAdminHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAdmin(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) checkAdmin(w http.ResponseWriter, r *http.Request) bool {
	// No credentials configured means no administration — never "no check".
	// Failing open here would hand write access to everyone on the network.
	if s.cfg.Admin.User == "" || s.cfg.Admin.Password == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "administration is disabled: set SPECTRA_ADMIN_USER and SPECTRA_ADMIN_PASSWORD",
		})
		return false
	}

	// A session cookie is the normal case; Basic stays accepted so that curl
	// and the login exchange itself keep working without a browser.
	if s.hasSession(r) || s.hasBasic(r) {
		return true
	}
	// No WWW-Authenticate header: it would make the browser pop its own login
	// dialog over the page, and the portal has its own. The dialog also cannot
	// be dismissed cleanly for a visitor who has no account and only wanted to
	// read a specification.
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "нужен вход администратора"})
	return false
}

func (s *Server) hasBasic(r *http.Request) bool {
	user, password, ok := r.BasicAuth()
	return ok && equal(user, s.cfg.Admin.User) && equal(password, s.cfg.Admin.Password)
}

// isPortalCredential reports whether the Authorization header on this request
// is the portal's own login.
//
// It exists so the proxy can strip it before forwarding. The header was meant
// for Spectra, not for the service behind it — without this the admin password
// lands in the access log of every service anyone tries out, where its owner,
// and anyone who can read those logs, would find it.
//
// Any other Authorization passes through untouched: "Try it out" against an
// API that needs a token has to keep working.
func (s *Server) isPortalCredential(r *http.Request) bool {
	user, password, ok := r.BasicAuth()
	return ok && equal(user, s.cfg.Admin.User) && equal(password, s.cfg.Admin.Password)
}

// equal compares in constant time so that a wrong password cannot be found by
// measuring how long the comparison took.
func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// IsAdmin reports whether the caller is authenticated, so the UI can hide the
// controls it would not be allowed to use. It is a convenience, not a control:
// the endpoints check for themselves.
func (s *Server) isAdmin(r *http.Request) bool {
	if s.cfg.Admin.User == "" || s.cfg.Admin.Password == "" {
		return false
	}
	return s.hasSession(r) || s.hasBasic(r)
}
