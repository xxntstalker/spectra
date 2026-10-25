package rabbitmq

import (
	"strings"
	"testing"
	"time"
)

func defsOf(t *testing.T, mutate func(*Raw)) *Definitions {
	t.Helper()
	raw := fixture()
	if mutate != nil {
		mutate(raw)
	}
	return DefinitionsOf(Build("prod", raw, nil, time.Now()))
}

func changeOf(changes []Change, objectKind, object string) *Change {
	for i := range changes {
		if changes[i].ObjectKind == objectKind && changes[i].Object == object {
			return &changes[i]
		}
	}
	return nil
}

func TestOrderAndScalingAreNotChanges(t *testing.T) {
	before := defsOf(t, nil)
	after := defsOf(t, func(r *Raw) {
		// The broker lists things in whatever order it likes.
		r.Queues[0], r.Queues[len(r.Queues)-1] = r.Queues[len(r.Queues)-1], r.Queues[0]
		r.Bindings[1], r.Bindings[2] = r.Bindings[2], r.Bindings[1]
		// Scaling a consumer is not a topology change either.
		r.Consumers = append(r.Consumers, consumer("documents", "documents.convert", "conv"))
		// Nor is a fresh reply queue.
		r.Queues = append(r.Queues, rawQueue{Name: "amq.gen-Yy7", Vhost: "documents", Exclusive: true})
	})
	if before.Hash() != after.Hash() {
		t.Fatalf("хэш поменялся без изменения топологии: %v", Diff(before, after))
	}
}

// Подписчик, которого не застал снимок, — не изменение топологии: задача,
// которая забирает очередь раз в сутки, иначе давала бы по записи на каждый
// снимок.
func TestSubscribersComingAndGoingAreNotHistory(t *testing.T) {
	before := defsOf(t, nil)
	after := defsOf(t, func(r *Raw) {
		r.Consumers = nil
	})
	if before.Hash() != after.Hash() {
		t.Fatalf("пропавшие подписчики попали в историю: %+v", Diff(before, after))
	}
}

func TestRemovedBindingIsBreaking(t *testing.T) {
	before := defsOf(t, nil)
	after := defsOf(t, func(r *Raw) {
		var kept []rawBinding
		for _, b := range r.Bindings {
			if b.RoutingKey != "document.signed" {
				kept = append(kept, b)
			}
		}
		r.Bindings = kept
	})
	c := changeOf(Diff(before, after), "binding", "documents.notify")
	if c == nil || c.Kind != "removed" || c.Severity != SeverityBreaking {
		t.Fatalf("удалённая привязка — ломающее изменение: %+v", c)
	}
}

func TestPolicyEditShowsWhatChanged(t *testing.T) {
	before := defsOf(t, nil)
	after := defsOf(t, func(r *Raw) {
		r.Policies[0].Definition = map[string]any{"dead-letter-exchange": "documents.dlx", "delivery-limit": float64(10)}
	})
	changes := Diff(before, after)
	p := changeOf(changes, "policy", "dlx-default")
	if p == nil || p.Severity != SeverityAttention || !strings.Contains(strings.Join(p.Lines, ";"), "delivery-limit: 5 → 10") {
		t.Fatalf("правка политики: %+v", p)
	}
	// The queues the policy applies to change too — and say where from.
	q := changeOf(changes, "queue", "documents.convert")
	if q == nil || !strings.Contains(strings.Join(q.Lines, ";"), "политика dlx-default") {
		t.Errorf("у очереди не видно, что параметр пришёл из политики: %+v", q)
	}
}

func TestLostPermissionIsNotMassDeletion(t *testing.T) {
	before := defsOf(t, nil)
	after := defsOf(t, func(r *Raw) {
		var kept []rawExchange
		for _, x := range r.Exchanges {
			if x.Vhost != "documents" {
				kept = append(kept, x)
			}
		}
		r.Exchanges = kept
		var queues []rawQueue
		for _, q := range r.Queues {
			if q.Vhost != "documents" {
				queues = append(queues, q)
			}
		}
		r.Queues = queues
	})
	changes := Diff(before, after)
	if len(changes) != 1 || changes[0].ObjectKind != "vhost" || changes[0].Severity != SeverityAttention {
		t.Fatalf("потеря прав должна быть одной записью про vhost, а не удалением всего: %+v", changes)
	}
}

func TestFirstSnapshotIsOneLine(t *testing.T) {
	changes := Diff(nil, defsOf(t, nil))
	if len(changes) != 1 || changes[0].ObjectKind != "cluster" {
		t.Fatalf("первый снимок — одна строка, а не появление каждого объекта: %+v", changes)
	}
}

func TestPublishersAreNotHistory(t *testing.T) {
	// Switching rates_mode on, or a publisher seen this hour and not the next,
	// is not a topology change.
	before := defsOf(t, nil)
	after := defsOf(t, func(r *Raw) {
		r.Incoming = map[string][]rawChannelStat{"documents\x00documents": {{ChannelDetails: rawChannelDetails{ConnectionName: "conv"}}}}
		r.Incoming["documents\x00documents"][0].Stats.Publish = 3
	})
	if before.Hash() != after.Hash() {
		t.Errorf("публикующие попали в историю: %+v", Diff(before, after))
	}
}
