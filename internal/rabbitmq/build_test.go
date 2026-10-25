package rabbitmq

import (
	"strings"
	"testing"
	"time"
)

// fixture mirrors what a 3.13 broker returned for the topology the live test
// was run against: two readable vhosts and one without permissions, a DLX set
// by a policy, a DLX to the default exchange set by arguments, a named and an
// unnamed connection.
func fixture() *Raw {
	return &Raw{
		Overview: rawOverview{RabbitMQVersion: "3.13.7", ClusterName: "rabbit@prod"},
		Vhosts:   []rawVhost{{Name: "/"}, {Name: "documents"}, {Name: "payments"}},
		Exchanges: []rawExchange{
			{Name: "", Vhost: "/", Type: "direct"},
			{Name: "amq.direct", Vhost: "/", Type: "direct"},
			{Name: "notifications", Vhost: "/", Type: "direct", Durable: true},
			{Name: "", Vhost: "documents", Type: "direct"},
			{Name: "documents", Vhost: "documents", Type: "topic", Durable: true},
			{Name: "documents.dlx", Vhost: "documents", Type: "fanout", Durable: true},
			{Name: "legacy.events", Vhost: "documents", Type: "fanout", Durable: true},
		},
		Queues: []rawQueue{
			{Name: "notifications.email", Vhost: "/", Type: "classic", Durable: true, Arguments: map[string]any{
				"x-dead-letter-exchange": "", "x-dead-letter-routing-key": "notifications.email.dlq", "x-message-ttl": float64(60000)}},
			{Name: "notifications.email.dlq", Vhost: "/", Type: "classic", Durable: true},
			{Name: "documents.convert", Vhost: "documents", Type: "quorum", Durable: true, Policy: "dlx-default",
				Arguments: map[string]any{"x-queue-type": "quorum"}},
			{Name: "documents.notify", Vhost: "documents", Type: "quorum", Durable: true, Policy: "dlx-default"},
			{Name: "documents.index", Vhost: "documents", Type: "classic", Durable: true},
			{Name: "documents.dlq", Vhost: "documents", Type: "classic", Durable: true},
			{Name: "amq.gen-Xk2", Vhost: "documents", Type: "classic", Exclusive: true, AutoDelete: true},
		},
		Bindings: []rawBinding{
			{Source: "", Vhost: "documents", Destination: "documents.index", DestinationType: "queue", RoutingKey: "documents.index"},
			{Source: "documents", Vhost: "documents", Destination: "documents.convert", DestinationType: "queue", RoutingKey: "convert.*"},
			{Source: "documents", Vhost: "documents", Destination: "documents.notify", DestinationType: "queue", RoutingKey: "document.signed"},
			{Source: "documents", Vhost: "documents", Destination: "documents.index", DestinationType: "queue", RoutingKey: "document.created"},
			{Source: "documents", Vhost: "documents", Destination: "amq.gen-Xk2", DestinationType: "queue", RoutingKey: "reply"},
			{Source: "documents.dlx", Vhost: "documents", Destination: "documents.dlq", DestinationType: "queue"},
			{Source: "notifications", Vhost: "/", Destination: "notifications.email", DestinationType: "queue", RoutingKey: "email"},
		},
		Consumers: []rawConsumer{
			consumer("documents", "documents.convert", "conv"),
			consumer("documents", "documents.convert", "conv"),
			consumer("documents", "documents.index", "search"),
			consumer("/", "notifications.email", "anon"),
		},
		Connections: []rawConnection{
			{Name: "conv", Vhost: "documents", PeerHost: "10.0.0.1", UserProvidedName: "documents-converter"},
			// Older brokers only carry the name in client_properties.
			{Name: "search", Vhost: "documents", PeerHost: "10.0.0.2", ClientProperties: map[string]any{"connection_name": "documents-search"}},
			{Name: "anon", Vhost: "/", PeerHost: "10.0.0.3", User: "app", ClientProperties: map[string]any{"product": "AMQP 0.9.1 Client"}},
		},
		Policies: []rawPolicy{{Name: "dlx-default", Vhost: "documents", Pattern: "^documents\\.", ApplyTo: "queues",
			Definition: map[string]any{"dead-letter-exchange": "documents.dlx", "delivery-limit": float64(5)}}},
	}
}

