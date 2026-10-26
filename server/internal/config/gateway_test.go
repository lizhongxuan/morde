package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayValidate(t *testing.T) {
	workbenchDir := t.TempDir()
	valid := Gateway{
		WorkbenchDir:    workbenchDir,
		PreviewPortMin:  1024,
		PreviewPortDeny: []int{8080},
		MaxBodyBytes:    1 << 30,
	}

	tests := []struct {
		name   string
		change func(*Gateway)
		want   string
	}{
		{name: "valid", change: func(*Gateway) {}},
		{name: "missing workbench dir", change: func(g *Gateway) { g.WorkbenchDir = "" }, want: "gateway.workbench_dir"},
		{name: "missing path", change: func(g *Gateway) { g.WorkbenchDir = filepath.Join(workbenchDir, "missing") }, want: "gateway.workbench_dir"},
		{name: "workbench path is file", change: func(g *Gateway) {
			file := filepath.Join(workbenchDir, "index.html")
			if err := os.WriteFile(file, []byte("ok"), 0o600); err != nil {
				t.Fatal(err)
			}
			g.WorkbenchDir = file
		}, want: "must be a directory"},
		{name: "preview minimum too low", change: func(g *Gateway) { g.PreviewPortMin = 0 }, want: "gateway.preview_port_min"},
		{name: "preview minimum too high", change: func(g *Gateway) { g.PreviewPortMin = 65536 }, want: "gateway.preview_port_min"},
		{name: "denied port out of range", change: func(g *Gateway) { g.PreviewPortDeny = []int{65536} }, want: "gateway.preview_port_deny"},
		{name: "duplicate denied port", change: func(g *Gateway) { g.PreviewPortDeny = []int{8080, 8080} }, want: "duplicate port 8080"},
		{name: "zero body limit", change: func(g *Gateway) { g.MaxBodyBytes = 0 }, want: "gateway.max_body_bytes"},
		{name: "negative body limit", change: func(g *Gateway) { g.MaxBodyBytes = -1 }, want: "gateway.max_body_bytes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := valid
			tt.change(&got)
			err := got.validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}
