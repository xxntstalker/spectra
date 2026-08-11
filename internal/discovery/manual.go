package discovery

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// blockedCIDRs are addresses that must never be fetched on a user's behalf.
// The cloud metadata endpoints hand out credentials to anything that asks from
// inside the instance, so a portal that will fetch an arbitrary URL is exactly
// the tool an attacker needs.
var blockedCIDRs = []string{
	"169.254.0.0/16", // link-local, incl. 169.254.169.254 metadata
	"127.0.0.0/8",    // loopback: the portal's own admin surface
	"::1/128",
	"fe80::/10",
	"fd00:ec2::254/128", // AWS IMDSv2 over IPv6
}

// ResolveManualSpecURL turns an address a person typed into one the poller can
// actually use.
//
// A bare path is resolved against the address the registry already polls — that
// is the common case, and nobody should have to type a host they are shown
// anyway.
//
// A full address is kept as typed, host and all. It used to be rewritten onto
// the in-cluster address, on the theory that the two are the same service by
// different names. They are not always: plenty of services publish their
// documentation only through the ingress — a rewrite rule, a sidecar, a route
// that simply is not wired on the backend port. Someone who types the public
// address has looked, found the specification there, and is telling the portal
// where it is. Answering with a different address and then failing to fetch it
// is both wrong and baffling.
//
// The confinement stays: only the service's own two addresses are accepted.
// Without that rule this endpoint would let anyone who can reach the portal —
// which has no login for reading — make it fetch an arbitrary URL from inside
// the cluster.
func ResolveManualSpecURL(baseURL, publicHost, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return "", fmt.Errorf("адрес сервиса %q непригоден", baseURL)
	}

	target, err := base.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q — не похоже на адрес или путь", raw)
	}
	switch target.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("допустимы только http и https, получено %q", target.Scheme)
	}

	sameAsBase := strings.EqualFold(target.Host, base.Host)
	sameAsPublic := publicHost != "" && strings.EqualFold(hostOnly(target.Host), hostOnly(publicHost))
	if !sameAsBase && !sameAsPublic {
		shown := publicHost
		if shown == "" {
			shown = base.Host
		}
		return "", fmt.Errorf(
			"адрес должен быть на %s — введите путь, например /v3/api-docs, "+
				"либо заведите %s отдельным сервисом", shown, target.Host)
	}
	if err := checkNotBlocked(target.Hostname()); err != nil {
		return "", err
	}

	// Хост из введённого адреса сохраняется: человек выбрал его сознательно.
	// Голый путь хоста не несёт и достраивается по адресу опроса.
	if typed, err := url.Parse(raw); err == nil && typed.Host != "" {
		return target.String(), nil
	}
	resolved := *base
	resolved.Path = target.Path
	resolved.RawQuery = target.RawQuery
	return resolved.String(), nil
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// checkNotBlocked rejects literal addresses in the reserved ranges. It does not
// resolve names: a DNS lookup here would be advisory anyway, since the name can
// resolve differently by the time the request goes out.
func checkNotBlocked(host string) error {
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	for _, cidr := range blockedCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return fmt.Errorf("адрес %s принадлежит зарезервированному диапазону (%s) и не может быть запрошен", host, cidr)
		}
	}
	return nil
}
