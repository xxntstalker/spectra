package api

import "spectra/internal/store"

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
	s.Backend = ""
	s.Namespace = ""
	s.IngressName = ""
	s.Key = ""
	s.SpecURL = ""
	s.SpecURLSource = ""
	s.ManualSpecURL = ""
	// Probe errors quote the addresses that were tried.
	s.LastError = ""
	return s
}

func redactServices(services []store.Service) []store.Service {
	out := make([]store.Service, len(services))
	for i, s := range services {
		out[i] = redactService(s)
	}
	return out
}
