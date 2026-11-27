package app

import (
	"net/url"
	"strings"
	"time"

	"morde/server/internal/domain"
)

const modelMaxOutputCapacityCatalogVersion = "2026-11-27"

const MaxModelOutputTokens = 2_000_000

type ModelMaxOutputSource string

const (
	ModelMaxOutputUnknown      ModelMaxOutputSource = "unknown"
	ModelMaxOutputOverride     ModelMaxOutputSource = "override"
	ModelMaxOutputModelCatalog ModelMaxOutputSource = "model_catalog"
)

type modelMaxOutputCapacityEntry struct {
	baseURL        string
	modelID        string
	maxOutput      int
	sourceURL      string
	catalogVersion string
}

var modelMaxOutputCapacityCatalog = []modelMaxOutputCapacityEntry{
	{baseURL: "https://api.openai.com/v1", modelID: "gpt-6-luna", maxOutput: 128_000,
		sourceURL: "https://developers.openai.com/api/docs/models/gpt-6-luna", catalogVersion: modelMaxOutputCapacityCatalogVersion},
	{baseURL: "https://api.openai.com/v1", modelID: "gpt-6.1-sol", maxOutput: 128_000,
		sourceURL: "https://developers.openai.com/api/docs/models/gpt-6.1-sol", catalogVersion: modelMaxOutputCapacityCatalogVersion},
	{baseURL: "https://api.openai.com/v1", modelID: "gpt-6-astra", maxOutput: 128_000,
		sourceURL: "https://developers.openai.com/api/docs/models/gpt-6-astra", catalogVersion: modelMaxOutputCapacityCatalogVersion},
	{baseURL: "https://api.deepseek.com", modelID: "deepseek-flash", maxOutput: 393_216,
		sourceURL: "https://api-docs.deepseek.com/api/create-chat-completion/", catalogVersion: modelMaxOutputCapacityCatalogVersion},
}

func lookupModelMaxOutputCapacity(baseURL, modelID string) (modelMaxOutputCapacityEntry, bool) {
	key := modelMaxOutputCapacityRouteKey(baseURL, modelID)
	if key == "" {
		return modelMaxOutputCapacityEntry{}, false
	}
	for _, entry := range modelMaxOutputCapacityCatalog {
		if modelMaxOutputCapacityRouteKey(entry.baseURL, entry.modelID) == key {
			return entry, true
		}
	}
	return modelMaxOutputCapacityEntry{}, false
}

func rejectSystemModelMaxOutputOverride(baseURL, modelID string, override *int) error {
	if override == nil {
		return nil
	}
	if _, ok := lookupModelMaxOutputCapacity(baseURL, modelID); ok {
		return domain.InvalidArgument("max_output_override_tokens", "system model output capacity is maintained by Morde")
	}
	return nil
}

func explicitModelMaxOutputOverride(command ModelConnectionCommand) (*int, error) {
	if !command.MaxOutputOverridePresent {
		return nil, nil
	}
	if command.MaxOutputOverrideTokens != nil && (*command.MaxOutputOverrideTokens < 1 || *command.MaxOutputOverrideTokens > MaxModelOutputTokens) {
		return nil, domain.InvalidArgument("max_output_override_tokens", "max output override is outside the allowed range")
	}
	return copyInt(command.MaxOutputOverrideTokens), nil
}

func applyModelMaxOutputCapacity(record *ModelConnectionRecord, now time.Time) {
	if entry, ok := lookupModelMaxOutputCapacity(record.BaseURL, record.ModelName); ok {
		maxOutput := entry.maxOutput
		resolvedAt := now.UTC()
		version := entry.catalogVersion
		sourceURL := entry.sourceURL
		record.MaxOutputTokens = &maxOutput
		record.MaxOutputOverrideTokens = nil
		record.MaxOutputSource = ModelMaxOutputModelCatalog
		record.MaxOutputResolvedAt = &resolvedAt
		record.MaxOutputCatalogVersion = &version
		record.MaxOutputSourceURL = &sourceURL
		return
	}
	if record.MaxOutputOverrideTokens != nil {
		record.MaxOutputTokens = copyInt(record.MaxOutputOverrideTokens)
		record.MaxOutputSource = ModelMaxOutputOverride
		record.MaxOutputResolvedAt = nil
		record.MaxOutputCatalogVersion = nil
		record.MaxOutputSourceURL = nil
		return
	}
	record.MaxOutputTokens = nil
	record.MaxOutputOverrideTokens = nil
	record.MaxOutputSource = ModelMaxOutputUnknown
	record.MaxOutputResolvedAt = nil
	record.MaxOutputCatalogVersion = nil
	record.MaxOutputSourceURL = nil
}

func modelMaxOutputCapacityNeedsRefresh(record ModelConnectionRecord) bool {
	entry, ok := lookupModelMaxOutputCapacity(record.BaseURL, record.ModelName)
	if !ok {
		return record.MaxOutputSource == ModelMaxOutputModelCatalog
	}
	return record.MaxOutputSource != ModelMaxOutputModelCatalog || record.MaxOutputTokens == nil || *record.MaxOutputTokens != entry.maxOutput ||
		record.MaxOutputCatalogVersion == nil || *record.MaxOutputCatalogVersion != entry.catalogVersion ||
		record.MaxOutputSourceURL == nil || *record.MaxOutputSourceURL != entry.sourceURL || record.MaxOutputOverrideTokens != nil
}

func modelMaxOutputCapacityRouteKey(baseURL, modelID string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || strings.TrimSpace(modelID) == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host) + strings.TrimRight(parsed.EscapedPath(), "/") + "\x00" + modelID
}
