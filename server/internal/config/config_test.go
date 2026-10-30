package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadReadsConfigExpandsEnvironmentAndConvertsDurations(t *testing.T) {
	workbenchDir := t.TempDir()
	t.Setenv("T02_TEST_INSTANCE", "test-instance")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(validConfigYAML(workbenchDir)), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instance != "test-instance" || cfg.HTTP.ReadTimeout != 10*time.Second || cfg.Orchestrator.Profiles["container"].RequestTimeout != 7*time.Second || cfg.CloudDSH.Enabled ||
		cfg.SiteStorage.RootDir == "" || cfg.SiteStorage.MaxReleaseBytes != 10737418240 || cfg.SiteStorage.MaxOwnerBytes != 53687091200 || cfg.SiteStorage.MinFreeBytes != 10737418240 {
		t.Fatalf("loaded config values are incomplete: instance=%q read_timeout=%s profile_timeout=%s site_storage=%+v", cfg.Instance, cfg.HTTP.ReadTimeout, cfg.Orchestrator.Profiles["container"].RequestTimeout, cfg.SiteStorage)
	}
	if cfg.MCP != (MCP{Enabled: true, MaxOutputBytes: 65536, ScreenshotMaxWidth: 1280}) {
		t.Fatalf("MCP defaults = %+v", cfg.MCP)
	}
}

