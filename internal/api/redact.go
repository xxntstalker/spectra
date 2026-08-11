package api

import (
	"net"
	"net/url"
	"strings"

	"spectra/internal/store"
)

// Reading is open to everyone on the network, but "everyone" should not be
// handed a map of the cluster.
//
// The internal address, the backend Service and its port, the namespace and
// the Ingress name are operational details: together they describe how to
// reach services directly, bypassing whatever the Ingress enforces. The public
// hostname stays — that is the address a consumer of the API actually needs,
// and it is the point of having a portal.
//
// Redaction happens here rather than in the browser because /api/services is
// public: hiding a field in JavaScript hides it from nobody.
func redactService(s store.Service) store.Service {
	s.BaseURL = ""
	// The fallback address is whichever of the two BaseURL is not, so depending
	// on SPECTRA_K8S_TARGET it is the in-cluster one. Blanked unconditionally
	// rather than reasoned about per configuration — the public host is already
	// published as Host, so nothing is lost.
	s.FallbackURL = ""
	s.UsingFallback = false
	s.Backend = ""
	s.Namespace = ""
	s.IngressName = ""
	s.Key = ""
	s.SpecURL = ""
	s.SpecURLSource = ""
	s.ManualSpecURL = ""
	// Probe errors quote the addresses that were tried.
	s.LastError = ""
	// Owner deliberately stays: "кого спросить про этот API" is the question
	// a visitor comes with, and a team name with a contact is not topology.
	// LastCheckedAt deliberately stays: it is how a reader judges whether the
	// specification in front of them is still what the service serves. That
	// question belongs to everyone, and a timestamp reveals no topology.
	return s
}

// publicAddress returns the address of the service that may be shown to
// anyone: the one built on its public hostname.
//
// The registry holds two addresses and which of them is public depends on
// SPECTRA_K8S_TARGET, so it is found by matching against the hostname rather
// than assumed. Everything else is an in-cluster address and stays hidden.
func publicAddress(s store.Service) string {
	if s.Host == "" {
		return ""
	}
	scheme := "http"
	for _, candidate := range []string{s.BaseURL, s.FallbackURL} {
		if candidate == "" {
			continue
		}
		u, err := url.Parse(candidate)
		if err != nil {
			continue
		}
		if strings.EqualFold(u.Hostname(), hostnameOf(s.Host)) {
			// Прямое попадание: этот адрес и построен на публичном хосте, со
			// своей схемой, портом и префиксом пути.
			return candidate
		}
		if u.Scheme != "" {
			scheme = u.Scheme
		}
	}
	// Запасного адреса ещё нет — так выглядят записи до первого сканирования
	// после обновления. Схема берётся от известного адреса: это догадка, но
	// куда более осмысленная, чем оставить спеку без адресов вовсе и получить
	// в примерах curl адрес самого портала.
	return scheme + "://" + s.Host
}

// hostnameOf drops a port if the stored host carries one.
func hostnameOf(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func redactServices(services []store.Service) []store.Service {
	out := make([]store.Service, len(services))
	for i, s := range services {
		out[i] = redactService(s)
	}
	return out
}
