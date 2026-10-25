package rabbitmq

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Definitions is the part of a snapshot that history is kept for: what is
// declared in the broker — vhosts, exchanges, queues, bindings, policies,
// shovels.
//
// Who is connected is not part of it. A snapshot sees a subscriber only if it
// happens to be subscribed at that moment: a job that drains its queue once a
// night, or one that reads with basic.get, is never seen at all. Recording
// "consumers: X → none" would turn every such job into a stream of false
// alarms, so the page shows who is seen right now and history stays with the
// configuration.
type Definitions struct {
	Vhosts map[string]*VhostDefs `json:"vhosts"`
}

type VhostDefs struct {
	Accessible bool                   `json:"accessible"`
	Exchanges  map[string]ExchangeDef `json:"exchanges"`
	Queues     map[string]QueueDef    `json:"queues"`
	Bindings   []BindingDef           `json:"bindings"`
	Policies   map[string]PolicyDef   `json:"policies"`
	Shovels    map[string]string      `json:"shovels"`
}

type ExchangeDef struct {
	Type       string         `json:"type"`
	Durable    bool           `json:"durable"`
	AutoDelete bool           `json:"autoDelete"`
	Internal   bool           `json:"internal"`
	Arguments  map[string]any `json:"arguments,omitempty"`
}

type QueueDef struct {
	Type      string            `json:"type"`
	Durable   bool              `json:"durable"`
	Effective map[string]string `json:"effective,omitempty"`
}

type BindingDef struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Type        string `json:"type"`
	RoutingKey  string `json:"routingKey"`
}

type PolicyDef struct {
	Operator   bool           `json:"operator"`
	Pattern    string         `json:"pattern"`
	ApplyTo    string         `json:"applyTo"`
	Priority   int            `json:"priority"`
	Definition map[string]any `json:"definition"`
}

// DefinitionsOf extracts the history-worthy part of a snapshot. Temporary
// queues and the built-in exchanges are left out: the first churn constantly,
// the second never change.
func DefinitionsOf(s *Snapshot) *Definitions {
	d := &Definitions{Vhosts: map[string]*VhostDefs{}}
	for _, v := range s.Vhosts {
		vd := &VhostDefs{
			Accessible: v.Accessible,
			Exchanges:  map[string]ExchangeDef{},
			Queues:     map[string]QueueDef{},
			Bindings:   []BindingDef{},
			Policies:   map[string]PolicyDef{},
			Shovels:    map[string]string{},
		}
		temporary := map[string]bool{}
		for _, q := range v.Queues {
			if q.Temporary() {
				temporary[q.Name] = true
				continue
			}
			qd := QueueDef{Type: q.Type, Durable: q.Durable}
			if len(q.Effective) > 0 {
				qd.Effective = map[string]string{}
				for _, p := range q.Effective {
					qd.Effective[p.Key] = describeParam(p)
				}
			}
			vd.Queues[q.Name] = qd
		}
		for _, x := range v.Exchanges {
			if x.BuiltIn() {
				continue
			}
			vd.Exchanges[x.Name] = ExchangeDef{Type: x.Type, Durable: x.Durable, AutoDelete: x.AutoDelete,
				Internal: x.Internal, Arguments: x.Arguments}
		}
		for _, b := range v.Bindings {
			if b.DestinationType == "queue" && temporary[b.Destination] {
				continue
			}
			vd.Bindings = append(vd.Bindings, BindingDef{Source: b.Source, Destination: b.Destination, Type: b.DestinationType, RoutingKey: b.RoutingKey})
		}
		for _, p := range v.Policies {
			key := p.Name
			if p.Operator {
				key = "operator:" + p.Name
			}
			vd.Policies[key] = PolicyDef{Operator: p.Operator, Pattern: p.Pattern, ApplyTo: p.ApplyTo, Priority: p.Priority, Definition: p.Definition}
		}
		for _, sh := range v.Shovels {
			vd.Shovels[sh.Name] = sh.Source + " → " + sh.Destination
		}
		d.Vhosts[v.Name] = vd
	}
	return d
}

func describeParam(p Param) string {
	value := formatValue(p.Value)
	switch p.Source {
	case "policy":
		return value + " (политика " + p.SourceName + ")"
	case "operator-policy":
		return value + " (operator policy " + p.SourceName + ")"
	}
	return value
}

