package orchestrator

import (
	"net/http"
	"sort"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/app"
	"morde/server/internal/config"
)

type Router struct {
	clients map[string]app.SandboxClient
}

func NewRouter(clients map[string]app.SandboxClient) *Router {
	copy := make(map[string]app.SandboxClient, len(clients))
	for name, client := range clients {
		copy[name] = client
	}
	return &Router{clients: copy}
}

func FromConfig(cfg config.Orchestrator, httpClient *http.Client, reg prometheus.Registerer) *Router {
	clients := make(map[string]app.SandboxClient, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		profile.Name = name
		clients[name] = NewClient(profile, httpClient, reg)
	}
	return NewRouter(clients)
}

func (r *Router) For(profile string) (app.SandboxClient, error) {
	client, ok := r.clients[profile]
	if !ok || client == nil {
		return nil, app.ErrSandboxUnsupported
	}
	return client, nil
}

func (r *Router) Profiles() []string {
	profiles := make([]string, 0, len(r.clients))
	for profile := range r.clients {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	return profiles
}

var _ app.SandboxRouter = (*Router)(nil)
