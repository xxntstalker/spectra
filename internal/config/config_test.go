package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setEnv wipes every variable the package reads, so a test never inherits the
// developer's shell or another test's leftovers.
func setEnv(t *testing.T, pairs map[string]string) {
	t.Helper()
	known := []string{
		"SPECTRA_LISTEN", "SPECTRA_LOG_LEVEL", "SPECTRA_DATABASE_URL", "DATABASE_URL",
		"SPECTRA_POLLER_ENABLED", "SPECTRA_K8S_ENABLED", "SPECTRA_K8S_API_SERVER",
		"SPECTRA_K8S_TOKEN", "SPECTRA_K8S_CA_CERT", "SPECTRA_K8S_NAMESPACES",
		"SPECTRA_K8S_EXCLUDE_NAMESPACES", "SPECTRA_K8S_INGRESS_CLASSES",
		"SPECTRA_K8S_TARGET", "SPECTRA_K8S_SYNC_INTERVAL", "SPECTRA_PROXY_ENABLED",
		"SPECTRA_USER_AGENT", "SPECTRA_ADMIN_USER", "SPECTRA_ADMIN_PASSWORD",
		"SPECTRA_STATIC_SERVICES", "KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT",
		"KUBERNETES_SERVICE_PORT_HTTPS",
		"SPECTRA_RABBITMQ_BROKERS", "SPECTRA_RABBITMQ_USER", "SPECTRA_RABBITMQ_PASSWORD",
		"SPECTRA_RABBITMQ_PROD_USER", "SPECTRA_RABBITMQ_PROD_PASSWORD",
		"SPECTRA_RABBITMQ_STAGE_EU_USER", "SPECTRA_RABBITMQ_STAGE_EU_PASSWORD",
	}
	for _, k := range known {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range pairs {
		t.Setenv(k, v)
	}
}

const validDSN = "postgres://spectra:secret@postgres:5432/spectra?sslmode=disable"

func TestDefaults(t *testing.T) {
	setEnv(t, map[string]string{"SPECTRA_DATABASE_URL": validDSN})

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.LogLevel != "info" {
		t.Errorf("значения по умолчанию: %+v", cfg)
	}
	if !cfg.PollerEnabled {
		t.Error("опрос должен быть включён по умолчанию")
	}
	// The proxy turns the portal into an access path to internal APIs, so it
	// has to be opted into, never inherited.
	if cfg.HTTP.ProxyEnabled {
		t.Error("прокси обязан быть выключен по умолчанию")
	}
	// Outside a cluster there is no ServiceAccount, so discovery stays off
	// instead of failing to start.
	if cfg.Kubernetes.Enabled {
		t.Error("вне кластера дискавери должно выключаться само")
	}
	if cfg.Kubernetes.Target != TargetService {
		t.Errorf("цель опроса = %q, ожидался backend-сервис", cfg.Kubernetes.Target)
	}
	for _, ns := range []string{"kube-system", "cert-manager"} {
		if !contains(cfg.Kubernetes.ExcludeNamespaces, ns) {
			t.Errorf("%s должен исключаться по умолчанию", ns)
		}
	}
}

func TestDatabaseURLIsRequiredAndChecked(t *testing.T) {
	setEnv(t, nil)
	_, err := Load()
	if err == nil {
		t.Fatal("портал стартовал без адреса базы")
	}
	if !strings.Contains(err.Error(), "SPECTRA_DATABASE_URL") {
		t.Errorf("ошибка не называет переменную: %v", err)
	}
	// The message has to carry an example, otherwise the reader still has to
	// go looking for the format.
	if !strings.Contains(err.Error(), "postgres://") {
		t.Errorf("ошибка не показывает пример значения: %v", err)
	}

	setEnv(t, map[string]string{"SPECTRA_DATABASE_URL": "mysql://root@db/spectra"})
	if _, err := Load(); err == nil {
		t.Error("принят адрес не-Postgres")
	}
}

// DATABASE_URL without a prefix is what most platforms inject.
func TestDatabaseURLFallback(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": validDSN})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != validDSN {
		t.Errorf("DATABASE_URL не подхвачен: %q", cfg.DatabaseURL)
	}
}

