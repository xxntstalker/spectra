// Package k8s builds the service registry from Ingress objects.
//
// It talks to the API server over plain HTTP with the pod's ServiceAccount
// token instead of using client-go or shelling out to kubectl. Listing
// ingresses is one GET against a documented, stable API; client-go would add
// tens of megabytes of dependencies for it, and kubectl would mean abandoning
// the distroless image, which has no shell to run it from.
package k8s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"spectra/internal/config"
	"spectra/internal/store"
)

type Client struct {
	cfg  config.Kubernetes
	http *http.Client
}

func New(cfg config.Kubernetes) (*Client, error) {
	transport := &http.Transport{}
	if pem, err := os.ReadFile(cfg.CACertPath); err == nil {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(pem) {
			transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		}
	} else if !strings.HasPrefix(cfg.APIServer, "http://") {
		// Refuse to fall back to an unverified connection: the token would be
		// sent to whatever answered.
		return nil, fmt.Errorf("cannot read cluster CA at %s: %w", cfg.CACertPath, err)
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second, Transport: transport}}, nil
}

// Minimal shape of the Ingress list. Only the fields the portal reads are
// declared, so an API version bump adding fields changes nothing here.
type ingressList struct {
	Items []ingress `json:"items"`
}

type ingress struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
		Labels      map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		IngressClassName string `json:"ingressClassName"`
		TLS              []struct {
			Hosts []string `json:"hosts"`
		} `json:"tls"`
		Rules []struct {
			Host string `json:"host"`
			HTTP struct {
				Paths []struct {
					Path    string `json:"path"`
					Backend struct {
						Service struct {
							Name string `json:"name"`
							Port struct {
								Number int    `json:"number"`
								Name   string `json:"name"`
							} `json:"port"`
						} `json:"service"`
					} `json:"backend"`
				} `json:"paths"`
			} `json:"http"`
		} `json:"rules"`
	} `json:"spec"`
}

// Annotations a service owner can put on an Ingress to steer the portal
// without anyone touching the portal's own configuration.
const (
	annotationSkip    = "spectra.io/skip"
	annotationSpecURL = "spectra.io/spec-url"
	annotationName    = "spectra.io/name"
	annotationGroup   = "spectra.io/group"
	annotationTeam    = "spectra.io/team"
	annotationContact = "spectra.io/contact"
	annotationRepo    = "spectra.io/repo"
	annotationOwners  = "spectra.io/owners"
)

// Discover lists ingresses and turns them into registry candidates.
func (c *Client) Discover(ctx context.Context) ([]store.Discovered, error) {
	list, err := c.listIngresses(ctx)
	if err != nil {
		return nil, err
	}

	// Several ingresses routinely point at the same backend — a public host
	// and an internal one, or a second rule added for a migration. They are
	// one API, so they collapse to one entry.
	seen := map[string]candidate{}
	for _, ing := range list.Items {
		if c.skip(ing) {
			continue
		}
		for _, item := range c.candidates(ing) {
			existing, exists := seen[item.Key]
			// A name the service owner wrote down wins over one derived from
			// the Service name, whichever ingress happens to be read first.
			// Map iteration order is random, so without this the displayed
			// name would change between sweeps.
			if !exists || (item.explicitName && !existing.explicitName) {
				seen[item.Key] = item
			}
		}
	}

	out := make([]store.Discovered, 0, len(seen))
	for _, item := range seen {
		out = append(out, item.Discovered)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (c *Client) skip(ing ingress) bool {
	if ing.Metadata.Annotations[annotationSkip] == "true" {
		return true
	}
	for _, ns := range c.cfg.ExcludeNamespaces {
		if ing.Metadata.Namespace == ns {
			return true
		}
	}
	if len(c.cfg.Namespaces) > 0 && !contains(c.cfg.Namespaces, ing.Metadata.Namespace) {
		return true
	}
	if len(c.cfg.IngressClasses) > 0 && !contains(c.cfg.IngressClasses, ing.Spec.IngressClassName) {
		return true
	}
	// cert-manager creates a short-lived Ingress per certificate challenge.
	// They appear and vanish constantly and never serve an API.
	if strings.HasPrefix(ing.Metadata.Name, "cm-acme-http-solver") {
		return true
	}
	return false
}

// candidate is a discovered entry plus how confident we are in its name.
type candidate struct {
	store.Discovered
	explicitName bool
}

func (c *Client) candidates(ing ingress) []candidate {
	tlsHosts := map[string]bool{}
	for _, t := range ing.Spec.TLS {
		for _, h := range t.Hosts {
			tlsHosts[h] = true
		}
	}

	var out []candidate
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" || strings.HasPrefix(rule.Host, "*") {
			continue // a wildcard host is not an address we can call
		}
		for _, p := range rule.HTTP.Paths {
			if strings.Contains(p.Path, "acme-challenge") {
				continue
			}
			backendName := p.Backend.Service.Name
			if backendName == "" {
				continue
			}
			port := p.Backend.Service.Port.Number
			if port == 0 {
				port = 80
			}
			backend := fmt.Sprintf("%s.%s:%d", backendName, ing.Metadata.Namespace, port)

			baseURL := c.baseURL(rule.Host, tlsHosts[rule.Host], backend, p.Path)

			name := ing.Metadata.Annotations[annotationName]
			explicit := name != ""
			if !explicit {
				name = defaultName(backendName, rule.Host)
			}

			out = append(out, candidate{
				Discovered: store.Discovered{
					// Объявления владельца сервиса: он управляет тем, как его
					// сервис попадает в Spectra, не трогая её конфигурацию.
					// Путь в аннотации приводится к полному адресу — опрашивать
					// Spectra будет по адресу, который знает сама.
					SpecURL: absoluteSpecURL(baseURL, ing.Metadata.Annotations[annotationSpecURL]),
					Group:   ing.Metadata.Annotations[annotationGroup],
					// The key is the cluster address rather than the public
					// host: a hostname can move between ingresses, while the
					// backend service is what actually serves the API.
					Key:         backend + pathSuffix(p.Path),
					DisplayName: name,
					BaseURL:     baseURL,
					Host:        rule.Host,
					Namespace:   ing.Metadata.Namespace,
					IngressName: ing.Metadata.Name,
					Backend:     backend,
				},
				explicitName: explicit,
			})
		}
	}
	return out
}

