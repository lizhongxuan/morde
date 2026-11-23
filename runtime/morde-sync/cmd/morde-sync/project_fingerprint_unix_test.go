//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStdioProjectFingerprintIsLocalAndIdentityBound(t *testing.T) {
	directory := t.TempDir()
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	params := map[string]any{"directory": directory, "device": fmt.Sprint(stat.Dev), "inode": fmt.Sprint(stat.Ino)}
	readFingerprint := func() string {
		t.Helper()
		response := invokeRPC(t, map[string]any{"jsonrpc": "2.0", "id": "fingerprint", "method": "project_fingerprint", "params": params})
		if response.Error != nil {
			t.Fatalf("fingerprint RPC failed: %+v", response.Error)
		}
		var result struct {
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.Unmarshal(response.Result, &result); err != nil || len(result.Fingerprint) != 64 {
			t.Fatalf("invalid fingerprint RPC result: %v", err)
		}
		return result.Fingerprint
	}
	initial := readFingerprint()
	if err := os.WriteFile(filepath.Join(directory, "synthetic.txt"), []byte("local data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if readFingerprint() == initial {
		t.Fatal("local file addition did not change the fingerprint")
	}
	params["inode"] = "0"
	response := invokeRPC(t, map[string]any{"jsonrpc": "2.0", "id": "wrong-root", "method": "project_fingerprint", "params": params})
	if response.Error == nil {
		t.Fatal("fingerprint RPC accepted the wrong root identity")
	}
}
