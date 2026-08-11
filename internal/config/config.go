// Package config reads boot configuration from the environment.
//
// The split is deliberate. Here lives everything the process needs before it
// can do anything at all: where to listen, how to reach the database, how to
// reach Kubernetes. Everything an operator changes while the portal runs —
// poll interval, timeouts, retention, per-service overrides — lives in the
// database and is edited in the UI, because a settings page and an immutable
// environment cannot both own the same value.
//
// The result is a stateless process: two replicas with the same environment
// behave identically, and all state is in Postgres.
package config

import (
	"crypto/x509"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen      string
	LogLevel    string
	DatabaseURL string

	// PollerEnabled lets a replica serve the UI without polling. The lock in
	// the collector already guarantees a single active poller, so this is for
	// deliberately splitting read and write workloads.
	PollerEnabled bool

	Kubernetes Kubernetes
	HTTP       HTTP
	Admin      Admin

	// StaticServices is a comma-separated list of base URLs seeded into the
	// registry at startup. Kubernetes discovery is the intended source, but
	// without this there is no way to run the portal outside a cluster at all
	// — not for a local trial, and not for the handful of services that live
	// somewhere else.
	StaticServices []string
}

// Kubernetes describes where the service list comes from.
type Kubernetes struct {
	// Enabled turns Ingress discovery on. Defaults to true when the process
	// looks like it is running inside a cluster.
	Enabled bool

	// APIServer, Token and CACert are read from the ServiceAccount mount by
	// default; the overrides exist so the discovery can be pointed at a
	// cluster from a laptop during development.
	APIServer  string
	Token      string
	CACertPath string

	// Namespaces limits the sweep. Empty means every namespace.
	Namespaces        []string
	ExcludeNamespaces []string
	// IngressClasses limits which ingresses count. Empty means all.
	IngressClasses []string

	// Target decides who the portal talks to for each Ingress:
	//
	//	"service" — the backend Service inside the cluster.
	//	"ingress" — the public hostname from the Ingress rule.
	//
	// Service is the default because an Ingress hostname often does not
	// resolve from inside the cluster, or resolves to the external load
	// balancer and hairpins back in.
	Target string

	SyncInterval time.Duration
}

// Admin holds the credentials that guard the settings page and every write.
// Reading the host list and the specs needs no account — the portal is reached
// over the corporate network and looking up an API should not require a login.
type Admin struct {
	User     string
	Password string
}

// HTTP covers how the portal talks to the services it watches.
type HTTP struct {
	// ProxyEnabled turns on "Try it out" through the portal. Off by default:
	// with it on, the portal becomes an access path to internal APIs and has
	// no authentication of its own.
	ProxyEnabled bool
	UserAgent    string

	// TLSSkipVerify отключает проверку сертификатов у опрашиваемых сервисов.
	// Нужен там, где внутренние сервисы стоят за самоподписанными
	// сертификатами. К Kubernetes не относится — там свой CA, и его проверка
	// не ослабляется никогда.
	TLSSkipVerify bool
	// CABundle — путь к дополнительным корневым сертификатам. Способ лучше
	// предыдущего: проверка остаётся, просто портал знает, кому верить.
	CABundle string
	caPool   *x509.CertPool
}

const (
	TargetService = "service"
	TargetIngress = "ingress"
)

// serviceAccountDir is where Kubernetes mounts the pod's credentials.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

