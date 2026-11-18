package app

import (
	"context"
	"os"
	"regexp"
	"testing"

	"morde/server/internal/domain/identity"
)

func TestModelContextCapacityCatalogCoversEveryDefaultPreset(t *testing.T) {
	source, err := os.ReadFile("../../../web/portal/src/app/model-presets.ts")
	if err != nil {
		t.Fatal(err)
	}
	preset := regexp.MustCompile(`\{ id: '([^']+)', name: '[^']+', baseURL: '([^']+)', modelName: '([^']+)' \}`)
	rows := preset.FindAllStringSubmatch(string(source), -1)
	if len(rows) != 5 {
		t.Fatalf("expected the five checked-in system presets, got %d", len(rows))
	}
	want := map[string]int{
		"gpt-6-luna":     1_050_000,
		"gpt-6.1-sol":    1_050_000,
		"gpt-6-astra":    1_050_000,
		"deepseek-flash": 1_048_576,
		"glm-5.1":        200_000,
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		presetID, baseURL, modelID := row[1], row[2], row[3]
		if seen[presetID] {
			t.Errorf("duplicate system preset %q", presetID)
		}
		seen[presetID] = true
		entry, ok := lookupModelContextCapacity(baseURL, modelID)
		if !ok {
			t.Errorf("system preset %q has no route-specific capacity entry", presetID)
			continue
		}
		if entry.contextTokens != want[presetID] || entry.sourceURL == "" || entry.catalogVersion != modelContextCapacityCatalogVersion {
			t.Errorf("system preset %q has incomplete capacity metadata: %+v", presetID, entry)
		}
	}
	for presetID := range want {
		if !seen[presetID] {
			t.Errorf("capacity catalog test does not cover system preset %q", presetID)
		}
	}
}

func TestModelContextCapacityRequiresExactProviderRoute(t *testing.T) {
	if _, ok := lookupModelContextCapacity("https://relay.example/v1", "gpt-6-luna"); ok {
		t.Fatal("OpenAI model capacity leaked to a different provider endpoint")
	}
	if _, ok := lookupModelContextCapacity("https://api.openai.com/v1", "GPT-6-Luna"); ok {
		t.Fatal("model capacity lookup must use the exact model id")
	}
	if _, ok := lookupModelContextCapacity("https://api.openai.com/v1/", "gpt-6-luna"); !ok {
		t.Fatal("trailing slash changed the canonical provider route")
	}
}

func TestDefaultModelConnectionUsesCatalogAndRejectsManualCapacity(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	service := modelConnectionTestService(t, store, &modelClientStub{})
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t1234567"}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "OpenAI preset", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-6-luna", APIKey: "secret-value", Shared: true,
	})
	if err != nil || created.ContextWindowTokens == nil || *created.ContextWindowTokens != 1_050_000 ||
		created.ContextWindowSource != ModelContextWindowModelCatalog || created.ContextWindowCatalogVersion == nil ||
		*created.ContextWindowCatalogVersion != modelContextCapacityCatalogVersion || created.ContextWindowResolvedAt == nil {
		t.Fatalf("default model capacity = %+v, %v", created, err)
	}
	window := 131_072
	if _, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "OpenAI preset with manual window", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-6-luna", APIKey: "secret-value", Shared: true,
		ContextWindowOverrideTokens: &window, ContextWindowOverridePresent: true,
	}); err == nil {
		t.Fatal("system preset accepted a manual capacity override")
	}
	customRoute, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "OpenAI compatible route", BaseURL: "https://api.openai.com/v1/proxy", ModelName: "gpt-6-luna", APIKey: "secret-value", Shared: true,
		ContextWindowOverrideTokens: &window, ContextWindowOverridePresent: true,
	})
	if err != nil || customRoute.ContextWindowTokens == nil || *customRoute.ContextWindowTokens != window ||
		customRoute.ContextWindowSource != ModelContextWindowOverride {
		t.Fatalf("custom route override = %+v, %v", customRoute, err)
	}
}
