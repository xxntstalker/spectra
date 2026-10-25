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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"spectra/internal/config"
	"spectra/internal/discovery"
	"spectra/internal/k8s"
	"spectra/internal/openapi"
	"spectra/internal/store"
)

type Collector struct {
	cfg   *config.Config
	store *store.Store
	k8s   *k8s.Client
	log   *slog.Logger

	trigger chan int64 // service id, or 0 for "everything"
	syncNow chan struct{}

	// polling records whether this replica currently holds the poller lock, so
	// metrics can show which one of several is actually doing the work.
	polling atomic.Bool

	// sweeping serialises full sweeps. A sweep can be started from the ticker,
	// from a manual refresh and from a cluster sync that found new services;
	// letting two overlap would double the requests every watched service
	// receives, which is precisely what this portal must not do.
	sweeping sync.Mutex

	// transport is built once and shared by every poll. Per-poll transports
	// would each keep their own connection pool, so nothing would ever be
	// reused and every request would open a fresh connection.
	transport *http.Transport

	// mqNow queues an out-of-band snapshot of the brokers; mqPolling keeps
	// two of them from running at once, for the same reason as sweeping.
	mqNow     chan struct{}
	mqPolling sync.Mutex
	// podsWarned keeps a missing `list pods` permission to one log line
	// rather than one every few minutes.
	podsWarned atomic.Bool
}

func New(cfg *config.Config, st *store.Store, kube *k8s.Client, log *slog.Logger) *Collector {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = cfg.HTTP.TLSClientConfig()
	if cfg.HTTP.TLSSkipVerify {
		// Строчка на видном месте: это состояние должно быть заметно тому, кто
		// смотрит в лог, а не выясняться из переменных окружения.
		log.Warn("проверка сертификатов опрашиваемых сервисов отключена " +
			"(SPECTRA_TLS_SKIP_VERIFY); соединение шифруется, но подлинность " +
			"сервиса не подтверждается")
	}
	return &Collector{
		cfg:       cfg,
		store:     st,
		k8s:       kube,
		log:       log,
		trigger:   make(chan int64, 64),
		syncNow:   make(chan struct{}, 1),
		mqNow:     make(chan struct{}, 1),
		transport: transport,
	}
}

// IsPolling reports whether this replica holds the poller lock.
func (c *Collector) IsPolling() bool { return c.polling.Load() }

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
		c.polling.Store(true)
		c.loop(ctx)
		c.polling.Store(false)
		release()
	}
}

