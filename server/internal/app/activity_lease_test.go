package app

import (
	"testing"
	"time"

	"morde/server/internal/domain"
)

func TestHostActivityLeaseValidation(t *testing.T) {
	valid := HostActivityLease{
		HostID: "h1234567", LeaseID: "preview-session_1", RuntimeGeneration: 1,
		ControlVersion: 0, Kind: HostActivityLeaseKindPreview, TTL: 30 * time.Second,
	}
	if err := valid.ValidateForAcquireOrRenew(); err != nil {
		t.Fatalf("valid lease rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*HostActivityLease)
	}{
		{name: "invalid kind", mutate: func(lease *HostActivityLease) { lease.Kind = "other" }},
		{name: "empty id", mutate: func(lease *HostActivityLease) { lease.LeaseID = "" }},
		{name: "unsafe id", mutate: func(lease *HostActivityLease) { lease.LeaseID = "preview/session" }},
		{name: "zero generation", mutate: func(lease *HostActivityLease) { lease.RuntimeGeneration = 0 }},
		{name: "negative control version", mutate: func(lease *HostActivityLease) { lease.ControlVersion = -1 }},
		{name: "short ttl", mutate: func(lease *HostActivityLease) { lease.TTL = HostActivityLeaseMinTTL - time.Nanosecond }},
		{name: "long ttl", mutate: func(lease *HostActivityLease) { lease.TTL = HostActivityLeaseMaxTTL + time.Nanosecond }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			lease := valid
			test.mutate(&lease)
			if err := lease.ValidateForAcquireOrRenew(); domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("invalid lease error = %v", err)
			}
		})
	}
}
