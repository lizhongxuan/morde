package gateway

import (
	"errors"
	"testing"
)

func TestParseHost(t *testing.T) {
	tests := []struct {
		name        string
		authority   string
		suffix      string
		wantHostID  string
		wantPreview int
		wantErr     bool
	}{
		{name: "host", authority: "hk3x9a2b.example-hosts.com", suffix: "example-hosts.com", wantHostID: "hk3x9a2b"},
		{name: "port on authority", authority: "hk3x9a2b.example-hosts.com:7081", suffix: "example-hosts.com", wantHostID: "hk3x9a2b"},
		{name: "uppercase normalized", authority: "HK3X9A2B.EXAMPLE-HOSTS.COM", suffix: "EXAMPLE-HOSTS.COM", wantHostID: "hk3x9a2b"},
		{name: "preview", authority: "5173-hk3x9a2b.example-hosts.com:7081", suffix: "example-hosts.com", wantHostID: "hk3x9a2b", wantPreview: 5173},
		{name: "lowest preview port", authority: "1-hk3x9a2b.example-hosts.com", suffix: "example-hosts.com", wantHostID: "hk3x9a2b", wantPreview: 1},
		{name: "highest preview port", authority: "65535-hk3x9a2b.example-hosts.com", suffix: "example-hosts.com", wantHostID: "hk3x9a2b", wantPreview: 65535},
		{name: "wrong suffix", authority: "hk3x9a2b.evil.test", suffix: "example-hosts.com", wantErr: true},
		{name: "wrong host id", authority: "H1.example-hosts.com", suffix: "example-hosts.com", wantErr: true},
		{name: "extra label", authority: "a.hk3x9a2b.example-hosts.com", suffix: "example-hosts.com", wantErr: true},
		{name: "leading zero preview port", authority: "08-hk3x9a2b.example-hosts.com", suffix: "example-hosts.com", wantErr: true},
		{name: "preview port too long", authority: "100000-hk3x9a2b.example-hosts.com", suffix: "example-hosts.com", wantErr: true},
		{name: "preview port zero", authority: "0-hk3x9a2b.example-hosts.com", suffix: "example-hosts.com", wantErr: true},
		{name: "preview port too high", authority: "65536-hk3x9a2b.example-hosts.com", suffix: "example-hosts.com", wantErr: true},
		{name: "invalid authority port", authority: "hk3x9a2b.example-hosts.com:nope", suffix: "example-hosts.com", wantErr: true},
		{name: "empty authority port", authority: "hk3x9a2b.example-hosts.com:", suffix: "example-hosts.com", wantErr: true},
		{name: "empty suffix", authority: "hk3x9a2b.example-hosts.com", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHost(tt.authority, tt.suffix)
			if tt.wantErr {
				if !errors.Is(err, errInvalidHost) {
					t.Fatalf("parseHost() error = %v, want %v", err, errInvalidHost)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHost() error = %v", err)
			}
			if got.HostID != tt.wantHostID {
				t.Errorf("HostID = %q, want %q", got.HostID, tt.wantHostID)
			}
			if tt.wantPreview == 0 {
				if got.PreviewPort != nil {
					t.Errorf("PreviewPort = %d, want nil", *got.PreviewPort)
				}
				return
			}
			if got.PreviewPort == nil || *got.PreviewPort != tt.wantPreview {
				t.Errorf("PreviewPort = %v, want %d", got.PreviewPort, tt.wantPreview)
			}
		})
	}
}
