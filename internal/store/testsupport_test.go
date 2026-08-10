package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests need a real Postgres: half of what this package does lives in SQL —
// upserts that must not clobber human edits, the per-service revision counter,
// advisory locks. A fake would test the fake.
//
//	make test           поднимет одноразовый Postgres сам
//	SPECTRA_TEST_DATABASE_URL=postgres://... go test ./...
//
// Without the variable these tests skip rather than fail, so `go test ./...`
// still works on a machine with no Docker.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SPECTRA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SPECTRA_TEST_DATABASE_URL не задан — пропускаю тесты, которым нужен Postgres")
	}
	return dsn
}

var schemaCounter atomic.Int64

// newStore opens a store in its own Postgres schema, so tests neither see each
// other's rows nor need cleaning up between runs.
func newStore(t *testing.T) *Store {
	t.Helper()
	base := testDatabaseURL(t)

	schema := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), schemaCounter.Add(1))
	dsn := base
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "search_path=" + schema

	ctx := context.Background()
	bootstrap, err := Open(ctx, base)
	if err != nil {
		t.Fatalf("подключение к тестовой базе: %v", err)
	}
	if _, err := bootstrap.pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		bootstrap.Close()
		t.Fatalf("создание схемы: %v", err)
	}
	bootstrap.Close()

	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("открытие store: %v", err)
	}
	t.Cleanup(func() {
		drop, err := Open(context.Background(), base)
		if err == nil {
			drop.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			drop.Close()
		}
		s.Close()
	})
	return s
}

func seed(t *testing.T, s *Store, names ...string) map[string]int64 {
	t.Helper()
	items := make([]Discovered, 0, len(names))
	for _, n := range names {
		items = append(items, Discovered{
			Key: n + ".ns:80", DisplayName: n, BaseURL: "http://" + n + ".ns:80",
			Host: n + ".example.com", Namespace: "ns", Backend: n + ".ns:80",
		})
	}
	if _, err := s.SyncDiscovered(context.Background(), "kubernetes", items); err != nil {
		t.Fatalf("наполнение реестра: %v", err)
	}

	ids := map[string]int64{}
	services, err := s.ListServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range services {
		ids[svc.DisplayName] = svc.ID
	}
	return ids
}
