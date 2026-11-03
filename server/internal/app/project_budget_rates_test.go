package app

import (
	"context"
	"testing"

	"morde/server/internal/domain/project"
)

func TestConfiguredProjectRates(t *testing.T) {
	rate := project.RateSnapshot{Provider: "operator", ModelID: "p1234567", Model: "model-x", Currency: "USD",
		Version: 3, InputMicrosPerMillion: 100, OutputMicrosPerMillion: 200}
	configured, err := NewConfiguredProjectRates([]project.RateSnapshot{rate})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := configured.ProjectRateSnapshot(context.Background(), rate.ModelID); err != nil || got != rate {
		t.Fatalf("ProjectRateSnapshot() = %#v, %v", got, err)
	}
	if _, err := configured.ProjectRateSnapshot(context.Background(), "p7654321"); err == nil {
		t.Fatal("ProjectRateSnapshot() accepted an unconfigured model")
	}
	if _, err := NewConfiguredProjectRates([]project.RateSnapshot{rate, rate}); err == nil {
		t.Fatal("NewConfiguredProjectRates() accepted duplicate model IDs")
	}
}
