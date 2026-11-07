package mcpconnector

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuiltinSearchRejectsUnsafeConfigurationAndArguments(t *testing.T) {
	for _, endpoint := range []string{"http://localhost:8189", "http://192.168.1.1:8189", "https://127.0.0.1:8189", "http://127.0.0.1:8189/path", "http://user@127.0.0.1:8189", "http://127.0.0.1:0"} {
		if validBuiltinSearxngURL(endpoint) {
			t.Errorf("accepted invalid internal endpoint %q", endpoint)
		}
	}
	if !validBuiltinSearxngURL("http://127.0.0.1:8189") {
		t.Fatal("rejected fixed loopback endpoint")
	}
	for _, input := range []struct{ name, args string }{
		{"other", `{"query":"news"}`},
		{"searxng_web_search", `{"query":"news","url":"http://127.0.0.1"}`},
		{"searxng_web_search", `{"query":"news","time_range":"forever"}`},
		{"searxng_web_search", `{"query":"news","time_range":null}`},
		{"searxng_web_search", `{"query":""}`},
		{"web_url_read", `{"url":"file:///etc/passwd"}`},
		{"web_url_read", `{"url":"https://user:password@example.com"}`},
	} {
		if validBuiltinSearchArguments(input.name, json.RawMessage(input.args)) {
			t.Errorf("accepted unsupported arguments for %s", input.name)
		}
	}
}

func TestBuiltinSearchUsesFreshProcessesAndDropsHostEnvironment(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is needed for the local MCP subprocess test")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	packageRoot := filepath.Join(root, "node_modules", "mcp-searxng")
	dist := filepath.Join(packageRoot, "dist")
	if err := os.MkdirAll(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "package.json"), []byte(`{"name":"mcp-searxng","version":"2.10.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "patches"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "patches", "page-date.mjs"), []byte("export function extractPageDateMetadata() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dist, "cli.js")
	readerPath := filepath.Join(dist, "url-reader.js")
	if err := os.WriteFile(readerPath, []byte("// upstream URL reader without the required Morde patch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	program := `const readline = require('readline');
let calls = 0;
readline.createInterface({input:process.stdin}).on('line', line => {
  const request = JSON.parse(line);
  if (request.id === undefined) return;
  let result;
  if (request.method === 'initialize') result = {protocolVersion:request.params.protocolVersion,capabilities:{tools:{}},serverInfo:{name:'fixture',version:'1'}};
	else if (request.method === 'tools/call') { calls++; result = {content:[{type:'text',text:JSON.stringify({calls,query:request.params.arguments.query,time_range:request.params.arguments.time_range,leaked:!!process.env.MORDE_SEARCH_PRIVATE_TEST})}]}; }
  else result = {};
  process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:request.id,result})+'\n');
});`
	if err := os.WriteFile(script, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBuiltinSearchClient(node, script, "http://127.0.0.1:8189", 1); err == nil || !strings.Contains(err.Error(), "requires the Morde page-date patch") {
		t.Fatalf("unpatched mcp-searxng package was accepted: %v", err)
	}
	if err := os.WriteFile(readerPath, []byte("// MORDE_PAGE_DATE_METADATA_PATCH_V1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBuiltinSearchClient(node, script, "http://127.0.0.1:8189", 1); err == nil || !strings.Contains(err.Error(), "requires the Morde page-date patch") {
		t.Fatalf("incomplete Morde page-date patch was accepted: %v", err)
	}
	if err := os.WriteFile(readerPath, []byte("import { extractPageDateMetadata } from \"../../../patches/page-date.mjs\"; // MORDE_PAGE_DATE_METADATA_PATCH_V1\nextractPageDateMetadata(read.text)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "patches", "page-date.mjs"), []byte("// broken helper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBuiltinSearchClient(node, script, "http://127.0.0.1:8189", 1); err == nil || !strings.Contains(err.Error(), "helper is unavailable") {
		t.Fatalf("missing Morde page-date helper was accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "patches", "page-date.mjs"), []byte("export function extractPageDateMetadata() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORDE_SEARCH_PRIVATE_TEST", "private")
	client, err := NewBuiltinSearchClient(node, script, "http://127.0.0.1:8189", 1)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := client.CallTool(context.Background(), "searxng_web_search", json.RawMessage(`{"query":"synthetic","time_range":"week"}`))
		if err != nil || result.IsError || result.UnsupportedContent || len(result.Text) != 1 {
			t.Fatalf("call result = %#v, err = %v", result, err)
		}
		var reply struct {
			Calls     int    `json:"calls"`
			Query     string `json:"query"`
			TimeRange string `json:"time_range"`
			Leaked    bool   `json:"leaked"`
		}
		if json.Unmarshal([]byte(result.Text[0]), &reply) != nil || reply.Calls != 1 || reply.Query != "synthetic" || reply.TimeRange != "week" || reply.Leaked {
			t.Fatalf("unexpected isolated response: %#v", reply)
		}
	}
	if err := os.WriteFile(script, []byte(`setInterval(() => {}, 1000)`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := client.CallTool(ctx, "searxng_web_search", json.RawMessage(`{"query":"synthetic"}`)); err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("cancellation did not stop search process promptly: %v", err)
	}
	if _, err := NewBuiltinSearchClient(node, script, "http://127.0.0.1:8189", 0); err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("invalid concurrency was not rejected: %v", err)
	}
}

func TestBuiltinSearchWithLocalSearxng(t *testing.T) {
	script := strings.TrimSpace(os.Getenv("CH_TEST_BUILTIN_SEARCH_SCRIPT"))
	if script == "" {
		t.Skip("CH_TEST_BUILTIN_SEARCH_SCRIPT is unset; real local search was not run")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewBuiltinSearchClient(node, script, "http://127.0.0.1:8189", 2)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.CallTool(context.Background(), "searxng_web_search", json.RawMessage(`{"query":"latest AI news","time_range":"week"}`))
	if err != nil || result.IsError || result.UnsupportedContent || len(result.Text) == 0 || !strings.Contains(strings.Join(result.Text, "\n"), "https://") {
		t.Fatalf("public news search failed or returned no source links: err=%v isError=%t unsupported=%t", err, result.IsError, result.UnsupportedContent)
	}
	blocked, err := client.CallTool(context.Background(), "web_url_read", json.RawMessage(`{"url":"http://127.0.0.1:8189/"}`))
	if err != nil || !blocked.IsError {
		t.Fatalf("private URL was not blocked by the fixed MCP server: err=%v isError=%t", err, blocked.IsError)
	}
}
