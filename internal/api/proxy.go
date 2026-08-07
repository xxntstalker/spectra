package api

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// proxyHandler forwards "Try it out" requests from the viewer to the service.
//
// Targets are now looked up in the database rather than pre-built from a config
// file, because the registry itself is dynamic. The safety property is the same
// and it matters more than before: the path only selects a service the portal
// already knows about, and the address comes from that record — never from the
// request. A caller cannot ask the portal to fetch an arbitrary host.
func (s *Server) proxyHandler() http.Handler {
	transport := &http.Transport{ResponseHeaderTimeout: 30 * time.Second}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.HTTP.ProxyEnabled {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "proxying is disabled (SPECTRA_PROXY_ENABLED)",
			})
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad service id"})
			return
		}
		svc, err := s.store.GetService(r.Context(), id)
		if err != nil || svc == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "service not found"})
			return
		}
		target, err := url.Parse(svc.BaseURL)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "service address is unusable"})
			return
		}

		reverse := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.SetXForwarded()
				// The portal's own cookies mean nothing upstream and must not
				// leak across services.
				pr.Out.Header.Del("Cookie")
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				s.log.Warn("proxy error", "service", id, "path", r.URL.Path, "err", err)
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			},
			Transport: transport,
		}
		http.StripPrefix("/proxy/"+strconv.FormatInt(id, 10), reverse).ServeHTTP(w, r)
	})
}

var _ = strings.TrimPrefix
