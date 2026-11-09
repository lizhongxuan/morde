package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"morde/server/internal/domain/task"
)

const (
	cloudDSHBuiltinSearchCacheTTL      = time.Minute
	cloudDSHBuiltinSearchCacheEntries  = 128
	cloudDSHBuiltinSearchCacheMaxBytes = 24 << 10
)

type cloudDSHBuiltinSearchCacheKey struct {
	OwnerID       string `json:"owner_id"`
	ActorID       string `json:"actor_id"`
	ProjectID     string `json:"project_id"`
	TaskID        string `json:"task_id"`
	GrantVersion  int64  `json:"grant_version"`
	ToolName      string `json:"tool_name"`
	ArgumentsJSON string `json:"arguments_json"`
}

type cloudDSHBuiltinSearchCacheEntry struct {
	expiresAt time.Time
	result    MCPConnectorToolResult
}

type cloudDSHBuiltinSearchCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]cloudDSHBuiltinSearchCacheEntry
	now     func() time.Time
}

type cloudDSHCachedBuiltinSearchClient struct {
	delegate MCPConnectorToolClient
	cache    *cloudDSHBuiltinSearchCache
	task     task.Task
}

func newCloudDSHBuiltinSearchCache() *cloudDSHBuiltinSearchCache {
	return &cloudDSHBuiltinSearchCache{entries: make(map[[sha256.Size]byte]cloudDSHBuiltinSearchCacheEntry), now: time.Now}
}

func (c *cloudDSHBuiltinSearchCache) key(current task.Task, toolName string, arguments []byte) [sha256.Size]byte {
	value, _ := json.Marshal(cloudDSHBuiltinSearchCacheKey{
		OwnerID: current.OwnerID, ActorID: taskActorID(current), ProjectID: current.ProjectID, TaskID: current.ID,
		GrantVersion: current.Authorization.Version, ToolName: toolName, ArgumentsJSON: string(arguments),
	})
	return sha256.Sum256(value)
}

func (c *cloudDSHBuiltinSearchCache) get(key [sha256.Size]byte) (MCPConnectorToolResult, bool) {
	if c == nil {
		return MCPConnectorToolResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return MCPConnectorToolResult{}, false
	}
	if !c.clock().Before(entry.expiresAt) {
		delete(c.entries, key)
		return MCPConnectorToolResult{}, false
	}
	return cloneCloudDSHBuiltinSearchResult(entry.result), true
}

func (c *cloudDSHBuiltinSearchCache) put(key [sha256.Size]byte, result MCPConnectorToolResult) {
	if c == nil || !cacheableCloudDSHBuiltinSearchResult(result) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	for existing, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, existing)
		}
	}
	if len(c.entries) >= cloudDSHBuiltinSearchCacheEntries {
		var oldestKey [sha256.Size]byte
		var oldest time.Time
		for existing, entry := range c.entries {
			if oldest.IsZero() || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = existing, entry.expiresAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = cloudDSHBuiltinSearchCacheEntry{expiresAt: now.Add(cloudDSHBuiltinSearchCacheTTL), result: cloneCloudDSHBuiltinSearchResult(result)}
}

func (c *cloudDSHBuiltinSearchCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func cacheableCloudDSHBuiltinSearchResult(result MCPConnectorToolResult) bool {
	if result.IsError || result.UnsupportedContent || len(result.Text) == 0 || len(result.StructuredContent) > cloudDSHBuiltinSearchCacheMaxBytes {
		return false
	}
	total := len(result.StructuredContent)
	hasText := false
	for _, text := range result.Text {
		total += len(text)
		if total > cloudDSHBuiltinSearchCacheMaxBytes {
			return false
		}
		hasText = hasText || strings.TrimSpace(text) != ""
	}
	return hasText
}

func cloneCloudDSHBuiltinSearchResult(result MCPConnectorToolResult) MCPConnectorToolResult {
	return MCPConnectorToolResult{
		IsError: result.IsError, UnsupportedContent: result.UnsupportedContent,
		StructuredContent: append(json.RawMessage(nil), result.StructuredContent...), Text: append([]string(nil), result.Text...),
	}
}

func (c cloudDSHCachedBuiltinSearchClient) ListTools(ctx context.Context, endpoint string, headers http.Header) ([]MCPConnectorToolDefinition, error) {
	if c.delegate == nil {
		return nil, errors.New("built-in search client is unavailable")
	}
	return c.delegate.ListTools(ctx, endpoint, headers)
}

func (c cloudDSHCachedBuiltinSearchClient) CallTool(ctx context.Context, endpoint string, headers http.Header, toolName string,
	arguments json.RawMessage) (MCPConnectorToolResult, error) {
	if ctx == nil || c.delegate == nil {
		return MCPConnectorToolResult{}, errors.New("built-in search client is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return MCPConnectorToolResult{}, err
	}
	if toolName != "searxng_web_search" || c.cache == nil {
		return c.delegate.CallTool(ctx, endpoint, headers, toolName, arguments)
	}
	key := c.cache.key(c.task, toolName, arguments)
	if result, ok := c.cache.get(key); ok {
		return result, nil
	}
	result, err := c.delegate.CallTool(ctx, endpoint, headers, toolName, arguments)
	if err == nil && ctx.Err() == nil {
		c.cache.put(key, result)
	}
	return result, err
}