func (c *Collector) loop(ctx context.Context) {
	c.syncStatic(ctx)
	c.syncCluster(ctx)
	// Brokers run on their own schedule and interval, so a spec sweep that
	// spends minutes on a slow service does not hold their snapshot back.
	// They start after the registry is filled, so the first snapshot can
	// already link connections to services.
	if len(c.cfg.RabbitMQ) > 0 {
		go c.mqLoop(ctx)
	}
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
			Key: "static:" + u.Host + u.Path,
			// Host and port, not just the hostname: several services behind
			// one host on different ports would otherwise all be called the
			// same thing.
			DisplayName: u.Host + u.Path,
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

// address is one way into a service: the URL to talk to, and whether it is the
// fallback rather than the primary one.
type address struct {
	baseURL  string
	fallback bool
}

// addressesFor lists the addresses to try, in order.
//
// A service found in Kubernetes has two: the backend Service inside the
// cluster and the public hostname from the Ingress rule. The in-cluster
// address is normally the primary one, but it is also the one that breaks
// first — a NetworkPolicy between namespaces, a Service renamed without the
// Ingress following, a port only the ingress controller may reach. In each of
// those the public host still answers, so reporting the service as dead would
// be wrong.
//
// The address that last worked comes first, so a service that permanently
// lives behind its public host does not pay for a failed in-cluster attempt on
// every single poll. A cluster sweep resets that preference, which is what
// gives the primary address another chance once the network is repaired.
func addressesFor(svc store.Service) []address {
	primary := address{baseURL: svc.BaseURL}
	if svc.FallbackURL == "" || svc.FallbackURL == svc.BaseURL {
		return []address{primary}
	}
	backup := address{baseURL: svc.FallbackURL, fallback: true}
	if svc.UsingFallback {
		return []address{backup, primary}
	}
	return []address{primary, backup}
}

// hostFixed reports whether the spec address must be used exactly as stored.
//
// An address a person typed is host-fixed. The rest of the poll may try the
// service by either of its two addresses, but a manual address is not a guess
// to be improved on: it usually points at the public host precisely because the
// documentation is not served on the backend port at all.
func hostFixed(svc store.Service) bool {
	return svc.ManualSpecURL != "" && svc.SpecURL == svc.ManualSpecURL
}

// specURLOn re-points a known spec address at another base address. The path
// identifies the document; the host is only how we get to it.
func specURLOn(specURL, base string) string {
	if specURL == "" || base == "" {
		return ""
	}
	s, err := url.Parse(specURL)
	if err != nil {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil || b.Host == "" {
		return ""
	}
	if s.Host == b.Host && s.Scheme == b.Scheme {
		return specURL
	}
	out := *b
	out.Path, out.RawQuery = s.Path, s.RawQuery
	return out.String()
}

func (c *Collector) pollOne(ctx context.Context, svc store.Service, settings store.Settings) {
	ctx, cancel := context.WithTimeout(ctx, settings.ProbeBudget)
	defer cancel()

	client := &discovery.Client{
		HTTP:      &http.Client{Timeout: settings.RequestTimeout, Transport: c.transport},
		UserAgent: c.cfg.HTTP.UserAgent,
		Delay:     settings.ProbeDelay,
	}
	now := time.Now()

	addresses := addressesFor(svc)
	// Each address gets its own slice of the budget. Sharing one would defeat
	// the fallback in exactly the case it exists for: a NetworkPolicy that
	// drops packets instead of refusing them makes the primary address hang
	// until the budget is gone, leaving nothing for the second attempt.
	perAddress := settings.ProbeBudget / time.Duration(len(addresses))

	var (
		res    pollResult
		failed []string
	)
	for i, addr := range addresses {
		// Адрес спеки, заданный человеком, не зависит от того, каким путём мы
		// идём к сервису, — пробуем его только на первой попытке.
		res = c.pollAddress(ctx, client, svc, addr, now, perAddress, i == 0 || !hostFixed(svc))
		if res.probe.Status != store.ProbeOK {
			failed = append(failed, addr.baseURL+": "+res.probe.Error)
		}
		// Only "nothing answered" is worth asking the other address about. A
		// service that answered and has no specification gives the same answer
		// whichever door we knock on, and probing again would double the
		// requests it sees for no new information.
		if res.probe.Status != store.ProbeUnreachable || i+1 >= len(addresses) {
			break
		}
		c.log.Info("адрес недоступен, пробую запасной",
			"service", svc.ID, "name", svc.DisplayName,
			"failed", addr.baseURL, "next", addresses[i+1].baseURL, "err", res.err)
	}
	probe := res.probe

	if probe.Status != store.ProbeOK {
		// Neither address worked: leave the preference where it was rather than
		// letting whichever happened to be tried last become the new default.
		probe.UsingFallback = svc.UsingFallback
		// The UI shows one error line, and "недоступен" without saying which
		// addresses were tried invites the same question every time.
		probe.Error = strings.Join(failed, "; ")
		// A service that answered and simply has no specification will not grow
		// one on its own, so it leaves the automatic schedule entirely: someone
		// has to supply an address and ask for a probe. An unreachable service
		// is a different story — that is usually temporary, so it stays on the
		// schedule and just backs off.
		if probe.Status == store.ProbeNoSpec {
			probe.AutoProbe = false
			c.record(ctx, probe)
			c.log.Warn("спецификация не найдена, автоматический опрос выключен",
				"service", svc.ID, "name", svc.DisplayName, "err", res.err)
			return
		}
		probe.AutoProbe = true
		delay := backoff(settings, svc.FailCount)
		next := now.Add(delay)
		probe.NextProbeAt = &next
		c.record(ctx, probe)
		c.log.Warn("сервис недоступен", "service", svc.ID, "name", svc.DisplayName,
			"status", probe.Status, "err", res.err, "retryIn", delay.String(),
			"tried", len(addresses))
		return
	}

	if probe.UsingFallback != svc.UsingFallback {
		// Worth a line of its own: an operator reading the log should see that
		// the portal changed which door it goes through, and when.
		c.log.Warn("опрос переключён на другой адрес", "service", svc.ID,
			"name", svc.DisplayName, "usingFallback", probe.UsingFallback,
			"url", probe.SpecURL)
	}

	c.record(ctx, probe)
	if res.doc == nil {
		return // 304: the document we already hold is still current
	}
	// The document is already in hand; losing it to a budget that was meant to
	// bound network probing, not database writes, would mean re-downloading it
	// on the next poll for nothing.
	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelSave()
	if err := c.storeRevision(saveCtx, svc, res.doc, now, settings); err != nil {
		c.log.Error("store revision", "service", svc.ID, "err", err)
	}
}

// pollResult is one attempt against one address.
type pollResult struct {
	probe store.ProbeResult
	doc   *openapi.Doc // nil when the spec was unchanged
	err   error
}

func (c *Collector) pollAddress(ctx context.Context, client *discovery.Client,
	svc store.Service, addr address, now time.Time, budget time.Duration,
	trySpecURL bool) pollResult {

	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	probe := store.ProbeResult{ServiceID: svc.ID, CheckedAt: now, UsingFallback: addr.fallback}

	// A spec address learned on one host applies to the other: same service,
	// same document, different way in. One a person typed does not — it is an
	// instruction, not an observation.
	specURL := svc.SpecURL
	if !hostFixed(svc) {
		specURL = specURLOn(svc.SpecURL, addr.baseURL)
	}
	if !trySpecURL {
		// Уже пробовали ровно этот адрес на прошлой попытке: он от адреса
		// опроса не зависит, и повтор был бы лишним запросом к чужому сервису.
		specURL = ""
	}
	if specURL != "" {
		// Validators belong to the host that issued them. Switching addresses
		// means downloading the document once, rather than trusting an ETag
		// across a boundary it was never scoped to.
		etag, lastMod := svc.ETag, svc.LastModified
		if specURL != svc.SpecURL {
			etag, lastMod = "", ""
		}
		doc, resp, err := c.fetchKnown(ctx, client, specURL, etag, lastMod)
		switch {
		case err != nil:
			c.log.Info("known spec URL failed, rediscovering",
				"service", svc.ID, "url", specURL, "err", err)
			// Only forget the stored address when the primary attempt failed.
			// On a fallback attempt the address is very likely still correct
			// and simply unreachable from where we stand.
			if !addr.fallback {
				_ = c.store.ClearSpecURL(ctx, svc.ID)
			}
		case resp.NotModified:
			probe.Status, probe.Succeeded, probe.AutoProbe = store.ProbeOK, true, true
			probe.SpecURL, probe.ETag, probe.LastModified = specURL, svc.ETag, svc.LastModified
			return pollResult{probe: probe}
		default:
			probe.Status, probe.Succeeded, probe.AutoProbe = store.ProbeOK, true, true
			probe.SpecURL, probe.ETag, probe.LastModified = specURL, resp.ETag, resp.LastModified
			return pollResult{probe: probe, doc: doc}
		}
	}

	result, err := client.Discover(ctx, addr.baseURL, svc.ManualSpecURL)
	if err != nil {
		probe.Status = classifyFailure(ctx, result)
		probe.Error = err.Error()
		return pollResult{probe: probe, err: err}
	}

	// A successful probe puts the service back on the automatic schedule —
	// this is what makes "enter the address, press probe" stick.
	probe.Status, probe.Succeeded, probe.AutoProbe = store.ProbeOK, true, true
	probe.SpecURL, probe.SpecURLSource = result.SpecURL, result.Source
	if result.Resp != nil {
		probe.ETag, probe.LastModified = result.Resp.ETag, result.Resp.LastModified
	}
	c.log.Info("discovered spec", "service", svc.ID, "name", svc.DisplayName,
		"url", result.SpecURL, "via", result.Source, "fallback", addr.fallback)
	return pollResult{probe: probe, doc: result.Doc}
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

	// Contacts declared in the specification are stored on every successful
	// poll, not only when the contract changes: a team may add them without
	// touching a single endpoint.
	owner := doc.Owner()
	if err := c.store.SetDiscoveredOwner(ctx, svc.ID, store.OwnerFields{
		Team: owner.Team, Contact: owner.Contact, Repo: owner.Repo, Owners: owner.Owners,
	}); err != nil {
		c.log.Error("сохранение владельца из спеки", "service", svc.ID, "err", err)
	}

	// A contract identical to one seen before is a rolled-back deploy, not a
	// new change. Recorded as such so the feed says what happened instead of
	// re-listing the endpoints as if they had just moved.
	rollbackOf, err := c.store.PreviousRevisionWithHash(ctx, svc.ID, hash)
	if err != nil {
		return err
	}

	versionID, err := c.store.InsertVersion(ctx, &store.Version{
		ServiceID: svc.ID, Hash: hash, CapturedAt: now, Spec: canonical,
		Title: doc.Title(), APIVersion: doc.APIVersion(), SpecVersion: doc.SpecVersion(),
		EndpointCount: len(index.Ops), RollbackOf: rollbackOf,
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
		if rollbackOf != nil {
			// The breaking flag is left as computed: anyone who integrated
			// against the version being rolled back really is broken by it.
			// Only the explanation changes.
			diff.Notes = append(diff.Notes, openapi.Detail{
				Code:    "spec.rollback",
				Subject: strconv.Itoa(*rollbackOf),
			})
		}
		change.FromVersionID = &previous.ID
		change.Breaking = diff.Breaking
		change.Added, change.Removed, change.Modified = diff.Added, diff.Removed, diff.Modified
		change.Diff, _ = json.Marshal(diff)
		c.log.Info("spec changed", "service", svc.ID, "added", diff.Added,
			"removed", diff.Removed, "modified", diff.Modified,
			"breaking", diff.Breaking, "rollbackOf", rollbackOf)
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

// record writes the outcome of a poll.
//
// It deliberately does not inherit the caller's context. The probe budget has
// usually just expired by the time a failure gets here — that is what a
// failure often *is* — and writing the result under the deadline that killed
// the probe means the row is never updated at all. The service then shows as
// last checked hours ago, which reads as "the poller is broken" rather than
// "this service does not answer".
func (c *Collector) record(ctx context.Context, p store.ProbeResult) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := c.store.RecordProbe(ctx, p); err != nil {
		c.log.Error("record probe", "service", p.ServiceID, "err", err)
	}
}