func TestCloudDSHRequiresIndependentPinnedRuntimeAndGuardian(t *testing.T) {
	cfg := validConfig(t)
	cfg.CloudDSH = CloudDSH{
		Enabled: true, MaxConcurrentTasks: 4, MaxConcurrentPerOwner: 2,
		DockerHost: "unix:///run/morde-dsh/docker.sock", Image: "morde-dsh@sha256:" + strings.Repeat("a", 64),
		NodePath: "/usr/local/bin/node", ScriptPath: "/opt/morde-cloud-runtime/bin.mjs", SupervisorID: "dsh-supervisor-1",
		SupervisorKeyFile: "/etc/morde/dsh-supervisor.key", GuardianSocket: "/run/morde/dsh-guardian.sock",
		GuardianPublicKeyFile: "/etc/morde/dsh-guardian.pub",
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid enabled cloud dsh config: %v", err)
	}
	atOwnerCap := cfg
	atOwnerCap.CloudDSH.MaxConcurrentTasks = 64
	atOwnerCap.CloudDSH.MaxConcurrentPerOwner = maxCloudDSHConcurrentPerOwner
	if err := atOwnerCap.validate(); err != nil {
		t.Fatalf("valid account runtime task cap: %v", err)
	}
	root := t.TempDir()
	realDir, aliasDir := filepath.Join(root, "docker"), filepath.Join(root, "docker-alias")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	aliased := cfg
	aliased.Storage.DockerHost = "unix://" + filepath.Join(realDir, "docker.sock")
	aliased.CloudDSH.DockerHost = "unix://" + filepath.Join(aliasDir, "docker.sock")
	if err := aliased.validate(); err == nil || !strings.Contains(err.Error(), "separate Docker endpoint") {
		t.Fatalf("validate() error = %v, want aliased Docker socket rejected", err)
	}

	tests := []struct {
		name   string
		mutate func(*CloudDSH)
		want   string
	}{
		{name: "shared docker endpoint", mutate: func(value *CloudDSH) { value.DockerHost = "unix:///var/run/docker.sock" }, want: "separate Docker endpoint"},
		{name: "unpinned image", mutate: func(value *CloudDSH) { value.Image = "morde-dsh:latest" }, want: "pin an image by sha256 digest"},
		{name: "relative runtime path", mutate: func(value *CloudDSH) { value.NodePath = "node" }, want: "node_path must be an absolute clean path"},
		{name: "invalid supervisor id", mutate: func(value *CloudDSH) { value.SupervisorID = "bad id" }, want: "supervisor_id is invalid"},
		{name: "missing guardian key", mutate: func(value *CloudDSH) { value.GuardianPublicKeyFile = "" }, want: "guardian_public_key_file must be an absolute clean path"},
		{name: "missing task concurrency limit", mutate: func(value *CloudDSH) { value.MaxConcurrentTasks = 0 }, want: "concurrency limits must be positive"},
		{name: "owner exceeds global task limit", mutate: func(value *CloudDSH) { value.MaxConcurrentPerOwner = 5 }, want: "concurrency limits must be positive"},
		{name: "owner exceeds account runtime capacity", mutate: func(value *CloudDSH) {
			value.MaxConcurrentTasks = 64
			value.MaxConcurrentPerOwner = maxCloudDSHConcurrentPerOwner + 1
		}, want: "concurrency limits must be positive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := cfg
			test.mutate(&value.CloudDSH)
			if err := value.validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestWorkspaceSyncNodeRequiresPinnedImageAndSeparateRoots(t *testing.T) {
	cfg := validConfig(t)
	root := t.TempDir()
	cfg.WorkspaceSyncNode = WorkspaceSyncNode{
		Enabled: true, ImageDigest: "morde-syncthing@sha256:" + strings.Repeat("a", 64),
		RelayImageDigest:   "morde-syncthing-relay@sha256:" + strings.Repeat("b", 64),
		RelayPublicAddress: "127.0.0.1:22067",
		RelayToken:         strings.Repeat("c", 64),
		RelayStateRoot:     filepath.Join(root, "relay"), StateRoot: filepath.Join(root, "state"),
		WorkspaceRoot: filepath.Join(root, "files"), RelayContainerName: "morde-sync-relay",
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid Workspace sync node config: %v", err)
	}
	localImageIDs := cfg
	localImageIDs.WorkspaceSyncNode.ImageDigest = "sha256:" + strings.Repeat("c", 64)
	localImageIDs.WorkspaceSyncNode.RelayImageDigest = "sha256:" + strings.Repeat("d", 64)
	if err := localImageIDs.validate(); err != nil {
		t.Fatalf("local content-pinned Workspace sync images: %v", err)
	}
	tests := []struct {
		name   string
		want   string
		mutate func(*WorkspaceSyncNode)
	}{
		{name: "unpinned image", want: "image_digest must pin an image by sha256 digest", mutate: func(value *WorkspaceSyncNode) { value.ImageDigest = "morde-syncthing:latest" }},
		{name: "unpinned relay image", want: "relay_image_digest must pin an image by sha256 digest", mutate: func(value *WorkspaceSyncNode) { value.RelayImageDigest = "morde-relay:latest" }},
		{name: "invalid relay public address", want: "relay_public_address must be a host and port", mutate: func(value *WorkspaceSyncNode) { value.RelayPublicAddress = "https://relay.example.test:22067/path" }},
		{name: "missing relay token", want: "relay_token must be a 32 to 128 character URL-safe secret", mutate: func(value *WorkspaceSyncNode) { value.RelayToken = "" }},
		{name: "missing relay state root", want: "relay_state_root must be an absolute clean non-root path", mutate: func(value *WorkspaceSyncNode) { value.RelayStateRoot = "" }},
		{name: "missing state root", want: "state_root must be an absolute clean non-root path", mutate: func(value *WorkspaceSyncNode) { value.StateRoot = "" }},
		{name: "relative workspace root", want: "workspace_root must be an absolute clean non-root path", mutate: func(value *WorkspaceSyncNode) { value.WorkspaceRoot = "relative/files" }},
		{name: "overlapping node roots", want: "roots must be separate", mutate: func(value *WorkspaceSyncNode) { value.WorkspaceRoot = filepath.Join(value.StateRoot, "files") }},
		{name: "overlapping relay root", want: "roots must be separate", mutate: func(value *WorkspaceSyncNode) { value.RelayStateRoot = filepath.Join(value.StateRoot, "relay") }},
		{name: "unsafe relay name", want: "relay_container_name is invalid", mutate: func(value *WorkspaceSyncNode) { value.RelayContainerName = "../relay" }},
	}
	for _, address := range []string{"relay.example.test:22067", "[2001:db8::1]:22067"} {
		if !validWorkspaceSyncRelayAddress(address) {
			t.Fatalf("valid relay public address %q rejected", address)
		}
	}
	for _, address := range []string{"", "relay.example.test", "relay.example.test:0", "relay.example.test:65536", "relay.example.test:22067/path"} {
		if validWorkspaceSyncRelayAddress(address) {
			t.Fatalf("invalid relay public address %q accepted", address)
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := cfg
			test.mutate(&value.WorkspaceSyncNode)
			if err := value.validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validate() error = %v, want %q", err, test.want)
			}
		})
	}
	blockStoreOverlap := cfg
	blockStoreOverlap.WorkspaceSyncNode.WorkspaceRoot = filepath.Join(cfg.WorkspaceStorage.RootDir, "current")
	if err := blockStoreOverlap.validate(); err == nil || !strings.Contains(err.Error(), "separate from config.workspace_storage.root_dir") {
		t.Fatalf("block-store overlap error = %v", err)
	}
	remoteDocker := cfg
	remoteDocker.Storage.DockerHost = "tcp://docker.example:2375"
	if err := remoteDocker.validate(); err == nil || !strings.Contains(err.Error(), "requires config.storage.docker_host to use a Unix socket") {
		t.Fatalf("remote Docker error = %v", err)
	}
}

func TestLoadReportsReadParseDurationAndUnknownFieldErrors(t *testing.T) {
	t.Setenv("T02_TEST_INSTANCE", "test-instance")
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("Load(missing) error = %v", err)
	}

	workbenchDir := t.TempDir()
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "malformed YAML", data: "instance: [\n", want: "parse config"},
		{name: "invalid duration", data: strings.Replace(validConfigYAML(workbenchDir), "read_timeout: 10s", "read_timeout: someday", 1), want: "config.http.read_timeout must be a duration"},
		{name: "duration must be scalar", data: strings.Replace(validConfigYAML(workbenchDir), "read_timeout: 10s", "read_timeout: [10s]", 1), want: "config.http.read_timeout must be a duration"},
		{name: "unknown field", data: validConfigYAML(workbenchDir) + "unknown_option: true\n", want: "field unknown_option not found"},
		{name: "invalid config", data: strings.Replace(validConfigYAML(workbenchDir), "instance: $T02_TEST_INSTANCE", "instance: '   '", 1), want: "config.instance is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestConfigValidateRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*Config)
	}{
		{name: "instance", want: "config.instance", mutate: func(c *Config) { c.Instance = " \t" }},
		{name: "api address", want: "config.listen.api", mutate: func(c *Config) { c.Listen.API = "" }},
		{name: "gateway address", want: "config.listen.gateway", mutate: func(c *Config) { c.Listen.Gateway = "host:0" }},
		{name: "metrics address", want: "config.listen.metrics", mutate: func(c *Config) { c.Listen.Metrics = "host:65536" }},
		{name: "http read header timeout", want: "http.read_header_timeout", mutate: func(c *Config) { c.HTTP.ReadHeaderTimeout = 0 }},
		{name: "http read timeout", want: "http.read_timeout", mutate: func(c *Config) { c.HTTP.ReadTimeout = 0 }},
		{name: "http write timeout", want: "http.write_timeout", mutate: func(c *Config) { c.HTTP.WriteTimeout = 0 }},
		{name: "http idle timeout", want: "http.idle_timeout", mutate: func(c *Config) { c.HTTP.IdleTimeout = 0 }},
		{name: "http shutdown timeout", want: "http.shutdown_timeout", mutate: func(c *Config) { c.HTTP.ShutdownTimeout = 0 }},
		{name: "api body limit", want: "request body limits", mutate: func(c *Config) { c.HTTP.APIMaxBodyBytes = 0 }},
		{name: "mcp body limit", want: "request body limits", mutate: func(c *Config) { c.HTTP.MCPMaxBodyBytes = 0 }},
		{name: "domain scheme", want: "domains.scheme", mutate: func(c *Config) { c.Domains.Scheme = "ftp" }},
		{name: "portal domain", want: "domains.portal", mutate: func(c *Config) { c.Domains.Portal = "" }},
		{name: "host suffix", want: "domains.portal", mutate: func(c *Config) { c.Domains.HostSuffix = "" }},
		{name: "database DSN", want: "database.dsn", mutate: func(c *Config) { c.Database.DSN = " \t" }},
		{name: "database max connections", want: "database.max_conns", mutate: func(c *Config) { c.Database.MaxConns = 0 }},
		{name: "data key path", want: "key file paths", mutate: func(c *Config) { c.Security.DataKeyFile = "" }},
		{name: "cookie key path", want: "key file paths", mutate: func(c *Config) { c.Security.CookieKeyFile = "" }},
		{name: "session TTL", want: "security.session_ttl", mutate: func(c *Config) { c.Security.SessionTTL = 0 }},
		{name: "ticket TTL", want: "security.ticket_ttl", mutate: func(c *Config) { c.Security.TicketTTL = 0 }},
		{name: "host cookie TTL", want: "security.host_cookie_ttl", mutate: func(c *Config) { c.Security.HostCookieTTL = 0 }},
		{name: "login rate limit", want: "login_rate_limit", mutate: func(c *Config) { c.Security.LoginRateLimit = "0/min" }},
		{name: "empty model host allowlist", want: "models.allowed_hosts must not be empty", mutate: func(c *Config) { c.Models.AllowedHosts = nil }},
		{name: "private model host literal", want: "invalid DNS name", mutate: func(c *Config) { c.Models.AllowedHosts = []string{"127.0.0.1"} }},
		{name: "duplicate model host", want: "duplicate host", mutate: func(c *Config) { c.Models.AllowedHosts = []string{"api.openai.com", "API.OPENAI.COM"} }},
		{name: "no orchestrator profiles", want: "profiles must not be empty", mutate: func(c *Config) { c.Orchestrator.Profiles = nil }},
		{name: "empty profile name", want: "contains an empty name", mutate: func(c *Config) {
			c.Orchestrator.Profiles = map[string]Profile{"": {BaseURL: "http://127.0.0.1:18080", APIKey: "key", RequestTimeout: time.Second}}
		}},
		{name: "malformed profile URL", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL = "http://%zz"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile URL scheme", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL = "https://127.0.0.1:18080"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile URL remote host", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL = "http://osb.example:18080"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile URL loopback alias", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL = "http://localhost:18080"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile URL missing port", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL = "http://127.0.0.1"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile URL port out of range", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL = "http://127.0.0.1:65536"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile URL path", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL = "http://127.0.0.1:18080/v1"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile URL credentials", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL = "http://user:pass@127.0.0.1:18080"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile URL query", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL += "?token=x"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile URL fragment", want: "base_url must be", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.BaseURL += "#fragment"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "missing network guard path", want: "network_guard_path must be an absolute path", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.NetworkGuardPath = ""
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "relative network guard path", want: "network_guard_path must be an absolute path", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.NetworkGuardPath = "network-policy.sh"
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile API key", want: "api_key and positive request_timeout", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.APIKey = ""
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "profile timeout", want: "api_key and positive request_timeout", mutate: func(c *Config) {
			p := c.Orchestrator.Profiles["container"]
			p.RequestTimeout = 0
			c.Orchestrator.Profiles["container"] = p
		}},
		{name: "template directory", want: "templates.dir", mutate: func(c *Config) { c.Templates.Dir = "" }},
		{name: "default template", want: "templates.dir", mutate: func(c *Config) { c.Templates.Default = "" }},
		{name: "host ready timeout", want: "readiness durations", mutate: func(c *Config) { c.Hosts.ReadyTimeout = 0 }},
		{name: "host polling interval", want: "readiness durations", mutate: func(c *Config) { c.Hosts.ReadyPollInterval = 0 }},
		{name: "CPU capacity", want: "capacity limits", mutate: func(c *Config) { c.Capacity.CPUMilli = 0 }},
		{name: "memory capacity", want: "capacity limits", mutate: func(c *Config) { c.Capacity.MemMiB = 0 }},
		{name: "reconciler interval", want: "reconciler durations", mutate: func(c *Config) { c.Reconciler.Interval = 0 }},
		{name: "renew interval", want: "reconciler durations", mutate: func(c *Config) { c.Reconciler.RenewBefore = 0 }},
		{name: "activity flush interval", want: "reconciler durations", mutate: func(c *Config) { c.Reconciler.ActivityFlushInterval = 0 }},
		{name: "storage driver", want: "storage.driver", mutate: func(c *Config) { c.Storage.Driver = "k8s" }},
		{name: "docker host", want: "docker_host", mutate: func(c *Config) { c.Storage.Driver = "docker"; c.Storage.DockerHost = "" }},
		{name: "docker data dir", want: "docker_data_dir must be an absolute path", mutate: func(c *Config) { c.Storage.DockerDataDir = "relative/docker" }},
		{name: "docker minimum free space", want: "min_free_bytes must be positive", mutate: func(c *Config) { c.Storage.MinFreeBytes = 0 }},
		{name: "workspace root relative", want: "workspace_storage.root_dir must be an absolute non-root path", mutate: func(c *Config) { c.WorkspaceStorage.RootDir = "relative/workspaces" }},
		{name: "workspace root filesystem root", want: "workspace_storage.root_dir must be an absolute non-root path", mutate: func(c *Config) { c.WorkspaceStorage.RootDir = string(filepath.Separator) }},
		{name: "workspace root overlaps docker data", want: "must be separate from config.storage.docker_data_dir", mutate: func(c *Config) { c.WorkspaceStorage.RootDir = "/var/lib/docker/workspaces" }},
		{name: "workspace quota", want: "workspace_storage quotas", mutate: func(c *Config) { c.WorkspaceStorage.MaxWorkspaceBytes = 0 }},
		{name: "owner quota", want: "workspace_storage quotas", mutate: func(c *Config) { c.WorkspaceStorage.MaxOwnerBytes = 0 }},
		{name: "workspace minimum free space", want: "workspace_storage quotas", mutate: func(c *Config) { c.WorkspaceStorage.MinFreeBytes = 0 }},
		{name: "site root relative", want: "site_storage.root_dir must be an absolute non-root path", mutate: func(c *Config) { c.SiteStorage.RootDir = "relative/sites" }},
		{name: "site root filesystem root", want: "site_storage.root_dir must be an absolute non-root path", mutate: func(c *Config) { c.SiteStorage.RootDir = string(filepath.Separator) }},
		{name: "site release quota", want: "site_storage.max_release_bytes, max_owner_bytes and min_free_bytes", mutate: func(c *Config) { c.SiteStorage.MaxReleaseBytes = 0 }},
		{name: "site owner quota", want: "site_storage.max_release_bytes, max_owner_bytes and min_free_bytes", mutate: func(c *Config) { c.SiteStorage.MaxOwnerBytes = 0 }},
		{name: "site minimum free space", want: "site_storage.max_release_bytes, max_owner_bytes and min_free_bytes", mutate: func(c *Config) { c.SiteStorage.MinFreeBytes = 0 }},
		{name: "site root overlaps workspace", want: "must be separate from config.workspace_storage.root_dir", mutate: func(c *Config) { c.SiteStorage.RootDir = filepath.Join(c.WorkspaceStorage.RootDir, "sites") }},
		{name: "site root overlaps docker data", want: "must be separate from config.storage.docker_data_dir", mutate: func(c *Config) { c.SiteStorage.RootDir = filepath.Join(c.Storage.DockerDataDir, "sites") }},
		{name: "site root overlaps workbench assets", want: "must be separate from config.gateway.workbench_dir", mutate: func(c *Config) { c.SiteStorage.RootDir = c.Gateway.WorkbenchDir }},
		{name: "site root overlaps portal assets", want: "must be separate from config.portal_web.dir", mutate: func(c *Config) { c.SiteStorage.RootDir = filepath.Join(mustAbs(t, c.PortalWeb.Dir), "sites") }},
		{name: "gateway", want: "config.gateway", mutate: func(c *Config) { c.Gateway.MaxBodyBytes = 0 }},
		{name: "mcp", want: "config.mcp", mutate: func(c *Config) { c.MCP.MaxOutputBytes = 0 }},
		{name: "portal web", want: "portal_web.dir", mutate: func(c *Config) { c.PortalWeb.Dir = "" }},
		{name: "log level", want: "log.level", mutate: func(c *Config) { c.Log.Level = "trace" }},
		{name: "log format", want: "log.format", mutate: func(c *Config) { c.Log.Format = "text" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			tt.mutate(&cfg)
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestConfigValidateRejectsSymlinkedWorkspaceRootOverlap(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cfg := validConfig(t)
	cfg.WorkspaceStorage.RootDir = filepath.Join(alias, "workspaces")
	cfg.Storage.DockerDataDir = filepath.Join(real, "workspaces", "docker")
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "must be separate from config.storage.docker_data_dir") {
		t.Fatalf("validate() error = %v, want overlapping symlinked paths rejected", err)
	}
}

func TestConfigValidateRejectsSymlinkedSiteRootOverlap(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cfg := validConfig(t)
	cfg.SiteStorage.RootDir = filepath.Join(alias, "sites")
	cfg.WorkspaceStorage.RootDir = filepath.Join(real, "sites", "workspaces")
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "must be separate from config.workspace_storage.root_dir") {
		t.Fatalf("validate() error = %v, want overlapping symlinked paths rejected", err)
	}
}

func TestValidateListenAddress(t *testing.T) {
	for _, address := range []string{"", ":8080", "host", "host:abc", "host:0", "host:65536", "0.0.0.0:8080", "[::1]:8080"} {
		t.Run(fmt.Sprintf("%q", address), func(t *testing.T) {
			if err := validateListenAddress(address); err == nil {
				t.Fatalf("validateListenAddress(%q) succeeded", address)
			}
		})
	}
	if err := validateListenAddress("127.0.0.1:8080"); err != nil {
		t.Fatalf("valid listen address: %v", err)
	}
}

func TestLoadTemplatesValidatesDirectoryAndTemplateFiles(t *testing.T) {
	if _, err := LoadTemplates(filepath.Join(t.TempDir(), "missing"), nil); err == nil || !strings.Contains(err.Error(), "read templates") {
		t.Fatalf("LoadTemplates(missing) error = %v", err)
	}

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "ignored.yaml"), []byte("bad: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.yml"), []byte("bad: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("T02_TEMPLATE_IMAGE", "registry.example/host:1@sha256:"+strings.Repeat("a", 64))
	if err := os.WriteFile(filepath.Join(dir, "host.yaml"), []byte(validTemplateYAML("std-a", "container", "$T02_TEMPLATE_IMAGE")), 0o600); err != nil {
		t.Fatal(err)
	}
	templates, err := LoadTemplates(dir, map[string]Profile{"container": {}, "vm": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 || templates["std-a"].Image != "registry.example/host:1@sha256:"+strings.Repeat("a", 64) || templates["std-a"].Lifecycle.TTL != 8*time.Hour {
		t.Fatalf("loaded templates = %#v", templates)
	}
}

func TestDeploymentTemplatesExposeRequestedHostSizes(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "deploy", "single", "templates")
	templates, err := LoadTemplates(dir, map[string]Profile{"container": {}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ cpu, memory string }{
		"std-1c2g": {cpu: "1", memory: "2Gi"},
		"std-2c2g": {cpu: "2", memory: "2Gi"},
		"std-2c4g": {cpu: "2", memory: "4Gi"},
	}
	if len(templates) != len(want) {
		t.Fatalf("deployment templates = %d, want %d", len(templates), len(want))
	}
	for id, resources := range want {
		tmpl, ok := templates[id]
		if !ok || tmpl.Resources.Limits.CPU != resources.cpu || tmpl.Resources.Limits.Memory != resources.memory {
			t.Fatalf("template %s = %#v, want limits %s CPU / %s memory", id, tmpl.Resources.Limits, resources.cpu, resources.memory)
		}
	}
}

func TestLoadTemplatesReportsInvalidFilesAndDuplicateIDs(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{name: "malformed YAML", files: map[string]string{"bad.yaml": "id: [\n"}, want: "parse template bad.yaml"},
		{name: "invalid duration", files: map[string]string{"bad.yaml": strings.Replace(validTemplateYAML("std-a", "container", "image"), "ttl: 8h", "ttl: later", 1)}, want: "template bad.yaml: template lifecycle.ttl must be a duration"},
		{name: "unknown field", files: map[string]string{"bad.yaml": validTemplateYAML("std-a", "container", "image") + "extra: true\n"}, want: "field extra not found"},
		{name: "unsafe idle action", files: map[string]string{"bad.yaml": strings.Replace(validTemplateYAML("std-a", "container", "image"), "onIdle: disabled", "onIdle: delete", 1)}, want: "disabled until activity leases"},
		{name: "removed network config", files: map[string]string{"bad.yaml": validTemplateYAML("std-a", "container", "image") + "network: {defaultAction: allow, deny: []}\n"}, want: "field network not found"},
		{name: "invalid template", files: map[string]string{"bad.yaml": validTemplateYAML("std-a", "missing", "image")}, want: "not configured"},
		{name: "duplicate ID", files: map[string]string{"a.yaml": validTemplateYAML("std-a", "container", "image"), "b.yaml": validTemplateYAML("std-a", "container", "image")}, want: "duplicate template id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadTemplates(dir, map[string]Profile{"container": {}}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadTemplates() error = %v, want substring %q", err, tt.want)
			}
		})
	}

	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "unreadable.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := LoadTemplates(dir, map[string]Profile{"container": {}}); err == nil || !strings.Contains(err.Error(), "read template unreadable.yaml") {
		t.Fatalf("LoadTemplates(dangling symlink) error = %v", err)
	}
}

func validConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Instance: "test", Listen: Listen{API: "127.0.0.1:8080", Gateway: "127.0.0.1:8081", Metrics: "127.0.0.1:8082"},
		HTTP:         HTTP{ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second, APIMaxBodyBytes: 1024, MCPMaxBodyBytes: 1024},
		Domains:      Domains{Scheme: "https", Portal: "portal.example", HostSuffix: "hosts.example"},
		Database:     Database{DSN: "postgres://db.example/app", MaxConns: 4},
		Security:     Security{DataKeyFile: "data.key", CookieKeyFile: "cookie.key", SessionTTL: time.Hour, TicketTTL: time.Minute, HostCookieTTL: time.Hour, LoginRateLimit: "10/min"},
		Orchestrator: Orchestrator{Profiles: map[string]Profile{"container": {BaseURL: "http://127.0.0.1:18080", APIKey: "key", RequestTimeout: time.Second, NetworkGuardPath: "/opt/cloudhost/network-policy.sh"}}},
		Models:       Models{AllowedHosts: []string{"api.openai.com"}},
		Templates:    Templates{Dir: "templates", Default: "std-a"}, Hosts: Hosts{ReadyTimeout: time.Minute, ReadyPollInterval: time.Second},
		Capacity: Capacity{CPUMilli: 1000, MemMiB: 2048}, Reconciler: Reconciler{Interval: time.Minute, RenewBefore: time.Minute, ActivityFlushInterval: time.Minute},
		Storage:          Storage{Driver: "docker", DockerHost: "unix:///var/run/docker.sock", DockerDataDir: "/var/lib/docker", MinFreeBytes: 10 << 30},
		WorkspaceStorage: WorkspaceStorage{RootDir: t.TempDir(), MaxWorkspaceBytes: 10 << 30, MaxOwnerBytes: 50 << 30, MinFreeBytes: 10 << 30},
		SiteStorage:      SiteStorage{RootDir: t.TempDir(), MaxReleaseBytes: 10 << 30, MaxOwnerBytes: 50 << 30, MinFreeBytes: 10 << 30},
		Gateway:          Gateway{WorkbenchDir: t.TempDir(), PreviewPortMin: 1024, MaxBodyBytes: 1024},
		MCP:              MCP{Enabled: true, MaxOutputBytes: 1024, ScreenshotMaxWidth: 640}, PortalWeb: PortalWeb{Dir: "web"}, Log: Log{Level: "info", Format: "json"},
	}
}

