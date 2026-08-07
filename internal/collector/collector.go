// Package collector discovers services from the cluster and polls them for
// their OpenAPI documents.
//
// Only one replica polls at a time, elected through a Postgres advisory lock.
// Without that, scaling the deployment would multiply the request load on
// every service the portal watches — the portal is a guest on someone else's
// infrastructure and must not be the reason a service sees more traffic.
package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"swaggeragg/internal/config"
	"swaggeragg/internal/discovery"
	"swaggeragg/internal/k8s"
	"swaggeragg/internal/openapi"
	"swaggeragg/internal/store"
)

type Collector struct {
	cfg   *config.Config
	store *store.Store
	k8s   *k8s.Client
	log   *slog.Logger

	trigger chan int64 // service id, or 0 for "everything"
	syncNow chan struct{}

	// sweeping serialises full sweeps. A sweep can be started from the ticker,
	// from a manual refresh and from a cluster sync that found new services;
	// letting two overlap would double the requests every watched service
	// receives, which is precisely what this portal must not do.
	sweeping sync.Mutex
}

func New(cfg *config.Config, st *store.Store, kube *k8s.Client, log *slog.Logger) *Collector {
	return &Collector{
		cfg:     cfg,
		store:   st,
		k8s:     kube,
		log:     log,
		trigger: make(chan int64, 64),
		syncNow: make(chan struct{}, 1),
	}
}

// Refresh queues a poll. serviceID 0 means every candidate.
func (c *Collector) Refresh(ctx context.Context, serviceID int64) {
	// A person asking for a retry should not be held by the backoff.
	if err := c.store.ClearBackoff(ctx, serviceID); err != nil {
		c.log.Error("clear backoff", "service", serviceID, "err", err)
	}
	select {
	case c.trigger <- serviceID:
	default:
	}
}

// SyncNow queues an out-of-band sweep of the cluster.
func (c *Collector) SyncNow() {
	select {
	case c.syncNow <- struct{}{}:
	default:
	}
}

// Run holds the poller election and does the work while it holds the lock.
func (c *Collector) Run(ctx context.Context) {
	if !c.cfg.PollerEnabled {
		c.log.Info("poller disabled by configuration; serving UI only")
		return
	}
	for ctx.Err() == nil {
		acquired, release, err := c.store.TryLock(ctx, store.LockPoller)
		if err != nil {
			c.log.Error("poller election", "err", err)
		}
		if !acquired {
			// Another replica is polling. Wait and try again, so that losing
			// the active replica hands the work over on its own.
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}
		c.log.Info("this replica is the active poller")
		c.loop(ctx)
		release()
	}
}

func (c *Collector) loop(ctx context.Context) {
	c.syncStatic(ctx)
	c.syncCluster(ctx)
	c.PollDue(ctx)

	settings, _ := c.store.LoadSettings(ctx)
	pollTicker := time.NewTicker(pollTick(settings.PollInterval))
	defer pollTicker.Stop()
	syncTicker := time.NewTicker(c.cfg.Kubernetes.SyncInterval)
	defer syncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			c.PollDue(ctx)
		case <-syncTicker.C:
			c.syncCluster(ctx)
		case <-c.syncNow:
			c.syncCluster(ctx)
		case id := <-c.trigger:
			if id == 0 {
				c.PollDue(ctx)
				continue
			}
			if svc, err := c.store.GetService(ctx, id); err == nil && svc != nil {
				c.pollOne(ctx, *svc, mustSettings(ctx, c.store))
			}
		}
	}
}

// pollTick keeps the ticker frequent enough to notice services whose backoff
// has expired, without polling anything more often than its own schedule says.
func pollTick(interval time.Duration) time.Duration {
	if interval > 5*time.Minute {
		return 5 * time.Minute
	}
	return interval
}

func mustSettings(ctx context.Context, st *store.Store) store.Settings {
	s, err := st.LoadSettings(ctx)
	if err != nil {
		return store.DefaultSettings()
	}
	return s
}

// syncStatic seeds the services listed in the environment. They are kept under
// their own source so a Kubernetes sweep never marks them absent.
func (c *Collector) syncStatic(ctx context.Context) {
	if len(c.cfg.StaticServices) == 0 {
		return
	}
	items := make([]store.Discovered, 0, len(c.cfg.StaticServices))
	for _, raw := range c.cfg.StaticServices {
		u, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil || u.Host == "" {
			c.log.Warn("static service address is unusable", "value", raw)
			continue
		}
		items = append(items, store.Discovered{
			Key:         "static:" + u.Host + u.Path,
			DisplayName: u.Hostname(),
			BaseURL:     u.String(),
			Host:        u.Host,
		})
	}
	if res, err := c.store.SyncDiscovered(ctx, "static", items); err != nil {
		c.log.Error("seed static services", "err", err)
	} else {
		c.log.Info("static services", "found", res.Found, "new", res.Added)
	}
}

