package rabbitmq

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Unrecognised is what a party is called when neither its connection name nor
// its address says which service it is.
const Unrecognised = "не определён"

// Build turns one pass over the management API into a snapshot. resolve may be
// nil: then every party is named only by what the client told the broker.
func Build(cluster string, raw *Raw, resolve Resolver, now time.Time) *Snapshot {
	if resolve == nil {
		resolve = func(name, _ string) (string, int64, string) {
			if name != "" {
				return name, 0, "name"
			}
			return "", 0, ""
		}
	}
	snap := &Snapshot{
		Cluster:         cluster,
		ClusterName:     raw.Overview.ClusterName,
		Version:         raw.Overview.RabbitMQVersion,
		CapturedAt:      now.UTC(),
		PublishersKnown: raw.Incoming != nil,
		Shovel:          raw.Shovels != nil,
		Federation:      raw.FederationLinks != nil,
		Vhosts:          []Vhost{},
		Connections:     []Connection{},
		Problems:        []Problem{},
	}

	// Connections first: consumers and publishers are attributed through them.
	conns := map[string]Connection{}
	for _, rc := range raw.Connections {
		c := Connection{
			Name:         rc.Name,
			Vhost:        rc.Vhost,
			User:         rc.User,
			PeerHost:     rc.PeerHost,
			ProvidedName: rc.UserProvidedName,
		}
		if c.ProvidedName == "" {
			c.ProvidedName, _ = rc.ClientProperties["connection_name"].(string)
		}
		c.Product, _ = rc.ClientProperties["product"].(string)
		c.Service, c.ServiceID, c.Via = resolve(c.ProvidedName, c.PeerHost)
		conns[c.Name] = c
		snap.Connections = append(snap.Connections, c)
	}
	sort.Slice(snap.Connections, func(i, j int) bool {
		a, b := snap.Connections[i], snap.Connections[j]
		if a.Vhost != b.Vhost {
			return a.Vhost < b.Vhost
		}
		return a.Name < b.Name
	})

	party := func(details rawChannelDetails) Party {
		if c, ok := conns[details.ConnectionName]; ok && c.Service != "" {
			return Party{Service: c.Service, ServiceID: c.ServiceID, Via: c.Via}
		}
		return Party{Service: Unrecognised}
	}

	vhosts := map[string]*Vhost{}
	order := []string{}
	vhost := func(name string) *Vhost {
		if v, ok := vhosts[name]; ok {
			return v
		}
		v := &Vhost{Name: name, Exchanges: []Exchange{}, Queues: []Queue{}, Bindings: []Binding{}, Policies: []Policy{}}
		vhosts[name] = v
		order = append(order, name)
		return v
	}
	for _, rv := range raw.Vhosts {
		vhost(rv.Name)
	}

	for _, rx := range raw.Exchanges {
		v := vhost(rx.Vhost)
		v.Accessible = true
		x := Exchange{
			Name: rx.Name, Type: rx.Type, Durable: rx.Durable, AutoDelete: rx.AutoDelete,
			Internal: rx.Internal, Arguments: rx.Arguments, Policy: rx.Policy, Publishers: []Party{},
		}
		if stats, ok := raw.Incoming[rx.Vhost+"\x00"+rx.Name]; ok {
			var parties []Party
			for _, s := range stats {
				if s.Stats.Publish > 0 {
					parties = append(parties, party(s.ChannelDetails))
				}
			}
			x.Publishers = groupParties(parties)
		}
		v.Exchanges = append(v.Exchanges, x)
	}

	policies := map[string]map[string]rawPolicy{}  // vhost → name → policy
	operators := map[string]map[string]rawPolicy{} // vhost → name → operator policy
	for _, list := range []struct {
		items    []rawPolicy
		target   map[string]map[string]rawPolicy
		operator bool
	}{{raw.Policies, policies, false}, {raw.OperatorPolicies, operators, true}} {
		for _, p := range list.items {
			if list.target[p.Vhost] == nil {
				list.target[p.Vhost] = map[string]rawPolicy{}
			}
			list.target[p.Vhost][p.Name] = p
		}
	}

	consumers := map[string][]Party{} // vhost\x00queue → parties
	for _, rc := range raw.Consumers {
		key := rc.Queue.Vhost + "\x00" + rc.Queue.Name
		consumers[key] = append(consumers[key], party(rc.ChannelDetails))
	}

	policyUse := map[string]int{} // vhost\x00policy → queues
	for _, rq := range raw.Queues {
		v := vhost(rq.Vhost)
		q := Queue{
			Name: rq.Name, Type: rq.Type, Durable: rq.Durable, AutoDelete: rq.AutoDelete,
			Exclusive: rq.Exclusive, Arguments: rq.Arguments, Policy: rq.Policy,
			OperatorPolicy: rq.OperatorPolicy,
			Consumers:      len(consumers[rq.Vhost+"\x00"+rq.Name]),
			ConsumedBy:     groupParties(consumers[rq.Vhost+"\x00"+rq.Name]),
		}
		if q.Type == "" {
			q.Type = "classic"
		}
		var policyDef, operatorDef map[string]any
		if p, ok := policies[rq.Vhost][rq.Policy]; ok {
			policyDef = p.Definition
			policyUse[usageKey(rq.Vhost, rq.Policy, false)]++
		}
		if p, ok := operators[rq.Vhost][rq.OperatorPolicy]; ok {
			operatorDef = p.Definition
			policyUse[usageKey(rq.Vhost, rq.OperatorPolicy, true)]++
		}
		q.Effective = effective(rq.Arguments, rq.Policy, policyDef, rq.OperatorPolicy, operatorDef)
		for _, p := range q.Effective {
			if p.Key == "dead-letter-exchange" {
				q.DeadLetterExchange = fmt.Sprint(p.Value)
			}
		}
		v.Queues = append(v.Queues, q)
	}

	for _, rb := range raw.Bindings {
		// The default exchange's implicit bindings — one per queue, to its own
		// name — say nothing a reader does not already know.
		if rb.Source == "" {
			continue
		}
		v := vhost(rb.Vhost)
		v.Bindings = append(v.Bindings, Binding{
			Source: rb.Source, Destination: rb.Destination,
			DestinationType: rb.DestinationType, RoutingKey: rb.RoutingKey,
		})
	}

	for operator, list := range map[bool]map[string]map[string]rawPolicy{false: policies, true: operators} {
		for vh, byName := range list {
			v := vhost(vh)
			for _, p := range byName {
				v.Policies = append(v.Policies, Policy{
					Name: p.Name, Pattern: p.Pattern, ApplyTo: p.ApplyTo, Priority: p.Priority,
					Definition: p.Definition, Operator: operator, Queues: policyUse[usageKey(vh, p.Name, operator)],
				})
			}
		}
	}

	for _, rs := range raw.Shovels {
		v := vhost(rs.Vhost)
		v.Shovels = append(v.Shovels, Shovel{
			Name: rs.Name, Type: rs.Type, State: rs.State,
			Source:      shovelEnd(rs.SrcURI, rs.SrcEx, rs.SrcQueue),
			Destination: shovelEnd(rs.DestURI, rs.DestEx, rs.DestQ),
		})
	}
	for _, rf := range raw.FederationLinks {
		v := vhost(rf.Vhost)
		object := rf.Exchange
		if object == "" {
			object = rf.Queue
		}
		v.Links = append(v.Links, Federation{Upstream: rf.Upstream, Type: rf.Type, Object: object, Status: rf.Status})
	}

	sort.Strings(order)
	for _, name := range order {
		v := vhosts[name]
		markDeadLetters(v)
		sortVhost(v)
		snap.Vhosts = append(snap.Vhosts, *v)
	}
	snap.Problems = problems(snap)
	return snap
}