func consumer(vhost, queue, conn string) rawConsumer {
	var c rawConsumer
	c.Queue.Vhost, c.Queue.Name = vhost, queue
	c.ChannelDetails.ConnectionName = conn
	return c
}

func find(t *testing.T, s *Snapshot, vhost string) Vhost {
	t.Helper()
	for _, v := range s.Vhosts {
		if v.Name == vhost {
			return v
		}
	}
	t.Fatalf("нет vhost %q", vhost)
	return Vhost{}
}

func queue(t *testing.T, v Vhost, name string) Queue {
	t.Helper()
	for _, q := range v.Queues {
		if q.Name == name {
			return q
		}
	}
	t.Fatalf("нет очереди %q в %s", name, v.Name)
	return Queue{}
}

func kinds(s *Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, p := range s.Problems {
		out[p.Kind] = append(out[p.Kind], p.Vhost+"/"+p.Object)
	}
	return out
}

func TestPolicySetDLXCountsAsADLX(t *testing.T) {
	s := Build("prod", fixture(), nil, time.Now())
	q := queue(t, find(t, s, "documents"), "documents.convert")
	if q.DeadLetterExchange != "documents.dlx" {
		t.Fatalf("DLX из политики не учтён: %q", q.DeadLetterExchange)
	}
	for _, p := range q.Effective {
		if p.Key == "dead-letter-exchange" && (p.Source != "policy" || p.SourceName != "dlx-default") {
			t.Errorf("источник DLX указан неверно: %+v", p)
		}
		if p.Key == "queue-type" {
			t.Error("x-queue-type — это тип очереди, а не параметр")
		}
	}
	for _, p := range s.Problems {
		if p.Kind == ProblemNoDLQ && p.Object == "documents.convert" {
			t.Error("очередь с DLX из политики помечена «без DLQ»")
		}
	}
}

func TestDeadLetterQueuesNeedNoConsumerAndNoDLQ(t *testing.T) {
	s := Build("prod", fixture(), nil, time.Now())
	// One reached through a DLX exchange, one through the default exchange.
	for _, ref := range [][2]string{{"documents", "documents.dlq"}, {"/", "notifications.email.dlq"}} {
		q := queue(t, find(t, s, ref[0]), ref[1])
		if !q.DeadLetterQueue {
			t.Errorf("%s/%s не распознана как DLQ", ref[0], ref[1])
		}
	}
	for kind, objects := range kinds(s) {
		for _, o := range objects {
			if strings.HasSuffix(o, ".dlq") {
				t.Errorf("DLQ %s помечена проблемой %s", o, kind)
			}
		}
	}
}

func TestProblemsAreTheOnesThatMatter(t *testing.T) {
	got := kinds(Build("prod", fixture(), nil, time.Now()))
	want := map[string][]string{
		ProblemNoDLQ:     {"documents/documents.index"},
		ProblemUnbound:   {"documents/legacy.events"},
		ProblemNoAccess:  {"payments/payments"},
		ProblemAnonymous: {"//anon"}, // vhost «/» плюс разделитель
	}
	// documents.notify в снимке никто не читает — но это не проблема: её
	// может забирать задача раз в сутки, а такую снимок не застаёт.
	for kind, objects := range got {
		if _, expected := want[kind]; !expected {
			t.Errorf("лишний вид проблем %s: %v", kind, objects)
		}
	}
	for kind, objects := range want {
		if strings.Join(got[kind], ",") != strings.Join(objects, ",") {
			t.Errorf("%s: получили %v, ожидали %v", kind, got[kind], objects)
		}
	}
	for _, list := range got {
		for _, o := range list {
			if strings.Contains(o, "amq.gen") {
				t.Errorf("временная очередь попала в проблемы: %s", o)
			}
		}
	}
}

func TestVhostWithoutPermissionsIsNotEmpty(t *testing.T) {
	v := find(t, Build("prod", fixture(), nil, time.Now()), "payments")
	if v.Accessible {
		t.Fatal("vhost без единого exchange — значит, без прав, а не пустой")
	}
}

