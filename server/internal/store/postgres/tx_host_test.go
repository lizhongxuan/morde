package postgres

import (
	"errors"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

func TestValidateAllocation(t *testing.T) {
	if err := validateAllocation("cpu_milli", 500, 250, 1000, domain.CodeQuotaExceeded); err != nil {
		t.Fatalf("within-limit allocation: %v", err)
	}
	err := validateAllocation("cpu_milli", 800, 250, 1000, domain.CodeQuotaExceeded)
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-limit code = %q, error=%v", domain.CodeOf(err), err)
	}
	var detailErr *domain.Error
	if !errors.As(err, &detailErr) || detailErr.Details["dimension"] != "cpu_milli" || detailErr.Details["limit"] != int64(1000) {
		t.Fatalf("over-limit details = %#v", detailErr)
	}
	if err := validateAllocation("mem_mib", 2000, 1, 1000, domain.CodeCapacityExceeded); domain.CodeOf(err) != domain.CodeCapacityExceeded {
		t.Fatalf("existing over-capacity allocation error = %v", err)
	}
	if err := validateAllocation("mem_mib", 0, 1, 0, domain.CodeQuotaExceeded); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("zero quota error = %v", err)
	}
}

func TestValidateCreateHost(t *testing.T) {
	hostID, err := domain.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	params := app.CreateHostParams{
		Host: host.Host{
			ID: hostID, OwnerID: ownerID, TemplateID: "test", RuntimeProfile: "default",
			State: host.StateCreating, CPUMilli: 500, MemMiB: 512, CPULimitMilli: 1000, MemLimitMiB: 1024,
			AIOKeyEnc: []byte("ciphertext"), RuntimeCredentialEnc: []byte("runtime ciphertext"), RuntimeGeneration: 1,
		},
		Capacity: app.Capacity{CPUMilli: 2000, MemMiB: 2048},
	}
	if err := validateCreateHost(params); err != nil {
		t.Fatalf("valid create host params: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*app.CreateHostParams)
	}{
		{name: "empty id", mutate: func(p *app.CreateHostParams) { p.Host.ID = "" }},
		{name: "wrong state", mutate: func(p *app.CreateHostParams) { p.Host.State = host.StateRunning }},
		{name: "missing encrypted key", mutate: func(p *app.CreateHostParams) { p.Host.AIOKeyEnc = nil }},
		{name: "missing runtime credential", mutate: func(p *app.CreateHostParams) { p.Host.RuntimeCredentialEnc = nil }},
		{name: "empty runtime credential", mutate: func(p *app.CreateHostParams) { p.Host.RuntimeCredentialEnc = []byte{} }},
		{name: "zero runtime generation", mutate: func(p *app.CreateHostParams) { p.Host.RuntimeGeneration = 0 }},
		{name: "negative runtime generation", mutate: func(p *app.CreateHostParams) { p.Host.RuntimeGeneration = -1 }},
		{name: "request exceeds limit", mutate: func(p *app.CreateHostParams) { p.Host.CPULimitMilli = 499 }},
		{name: "invalid workspace ID", mutate: func(p *app.CreateHostParams) { p.WorkspaceIDs = []string{"invalid"} }},
		{name: "capacity missing", mutate: func(p *app.CreateHostParams) { p.Capacity.MemMiB = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copy := params
			copy.WorkspaceIDs = append([]string(nil), params.WorkspaceIDs...)
			tc.mutate(&copy)
			if err := validateCreateHost(copy); domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("invalid create host error = %v", err)
			}
		})
	}
}
