package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Sessions exist because a login has to survive opening a second tab.
//
// The first version kept the credentials in the page's sessionStorage. That is
// per-tab by definition: a new tab, and often a duplicated one, started logged
// out, and the operator retyped the password several times a day. Moving them
// to localStorage would have fixed the symptom by writing an admin password to
// disk in a form any script on the page can read — a worse trade for a
// credential that guards every write.
//
// So the credentials are exchanged once for a cookie the page cannot read and
// does not have to store. The browser shares it across tabs on its own, which
// is the behaviour that was missing.
//
// The token carries only an expiry and a signature over it. There is no server
// state, so it works with several replicas and survives a restart. The signing
// key is derived from the configured credentials, so changing the admin
// password invalidates every session that was issued under the old one.
const sessionCookie = "spectra_session"

// sessionTTL is deliberately long enough to cover a working day: this guards a
// read-only-by-default portal on an internal network, and an expiry that
// interrupts someone mid-edit buys nothing.
const sessionTTL = 12 * time.Hour

func (s *Server) sessionKey() []byte {
	// The label keeps this key from being usable anywhere else the same secret
	// might be hashed.
	mac := hmac.New(sha256.New, []byte(s.cfg.Admin.Password))
	mac.Write([]byte("spectra/session/v1\x00" + s.cfg.Admin.User))
	return mac.Sum(nil)
}

func (s *Server) issueSession(expires time.Time) string {
	payload := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, s.sessionKey())
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validSession(token string) bool {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || time.Now().After(time.Unix(expires, 0)) {
		return false
	}
	want := hmac.New(sha256.New, s.sessionKey())
	want.Write([]byte(payload))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	// hmac.Equal is constant time, so a forged signature cannot be searched for
	// by measuring how far the comparison got.
	return hmac.Equal(got, want.Sum(nil))
}

func (s *Server) hasSession(r *http.Request) bool {
	if s.cfg.Admin.User == "" || s.cfg.Admin.Password == "" {
		return false
	}
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.validSession(c.Value)
}

// login exchanges the credentials for a session cookie.
//
// SameSite=Strict is what stands in for a CSRF token here: the cookie is simply
// not sent on a request that originates anywhere else, so a form on another
// site cannot make a write on the operator's behalf. HttpOnly keeps it out of
// reach of scripts, including anything injected into a specification the portal
// renders.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	expires := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.issueSession(expires),
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Only over HTTPS when the request arrived over HTTPS. Marking it
		// secure on a plain-HTTP deployment would mean the browser silently
		// drops it and the login appears to do nothing.
		Secure: isHTTPS(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"admin": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"admin": false})
}

// isHTTPS reports whether the browser reached the portal over TLS. Behind an
// ingress that terminates TLS the connection here is plain, and the forwarded
// header is the only thing that still knows.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