func TestPartiesAreServicesNotChannels(t *testing.T) {
	s := Build("prod", fixture(), nil, time.Now())
	q := queue(t, find(t, s, "documents"), "documents.convert")
	if q.Consumers != 2 || len(q.ConsumedBy) != 1 || q.ConsumedBy[0].Service != "documents-converter" || q.ConsumedBy[0].Count != 2 {
		t.Fatalf("два канала одного сервиса должны быть одним участником ×2: %+v (consumers=%d)", q.ConsumedBy, q.Consumers)
	}
	idx := queue(t, find(t, s, "documents"), "documents.index")
	if idx.ConsumedBy[0].Service != "documents-search" {
		t.Errorf("имя из client_properties не подхвачено: %+v", idx.ConsumedBy)
	}
	anon := queue(t, find(t, s, "/"), "notifications.email")
	if anon.ConsumedBy[0].Service != Unrecognised || anon.ConsumedBy[0].Via != "" {
		t.Errorf("безымянное соединение без резолвера должно быть «не определён»: %+v", anon.ConsumedBy)
	}
}

func TestResolverNamesAnonymousConnectionsByAddress(t *testing.T) {
	resolve := func(name, peer string) (string, int64, string) {
		if name != "" {
			return name, 0, "name"
		}
		if peer == "10.0.0.3" {
			return "notification", 42, "ip"
		}
		return "", 0, ""
	}
	s := Build("prod", fixture(), resolve, time.Now())
	p := queue(t, find(t, s, "/"), "notifications.email").ConsumedBy[0]
	if p.Service != "notification" || p.ServiceID != 42 || p.Via != "ip" {
		t.Fatalf("по адресу пода сервис не найден: %+v", p)
	}
	for _, problem := range s.Problems {
		if problem.Kind == ProblemAnonymous && !strings.Contains(problem.Detail, "notification") {
			t.Errorf("в проблеме не сказано, на какой сервис похоже соединение: %s", problem.Detail)
		}
	}
}

func TestNumericLimitsTakeTheLowestValue(t *testing.T) {
	params := effective(
		map[string]any{"x-message-ttl": float64(60000), "x-dead-letter-exchange": "own"},
		"p", map[string]any{"message-ttl": float64(30000), "dead-letter-exchange": "from-policy", "max-length": float64(100)},
		"op", map[string]any{"max-length": float64(10), "dead-letter-exchange": "ignored"},
	)
	got := map[string]Param{}
	for _, p := range params {
		got[p.Key] = p
	}
	if got["message-ttl"].Value != float64(30000) || got["message-ttl"].Source != "policy" {
		t.Errorf("TTL: берётся меньшее значение, получили %+v", got["message-ttl"])
	}
	if got["dead-letter-exchange"].Value != "own" {
		t.Errorf("DLX: аргумент очереди сильнее политики, получили %+v", got["dead-letter-exchange"])
	}
	if got["max-length"].Value != float64(10) || got["max-length"].Source != "operator-policy" {
		t.Errorf("operator policy снижает лимит, получили %+v", got["max-length"])
	}
}

func TestShovelPasswordNeverReachesTheSnapshot(t *testing.T) {
	raw := fixture()
	raw.Shovels = []rawShovel{{Name: "to-dr", Vhost: "documents", State: "running",
		SrcURI: "amqp://user:secret@rabbit-a:5672/%2F", SrcQueue: "documents.notify",
		DestURI: "amqps://u:p@rabbit-dr:5671", DestEx: "documents"}}
	s := Build("prod", raw, nil, time.Now())
	sh := find(t, s, "documents").Shovels[0]
	if strings.Contains(sh.Source+sh.Destination, "secret") || strings.Contains(sh.Destination, "u:p") {
		t.Fatalf("пароль из URI shovel попал в снимок: %q → %q", sh.Source, sh.Destination)
	}
	if !strings.Contains(sh.Source, "rabbit-a:5672") || !strings.Contains(sh.Destination, "rabbit-dr") {
		t.Errorf("хост shovel потерялся: %q → %q", sh.Source, sh.Destination)
	}
}
