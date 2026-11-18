package app

import (
	"net/url"
	"strings"
	"time"

	"morde/server/internal/domain"
)

const modelContextCapacityCatalogVersion = "2026-11-18"

type modelContextCapacityEntry struct {
	provider       string
	baseURL        string
	modelID        string
	contextTokens  int
	sourceURL      string
	catalogVersion string
}

var modelContextCapacityCatalog = []modelContextCapacityEntry{
	// Use API model limits; product-specific Codex budgets are not API endpoint limits.
	{provider: "OpenAI API", baseURL: "https://api.openai.com/v1", modelID: "gpt-6-luna", contextTokens: 1_050_000, sourceURL: "https://developers.openai.com/api/docs/models/gpt-6-luna", catalogVersion: modelContextCapacityCatalogVersion},
	{provider: "OpenAI API", baseURL: "https://api.openai.com/v1", modelID: "gpt-6.1-sol", contextTokens: 1_050_000, sourceURL: "https://developers.openai.com/api/docs/models/gpt-6.1-sol", catalogVersion: modelContextCapacityCatalogVersion},
	{provider: "OpenAI API", baseURL: "https://api.openai.com/v1", modelID: "gpt-6-astra", contextTokens: 1_050_000, sourceURL: "https://developers.openai.com/api/docs/models/gpt-6-astra", catalogVersion: modelContextCapacityCatalogVersion},
	{provider: "DeepSeek API", baseURL: "https://api.deepseek.com", modelID: "deepseek-flash", contextTokens: 1_048_576, sourceURL: "https://api-docs.deepseek.com/api/list-models/", catalogVersion: modelContextCapacityCatalogVersion},
	{provider: "Z.AI", baseURL: "https://api.z.ai/api/paas/v4", modelID: "glm-5.1", contextTokens: 200_000, sourceURL: "https://docs.z.ai/guides/llm/glm-5.1", catalogVersion: modelContextCapacityCatalogVersion},
}

func lookupModelContextCapacity(baseURL, modelID string) (modelContextCapacityEntry, bool) {
	key := modelContextCapacityRouteKey(baseURL, modelID)
	if key == "" {
		return modelContextCapacityEntry{}, false
	}
	for _, entry := range modelContextCapacityCatalog {
		if modelContextCapacityRouteKey(entry.baseURL, entry.modelID) == key {
			return entry, true
		}
	}
	return modelContextCapacityEntry{}, false
}

func rejectDefaultModelContextOverride(baseURL, modelID string, override *int) error {
	if override == nil {
		return nil
	}
	if _, ok := lookupModelContextCapacity(baseURL, modelID); ok {
		return domain.InvalidArgument("context_window_override_tokens", "system model capacity is maintained by Morde")
	}
	return nil
}

func applyModelContextCapacity(record *ModelConnectionRecord, now time.Time) {
	if entry, ok := lookupModelContextCapacity(record.BaseURL, record.ModelName); ok {
		contextTokens := entry.contextTokens
		resolvedAt := now.UTC()
		version := entry.catalogVersion
		record.ContextWindowTokens = &contextTokens
		record.ContextWindowOverrideTokens = nil
		record.ContextWindowSource = ModelContextWindowModelCatalog
		record.ContextWindowResolvedAt = &resolvedAt
		record.ContextWindowCatalogVersion = &version
		return
	}
	if record.ContextWindowOverrideTokens != nil {
		record.ContextWindowTokens = copyInt(record.ContextWindowOverrideTokens)
		if record.ContextWindowSource != ModelContextWindowLegacyManual {
			record.ContextWindowSource = ModelContextWindowOverride
		}
		record.ContextWindowResolvedAt = nil
		record.ContextWindowCatalogVersion = nil
		return
	}
	record.ContextWindowTokens = nil
	record.ContextWindowSource = ModelContextWindowUnknown
	record.ContextWindowResolvedAt = nil
	record.ContextWindowCatalogVersion = nil
}

func modelContextCapacityRouteKey(baseURL, modelID string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || strings.TrimSpace(modelID) == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host) + strings.TrimRight(parsed.EscapedPath(), "/") + "\x00" + modelID
}
