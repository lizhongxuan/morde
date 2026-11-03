package app

import (
	"context"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

type ConfiguredProjectRates struct {
	rates map[string]project.RateSnapshot
}

func NewConfiguredProjectRates(rates []project.RateSnapshot) (*ConfiguredProjectRates, error) {
	configured := &ConfiguredProjectRates{rates: make(map[string]project.RateSnapshot, len(rates))}
	for _, rate := range rates {
		if rate.Validate() != nil {
			return nil, domain.InvalidArgument("project_rates", "project model rate is invalid")
		}
		if _, exists := configured.rates[rate.ModelID]; exists {
			return nil, domain.InvalidArgument("project_rates", "project model rate model_id is duplicated")
		}
		configured.rates[rate.ModelID] = rate
	}
	return configured, nil
}

func (r *ConfiguredProjectRates) ProjectRateSnapshot(_ context.Context, modelID string) (project.RateSnapshot, error) {
	if r == nil || r.rates == nil {
		return project.RateSnapshot{}, fmt.Errorf("project model rates are unavailable")
	}
	rate, ok := r.rates[modelID]
	if !ok {
		return project.RateSnapshot{}, fmt.Errorf("project model rate is not configured")
	}
	return rate, nil
}
