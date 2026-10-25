package store

import (
	"context"
	"testing"
	"time"
)

func TestFailedBrokerPollKeepsTheLastSnapshot(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)

	if err := s.SaveMQSnapshot(ctx, "prod", []byte(`{"cluster":"prod"}`), []byte(`{}`), "h1", at, []MQChange{
		{ObjectKind: "cluster", Kind: "added", Severity: "info", Lines: []string{"первый снимок"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMQFailure(ctx, "prod", "брокер не отвечает", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	st, err := s.MQState(ctx, "prod")
	if err != nil || st == nil {
		t.Fatalf("состояние: %v", err)
	}
	// Брокер, который ненадолго недоступен, не потерял свои очереди.
	if st.Snapshot != `{"cluster":"prod"}` || st.Hash != "h1" {
		t.Errorf("неудачный опрос стёр снимок: %+v", st)
	}
	if st.LastError == "" || !st.LastSuccessAt.Equal(at) || !st.LastCheckedAt.After(at) {
		t.Errorf("ошибка и время проверки записаны неверно: %+v", st)
	}

	if err := s.SaveMQSnapshot(ctx, "prod", []byte(`{"cluster":"prod","v":2}`), []byte(`{}`), "h1", at.Add(2*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	st, _ = s.MQState(ctx, "prod")
	if st.LastError != "" {
		t.Errorf("успешный опрос должен снимать ошибку: %q", st.LastError)
	}
	changes, err := s.ListMQChanges(ctx, MQChangeFilter{Cluster: "prod"})
	if err != nil || len(changes) != 1 {
		t.Fatalf("снимок без изменений не должен добавлять записей: %v %+v", err, changes)
	}
}

func TestBrokerFirstPollFailingStillShowsUp(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.SaveMQFailure(ctx, "stage", "401", time.Now()); err != nil {
		t.Fatal(err)
	}
	states, err := s.ListMQStates(ctx)
	if err != nil || len(states) != 1 || states[0].Snapshot != "" || states[0].LastError != "401" {
		t.Fatalf("брокер, ни разу не ответивший, должен быть виден с ошибкой: %v %+v", err, states)
	}
}

func TestQueueHistoryIncludesItsBindings(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	at := time.Now().UTC()
	rows := []MQChange{
		{Vhost: "documents", ObjectKind: "queue", Object: "documents.notify", Kind: "consumers", Severity: "attention"},
		{Vhost: "documents", ObjectKind: "binding", Object: "documents.notify", Kind: "removed", Severity: "breaking"},
		{Vhost: "documents", ObjectKind: "exchange", Object: "documents.notify", Kind: "added", Severity: "info"},
		{Vhost: "other", ObjectKind: "queue", Object: "documents.notify", Kind: "added", Severity: "info"},
	}
	if err := s.SaveMQSnapshot(ctx, "prod", []byte(`{}`), []byte(`{}`), "h", at, rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListMQChanges(ctx, MQChangeFilter{
		Cluster: "prod", Vhost: "documents", Object: "documents.notify", Kinds: []string{"queue", "binding"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("история очереди — её изменения и привязок к ней, только в её vhost: %+v", got)
	}
	for _, c := range got {
		if c.ObjectKind == "exchange" || c.Vhost != "documents" {
			t.Errorf("лишняя запись: %+v", c)
		}
	}
}