// syncCluster refreshes the registry from Kubernetes.
func (c *Collector) syncCluster(ctx context.Context) {
	if c.k8s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	found, err := c.k8s.Discover(ctx)
	if err != nil {
		c.log.Error("cluster discovery failed", "err", err)
		_ = c.store.RecordSyncFailure(ctx, "kubernetes", err)
		return
	}
	res, err := c.store.SyncDiscovered(ctx, "kubernetes", found)
	if err != nil {
		c.log.Error("store discovered services", "err", err)
		return
	}
	c.log.Info("cluster sync", "found", res.Found, "new", res.Added, "gone", res.Gone)
	if res.Added > 0 {
		// Give the new arrivals a first look right away rather than making
		// someone wait a whole poll interval to see whether they have a spec.
		c.SyncNow()
		go c.PollDue(context.WithoutCancel(ctx))
	}
}

// PollDue polls every service whose backoff has expired. Sweeps never overlap.
func (c *Collector) PollDue(ctx context.Context) {
	c.sweeping.Lock()
	defer c.sweeping.Unlock()

	settings := mustSettings(ctx, c.store)
	services, err := c.store.PollCandidates(ctx, time.Now())
	if err != nil {
		c.log.Error("list poll candidates", "err", err)
		return
	}

	sem := make(chan struct{}, settings.Concurrency)
	var wg sync.WaitGroup
	for _, svc := range services {
		if ctx.Err() != nil {
			return
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(svc store.Service) {
			defer wg.Done()
			defer func() { <-sem }()
			c.pollOne(ctx, svc, settings)
		}(svc)
	}
	wg.Wait()
}

func (c *Collector) pollOne(ctx context.Context, svc store.Service, settings store.Settings) {
	ctx, cancel := context.WithTimeout(ctx, settings.ProbeBudget)
	defer cancel()

	client := &discovery.Client{
		HTTP:      &http.Client{Timeout: settings.RequestTimeout},
		UserAgent: c.cfg.HTTP.UserAgent,
	}
	now := time.Now()
	probe := store.ProbeResult{ServiceID: svc.ID, CheckedAt: now}

	var (
		doc      *openapi.Doc
		cachedOK bool
	)
	if svc.SpecURL != "" {
		fetched, resp, err := c.fetchKnown(ctx, client, svc.SpecURL, svc.ETag, svc.LastModified)
		switch {
		case err != nil:
			c.log.Info("known spec URL failed, rediscovering",
				"service", svc.ID, "url", svc.SpecURL, "err", err)
			_ = c.store.ClearSpecURL(ctx, svc.ID)
		case resp.NotModified:
			probe.Status, probe.Succeeded, probe.AutoProbe = store.ProbeOK, true, true
			probe.SpecURL, probe.ETag, probe.LastModified = svc.SpecURL, svc.ETag, svc.LastModified
			c.record(ctx, probe)
			return
		default:
			doc, cachedOK = fetched, true
			probe.SpecURL, probe.ETag, probe.LastModified = svc.SpecURL, resp.ETag, resp.LastModified
		}
	}

	if !cachedOK {
		result, err := client.Discover(ctx, svc.BaseURL, svc.ManualSpecURL)
		if err != nil {
			probe.Status = classifyFailure(ctx, result)
			probe.Error = err.Error()

			// A service that answered and simply has no specification will not
			// grow one on its own, so it leaves the automatic schedule
			// entirely: someone has to supply an address and ask for a probe.
			// An unreachable service is a different story — that is usually
			// temporary, so it stays on the schedule and just backs off.
			if probe.Status == store.ProbeNoSpec {
				probe.AutoProbe = false
				c.record(ctx, probe)
				c.log.Warn("спецификация не найдена, автоматический опрос выключен",
					"service", svc.ID, "name", svc.DisplayName, "err", err)
				return
			}

			probe.AutoProbe = true
			delay := backoff(settings, svc.FailCount)
			next := now.Add(delay)
			probe.NextProbeAt = &next
			c.record(ctx, probe)
			c.log.Warn("сервис недоступен", "service", svc.ID, "name", svc.DisplayName,
				"status", probe.Status, "err", err, "retryIn", delay.String())
			return
		}
		doc = result.Doc
		probe.SpecURL, probe.SpecURLSource = result.SpecURL, result.Source
		if result.Resp != nil {
			probe.ETag, probe.LastModified = result.Resp.ETag, result.Resp.LastModified
		}
		c.log.Info("discovered spec", "service", svc.ID, "name", svc.DisplayName,
			"url", result.SpecURL, "via", result.Source)
	}

	// A successful probe puts the service back on the automatic schedule —
	// this is what makes "enter the address, press probe" stick.
	probe.Status, probe.Succeeded, probe.AutoProbe = store.ProbeOK, true, true
	c.record(ctx, probe)

	if err := c.storeRevision(ctx, svc, doc, now, settings); err != nil {
		c.log.Error("store revision", "service", svc.ID, "err", err)
	}
}

func (c *Collector) fetchKnown(ctx context.Context, client *discovery.Client, url, etag, lastMod string) (*openapi.Doc, *discovery.Response, error) {
	resp, err := client.Get(ctx, url, etag, lastMod)
	if err != nil {
		return nil, resp, err
	}
	if resp.NotModified {
		return nil, resp, nil
	}
	doc, err := openapi.Parse(resp.Body)
	if err != nil {
		return nil, resp, err
	}
	return doc, resp, nil
}

func classifyFailure(ctx context.Context, result *discovery.Result) string {
	switch {
	case ctx.Err() != nil:
		return store.ProbeUnreachable
	case result == nil || !result.Reached:
		return store.ProbeUnreachable
	default:
		return store.ProbeNoSpec
	}
}

// backoff widens the gap between attempts for a service that keeps failing.
// A host with no API at all is normal in a cluster, and it must not cost a
// full path sweep on every tick forever.
func backoff(settings store.Settings, failures int) time.Duration {
	delay := settings.PollInterval
	for i := 0; i < failures && delay < settings.MaxBackoff; i++ {
		delay *= 2
	}
	if delay > settings.MaxBackoff {
		delay = settings.MaxBackoff
	}
	return delay
}

func (c *Collector) storeRevision(ctx context.Context, svc store.Service, doc *openapi.Doc, now time.Time, settings store.Settings) error {
	opts := openapi.HashOptions{
		IgnoreServers: settings.IgnoreServers,
		IgnoreFields:  settings.IgnoreFields,
	}
	normalized := doc.Normalized(opts)
	hash, err := normalized.Hash()
	if err != nil {
		return err
	}

	previous, err := c.store.LatestVersion(ctx, svc.ID)
	if err != nil {
		return err
	}
	if previous != nil && previous.Hash == hash {
		return nil
	}

	canonical, err := doc.JSON()
	if err != nil {
		return err
	}
	index := normalized.BuildIndex()

	versionID, err := c.store.InsertVersion(ctx, &store.Version{
		ServiceID: svc.ID, Hash: hash, CapturedAt: now, Spec: canonical,
		Title: doc.Title(), APIVersion: doc.APIVersion(), SpecVersion: doc.SpecVersion(),
		EndpointCount: len(index.Ops),
	})
	if err != nil {
		return err
	}

	change := &store.Change{ServiceID: svc.ID, ToVersionID: versionID, DetectedAt: now}
	if previous == nil {
		change.Added = len(index.Ops)
		change.Diff, _ = json.Marshal(openapi.Diff{
			Notes: []openapi.Detail{{Code: "spec.initial"}}, Added: len(index.Ops),
		})
		c.log.Info("first spec revision", "service", svc.ID, "endpoints", len(index.Ops))
	} else {
		prevDoc, perr := openapi.Parse(previous.Spec)
		if perr != nil {
			return perr
		}
		diff := openapi.Compare(prevDoc.Normalized(opts), normalized)
		change.FromVersionID = &previous.ID
		change.Breaking = diff.Breaking
		change.Added, change.Removed, change.Modified = diff.Added, diff.Removed, diff.Modified
		change.Diff, _ = json.Marshal(diff)
		c.log.Info("spec changed", "service", svc.ID, "added", diff.Added,
			"removed", diff.Removed, "modified", diff.Modified, "breaking", diff.Breaking)
	}
	if err := c.store.InsertChange(ctx, change); err != nil {
		return err
	}

	removed, err := c.store.PruneVersions(ctx, svc.ID, settings.RetainVersions)
	if err != nil {
		return fmt.Errorf("prune versions: %w", err)
	}
	if removed > 0 {
		c.log.Info("pruned old revisions", "service", svc.ID, "removed", removed)
	}
	return nil
}

func (c *Collector) record(ctx context.Context, p store.ProbeResult) {
	if err := c.store.RecordProbe(ctx, p); err != nil {
		c.log.Error("record probe", "service", p.ServiceID, "err", err)
	}
}
