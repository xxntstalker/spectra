// Package rabbitmq reads the topology of a RabbitMQ cluster through its
// management API: virtual hosts, exchanges, queues, bindings, policies, who is
// connected and who consumes what.
//
// It reads metadata only, never messages. Every list is one request for the
// whole cluster, so the load does not grow with the number of virtual hosts or
// queues — the portal is a guest on the broker exactly as it is on the
// services it polls.
package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"spectra/internal/config"
)

// ErrNotFound is what the management API answers for a plugin that is not
// enabled. Shovel and federation are optional, and their absence is not an
// error.
var ErrNotFound = errors.New("not found")

type Client struct {
	broker config.Broker
	http   *http.Client
}

func NewClient(b config.Broker, transport http.RoundTripper, timeout time.Duration) *Client {
	return &Client{broker: b, http: &http.Client{Transport: transport, Timeout: timeout}}
}

// Raw is what one pass over the management API returned, before any of it is
// interpreted.
type Raw struct {
	Overview         rawOverview
	Vhosts           []rawVhost
	Exchanges        []rawExchange
	Queues           []rawQueue
	Bindings         []rawBinding
	Consumers        []rawConsumer
	Connections      []rawConnection
	Policies         []rawPolicy
	OperatorPolicies []rawPolicy
	// Shovels and FederationLinks are nil when the plugin is off.
	Shovels         []rawShovel
	FederationLinks []rawFederationLink
	// Incoming maps "vhost\x00exchange" to the channels that published into
	// that exchange. Nil when the broker does not collect per-channel
	// publishing statistics (management.rates_mode other than "detailed").
	Incoming map[string][]rawChannelStat
}

type rawOverview struct {
	RabbitMQVersion   string `json:"rabbitmq_version"`
	ManagementVersion string `json:"management_version"`
	ClusterName       string `json:"cluster_name"`
}

type rawVhost struct {
	Name string `json:"name"`
}

type rawExchange struct {
	Name       string         `json:"name"`
	Vhost      string         `json:"vhost"`
	Type       string         `json:"type"`
	Durable    bool           `json:"durable"`
	AutoDelete bool           `json:"auto_delete"`
	Internal   bool           `json:"internal"`
	Arguments  map[string]any `json:"arguments"`
	Policy     string         `json:"policy"`
}

type rawQueue struct {
	Name       string         `json:"name"`
	Vhost      string         `json:"vhost"`
	Type       string         `json:"type"`
	Durable    bool           `json:"durable"`
	AutoDelete bool           `json:"auto_delete"`
	Exclusive  bool           `json:"exclusive"`
	Arguments  map[string]any `json:"arguments"`
	// Policy and OperatorPolicy name what the broker matched to the queue.
	// Matching is the broker's job — pattern, apply-to, priority — so the
	// portal only looks the definitions up by these names.
	Policy         string `json:"policy"`
	OperatorPolicy string `json:"operator_policy"`
	// No consumer count and no message count. The first the broker leaves out
	// with disable_stats (проверено на 3.13), and the consumer list says more
	// anyway; the second is monitoring, and Spectra is a catalog.
}

type rawBinding struct {
	Source          string `json:"source"`
	Vhost           string `json:"vhost"`
	Destination     string `json:"destination"`
	DestinationType string `json:"destination_type"`
	RoutingKey      string `json:"routing_key"`
}

type rawChannelDetails struct {
	// ConnectionName is the broker's name for the connection, "ip:port -> ip:port",
	// not the name the client gave it. The client-given one is on the
	// connection itself.
	ConnectionName string `json:"connection_name"`
	Name           string `json:"name"`
	PeerHost       string `json:"peer_host"`
	User           string `json:"user"`
}

type rawConsumer struct {
	Queue struct {
		Name  string `json:"name"`
		Vhost string `json:"vhost"`
	} `json:"queue"`
	ChannelDetails rawChannelDetails `json:"channel_details"`
	ConsumerTag    string            `json:"consumer_tag"`
	PrefetchCount  int               `json:"prefetch_count"`
}

type rawConnection struct {
	Name     string `json:"name"`
	Vhost    string `json:"vhost"`
	User     string `json:"user"`
	PeerHost string `json:"peer_host"`
	// UserProvidedName is set by brokers that copy client_properties
	// .connection_name to the top level; the property itself is read too.
	UserProvidedName string         `json:"user_provided_name"`
	ClientProperties map[string]any `json:"client_properties"`
}

type rawPolicy struct {
	Name       string         `json:"name"`
	Vhost      string         `json:"vhost"`
	Pattern    string         `json:"pattern"`
	ApplyTo    string         `json:"apply-to"`
	Priority   int            `json:"priority"`
	Definition map[string]any `json:"definition"`
}

type rawShovel struct {
	Name     string `json:"name"`
	Vhost    string `json:"vhost"`
	Type     string `json:"type"`
	State    string `json:"state"`
	SrcURI   string `json:"src_uri"`
	SrcQueue string `json:"src_queue"`
	SrcEx    string `json:"src_exchange"`
	DestURI  string `json:"dest_uri"`
	DestQ    string `json:"dest_queue"`
	DestEx   string `json:"dest_exchange"`
}