// usageKey keeps a policy and an operator policy of the same name apart.
func usageKey(vhost, policy string, operator bool) string {
	if operator {
		return vhost + "\x00operator\x00" + policy
	}
	return vhost + "\x00" + policy
}

func shovelEnd(uri, exchange, queue string) string {
	target := queue
	if exchange != "" {
		target = "exchange " + exchange
	} else if queue != "" {
		target = "очередь " + queue
	}
	if host := hostOf(uri); host != "" {
		return host + " · " + target
	}
	return target
}

// hostOf strips everything but the host from an AMQP URI. The shovel's URI can
// carry a password, and a shovel's source is shown to anyone.
func hostOf(uri string) string {
	rest := uri
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// numericKeys are the settings where RabbitMQ applies the lowest of the values
// given by the arguments, the policy and the operator policy.
var numericKeys = map[string]bool{
	"message-ttl": true, "expires": true, "max-length": true,
	"max-length-bytes": true, "delivery-limit": true,
}

// effective resolves what applies to a queue. Arguments win over the policy
// for everything except the numeric limits, where the lowest value wins; the
// operator policy can only lower those limits.
func effective(args map[string]any, policy string, policyDef map[string]any,
	operator string, operatorDef map[string]any) []Param {

	byKey := map[string]Param{}
	for k, v := range args {
		key := strings.TrimPrefix(k, "x-")
		if key == "queue-type" {
			continue // shown as the queue's type
		}
		byKey[key] = Param{Key: key, Value: v, Source: "argument"}
	}
	apply := func(def map[string]any, source, name string, onlyLower bool) {
		for key, v := range def {
			current, ok := byKey[key]
			switch {
			case !ok && !onlyLower:
				byKey[key] = Param{Key: key, Value: v, Source: source, SourceName: name}
			case !ok && onlyLower && numericKeys[key]:
				byKey[key] = Param{Key: key, Value: v, Source: source, SourceName: name}
			case ok && numericKeys[key]:
				if lower(v, current.Value) {
					byKey[key] = Param{Key: key, Value: v, Source: source, SourceName: name}
				}
			}
		}
	}
	apply(policyDef, "policy", policy, false)
	apply(operatorDef, "operator-policy", operator, true)

	out := make([]Param, 0, len(byKey))
	for _, p := range byKey {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func lower(a, b any) bool {
	x, okA := number(a)
	y, okB := number(b)
	return okA && okB && x < y
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// deadLetterSuffixes catch DLQs that applications fill themselves, without a
// DLX: by convention, not by configuration.
var deadLetterSuffixes = []string{".dlq", "-dlq", "_dlq", ".dead", ".deadletter", ".dead-letter"}

func markDeadLetters(v *Vhost) {
	dlx := map[string]bool{}
	directDLQ := map[string]bool{} // dead-lettering to the default exchange names the queue directly
	for _, q := range v.Queues {
		hasDLX := false
		var routingKey string
		for _, p := range q.Effective {
			switch p.Key {
			case "dead-letter-exchange":
				hasDLX = true
			case "dead-letter-routing-key":
				routingKey = fmt.Sprint(p.Value)
			}
		}
		if !hasDLX {
			continue
		}
		if q.DeadLetterExchange == "" {
			directDLQ[routingKey] = true
		} else {
			dlx[q.DeadLetterExchange] = true
		}
	}
	dlqs := map[string]bool{}
	for _, b := range v.Bindings {
		if dlx[b.Source] && b.DestinationType == "queue" {
			dlqs[b.Destination] = true
		}
	}
	for i := range v.Exchanges {
		v.Exchanges[i].DeadLetter = dlx[v.Exchanges[i].Name]
	}
	for i := range v.Queues {
		q := &v.Queues[i]
		lowerName := strings.ToLower(q.Name)
		q.DeadLetterQueue = dlqs[q.Name] || directDLQ[q.Name]
		for _, suffix := range deadLetterSuffixes {
			if strings.HasSuffix(lowerName, suffix) {
				q.DeadLetterQueue = true
			}
		}
	}
}

// Temporary reports queues that live as long as one connection: RPC reply
// queues and the like. They come and go every minute, and neither their
// existence nor their lack of a DLQ says anything about the system.
func (q Queue) Temporary() bool {
	return q.Exclusive || q.AutoDelete || strings.HasPrefix(q.Name, "amq.gen-")
}

// BuiltIn reports the exchanges every vhost has from birth.
func (x Exchange) BuiltIn() bool {
	return x.Name == "" || strings.HasPrefix(x.Name, "amq.")
}

func problems(s *Snapshot) []Problem {
	out := []Problem{}
	for _, v := range s.Vhosts {
		if !v.Accessible {
			out = append(out, Problem{Kind: ProblemNoAccess, Vhost: v.Name, ObjectKind: "vhost", Object: v.Name,
				Detail: "у пользователя портала нет прав в этом vhost — его топология не видна"})
			continue
		}
		bound := map[string]bool{}
		for _, b := range v.Bindings {
			bound[b.Source] = true
		}
		for _, x := range v.Exchanges {
			if x.BuiltIn() || bound[x.Name] {
				continue
			}
			detail := "нет привязок: опубликованное сюда сообщение никуда не попадёт"
			if x.DeadLetter {
				detail = "это DLX, но к нему не привязана ни одна очередь: недоставленные сообщения теряются"
			}
			out = append(out, Problem{Kind: ProblemUnbound, Vhost: v.Name, ObjectKind: "exchange", Object: x.Name, Detail: detail})
		}
		for _, q := range v.Queues {
			if q.Temporary() {
				continue
			}
			// No "nobody reads it": a snapshot only sees who is subscribed at
			// that moment, and a nightly job or a basic.get reader is never
			// seen. What is seen is shown; what is not, is not concluded.
			hasDLX := false
			for _, p := range q.Effective {
				if p.Key == "dead-letter-exchange" {
					hasDLX = true
				}
			}
			if !hasDLX && !q.DeadLetterQueue && q.Type != "stream" {
				out = append(out, Problem{Kind: ProblemNoDLQ, Vhost: v.Name, ObjectKind: "queue", Object: q.Name,
					Detail: "нет dead-letter-exchange ни в аргументах, ни в политиках: сообщение, которое не удалось обработать, потеряется или зациклится"})
			}
		}
		for _, sh := range v.Shovels {
			if sh.State != "running" {
				out = append(out, Problem{Kind: ProblemShovel, Vhost: v.Name, ObjectKind: "shovel", Object: sh.Name,
					Detail: "shovel в состоянии «" + sh.State + "»: " + sh.Source + " → " + sh.Destination})
			}
		}
		for _, l := range v.Links {
			if l.Status != "running" {
				out = append(out, Problem{Kind: ProblemShovel, Vhost: v.Name, ObjectKind: "federation", Object: l.Upstream,
					Detail: "federation link в состоянии «" + l.Status + "»: " + l.Upstream + " → " + l.Object})
			}
		}
	}
	for _, c := range s.Connections {
		if c.ProvidedName == "" {
			detail := "соединение без connection_name с " + c.PeerHost
			if c.Service != "" {
				detail += " — по адресу похоже на " + c.Service
			}
			out = append(out, Problem{Kind: ProblemAnonymous, Vhost: c.Vhost, ObjectKind: "connection", Object: c.Name, Detail: detail})
		}
	}
	return out
}

// groupParties folds one entry per channel into one per service.
func groupParties(list []Party) []Party {
	byService := map[string]*Party{}
	var order []string
	for _, p := range list {
		if existing, ok := byService[p.Service]; ok {
			existing.Count++
			continue
		}
		p.Count = 1
		copied := p
		byService[p.Service] = &copied
		order = append(order, p.Service)
	}
	sort.Strings(order)
	out := make([]Party, 0, len(order))
	for _, name := range order {
		out = append(out, *byService[name])
	}
	return out
}

func sortVhost(v *Vhost) {
	sort.Slice(v.Exchanges, func(i, j int) bool { return v.Exchanges[i].Name < v.Exchanges[j].Name })
	sort.Slice(v.Queues, func(i, j int) bool { return v.Queues[i].Name < v.Queues[j].Name })
	sort.Slice(v.Bindings, func(i, j int) bool {
		a, b := v.Bindings[i], v.Bindings[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Destination != b.Destination {
			return a.Destination < b.Destination
		}
		return a.RoutingKey < b.RoutingKey
	})
	sort.Slice(v.Policies, func(i, j int) bool {
		if v.Policies[i].Operator != v.Policies[j].Operator {
			return !v.Policies[i].Operator
		}
		return v.Policies[i].Name < v.Policies[j].Name
	})
	sort.Slice(v.Shovels, func(i, j int) bool { return v.Shovels[i].Name < v.Shovels[j].Name })
	sort.Slice(v.Links, func(i, j int) bool {
		return v.Links[i].Upstream+v.Links[i].Object < v.Links[j].Upstream+v.Links[j].Object
	})
}
