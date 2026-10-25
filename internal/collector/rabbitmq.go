package collector

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"spectra/internal/config"
	"spectra/internal/k8s"
	"spectra/internal/rabbitmq"
	"spectra/internal/store"
)

// RefreshMQ queues an out-of-band snapshot of every broker.
func (c *Collector) RefreshMQ() {
	select {
	case c.mqNow <- struct{}{}:
	default:
	}
}

func (c *Collector) mqLoop(ctx context.Context) {
	c.PollBrokers(ctx)
	ticker := time.NewTicker(mustSettings(ctx, c.store).MQPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.PollBrokers(ctx)
		case <-c.mqNow:
			c.PollBrokers(ctx)
		}
	}
}

// PollBrokers takes one snapshot of every configured cluster. A snapshot that
// is still running when the next one is due is not doubled: the second call
// returns at once.
func (c *Collector) PollBrokers(ctx context.Context) {
	if len(c.cfg.RabbitMQ) == 0 || !c.mqPolling.TryLock() {
		return
	}
	defer c.mqPolling.Unlock()

	settings := mustSettings(ctx, c.store)
	resolve := c.mqResolver(ctx)
	for _, b := range c.cfg.RabbitMQ {
		c.pollBroker(ctx, b, settings, resolve)
	}
}

func (c *Collector) pollBroker(ctx context.Context, b config.Broker, settings store.Settings, resolve rabbitmq.Resolver) {
	now := time.Now().UTC()
	raw, err := rabbitmq.NewClient(b, c.transport, settings.RequestTimeout).Fetch(ctx)
	if err != nil {
		c.log.Warn("rabbitmq snapshot failed", "cluster", b.Name, "err", err)
		if saveErr := c.store.SaveMQFailure(ctx, b.Name, err.Error(), now); saveErr != nil {
			c.log.Error("save rabbitmq failure", "cluster", b.Name, "err", saveErr)
		}
		return
	}

	snap := rabbitmq.Build(b.Name, raw, resolve, now)
	defs := rabbitmq.DefinitionsOf(snap)
	hash := defs.Hash()

	prev, err := c.store.MQState(ctx, b.Name)
	if err != nil {
		c.log.Error("load rabbitmq state", "cluster", b.Name, "err", err)
		return
	}
	var changes []rabbitmq.Change
	switch {
	case prev == nil || prev.Definitions == "":
		changes = rabbitmq.Diff(nil, defs)
	case prev.Hash != hash:
		var before rabbitmq.Definitions
		if err := json.Unmarshal([]byte(prev.Definitions), &before); err != nil {
			// An unreadable previous state is treated as none: one "first
			// snapshot" line is better than inventing removals.
			changes = rabbitmq.Diff(nil, defs)
		} else {
			changes = rabbitmq.Diff(&before, defs)
		}
	}

	snapshotJSON, err := json.Marshal(snap)
	if err != nil {
		c.log.Error("encode rabbitmq snapshot", "cluster", b.Name, "err", err)
		return
	}
	defsJSON, err := json.Marshal(defs)
	if err != nil {
		c.log.Error("encode rabbitmq definitions", "cluster", b.Name, "err", err)
		return
	}
	rows := make([]store.MQChange, 0, len(changes))
	for _, ch := range changes {
		rows = append(rows, store.MQChange{
			Cluster: b.Name, Vhost: ch.Vhost, ObjectKind: ch.ObjectKind, Object: ch.Object,
			Kind: ch.Kind, Severity: ch.Severity, Lines: ch.Lines,
		})
	}
	if err := c.store.SaveMQSnapshot(ctx, b.Name, snapshotJSON, defsJSON, hash, now, rows); err != nil {
		c.log.Error("save rabbitmq snapshot", "cluster", b.Name, "err", err)
		return
	}
	if len(rows) > 0 {
		c.log.Info("rabbitmq topology changed", "cluster", b.Name, "changes", len(rows))
	}
}

// mqResolver names the services behind broker connections. A connection that
// named itself is taken at its word; one that did not is looked up by the pod
// it came from — which needs `list pods`, so the pod list is fetched only if
// some connection actually needs it, and at most once per snapshot.
func (c *Collector) mqResolver(ctx context.Context) rabbitmq.Resolver {
	ids := map[string]int64{}
	if services, err := c.store.ListServices(ctx); err == nil {
		for _, svc := range services {
			backend, _, _ := strings.Cut(svc.Backend, ".")
			for _, name := range []string{backend, svc.DisplayName, svc.Label} {
				name = strings.ToLower(strings.TrimSpace(name))
				if _, taken := ids[name]; name != "" && !taken {
					ids[name] = svc.ID
				}
			}
		}
	}

	var pods map[string]k8s.Pod
	loaded := false
	return func(provided, peer string) (string, int64, string) {
		if provided != "" {
			return provided, ids[strings.ToLower(provided)], "name"
		}
		if c.k8s == nil || peer == "" {
			return "", 0, ""
		}
		if !loaded {
			loaded = true
			list, err := c.k8s.PodsByIP(ctx)
			if err != nil {
				if !c.podsWarned.Swap(true) {
					c.log.Warn("rabbitmq: pods are not readable, unnamed connections stay unnamed", "err", err)
				}
			}
			pods = list
		}
		if pod, ok := pods[peer]; ok {
			return pod.Service, ids[strings.ToLower(pod.Service)], "ip"
		}
		return "", 0, ""
	}
}