// baseURL decides who the portal actually talks to.
//
// Going through the Ingress hostname from inside the cluster is unreliable:
// split-horizon DNS may not resolve it at all, and when it does the request
// usually leaves for the external load balancer and comes back in. Talking to
// the backend Service is a plain in-cluster call.
func (c *Client) baseURL(host string, secure bool, backend, path string) string {
	if c.cfg.Target == config.TargetService {
		return "http://" + backend + trimPathSuffix(path)
	}
	scheme := "http"
	if secure {
		scheme = "https"
	}
	return scheme + "://" + host + trimPathSuffix(path)
}

// trimPathSuffix turns an Ingress path into a base-URL prefix: "/" and regex
// captures used by rewrite rules carry no meaning for us.
func trimPathSuffix(path string) string {
	path = strings.TrimSuffix(path, "/*")
	path = strings.TrimSuffix(path, "(/|$)(.*)")
	path = strings.TrimRight(path, "/")
	if path == "" || strings.ContainsAny(path, "()[]*") {
		return ""
	}
	return path
}

func pathSuffix(path string) string {
	if p := trimPathSuffix(path); p != "" {
		return p
	}
	return ""
}

// absoluteSpecURL resolves a spec address from an annotation against the
// address the poller actually uses. Teams write a path; an absolute URL on a
// different host is ignored rather than followed — Spectra runs inside the
// cluster and must not be talked into fetching arbitrary addresses.
func absoluteSpecURL(baseURL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	target, err := base.Parse(raw)
	if err != nil || target.Host != base.Host {
		return ""
	}
	return target.String()
}

func defaultName(service, host string) string {
	if service != "" {
		return service
	}
	return host
}

func (c *Client) listIngresses(ctx context.Context) (*ingressList, error) {
	endpoint := strings.TrimRight(c.cfg.APIServer, "/") + "/apis/networking.k8s.io/v1/ingresses"
	if len(c.cfg.Namespaces) == 1 {
		endpoint = fmt.Sprintf("%s/apis/networking.k8s.io/v1/namespaces/%s/ingresses",
			strings.TrimRight(c.cfg.APIServer, "/"), url.PathEscape(c.cfg.Namespaces[0]))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list ingresses: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		reason := apiServerMessage(body)
		switch resp.StatusCode {
		case http.StatusForbidden:
			// The most likely failure in a real cluster, and the one an
			// operator can fix in a minute — so name the fix, not the status.
			return nil, fmt.Errorf("нет прав на чтение Ingress (HTTP 403). "+
				"Выдайте ServiceAccount право list на ingresses.networking.k8s.io "+
				"(ClusterRole, либо Role в нужных namespace + SPECTRA_K8S_NAMESPACES). "+
				"Ответ API-сервера: %s", reason)
		case http.StatusUnauthorized:
			return nil, fmt.Errorf("токен ServiceAccount не принят (HTTP 401). "+
				"Проверьте serviceAccountName в поде. Ответ API-сервера: %s", reason)
		default:
			return nil, fmt.Errorf("не удалось получить список Ingress: HTTP %d: %s",
				resp.StatusCode, reason)
		}
	}

	var list ingressList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode ingress list: %w", err)
	}
	return &list, nil
}

// splitList reads a comma-separated annotation value.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// apiServerMessage pulls the human-readable line out of a Kubernetes Status
// object. Dumping the raw JSON buries the one sentence that explains what
// happened under three levels of escaping.
func apiServerMessage(body []byte) string {
	var status struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &status); err == nil && status.Message != "" {
		return status.Message
	}
	return truncate(strings.TrimSpace(string(body)), 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
