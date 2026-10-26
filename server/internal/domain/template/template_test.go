package template

import (
	"strings"
	"testing"
	"time"
)

func TestQuantityConversion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"500m", 500}, {"2", 2000},
	} {
		got, err := CPUMilli(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("CPUMilli(%q) = %d, %v", tc.in, got, err)
		}
	}
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"1536Mi", 1536}, {"4Gi", 4096}, {"2048Ki", 2},
	} {
		got, err := MemMiB(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("MemMiB(%q) = %d, %v", tc.in, got, err)
		}
	}
	if _, err := CPUMilli("0.5"); err == nil {
		t.Fatal("accepted fractional CPU quantity")
	}
	if _, err := MemMiB("1537Ki"); err == nil {
		t.Fatal("accepted memory smaller than one MiB")
	}
}

func TestTemplateValidate(t *testing.T) {
	tmpl := Template{
		ID: "std-2c4g", Name: "Standard", Image: "example/host:1@sha256:" + strings.Repeat("a", 64), Entrypoint: []string{RuntimeBootstrapEntrypoint},
		Resources: Resources{Requests: Resource{CPU: "500m", Memory: "1536Mi"}, Limits: Resource{CPU: "2", Memory: "4Gi"}},
		Workspace: Workspace{MountPath: "/home/gem/workspace", Size: "20Gi"},
		Lifecycle: Lifecycle{TTL: 8 * 60 * 60 * 1e9, IdleTimeout: 2 * 60 * 60 * 1e9, OnIdle: OnIdleDisabled},
		Isolation: "container",
	}
	if err := tmpl.Validate([]string{"container", "vm"}); err != nil {
		t.Fatal(err)
	}
	tmpl.Isolation = "missing"
	if err := tmpl.Validate([]string{"container"}); err == nil {
		t.Fatal("accepted unconfigured isolation")
	}
}

func TestTemplateValidateRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*Template)
	}{
		{name: "invalid ID", want: "template.id", mutate: func(t *Template) { t.ID = "Has Spaces" }},
		{name: "missing name", want: "template name and image", mutate: func(t *Template) { t.Name = " \t" }},
		{name: "missing image", want: "template name and image", mutate: func(t *Template) { t.Image = "" }},
		{name: "mutable image", want: "pinned sha256 digest", mutate: func(t *Template) { t.Image = "example/host:latest" }},
		{name: "invalid image digest", want: "pinned sha256 digest", mutate: func(t *Template) { t.Image = "example/host@sha256:invalid" }},
		{name: "missing entrypoint", want: "template.entrypoint is required", mutate: func(t *Template) { t.Entrypoint = nil }},
		{name: "blank entrypoint argument", want: "template.entrypoint contains an empty value", mutate: func(t *Template) { t.Entrypoint = []string{"/run.sh", "  "} }},
		{name: "untrusted entrypoint", want: "Morde runtime bootstrap", mutate: func(t *Template) { t.Entrypoint = []string{"/opt/gem/run.sh"} }},
		{name: "invalid requested CPU", want: "resources.requests.cpu", mutate: func(t *Template) { t.Resources.Requests.CPU = "0.5" }},
		{name: "invalid CPU limit", want: "resources.limits.cpu", mutate: func(t *Template) { t.Resources.Limits.CPU = "0.5" }},
		{name: "invalid requested memory", want: "resources.requests.memory", mutate: func(t *Template) { t.Resources.Requests.Memory = "nope" }},
		{name: "invalid memory limit", want: "resources.limits.memory", mutate: func(t *Template) { t.Resources.Limits.Memory = "nope" }},
		{name: "CPU request above limit", want: "requests exceed limits", mutate: func(t *Template) { t.Resources.Requests.CPU = "3" }},
		{name: "memory request above limit", want: "requests exceed limits", mutate: func(t *Template) { t.Resources.Requests.Memory = "5Gi" }},
		{name: "missing mount path", want: "workspace.mountPath", mutate: func(t *Template) { t.Workspace.MountPath = " " }},
		{name: "invalid workspace size", want: "workspace.size", mutate: func(t *Template) { t.Workspace.Size = "1.5Gi" }},
		{name: "missing TTL", want: "lifecycle durations", mutate: func(t *Template) { t.Lifecycle.TTL = 0 }},
		{name: "missing idle timeout", want: "lifecycle durations", mutate: func(t *Template) { t.Lifecycle.IdleTimeout = 0 }},
		{name: "unsafe onIdle", want: "disabled until activity leases", mutate: func(t *Template) { t.Lifecycle.OnIdle = "delete" }},
		{name: "unconfigured isolation", want: "not configured", mutate: func(t *Template) { t.Isolation = "vm" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := validTemplate()
			tc.mutate(&tmpl)
			if err := tmpl.Validate([]string{"container"}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestTemplateValidateAcceptsDisabledIdlePolicy(t *testing.T) {
	tmpl := validTemplate()
	tmpl.Lifecycle.OnIdle = OnIdleDisabled
	if err := tmpl.Validate([]string{"container"}); err != nil {
		t.Fatalf("Validate() rejected supported options: %v", err)
	}
}

func validTemplate() Template {
	return Template{
		ID: "std-2c4g", Name: "Standard", Image: "example/host:1@sha256:" + strings.Repeat("a", 64), Entrypoint: []string{RuntimeBootstrapEntrypoint},
		Resources: Resources{Requests: Resource{CPU: "500m", Memory: "1536Mi"}, Limits: Resource{CPU: "2", Memory: "4Gi"}},
		Workspace: Workspace{MountPath: "/workspace", Size: "20Gi"},
		Lifecycle: Lifecycle{TTL: 8 * time.Hour, IdleTimeout: 2 * time.Hour, OnIdle: OnIdleDisabled},
		Isolation: "container",
	}
}
