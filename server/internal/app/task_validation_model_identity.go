package app

import (
	"net/url"
	"strings"
)

const taskValidationModelIdentityCatalogVersion = "2026-11-20"

type TaskValidationModelIdentity struct {
	Family         string `json:"family"`
	CatalogVersion string `json:"catalog_version"`
}

type taskValidationModelIdentityEntry struct {
	baseURL, configuredModel, reportedModel string
	identity                                TaskValidationModelIdentity
}

// These exact IDs appear in the providers' API documentation as of 2026-11-20:
// https://developers.openai.com/api/docs/models,
// https://api-docs.deepseek.com/api/list-models/, and
// https://docs.z.ai/guides/capabilities/mcp-call. Unknown aliases and compatible
// endpoints stay unclassified.
var taskValidationModelIdentityCatalog = []taskValidationModelIdentityEntry{
	{baseURL: "https://api.openai.com/v1", configuredModel: "gpt-6-luna", reportedModel: "gpt-6-luna", identity: TaskValidationModelIdentity{Family: "openai-gpt6", CatalogVersion: taskValidationModelIdentityCatalogVersion}},
	{baseURL: "https://api.openai.com/v1", configuredModel: "gpt-6.2-astra", reportedModel: "gpt-6.2-astra", identity: TaskValidationModelIdentity{Family: "openai-gpt6", CatalogVersion: taskValidationModelIdentityCatalogVersion}},
	{baseURL: "https://api.openai.com/v1", configuredModel: "gpt-6.2-sol", reportedModel: "gpt-6.2-sol", identity: TaskValidationModelIdentity{Family: "openai-gpt6", CatalogVersion: taskValidationModelIdentityCatalogVersion}},
	{baseURL: "https://api.openai.com/v1", configuredModel: "gpt-6.1-terra", reportedModel: "gpt-6.1-terra", identity: TaskValidationModelIdentity{Family: "openai-gpt6", CatalogVersion: taskValidationModelIdentityCatalogVersion}},
	{baseURL: "https://api.deepseek.com", configuredModel: "deepseek-flash", reportedModel: "deepseek-flash", identity: TaskValidationModelIdentity{Family: "deepseek-v4", CatalogVersion: taskValidationModelIdentityCatalogVersion}},
	{baseURL: "https://api.deepseek.com", configuredModel: "deepseek-v4-pro", reportedModel: "deepseek-v4-pro", identity: TaskValidationModelIdentity{Family: "deepseek-v4", CatalogVersion: taskValidationModelIdentityCatalogVersion}},
	{baseURL: "https://api.z.ai/api/paas/v4", configuredModel: "glm-4.6", reportedModel: "glm-4.6", identity: TaskValidationModelIdentity{Family: "zai-glm", CatalogVersion: taskValidationModelIdentityCatalogVersion}},
}

func ResolveTaskValidationModelIdentity(baseURL, configuredModel, reportedModel string) (TaskValidationModelIdentity, bool) {
	route := taskValidationModelIdentityRouteKey(baseURL)
	if route == "" || configuredModel == "" || strings.TrimSpace(configuredModel) != configuredModel ||
		reportedModel == "" || strings.TrimSpace(reportedModel) != reportedModel || !ValidProviderReportedModel(reportedModel) {
		return TaskValidationModelIdentity{}, false
	}
	for _, entry := range taskValidationModelIdentityCatalog {
		if taskValidationModelIdentityRouteKey(entry.baseURL) == route && entry.configuredModel == configuredModel && entry.reportedModel == reportedModel {
			return entry.identity, true
		}
	}
	return TaskValidationModelIdentity{}, false
}

func taskValidationModelIdentityRouteKey(baseURL string) string {
	if baseURL == "" || strings.TrimSpace(baseURL) != baseURL || len(baseURL) > 2048 || strings.Contains(baseURL, "#") {
		return ""
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || strings.HasSuffix(parsed.Host, ":") ||
		parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return ""
	}
	return "https://" + strings.ToLower(parsed.Host) + strings.TrimRight(parsed.EscapedPath(), "/")
}