func Load() (*Config, error) {
	c := &Config{
		Listen:        env("SPECTRA_LISTEN", ":8080"),
		LogLevel:      env("SPECTRA_LOG_LEVEL", "info"),
		DatabaseURL:   firstEnv("SPECTRA_DATABASE_URL", "DATABASE_URL"),
		PollerEnabled: envBool("SPECTRA_POLLER_ENABLED", true),
		Kubernetes: Kubernetes{
			Enabled:    envBool("SPECTRA_K8S_ENABLED", inCluster()),
			APIServer:  env("SPECTRA_K8S_API_SERVER", apiServerFromEnv()),
			Token:      env("SPECTRA_K8S_TOKEN", ""),
			CACertPath: env("SPECTRA_K8S_CA_CERT", serviceAccountDir+"/ca.crt"),
			Namespaces: envList("SPECTRA_K8S_NAMESPACES", nil),
			// The defaults are the namespaces that never host a documented
			// API but do host ingresses — cert-manager's ACME solvers in
			// particular would otherwise be probed on every sweep.
			ExcludeNamespaces: envList("SPECTRA_K8S_EXCLUDE_NAMESPACES",
				[]string{"kube-system", "kube-public", "kube-node-lease", "cert-manager"}),
			IngressClasses: envList("SPECTRA_K8S_INGRESS_CLASSES", nil),
			Target:         env("SPECTRA_K8S_TARGET", TargetService),
			SyncInterval:   envDuration("SPECTRA_K8S_SYNC_INTERVAL", time.Hour),
		},
		HTTP: HTTP{
			ProxyEnabled:  envBool("SPECTRA_PROXY_ENABLED", false),
			UserAgent:     env("SPECTRA_USER_AGENT", "spectra/2.0"),
			TLSSkipVerify: envBool("SPECTRA_TLS_SKIP_VERIFY", false),
			CABundle:      env("SPECTRA_CA_BUNDLE", ""),
		},
		Admin: Admin{
			User:     env("SPECTRA_ADMIN_USER", ""),
			Password: env("SPECTRA_ADMIN_PASSWORD", ""),
		},
		StaticServices: envList("SPECTRA_STATIC_SERVICES", nil),
	}

	// Сертификаты читаются на старте: неверный путь должен ронять запуск
	// сразу, а не всплывать на первом опросе через час.
	if c.HTTP.CABundle != "" {
		pool, err := loadCABundle(c.HTTP.CABundle)
		if err != nil {
			return nil, err
		}
		c.HTTP.caPool = pool
	}

	if c.Kubernetes.Enabled && c.Kubernetes.Token == "" {
		if raw, err := os.ReadFile(serviceAccountDir + "/token"); err == nil {
			c.Kubernetes.Token = strings.TrimSpace(string(raw))
		}
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("SPECTRA_DATABASE_URL (or DATABASE_URL) is required, " +
			"for example postgres://portal:secret@postgres:5432/portal?sslmode=disable")
	}
	if !strings.HasPrefix(c.DatabaseURL, "postgres://") && !strings.HasPrefix(c.DatabaseURL, "postgresql://") {
		return fmt.Errorf("SPECTRA_DATABASE_URL must be a postgres:// URL")
	}
	switch c.Kubernetes.Target {
	case TargetService, TargetIngress:
	default:
		return fmt.Errorf("SPECTRA_K8S_TARGET must be %q or %q, got %q",
			TargetService, TargetIngress, c.Kubernetes.Target)
	}
	if c.Kubernetes.Enabled {
		if c.Kubernetes.APIServer == "" {
			return fmt.Errorf("SPECTRA_K8S_ENABLED is on but the API server address is unknown; " +
				"set SPECTRA_K8S_API_SERVER when running outside a cluster")
		}
		if c.Kubernetes.Token == "" {
			return fmt.Errorf("SPECTRA_K8S_ENABLED is on but no ServiceAccount token was found at %s/token; "+
				"set SPECTRA_K8S_TOKEN when running outside a cluster", serviceAccountDir)
		}
	}
	return nil
}

// inCluster reports whether the process looks like a pod, so that discovery
// switches itself on in production and stays off on a laptop.
func inCluster() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		return false
	}
	_, err := os.Stat(serviceAccountDir + "/token")
	return err == nil
}

func apiServerFromEnv() string {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host == "" {
		return ""
	}
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	if port == "" {
		port = "443"
	}
	// An IPv6 address has to be bracketed before it can carry a port.
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return "https://" + host + ":" + port
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// envList reads a comma-separated value, trimming blanks so that a trailing
// comma or a stray space does not become an empty entry that matches nothing.
func envList(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
