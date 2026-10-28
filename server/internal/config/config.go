package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	templatedomain "morde/server/internal/domain/template"
)

const maxCloudDSHConcurrentPerOwner = 16

type Config struct {
	Instance         string           `yaml:"instance"`
	Listen           Listen           `yaml:"listen"`
	HTTP             HTTP             `yaml:"http"`
	Domains          Domains          `yaml:"domains"`
	Database         Database         `yaml:"database"`
	Security         Security         `yaml:"security"`
	Orchestrator     Orchestrator     `yaml:"orchestrator"`
	Templates        Templates        `yaml:"templates"`
	Hosts            Hosts            `yaml:"hosts"`
	Capacity         Capacity         `yaml:"capacity"`
	Reconciler       Reconciler       `yaml:"reconciler"`
	Storage          Storage          `yaml:"storage"`
	WorkspaceStorage WorkspaceStorage `yaml:"workspace_storage"`
	SiteStorage      SiteStorage      `yaml:"site_storage"`
	Models           Models           `yaml:"models"`
	CloudDSH         CloudDSH         `yaml:"cloud_dsh"`
	Gateway          Gateway          `yaml:"gateway"`
	MCP              MCP              `yaml:"mcp"`
	PortalWeb        PortalWeb        `yaml:"portal_web"`
	Log              Log              `yaml:"log"`
}

type Listen struct {
	API     string `yaml:"api"`
	Gateway string `yaml:"gateway"`
	Metrics string `yaml:"metrics"`
}

type HTTP struct {
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	APIMaxBodyBytes   int64         `yaml:"api_max_body_bytes"`
	MCPMaxBodyBytes   int64         `yaml:"mcp_max_body_bytes"`
}

type Domains struct {
	Scheme       string `yaml:"scheme"`
	Portal       string `yaml:"portal"`
	HostSuffix   string `yaml:"host_suffix"`
	CookieSecure bool   `yaml:"cookie_secure"`
}

type Database struct {
	DSN      string `yaml:"dsn"`
	MaxConns int32  `yaml:"max_conns"`
}

type Security struct {
	DataKeyFile    string        `yaml:"data_key_file"`
	CookieKeyFile  string        `yaml:"cookie_key_file"`
	SessionTTL     time.Duration `yaml:"session_ttl"`
	TicketTTL      time.Duration `yaml:"ticket_ttl"`
	HostCookieTTL  time.Duration `yaml:"host_cookie_ttl"`
	LoginRateLimit string        `yaml:"login_rate_limit"`
}

type Orchestrator struct {
	Profiles map[string]Profile `yaml:"profiles"`
}

type Profile struct {
	Name             string        `yaml:"-"`
	BaseURL          string        `yaml:"base_url"`
	APIKey           string        `yaml:"api_key"`
	RequestTimeout   time.Duration `yaml:"request_timeout"`
	NetworkGuardPath string        `yaml:"network_guard_path"`
}

type Templates struct {
	Dir     string `yaml:"dir"`
	Default string `yaml:"default"`
}

type Hosts struct {
	ReadyTimeout      time.Duration `yaml:"ready_timeout"`
	ReadyPollInterval time.Duration `yaml:"ready_poll_interval"`
}

type Capacity struct {
	CPUMilli int64 `yaml:"cpu_milli"`
	MemMiB   int64 `yaml:"mem_mib"`
}

type Reconciler struct {
	Interval              time.Duration `yaml:"interval"`
	RenewBefore           time.Duration `yaml:"renew_before"`
	ActivityFlushInterval time.Duration `yaml:"activity_flush_interval"`
}

type Storage struct {
	Driver        string `yaml:"driver"`
	DockerHost    string `yaml:"docker_host"`
	DockerDataDir string `yaml:"docker_data_dir"`
	MinFreeBytes  int64  `yaml:"min_free_bytes"`
}

type WorkspaceStorage struct {
	RootDir           string `yaml:"root_dir"`
	MaxWorkspaceBytes int64  `yaml:"max_workspace_bytes"`
	MaxOwnerBytes     int64  `yaml:"max_owner_bytes"`
	MinFreeBytes      int64  `yaml:"min_free_bytes"`
}

