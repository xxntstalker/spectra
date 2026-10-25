package rabbitmq

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"spectra/internal/config"
)

// TestLiveBroker reads a real broker. The shapes of the management API —
// which fields come back with disable_stats, what "incoming" holds in the
// default rates mode — were wrong in the first draft of this package, and
// only a real broker showed it. Off unless pointed at one:
//
//	SPECTRA_TEST_RABBITMQ_URL=http://localhost:15672 \
//	SPECTRA_TEST_RABBITMQ_USER=spectra SPECTRA_TEST_RABBITMQ_PASSWORD=spectra \
//	go test ./internal/rabbitmq -run Live -v
func TestLiveBroker(t *testing.T) {
	endpoint := os.Getenv("SPECTRA_TEST_RABBITMQ_URL")
	if endpoint == "" {
		t.Skip("SPECTRA_TEST_RABBITMQ_URL не задан — пропускаю проверку на живом брокере")
	}
	b := config.Broker{
		Name: "live", URL: endpoint,
		User: os.Getenv("SPECTRA_TEST_RABBITMQ_USER"), Password: os.Getenv("SPECTRA_TEST_RABBITMQ_PASSWORD"),
	}
	raw, err := NewClient(b, http.DefaultTransport, 10*time.Second).Fetch(context.Background())
	if err != nil {
		t.Fatalf("снимок не снялся: %v", err)
	}
	snap := Build("live", raw, nil, time.Now())
	if len(snap.Vhosts) == 0 {
		t.Fatal("брокер не вернул ни одного vhost")
	}
	body, _ := json.MarshalIndent(snap, "", "  ")
	t.Logf("снимок:\n%s", body)
	defs := DefinitionsOf(snap)
	t.Logf("хэш: %s; изменения от пустого: %v", defs.Hash(), Diff(nil, defs))
}