type rawFederationLink struct {
	Vhost    string `json:"vhost"`
	Upstream string `json:"upstream"`
	Type     string `json:"type"`
	Exchange string `json:"exchange"`
	Queue    string `json:"queue"`
	Status   string `json:"status"`
}

type rawChannelStat struct {
	ChannelDetails rawChannelDetails `json:"channel_details"`
	Stats          struct {
		Publish int64 `json:"publish"`
	} `json:"stats"`
}

// Fetch reads everything the topology is built from. One request per list,
// sequentially: the order does not matter to the broker and parallel requests
// would only make the portal's load spikier.
func (c *Client) Fetch(ctx context.Context) (*Raw, error) {
	raw := &Raw{}
	steps := []struct {
		path     string
		query    url.Values
		target   any
		optional bool
	}{
		{"/api/overview", url.Values{"columns": {"rabbitmq_version,management_version,cluster_name"}}, &raw.Overview, false},
		{"/api/vhosts", url.Values{"columns": {"name"}}, &raw.Vhosts, false},
		{"/api/exchanges", url.Values{
			"disable_stats": {"true"},
			"columns":       {"name,vhost,type,durable,auto_delete,internal,arguments,policy"},
		}, &raw.Exchanges, false},
		{"/api/queues", url.Values{
			"disable_stats": {"true"},
			"columns":       {"name,vhost,type,durable,auto_delete,exclusive,arguments,policy,operator_policy"},
		}, &raw.Queues, false},
		{"/api/bindings", url.Values{"columns": {"source,vhost,destination,destination_type,routing_key"}}, &raw.Bindings, false},
		{"/api/consumers", nil, &raw.Consumers, false},
		{"/api/connections", url.Values{
			"columns": {"name,vhost,user,peer_host,user_provided_name,client_properties"},
		}, &raw.Connections, false},
		{"/api/policies", nil, &raw.Policies, false},
		{"/api/operator-policies", nil, &raw.OperatorPolicies, true},
		{"/api/shovels", nil, &raw.Shovels, true},
		{"/api/federation-links", nil, &raw.FederationLinks, true},
	}
	for _, step := range steps {
		err := c.get(ctx, step.path, step.query, step.target)
		if errors.Is(err, ErrNotFound) {
			if step.optional {
				continue
			}
			return nil, fmt.Errorf("брокер %s: %s не найден — это точно адрес management API "+
				"(обычно порт 15672), а не AMQP?", c.broker.Name, step.path)
		}
		if err != nil {
			return nil, err
		}
	}

	incoming, err := c.fetchIncoming(ctx, raw.Exchanges)
	if err != nil {
		return nil, err
	}
	raw.Incoming = incoming
	return raw, nil
}

// fetchIncoming asks each exchange who published into it.
//
// The broker always answers with an "incoming" list, but fills it only with
// management.rates_mode = detailed; in the default mode it is empty while the
// exchange's own counter says messages did arrive (проверено на 3.13). So the
// first exchange that has received anything and lists nobody settles it: the
// broker does not know, the rest are not asked, and a broker in the default
// mode costs one extra request per snapshot rather than one per exchange.
func (c *Client) fetchIncoming(ctx context.Context, exchanges []rawExchange) (map[string][]rawChannelStat, error) {
	out := map[string][]rawChannelStat{}
	for _, x := range exchanges {
		if x.Name == "" || strings.HasPrefix(x.Name, "amq.") {
			continue
		}
		var detail struct {
			Incoming     []rawChannelStat `json:"incoming"`
			MessageStats struct {
				PublishIn int64 `json:"publish_in"`
			} `json:"message_stats"`
		}
		path := "/api/exchanges/" + url.PathEscape(x.Vhost) + "/" + url.PathEscape(x.Name)
		err := c.get(ctx, path, url.Values{"columns": {"incoming,message_stats"}}, &detail)
		if errors.Is(err, ErrNotFound) {
			// Deleted between the list and this request.
			continue
		}
		if err != nil {
			return nil, err
		}
		if detail.MessageStats.PublishIn > 0 && len(detail.Incoming) == 0 {
			return nil, nil
		}
		out[x.Vhost+"\x00"+x.Name] = detail.Incoming
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values, target any) error {
	endpoint := c.broker.URL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.broker.User, c.broker.Password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("брокер %s: %s: %w", c.broker.Name, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("брокер %s: %s: %w", c.broker.Name, path, err)
	}
	switch {
	// A path the management plugin does not serve — a disabled plugin's —
	// falls through to its static file handler, which answers 404, or 406 to
	// a request that asks for JSON (проверено на 3.13).
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusNotAcceptable:
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		// The fix is on the broker, and it is a one-liner — so say which.
		return fmt.Errorf("брокер %s не принял логин %q (HTTP 401). Нужен пользователь с тегом "+
			"monitoring и правами в каждом vhost: rabbitmqctl set_user_tags %s monitoring",
			c.broker.Name, c.broker.User, c.broker.User)
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("брокер %s: нет прав на %s (HTTP 403). Пользователю %q нужен тег monitoring",
			c.broker.Name, path, c.broker.User)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("брокер %s: %s: HTTP %d: %s", c.broker.Name, path, resp.StatusCode,
			truncate(strings.TrimSpace(string(body)), 300))
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("брокер %s: %s: ответ не разбирается: %w", c.broker.Name, path, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