type SiteStorage struct {
	RootDir         string `yaml:"root_dir"`
	MaxReleaseBytes int64  `yaml:"max_release_bytes"`
	MaxOwnerBytes   int64  `yaml:"max_owner_bytes"`
	MinFreeBytes    int64  `yaml:"min_free_bytes"`
}

type Models struct {
	AllowedHosts []string `yaml:"allowed_hosts"`
}

type CloudDSH struct {
	Enabled               bool   `yaml:"enabled"`
	MaxConcurrentTasks    int    `yaml:"max_concurrent_tasks"`
	MaxConcurrentPerOwner int    `yaml:"max_concurrent_tasks_per_owner"`
	DockerHost            string `yaml:"docker_host"`
	Image                 string `yaml:"image"`
	NodePath              string `yaml:"node_path"`
	ScriptPath            string `yaml:"script_path"`
	SupervisorID          string `yaml:"supervisor_id"`
	SupervisorKeyFile     string `yaml:"supervisor_key_file"`
	GuardianSocket        string `yaml:"guardian_socket"`
	GuardianPublicKeyFile string `yaml:"guardian_public_key_file"`
}

type PortalWeb struct {
	Dir string `yaml:"dir"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

var durationKeys = map[string]struct{}{
	"read_header_timeout": {}, "read_timeout": {}, "write_timeout": {}, "idle_timeout": {}, "shutdown_timeout": {},
	"session_ttl": {}, "ticket_ttl": {}, "host_cookie_ttl": {},
	"request_timeout": {}, "ready_timeout": {}, "ready_poll_interval": {},
	"upstream_cache_ttl": {}, "credential_cache_ttl": {}, "interval": {}, "renew_before": {}, "activity_flush_interval": {},
	"ttl": {}, "idleTimeout": {},
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return parseConfig(os.ExpandEnv(string(data)))
}

func parseConfig(data string) (Config, error) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(data), &node); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := convertDurations(&node, ""); err != nil {
		return Config{}, fmt.Errorf("config.%w", err)
	}
	encoded, err := yaml.Marshal(&node)
	if err != nil {
		return Config{}, fmt.Errorf("encode config: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	var cfg Config
	cfg.MCP.setDefaults()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func convertDurations(node *yaml.Node, path string) error {
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := convertDurations(child, path); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			field := key.Value
			if path != "" {
				field = path + "." + field
			}
			if _, ok := durationKeys[key.Value]; ok {
				_, err := time.ParseDuration(value.Value)
				if err != nil || value.Kind != yaml.ScalarNode {
					return fmt.Errorf("%s must be a duration", field)
				}
				value.Tag = "!!str"
				continue
			}
			if err := convertDurations(value, field); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := convertDurations(child, path); err != nil {
				return err
			}
		}
	}
	return nil
}

var rateLimitPattern = regexp.MustCompile(`^[1-9][0-9]*/min$`)
var cloudDSHImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var cloudDSHSupervisorIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (c Config) validate() error {
	if strings.TrimSpace(c.Instance) == "" {
		return fmt.Errorf("config.instance is required")
	}
	for name, address := range map[string]string{"listen.api": c.Listen.API, "listen.gateway": c.Listen.Gateway, "listen.metrics": c.Listen.Metrics} {
		if err := validateListenAddress(address); err != nil {
			return fmt.Errorf("config.%s: %w", name, err)
		}
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"http.read_header_timeout", c.HTTP.ReadHeaderTimeout},
		{"http.read_timeout", c.HTTP.ReadTimeout},
		{"http.write_timeout", c.HTTP.WriteTimeout},
		{"http.idle_timeout", c.HTTP.IdleTimeout},
		{"http.shutdown_timeout", c.HTTP.ShutdownTimeout},
	} {
		if field.value <= 0 {
			return fmt.Errorf("config.%s must be positive", field.name)
		}
	}
	if c.HTTP.APIMaxBodyBytes <= 0 || c.HTTP.MCPMaxBodyBytes <= 0 {
		return fmt.Errorf("config.http request body limits must be positive")
	}
	if c.Domains.Scheme != "http" && c.Domains.Scheme != "https" {
		return fmt.Errorf("config.domains.scheme must be http or https")
	}
	if c.Domains.Portal == "" || c.Domains.HostSuffix == "" {
		return fmt.Errorf("config.domains.portal and domains.host_suffix are required")
	}
	if strings.TrimSpace(c.Database.DSN) == "" {
		return fmt.Errorf("config.database.dsn is required")
	}
	if c.Database.MaxConns <= 0 {
		return fmt.Errorf("config.database.max_conns must be positive")
	}
	if c.Security.DataKeyFile == "" || c.Security.CookieKeyFile == "" {
		return fmt.Errorf("config.security key file paths are required")
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"security.session_ttl", c.Security.SessionTTL},
		{"security.ticket_ttl", c.Security.TicketTTL},
		{"security.host_cookie_ttl", c.Security.HostCookieTTL},
	} {
		if field.value <= 0 {
			return fmt.Errorf("config.%s must be positive", field.name)
		}
	}
	if !rateLimitPattern.MatchString(c.Security.LoginRateLimit) {
		return fmt.Errorf("config.security.login_rate_limit is invalid")
	}
	if len(c.Models.AllowedHosts) == 0 {
		return fmt.Errorf("config.models.allowed_hosts must not be empty")
	}
	seenModelHosts := make(map[string]struct{}, len(c.Models.AllowedHosts))
	for _, value := range c.Models.AllowedHosts {
		host := strings.ToLower(strings.TrimSpace(value))
		if !validModelOutboundHost(host) {
			return fmt.Errorf("config.models.allowed_hosts contains an invalid DNS name")
		}
		if _, exists := seenModelHosts[host]; exists {
			return fmt.Errorf("config.models.allowed_hosts contains a duplicate host")
		}
		seenModelHosts[host] = struct{}{}
	}
	if len(c.Orchestrator.Profiles) == 0 {
		return fmt.Errorf("config.orchestrator.profiles must not be empty")
	}
	profiles := make([]string, 0, len(c.Orchestrator.Profiles))
	for name, profile := range c.Orchestrator.Profiles {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("config.orchestrator.profiles contains an empty name")
		}
		if strings.TrimSpace(profile.NetworkGuardPath) == "" || !filepath.IsAbs(profile.NetworkGuardPath) {
			return fmt.Errorf("config.orchestrator.profiles.%s.network_guard_path must be an absolute path", name)
		}
		parsed, err := url.Parse(profile.BaseURL)
		portText := ""
		if err == nil && parsed != nil {
			portText = parsed.Port()
		}
		port, portErr := strconv.Atoi(portText)
		if err != nil || parsed == nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || portErr != nil || port < 1 || port > 65535 || (parsed.Path != "" && parsed.Path != "/") || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(profile.BaseURL, "#") {
			return fmt.Errorf("config.orchestrator.profiles.%s.base_url must be an http URL to 127.0.0.1 with an explicit port", name)
		}
		if profile.APIKey == "" || profile.RequestTimeout <= 0 {
			return fmt.Errorf("config.orchestrator.profiles.%s api_key and positive request_timeout are required", name)
		}
		profiles = append(profiles, name)
	}
	sort.Strings(profiles)
	if c.Templates.Dir == "" || c.Templates.Default == "" {
		return fmt.Errorf("config.templates.dir and templates.default are required")
	}
	if c.Hosts.ReadyTimeout <= 0 || c.Hosts.ReadyPollInterval <= 0 {
		return fmt.Errorf("config.hosts readiness durations must be positive")
	}
	if c.Capacity.CPUMilli <= 0 || c.Capacity.MemMiB <= 0 {
		return fmt.Errorf("config.capacity limits must be positive")
	}
	if c.Reconciler.Interval <= 0 || c.Reconciler.RenewBefore <= 0 || c.Reconciler.ActivityFlushInterval <= 0 {
		return fmt.Errorf("config.reconciler durations must be positive")
	}
	if c.Storage.Driver != "docker" {
		return fmt.Errorf("config.storage.driver must be docker")
	}
	if c.Storage.Driver == "docker" {
		if c.Storage.DockerHost == "" {
			return fmt.Errorf("config.storage.docker_host is required for docker storage")
		}
		if strings.TrimSpace(c.Storage.DockerDataDir) == "" || !filepath.IsAbs(c.Storage.DockerDataDir) {
			return fmt.Errorf("config.storage.docker_data_dir must be an absolute path")
		}
		if c.Storage.MinFreeBytes <= 0 {
			return fmt.Errorf("config.storage.min_free_bytes must be positive")
		}
	}
	if c.CloudDSH.Enabled {
		if c.CloudDSH.MaxConcurrentTasks < 1 || c.CloudDSH.MaxConcurrentTasks > 64 ||
			c.CloudDSH.MaxConcurrentPerOwner < 1 || c.CloudDSH.MaxConcurrentPerOwner > c.CloudDSH.MaxConcurrentTasks ||
			c.CloudDSH.MaxConcurrentPerOwner > maxCloudDSHConcurrentPerOwner {
			return fmt.Errorf("config.cloud_dsh concurrency limits must be positive and per-owner may not exceed the global limit")
		}
		dshSocket, dshSocketOK := localDockerSocketPath(c.CloudDSH.DockerHost)
		storageSocket, storageSocketOK := localDockerSocketPath(c.Storage.DockerHost)
		if !dshSocketOK || !storageSocketOK || resolvePath(dshSocket) == resolvePath(storageSocket) {
			return fmt.Errorf("config.cloud_dsh.docker_host must use a separate Docker endpoint")
		}
		if !cloudDSHImagePattern.MatchString(c.CloudDSH.Image) {
			return fmt.Errorf("config.cloud_dsh.image must pin an image by sha256 digest")
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{"node_path", c.CloudDSH.NodePath}, {"script_path", c.CloudDSH.ScriptPath},
			{"supervisor_key_file", c.CloudDSH.SupervisorKeyFile}, {"guardian_public_key_file", c.CloudDSH.GuardianPublicKeyFile},
			{"guardian_socket", c.CloudDSH.GuardianSocket},
		} {
			if !filepath.IsAbs(field.value) || filepath.Clean(field.value) != field.value || field.value == string(filepath.Separator) {
				return fmt.Errorf("config.cloud_dsh.%s must be an absolute clean path", field.name)
			}
		}
		if !cloudDSHSupervisorIDPattern.MatchString(c.CloudDSH.SupervisorID) {
			return fmt.Errorf("config.cloud_dsh.supervisor_id is invalid")
		}
	}
	if strings.TrimSpace(c.WorkspaceStorage.RootDir) == "" || !filepath.IsAbs(c.WorkspaceStorage.RootDir) || filepath.Clean(c.WorkspaceStorage.RootDir) == string(filepath.Separator) {
		return fmt.Errorf("config.workspace_storage.root_dir must be an absolute non-root path")
	}
	if c.WorkspaceStorage.MaxWorkspaceBytes <= 0 || c.WorkspaceStorage.MaxOwnerBytes <= 0 || c.WorkspaceStorage.MinFreeBytes <= 0 {
		return fmt.Errorf("config.workspace_storage quotas and min_free_bytes must be positive")
	}
	workspaceRoot := filepath.Clean(c.WorkspaceStorage.RootDir)
	dockerRoot := filepath.Clean(c.Storage.DockerDataDir)
	if pathsOverlap(workspaceRoot, dockerRoot) {
		return fmt.Errorf("config.workspace_storage.root_dir must be separate from config.storage.docker_data_dir")
	}
	if strings.TrimSpace(c.SiteStorage.RootDir) == "" || !filepath.IsAbs(c.SiteStorage.RootDir) || filepath.Clean(c.SiteStorage.RootDir) == string(filepath.Separator) {
		return fmt.Errorf("config.site_storage.root_dir must be an absolute non-root path")
	}
	if c.SiteStorage.MaxReleaseBytes <= 0 || c.SiteStorage.MaxOwnerBytes <= 0 || c.SiteStorage.MinFreeBytes <= 0 {
		return fmt.Errorf("config.site_storage.max_release_bytes, max_owner_bytes and min_free_bytes must be positive")
	}
	siteRoot := filepath.Clean(c.SiteStorage.RootDir)
	for _, protected := range []struct {
		name string
		path string
	}{
		{"config.workspace_storage.root_dir", workspaceRoot},
		{"config.storage.docker_data_dir", dockerRoot},
		{"config.gateway.workbench_dir", c.Gateway.WorkbenchDir},
		{"config.portal_web.dir", c.PortalWeb.Dir},
	} {
		if protected.path != "" && pathsOverlap(siteRoot, protected.path) {
			return fmt.Errorf("config.site_storage.root_dir must be separate from %s", protected.name)
		}
	}
	if err := c.Gateway.validate(); err != nil {
		return fmt.Errorf("config.gateway: %w", err)
	}
	if err := c.MCP.validate(); err != nil {
		return fmt.Errorf("config.mcp: %w", err)
	}
	if c.PortalWeb.Dir == "" {
		return fmt.Errorf("config.portal_web.dir is required")
	}
	if c.Log.Level != "debug" && c.Log.Level != "info" && c.Log.Level != "warn" && c.Log.Level != "error" {
		return fmt.Errorf("config.log.level must be debug, info, warn, or error")
	}
	if c.Log.Format != "json" && c.Log.Format != "console" {
		return fmt.Errorf("config.log.format must be json or console")
	}
	return nil
}

func localDockerSocketPath(endpoint string) (string, bool) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "unix" || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || !filepath.IsAbs(parsed.Path) || filepath.Clean(parsed.Path) != parsed.Path || parsed.Path == string(filepath.Separator) {
		return "", false
	}
	return parsed.Path, true
}

func pathsOverlap(a, b string) bool {
	return containsPath(a, b) || containsPath(b, a) || containsPath(resolvePath(a), resolvePath(b)) || containsPath(resolvePath(b), resolvePath(a))
}

func containsPath(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func resolvePath(value string) string {
	value = filepath.Clean(value)
	if !filepath.IsAbs(value) {
		if absolute, err := filepath.Abs(value); err == nil {
			value = absolute
		}
	}
	original := value
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(value)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(value)
		if parent == value {
			return original
		}
		suffix = append(suffix, filepath.Base(value))
		value = parent
	}
}

func validModelOutboundHost(host string) bool {
	if host == "" || len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil || strings.ContainsAny(host, ":/@?#*\\") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func validateListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return fmt.Errorf("must be host:port")
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("must bind to 127.0.0.1")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func LoadTemplates(dir string, profiles map[string]Profile) (map[string]templatedomain.Template, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read templates: %w", err)
	}
	profileNames := make([]string, 0, len(profiles))
	for name := range profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	result := make(map[string]templatedomain.Template)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read template %s: %w", entry.Name(), err)
		}
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(data))), &node); err != nil {
			return nil, fmt.Errorf("parse template %s: %w", entry.Name(), err)
		}
		if err := convertDurations(&node, ""); err != nil {
			return nil, fmt.Errorf("template %s: template %w", entry.Name(), err)
		}
		encoded, err := yaml.Marshal(&node)
		if err != nil {
			return nil, fmt.Errorf("encode template %s: %w", entry.Name(), err)
		}
		decoder := yaml.NewDecoder(bytes.NewReader(encoded))
		decoder.KnownFields(true)
		var tmpl templatedomain.Template
		if err := decoder.Decode(&tmpl); err != nil {
			return nil, fmt.Errorf("parse template %s: %w", entry.Name(), err)
		}
		if err := tmpl.Validate(profileNames); err != nil {
			return nil, fmt.Errorf("template %s: %w", entry.Name(), err)
		}
		if _, exists := result[tmpl.ID]; exists {
			return nil, fmt.Errorf("duplicate template id %q", tmpl.ID)
		}
		result[tmpl.ID] = tmpl
	}
	return result, nil
}