func validConfigYAML(workbenchDir string) string {
	siteStorageDir := filepath.Join(filepath.Dir(workbenchDir), "site-artifacts")
	return fmt.Sprintf("instance: $T02_TEST_INSTANCE\n"+
		"listen:\n  api: 127.0.0.1:8080\n  gateway: 127.0.0.1:8081\n  metrics: 127.0.0.1:8082\n"+
		"http:\n  read_header_timeout: 5s\n  read_timeout: 10s\n  write_timeout: 15s\n  idle_timeout: 20s\n  shutdown_timeout: 30s\n  api_max_body_bytes: 1048576\n  mcp_max_body_bytes: 1048576\n"+
		"domains:\n  scheme: https\n  portal: portal.example\n  host_suffix: hosts.example\n"+
		"database:\n  dsn: postgres://db.example/app\n  max_conns: 4\n"+
		"security:\n  data_key_file: data.key\n  cookie_key_file: cookie.key\n  session_ttl: 24h\n  ticket_ttl: 5m\n  host_cookie_ttl: 24h\n  login_rate_limit: 10/min\n"+
		"models:\n  allowed_hosts: [api.openai.com]\n"+
		"orchestrator:\n  profiles:\n    container:\n      base_url: http://127.0.0.1:18080\n      api_key: test-key\n      request_timeout: 7s\n      network_guard_path: /opt/cloudhost/network-policy.sh\n"+
		"templates:\n  dir: templates\n  default: std-a\n"+
		"hosts:\n  ready_timeout: 2m\n  ready_poll_interval: 2s\n"+
		"capacity:\n  cpu_milli: 4000\n  mem_mib: 8192\n"+
		"reconciler:\n  interval: 1m\n  renew_before: 5m\n  activity_flush_interval: 10s\n"+
		"storage:\n  driver: docker\n  docker_host: unix:///var/run/docker.sock\n  docker_data_dir: /var/lib/docker\n  min_free_bytes: 10737418240\n"+
		"cloud_dsh:\n  enabled: false\n"+
		"workspace_storage:\n  root_dir: /var/lib/morde/workspaces\n  max_workspace_bytes: 10737418240\n  max_owner_bytes: 53687091200\n  min_free_bytes: 10737418240\n"+
		"site_storage:\n  root_dir: %q\n  max_release_bytes: 10737418240\n  max_owner_bytes: 53687091200\n  min_free_bytes: 10737418240\n"+
		"gateway:\n  workbench_dir: %q\n  upstream_cache_ttl: 1m\n  credential_cache_ttl: 1m\n  preview_port_min: 1024\n  max_body_bytes: 1048576\n"+
		"portal_web:\n  dir: web\n"+
		"log:\n  level: info\n  format: json\n", siteStorageDir, workbenchDir)
}

func mustAbs(t *testing.T, value string) string {
	t.Helper()
	absolute, err := filepath.Abs(value)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func validTemplateYAML(id, isolation, image string) string {
	if image == "image" {
		image = "registry.example/host:1@sha256:" + strings.Repeat("a", 64)
	}
	return fmt.Sprintf("id: %s\nname: Standard\nimage: %s\nentrypoint: [\"/usr/local/bin/morde-host-entrypoint\"]\n"+
		"resources:\n  requests: {cpu: \"500m\", memory: \"512Mi\"}\n  limits: {cpu: \"2\", memory: \"2Gi\"}\n"+
		"workspace: {mountPath: /workspace, size: 20Gi}\n"+
		"lifecycle: {ttl: 8h, idleTimeout: 2h, onIdle: disabled}\n"+
		"isolation: %s\n", id, image, isolation)
}
