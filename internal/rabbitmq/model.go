package rabbitmq

import "time"

// Snapshot is one cluster as the portal shows it: the topology plus who is
// attached to it right now. It is stored whole and served whole — at the size
// of a real installation (tens of virtual hosts, hundreds of queues) that is
// cheaper than modelling every object as a row.
type Snapshot struct {
	Cluster     string    `json:"cluster"`
	ClusterName string    `json:"clusterName,omitempty"`
	Version     string    `json:"version,omitempty"`
	CapturedAt  time.Time `json:"capturedAt"`

	// PublishersKnown says whether the broker reports who publishes where.
	// It does only with management.rates_mode = detailed; otherwise the page
	// says so instead of showing every exchange as unused.
	PublishersKnown bool `json:"publishersKnown"`
	Shovel          bool `json:"shovel"`
	Federation      bool `json:"federation"`

	Vhosts      []Vhost      `json:"vhosts"`
	Connections []Connection `json:"connections"`
	Problems    []Problem    `json:"problems"`
}

type Vhost struct {
	Name string `json:"name"`
	// Accessible is false when the vhost is listed but the portal's user has
	// no permissions in it. Every vhost the user can read has the built-in
	// amq.* exchanges, so seeing none of them is the tell.
	Accessible bool `json:"accessible"`

	Exchanges []Exchange   `json:"exchanges"`
	Queues    []Queue      `json:"queues"`
	Bindings  []Binding    `json:"bindings"`
	Policies  []Policy     `json:"policies"`
	Shovels   []Shovel     `json:"shovels,omitempty"`
	Links     []Federation `json:"federation,omitempty"`
}

type Exchange struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Durable    bool           `json:"durable"`
	AutoDelete bool           `json:"autoDelete"`
	Internal   bool           `json:"internal"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	Policy     string         `json:"policy,omitempty"`
	// DeadLetter marks an exchange some queue dead-letters into.
	DeadLetter bool    `json:"deadLetter"`
	Publishers []Party `json:"publishers"`
}

type Queue struct {
	Name           string         `json:"name"`
	Type           string         `json:"type"`
	Durable        bool           `json:"durable"`
	AutoDelete     bool           `json:"autoDelete"`
	Exclusive      bool           `json:"exclusive"`
	Arguments      map[string]any `json:"arguments,omitempty"`
	Policy         string         `json:"policy,omitempty"`
	OperatorPolicy string         `json:"operatorPolicy,omitempty"`
	// Effective are the settings that actually apply, each with where it
	// came from. DLX, TTL and length limits are as often set by a policy as
	// by the queue's own arguments; judging a queue by its arguments alone
	// reports "no dead-letter queue" for queues that have one.
	Effective []Param `json:"effective"`
	// DeadLetterExchange is the effective x-dead-letter-exchange, if any.
	DeadLetterExchange string `json:"deadLetterExchange,omitempty"`
	// DeadLetterQueue marks a queue that receives dead letters — it needs
	// neither a consumer of its own right now nor a DLQ of its own.
	DeadLetterQueue bool    `json:"deadLetterQueue"`
	Consumers       int     `json:"consumers"`
	ConsumedBy      []Party `json:"consumedBy"`
}

// Param is one effective queue setting.
type Param struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
	// Source is "argument", "policy" or "operator-policy".
	Source     string `json:"source"`
	SourceName string `json:"sourceName,omitempty"`
}

type Binding struct {
	Source          string `json:"source"`
	Destination     string `json:"destination"`
	DestinationType string `json:"destinationType"`
	RoutingKey      string `json:"routingKey"`
}

type Policy struct {
	Name       string         `json:"name"`
	Pattern    string         `json:"pattern"`
	ApplyTo    string         `json:"applyTo"`
	Priority   int            `json:"priority"`
	Definition map[string]any `json:"definition"`
	Operator   bool           `json:"operator"`
	// Queues counts the queues this policy is the effective one for. A policy
	// edit changes the behaviour of every one of them at once.
	Queues int `json:"queues"`
}

type Shovel struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	State       string `json:"state"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

type Federation struct {
	Upstream string `json:"upstream"`
	Type     string `json:"type"`
	Object   string `json:"object"`
	Status   string `json:"status"`
}

// Party is a service attached to an exchange or a queue.
type Party struct {
	Service   string `json:"service"`
	ServiceID int64  `json:"serviceId,omitempty"`
	// Via says how the service was recognised: "name" — the client named its
	// connection, "ip" — matched by the pod's address, "" — not recognised.
	Via   string `json:"via"`
	Count int    `json:"count"`
}

type Connection struct {
	Name     string `json:"name"`
	Vhost    string `json:"vhost"`
	User     string `json:"user,omitempty"`
	PeerHost string `json:"peerHost"`
	// ProvidedName is the connection_name the client set. Without it the
	// broker only knows an address.
	ProvidedName string `json:"providedName,omitempty"`
	Product      string `json:"product,omitempty"`
	Service      string `json:"service,omitempty"`
	ServiceID    int64  `json:"serviceId,omitempty"`
	Via          string `json:"via"`
}

// Problem kinds.
const (
	ProblemNoDLQ     = "nodlq"     // нет x-dead-letter-exchange
	ProblemUnbound   = "unbound"   // exchange без привязок
	ProblemAnonymous = "anonymous" // соединение без connection_name
	ProblemNoAccess  = "noaccess"  // vhost без прав у пользователя портала
	ProblemShovel    = "shovel"    // shovel или federation не в рабочем состоянии
)

type Problem struct {
	Kind       string `json:"kind"`
	Vhost      string `json:"vhost,omitempty"`
	ObjectKind string `json:"objectKind,omitempty"`
	Object     string `json:"object,omitempty"`
	Detail     string `json:"detail"`
}

// Change is one difference between two snapshots of the topology.
type Change struct {
	Vhost      string   `json:"vhost"`
	ObjectKind string   `json:"objectKind"` // cluster | vhost | exchange | queue | binding | policy | shovel
	Object     string   `json:"object"`
	Kind       string   `json:"kind"`     // added | removed | changed
	Severity   string   `json:"severity"` // info | attention | breaking
	Lines      []string `json:"lines"`
}

const (
	SeverityInfo      = "info"
	SeverityAttention = "attention"
	SeverityBreaking  = "breaking"
)

// Resolver names the service behind a connection: by the name the client
// gave it, else by the address it came from. It returns the service name, its
// registry id when the portal knows it, and how it was recognised.
type Resolver func(providedName, peerHost string) (service string, id int64, via string)