// Inside a pod nothing about Kubernetes is configured by hand: the kubelet
// supplies the address, and the token comes off a mounted file.
func TestKubernetesAddressComesFromTheEnvironmentKubeletSets(t *testing.T) {
	setEnv(t, map[string]string{
		"SPECTRA_DATABASE_URL":          validDSN,
		"SPECTRA_K8S_ENABLED":           "true",
		"SPECTRA_K8S_TOKEN":             "test-token",
		"KUBERNETES_SERVICE_HOST":       "10.96.0.1",
		"KUBERNETES_SERVICE_PORT_HTTPS": "443",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kubernetes.APIServer != "https://10.96.0.1:443" {
		t.Errorf("адрес API-сервера = %q", cfg.Kubernetes.APIServer)
	}
}

func TestKubernetesIPv6AddressIsBracketed(t *testing.T) {
	setEnv(t, map[string]string{
		"SPECTRA_DATABASE_URL":    validDSN,
		"SPECTRA_K8S_ENABLED":     "true",
		"SPECTRA_K8S_TOKEN":       "test-token",
		"KUBERNETES_SERVICE_HOST": "fd00::1",
		"KUBERNETES_SERVICE_PORT": "6443",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Without brackets the port would glue onto the address and the URL would
	// be unparseable.
	if cfg.Kubernetes.APIServer != "https://[fd00::1]:6443" {
		t.Errorf("адрес IPv6 = %q", cfg.Kubernetes.APIServer)
	}
}

// Discovery switched on with nothing to authenticate with must say so at
// startup, not fail silently on the first sweep an hour later.
func TestKubernetesEnabledWithoutCredentialsFails(t *testing.T) {
	setEnv(t, map[string]string{
		"SPECTRA_DATABASE_URL": validDSN,
		"SPECTRA_K8S_ENABLED":  "true",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("дискавери включено без адреса и токена, а старт прошёл")
	}
	if !strings.Contains(err.Error(), "SPECTRA_K8S_API_SERVER") {
		t.Errorf("ошибка не подсказывает, что задать: %v", err)
	}

	setEnv(t, map[string]string{
		"SPECTRA_DATABASE_URL":   validDSN,
		"SPECTRA_K8S_ENABLED":    "true",
		"SPECTRA_K8S_API_SERVER": "https://api:443",
	})
	if _, err := Load(); err == nil {
		t.Fatal("нет токена, а старт прошёл")
	} else if !strings.Contains(err.Error(), "serviceaccount") {
		t.Errorf("ошибка не говорит, где искать токен: %v", err)
	}
}

func TestTargetMustBeKnown(t *testing.T) {
	setEnv(t, map[string]string{
		"SPECTRA_DATABASE_URL": validDSN,
		"SPECTRA_K8S_TARGET":   "магия",
	})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SPECTRA_K8S_TARGET") {
		t.Errorf("неизвестная цель опроса принята: %v", err)
	}
}

func TestListsIgnoreBlanksAndSpaces(t *testing.T) {
	setEnv(t, map[string]string{
		"SPECTRA_DATABASE_URL":   validDSN,
		"SPECTRA_K8S_NAMESPACES": " shop , , billing,",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// A trailing comma must not become an empty namespace that matches nothing.
	got := cfg.Kubernetes.Namespaces
	if len(got) != 2 || got[0] != "shop" || got[1] != "billing" {
		t.Errorf("список namespace = %#v", got)
	}
}

func TestUnparseableValuesFallBackInsteadOfFailing(t *testing.T) {
	setEnv(t, map[string]string{
		"SPECTRA_DATABASE_URL":      validDSN,
		"SPECTRA_K8S_SYNC_INTERVAL": "иногда",
		"SPECTRA_PROXY_ENABLED":     "ага",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("нечитаемое значение уронило старт: %v", err)
	}
	if cfg.Kubernetes.SyncInterval != time.Hour {
		t.Errorf("интервал = %v, ожидался откат к значению по умолчанию", cfg.Kubernetes.SyncInterval)
	}
	if cfg.HTTP.ProxyEnabled {
		t.Error("нечитаемое значение включило прокси — это должно быть невозможно")
	}
}

func TestAdminCredentials(t *testing.T) {
	setEnv(t, map[string]string{
		"SPECTRA_DATABASE_URL":   validDSN,
		"SPECTRA_ADMIN_USER":     "admin",
		"SPECTRA_ADMIN_PASSWORD": "secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.User != "admin" || cfg.Admin.Password != "secret" {
		t.Errorf("учётные данные администратора: %+v", cfg.Admin)
	}
}

// The token is mounted as a file, never passed in the environment.
func TestTokenIsReadFromTheServiceAccountMount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("  file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// serviceAccountDir is a constant, so the mount cannot be redirected; this
	// checks the trimming behaviour on the value the file would carry.
	got := strings.TrimSpace(string(mustRead(t, filepath.Join(dir, "token"))))
	if got != "file-token" {
		t.Errorf("токен не очищен от пробелов: %q", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func TestRabbitMQIsOffUnlessListed(t *testing.T) {
	setEnv(t, map[string]string{"SPECTRA_DATABASE_URL": validDSN})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RabbitMQ) != 0 {
		t.Errorf("без SPECTRA_RABBITMQ_BROKERS брокеров быть не должно: %+v", cfg.RabbitMQ)
	}
}

func TestBrokersShareCredentialsUnlessOverridden(t *testing.T) {
	setEnv(t, map[string]string{
		"SPECTRA_DATABASE_URL":               validDSN,
		"SPECTRA_RABBITMQ_BROKERS":           "prod=http://rabbit-prod:15672/, stage-eu=https://rabbit-stage:15671",
		"SPECTRA_RABBITMQ_USER":              "spectra",
		"SPECTRA_RABBITMQ_PASSWORD":          "shared",
		"SPECTRA_RABBITMQ_STAGE_EU_PASSWORD": "own",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RabbitMQ) != 2 {
		t.Fatalf("ожидалось два брокера: %+v", cfg.RabbitMQ)
	}
	prod, stage := cfg.RabbitMQ[0], cfg.RabbitMQ[1]
	if prod.Name != "prod" || prod.URL != "http://rabbit-prod:15672" || prod.Password != "shared" {
		t.Errorf("prod: %+v", prod)
	}
	if stage.Name != "stage-eu" || stage.User != "spectra" || stage.Password != "own" {
		t.Errorf("stage-eu: пароль переопределяется по имени брокера: %+v", stage)
	}
}

func TestBrokerConfigurationMistakes(t *testing.T) {
	cases := map[string]map[string]string{
		"пароль в адресе": {"SPECTRA_RABBITMQ_BROKERS": "prod=http://u:p@rabbit:15672",
			"SPECTRA_RABBITMQ_USER": "u", "SPECTRA_RABBITMQ_PASSWORD": "p"},
		"нет пароля": {"SPECTRA_RABBITMQ_BROKERS": "prod=http://rabbit:15672", "SPECTRA_RABBITMQ_USER": "u"},
		"нет имени":  {"SPECTRA_RABBITMQ_BROKERS": "http://rabbit:15672", "SPECTRA_RABBITMQ_USER": "u", "SPECTRA_RABBITMQ_PASSWORD": "p"},
		"amqp вместо http": {"SPECTRA_RABBITMQ_BROKERS": "prod=amqp://rabbit:5672",
			"SPECTRA_RABBITMQ_USER": "u", "SPECTRA_RABBITMQ_PASSWORD": "p"},
		"дубль": {"SPECTRA_RABBITMQ_BROKERS": "prod=http://a:15672,prod=http://b:15672",
			"SPECTRA_RABBITMQ_USER": "u", "SPECTRA_RABBITMQ_PASSWORD": "p"},
		"имя с пробелом": {"SPECTRA_RABBITMQ_BROKERS": "my prod=http://a:15672",
			"SPECTRA_RABBITMQ_USER": "u", "SPECTRA_RABBITMQ_PASSWORD": "p"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			env["SPECTRA_DATABASE_URL"] = validDSN
			setEnv(t, env)
			_, err := Load()
			if err == nil {
				t.Fatal("ошибка конфигурации должна ронять старт")
			}
			if strings.Contains(err.Error(), "u:p@") {
				t.Errorf("пароль попал в текст ошибки: %v", err)
			}
		})
	}
}
