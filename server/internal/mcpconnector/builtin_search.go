package mcpconnector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const builtinSearchTimeout = 25 * time.Second

// BuiltinSearchClient runs the fixed search MCP executable in a fresh process
// per call, so its search and page caches never cross task boundaries.
type BuiltinSearchClient struct {
	nodePath    string
	scriptPath  string
	searxngURL  string
	concurrency chan struct{}
}

func NewBuiltinSearchClient(nodePath, scriptPath, searxngURL string, maxConcurrent int) (*BuiltinSearchClient, error) {
	if !filepath.IsAbs(nodePath) || filepath.Clean(nodePath) != nodePath || !filepath.IsAbs(scriptPath) ||
		filepath.Clean(scriptPath) != scriptPath || maxConcurrent < 1 || maxConcurrent > 16 || !validBuiltinSearxngURL(searxngURL) {
		return nil, errors.New("built-in search configuration is invalid")
	}
	for _, file := range []string{nodePath, scriptPath} {
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("built-in search executable is unavailable")
		}
	}
	if info, _ := os.Stat(nodePath); info.Mode()&0111 == 0 {
		return nil, errors.New("built-in search executable is not executable")
	}
	metadata, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(scriptPath)), "package.json"))
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err != nil || len(metadata) > 64<<10 || json.Unmarshal(metadata, &pkg) != nil || pkg.Name != "mcp-searxng" || pkg.Version != "2.10.0" {
		return nil, errors.New("built-in search package must be mcp-searxng 2.10.0")
	}
	readerPatch, err := os.ReadFile(filepath.Join(filepath.Dir(scriptPath), "url-reader.js"))
	if err != nil || len(readerPatch) > 1<<20 || !strings.Contains(string(readerPatch), "MORDE_PAGE_DATE_METADATA_PATCH_V1") ||
		!strings.Contains(string(readerPatch), `import { extractPageDateMetadata } from "../../../patches/page-date.mjs";`) ||
		!strings.Contains(string(readerPatch), "extractPageDateMetadata(read.text)") {
		return nil, errors.New("built-in search requires the Morde page-date patch")
	}
	helperPath := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(scriptPath)))), "patches", "page-date.mjs")
	helper, err := os.ReadFile(helperPath)
	if err != nil || len(helper) > 64<<10 || !strings.Contains(string(helper), "export function extractPageDateMetadata") {
		return nil, errors.New("built-in search page-date helper is unavailable")
	}
	return &BuiltinSearchClient{nodePath: nodePath, scriptPath: scriptPath, searxngURL: searxngURL,
		concurrency: make(chan struct{}, maxConcurrent)}, nil
}

func validBuiltinSearxngURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" || parsed.Path != "" || parsed.Port() == "" {
		return false
	}
	port, err := strconv.Atoi(parsed.Port())
	return err == nil && port > 0 && port <= 65535 && parsed.Host == net.JoinHostPort("127.0.0.1", parsed.Port())
}

func (c *BuiltinSearchClient) CallTool(ctx context.Context, name string, arguments json.RawMessage) (ConnectorToolResult, error) {
	if c == nil || ctx == nil || !validBuiltinSearchArguments(name, arguments) {
		return ConnectorToolResult{}, errors.New("built-in search tool call is invalid")
	}
	callCtx, cancel := context.WithTimeout(ctx, builtinSearchTimeout)
	defer cancel()
	select {
	case c.concurrency <- struct{}{}:
		defer func() { <-c.concurrency }()
	case <-callCtx.Done():
		return ConnectorToolResult{}, errors.New("built-in search capacity or request timed out")
	}
	command := exec.CommandContext(callCtx, c.nodePath, c.scriptPath)
	command.Env = []string{
		"PATH=/usr/bin:/bin", "HOME=/nonexistent", "NODE_ENV=production", "SEARXNG_URL=" + c.searxngURL,
		"SEARXNG_LITE_TOOLS=true", "MCP_HTTP_ALLOW_PRIVATE_URLS=false", "SEARCH_CACHE_TTL_MS=300000", "CACHE_TTL_MS=300000",
		"SEARXNG_TIMEOUT_MS=10000", "FETCH_TIMEOUT_MS=10000", "SEARXNG_MAX_RESULTS=10", "SEARXNG_MAX_RESULT_CHARS=500",
		"URL_READ_MAX_CHARS=24000", "SEARXNG_MAX_RESPONSE_BYTES=1048576", "URL_READ_MAX_CONTENT_LENGTH_BYTES=1048576",
	}
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	client := sdk.NewClient(&sdk.Implementation{Name: "morde-builtin-search", Version: "1"}, &sdk.ClientOptions{})
	session, err := client.Connect(callCtx, &sdk.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		return ConnectorToolResult{}, errors.New("built-in search service is unavailable")
	}
	defer func() { _ = session.Close() }()
	var input map[string]any
	if json.Unmarshal(arguments, &input) != nil {
		return ConnectorToolResult{}, errors.New("built-in search arguments are invalid")
	}
	result, err := session.CallTool(callCtx, &sdk.CallToolParams{Name: name, Arguments: input})
	if err != nil || result == nil {
		return ConnectorToolResult{}, errors.New("built-in search returned no confirmed result")
	}
	return decodeConnectorToolResult(result), nil
}

func validBuiltinSearchArguments(name string, arguments json.RawMessage) bool {
	if len(arguments) == 0 || len(arguments) > 4096 || !utf8.Valid(arguments) {
		return false
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(arguments, &input) != nil {
		return false
	}
	key := "query"
	if name == "web_url_read" {
		key = "url"
	} else if name != "searxng_web_search" {
		return false
	}
	if key == "url" {
		if len(input) != 1 {
			return false
		}
	} else {
		if len(input) < 1 || len(input) > 2 {
			return false
		}
		for argument := range input {
			if argument != "query" && argument != "time_range" {
				return false
			}
		}
		if rawTimeRange, ok := input["time_range"]; ok {
			var timeRange string
			if json.Unmarshal(rawTimeRange, &timeRange) != nil || (timeRange != "day" && timeRange != "week" && timeRange != "month" && timeRange != "year") {
				return false
			}
		}
	}
	var value string
	if json.Unmarshal(input[key], &value) != nil || strings.TrimSpace(value) == "" || len(value) > 2048 || strings.ContainsRune(value, '\x00') {
		return false
	}
	if key == "url" {
		parsed, err := url.Parse(value)
		return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Hostname() != "" &&
			parsed.User == nil && parsed.Fragment == "" && parsed.Opaque == ""
	}
	return len(value) <= 512
}
