package discovery

import "testing"

const (
	insideCluster = "http://orders.shop:8080"
	publicHost    = "orders.example.com"
)

// Случай с эксплуатации: спека отдаётся только через ingress. Человек вводит
// публичный адрес, а портал возвращал внутренний — и потом не мог по нему
// ничего забрать. Адрес, указанный целиком, должен сохраняться как есть.
func TestTypedHostIsKept(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"публичный адрес целиком",
			"https://orders.example.com/swagger/v1/swagger.json",
			"https://orders.example.com/swagger/v1/swagger.json"},
		{"публичный по http",
			"http://orders.example.com/docs.json",
			"http://orders.example.com/docs.json"},
		{"внутренний адрес целиком",
			"http://orders.shop:8080/v3/api-docs",
			"http://orders.shop:8080/v3/api-docs"},
		{"с параметром запроса",
			"https://orders.example.com/docs?format=json",
			"https://orders.example.com/docs?format=json"},
		// Голый путь хоста не несёт: достраивается по адресу опроса — это и
		// есть обычный случай, ради которого поле принимает просто путь.
		{"просто путь", "/v3/api-docs", "http://orders.shop:8080/v3/api-docs"},
		{"путь без ведущего слэша", "openapi.json", "http://orders.shop:8080/openapi.json"},
		{"пусто — снять адрес", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveManualSpecURL(insideCluster, publicHost, c.raw)
			if err != nil {
				t.Fatalf("%q отвергнут: %v", c.raw, err)
			}
			if got != c.want {
				t.Errorf("%q → %q, ожидалось %q", c.raw, got, c.want)
			}
		})
	}
}

// Ограничение остаётся: портал ходит изнутри кластера, и произвольный адрес
// здесь стал бы способом дотянуться до чего угодно.
func TestForeignHostsAreStillRefused(t *testing.T) {
	for _, raw := range []string{
		"http://evil.example.com/openapi.json",
		"https://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/api/settings",
		"ftp://orders.example.com/spec",
		"http://orders.example.com.evil.net/spec",
	} {
		if got, err := ResolveManualSpecURL(insideCluster, publicHost, raw); err == nil {
			t.Errorf("%q принят и превращён в %q", raw, got)
		}
	}
}

// Ошибка должна называть адрес, на котором можно писать, иначе человеку нечего
// с ней делать.
func TestRefusalNamesTheAddressYouMayUse(t *testing.T) {
	_, err := ResolveManualSpecURL(insideCluster, publicHost, "http://elsewhere.internal/spec")
	if err == nil {
		t.Fatal("чужой адрес принят")
	}
	if !contains(err.Error(), publicHost) {
		t.Errorf("в ошибке нет адреса, который можно указать: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