// Hash identifies the definitions. encoding/json sorts map keys, and every
// slice above is sorted when built, so equal topologies hash equally whatever
// order the broker listed them in.
func (d *Definitions) Hash() string {
	body, _ := json.Marshal(d)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Diff lists what changed from prev to cur. A nil prev is the first snapshot of
// the cluster, recorded as one line rather than as every object "appearing".
func Diff(prev, cur *Definitions) []Change {
	if prev == nil {
		queues, exchanges := 0, 0
		for _, v := range cur.Vhosts {
			queues += len(v.Queues)
			exchanges += len(v.Exchanges)
		}
		return []Change{{
			ObjectKind: "cluster", Kind: "added", Severity: SeverityInfo,
			Lines: []string{fmt.Sprintf("первый снимок: vhost %d, exchange %d, очередей %d", len(cur.Vhosts), exchanges, queues)},
		}}
	}
	var out []Change
	for _, name := range unionKeys(prev.Vhosts, cur.Vhosts) {
		p, c := prev.Vhosts[name], cur.Vhosts[name]
		switch {
		case p == nil:
			out = append(out, Change{Vhost: name, ObjectKind: "vhost", Object: name, Kind: "added", Severity: SeverityInfo,
				Lines: []string{fmt.Sprintf("+ vhost: exchange %d, очередей %d", len(c.Exchanges), len(c.Queues))}})
			continue
		case c == nil:
			out = append(out, Change{Vhost: name, ObjectKind: "vhost", Object: name, Kind: "removed", Severity: SeverityBreaking,
				Lines: []string{fmt.Sprintf("− vhost вместе с exchange %d и очередями %d", len(p.Exchanges), len(p.Queues))}})
			continue
		case p.Accessible && !c.Accessible:
			// Losing access looks exactly like everything being deleted. It
			// is reported as what it is, and nothing below is diffed.
			out = append(out, Change{Vhost: name, ObjectKind: "vhost", Object: name, Kind: "changed", Severity: SeverityAttention,
				Lines: []string{"у пользователя портала пропали права в vhost — топология больше не видна"}})
			continue
		case !p.Accessible && c.Accessible:
			out = append(out, Change{Vhost: name, ObjectKind: "vhost", Object: name, Kind: "changed", Severity: SeverityInfo,
				Lines: []string{"портал получил права в vhost"}})
			continue
		case !c.Accessible:
			continue
		}
		out = append(out, diffVhost(name, p, c)...)
	}
	return out
}

func diffVhost(vhost string, p, c *VhostDefs) []Change {
	var out []Change
	bindingsTo := func(defs *VhostDefs, queue string) int {
		n := 0
		for _, b := range defs.Bindings {
			if b.Type == "queue" && b.Destination == queue {
				n++
			}
		}
		return n
	}
	bindingsFrom := func(defs *VhostDefs, exchange string) int {
		n := 0
		for _, b := range defs.Bindings {
			if b.Source == exchange {
				n++
			}
		}
		return n
	}

	for _, name := range unionKeys(p.Exchanges, c.Exchanges) {
		before, had := p.Exchanges[name]
		after, has := c.Exchanges[name]
		switch {
		case !had:
			out = append(out, Change{Vhost: vhost, ObjectKind: "exchange", Object: name, Kind: "added", Severity: SeverityInfo,
				Lines: []string{"+ exchange (" + after.Type + ")"}})
		case !has:
			severity := SeverityInfo
			line := "− exchange (" + before.Type + ")"
			if n := bindingsFrom(p, name); n > 0 {
				severity = SeverityBreaking
				line += fmt.Sprintf(": у него было привязок %d", n)
			}
			out = append(out, Change{Vhost: vhost, ObjectKind: "exchange", Object: name, Kind: "removed", Severity: severity, Lines: []string{line}})
		default:
			var lines []string
			if before.Type != after.Type {
				lines = append(lines, "тип: "+before.Type+" → "+after.Type)
			}
			lines = append(lines, diffMap("", before.Arguments, after.Arguments)...)
			if len(lines) > 0 {
				out = append(out, Change{Vhost: vhost, ObjectKind: "exchange", Object: name, Kind: "changed", Severity: SeverityAttention, Lines: lines})
			}
		}
	}

	for _, name := range unionKeys(p.Queues, c.Queues) {
		before, had := p.Queues[name]
		after, has := c.Queues[name]
		switch {
		case !had:
			out = append(out, Change{Vhost: vhost, ObjectKind: "queue", Object: name, Kind: "added", Severity: SeverityInfo,
				Lines: []string{"+ очередь (" + after.Type + ")"}})
		case !has:
			severity := SeverityInfo
			line := "− очередь (" + before.Type + ")"
			if n := bindingsTo(p, name); n > 0 {
				severity = SeverityBreaking
				line += fmt.Sprintf(": в неё вело привязок %d", n)
			}
			out = append(out, Change{Vhost: vhost, ObjectKind: "queue", Object: name, Kind: "removed", Severity: severity, Lines: []string{line}})
		default:
			var lines []string
			if before.Type != after.Type {
				lines = append(lines, "пересоздана: "+before.Type+" → "+after.Type)
			}
			if before.Durable != after.Durable {
				lines = append(lines, fmt.Sprintf("durable: %v → %v", before.Durable, after.Durable))
			}
			lines = append(lines, diffStrings(before.Effective, after.Effective)...)
			if len(lines) > 0 {
				out = append(out, Change{Vhost: vhost, ObjectKind: "queue", Object: name, Kind: "changed", Severity: SeverityInfo, Lines: lines})
			}
		}
	}

	key := func(b BindingDef) string {
		return b.Source + "\x00" + b.Type + "\x00" + b.Destination + "\x00" + b.RoutingKey
	}
	prevBindings, curBindings := map[string]BindingDef{}, map[string]BindingDef{}
	for _, b := range p.Bindings {
		prevBindings[key(b)] = b
	}
	for _, b := range c.Bindings {
		curBindings[key(b)] = b
	}
	for _, k := range unionKeys(prevBindings, curBindings) {
		before, had := prevBindings[k]
		after, has := curBindings[k]
		switch {
		case !had:
			out = append(out, Change{Vhost: vhost, ObjectKind: "binding", Object: after.Destination, Kind: "added", Severity: SeverityInfo,
				Lines: []string{"+ привязка " + describeBinding(after)}})
		case !has:
			out = append(out, Change{Vhost: vhost, ObjectKind: "binding", Object: before.Destination, Kind: "removed", Severity: SeverityBreaking,
				Lines: []string{"− привязка " + describeBinding(before) + ": сообщения с этим ключом сюда больше не попадают"}})
		}
	}

	for _, name := range unionKeys(p.Policies, c.Policies) {
		before, had := p.Policies[name]
		after, has := c.Policies[name]
		display := strings.TrimPrefix(name, "operator:")
		switch {
		case !had:
			out = append(out, Change{Vhost: vhost, ObjectKind: "policy", Object: display, Kind: "added", Severity: SeverityAttention,
				Lines: []string{"+ политика: шаблон " + after.Pattern + ", применяется к " + applyTo(after.ApplyTo)}})
		case !has:
			out = append(out, Change{Vhost: vhost, ObjectKind: "policy", Object: display, Kind: "removed", Severity: SeverityAttention,
				Lines: []string{"− политика: шаблон " + before.Pattern}})
		default:
			var lines []string
			if before.Pattern != after.Pattern {
				lines = append(lines, "шаблон: "+before.Pattern+" → "+after.Pattern)
			}
			if before.ApplyTo != after.ApplyTo {
				lines = append(lines, "применяется к: "+applyTo(before.ApplyTo)+" → "+applyTo(after.ApplyTo))
			}
			if before.Priority != after.Priority {
				lines = append(lines, fmt.Sprintf("приоритет: %d → %d", before.Priority, after.Priority))
			}
			lines = append(lines, diffMap("", before.Definition, after.Definition)...)
			if len(lines) > 0 {
				out = append(out, Change{Vhost: vhost, ObjectKind: "policy", Object: display, Kind: "changed", Severity: SeverityAttention, Lines: lines})
			}
		}
	}

	for _, name := range unionKeys(p.Shovels, c.Shovels) {
		before, had := p.Shovels[name]
		after, has := c.Shovels[name]
		switch {
		case !had:
			out = append(out, Change{Vhost: vhost, ObjectKind: "shovel", Object: name, Kind: "added", Severity: SeverityInfo, Lines: []string{"+ shovel " + after}})
		case !has:
			out = append(out, Change{Vhost: vhost, ObjectKind: "shovel", Object: name, Kind: "removed", Severity: SeverityBreaking, Lines: []string{"− shovel " + before}})
		case before != after:
			out = append(out, Change{Vhost: vhost, ObjectKind: "shovel", Object: name, Kind: "changed", Severity: SeverityAttention, Lines: []string{before + " → " + after}})
		}
	}
	return out
}

func describeBinding(b BindingDef) string {
	target := b.Destination
	if b.Type == "exchange" {
		target = "exchange " + b.Destination
	}
	if b.RoutingKey == "" {
		return b.Source + " → " + target
	}
	return b.Source + " [" + b.RoutingKey + "] → " + target
}

func applyTo(v string) string {
	switch v {
	case "queues":
		return "очередям"
	case "exchanges":
		return "exchange"
	case "all", "":
		return "всему"
	}
	return v
}

func diffMap(prefix string, before, after map[string]any) []string {
	var lines []string
	for _, k := range unionKeys(before, after) {
		a, hadA := before[k]
		b, hasB := after[k]
		switch {
		case !hadA:
			lines = append(lines, "+ "+prefix+k+": "+formatValue(b))
		case !hasB:
			lines = append(lines, "− "+prefix+k+": "+formatValue(a))
		case formatValue(a) != formatValue(b):
			lines = append(lines, prefix+k+": "+formatValue(a)+" → "+formatValue(b))
		}
	}
	return lines
}

func diffStrings(before, after map[string]string) []string {
	var lines []string
	for _, k := range unionKeys(before, after) {
		a, hadA := before[k]
		b, hasB := after[k]
		switch {
		case !hadA:
			lines = append(lines, "+ "+k+": "+b)
		case !hasB:
			lines = append(lines, "− "+k+": "+a)
		case a != b:
			lines = append(lines, k+": "+a+" → "+b)
		}
	}
	return lines
}

func formatValue(v any) string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return `""`
		}
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
	}
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(body)
}

func unionKeys[V any](a, b map[string]V) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
