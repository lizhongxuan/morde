package gateway

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

func TestRejectedGatewayRequestsDoNotReachHostService(t *testing.T) {
	handler, g, _, _, hosts := newTestDeps(t)
	spy := &getCountingHostService{HostService: hosts}
	g.deps.Hosts = spy

	tests := []struct {
		name string
		host string
	}{
		{name: "invalid host", host: "unknown.other.test"},
		{name: "missing cookie", host: hostAuthority()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := spy.getCalls.Load()
			response := request(handler, http.MethodGet, tt.host, "/__ch/api/host", nil, "", "")
			if response.Code != http.StatusNotFound || response.Body.String() != validNotFoundBody {
				t.Errorf("rejected response = %d %q, want one not_found JSON body", response.Code, response.Body.String())
			}
			if got := spy.getCalls.Load() - before; got != 0 {
				t.Errorf("HostService.Get called %d times for a rejected request", got)
			}
		})
	}
}

type getCountingHostService struct {
	app.HostService
	getCalls atomic.Int32
}

func (s *getCountingHostService) Get(ctx context.Context, principal identity.Principal, hostID string) (host.Host, error) {
	s.getCalls.Add(1)
	return s.HostService.Get(ctx, principal, hostID)
}

var _ app.HostService = (*getCountingHostService)(nil)
