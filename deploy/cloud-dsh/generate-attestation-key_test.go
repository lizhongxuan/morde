package main

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateAttestationKeyCreatesProtectedPairWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "guardian.key")
	publicPath := filepath.Join(dir, "guardian.pub")
	if err := run([]string{privatePath, publicPath}); err != nil {
		t.Fatal(err)
	}
	private, err := os.ReadFile(privatePath)
	if err != nil || len(private) != ed25519.PrivateKeySize {
		t.Fatalf("private key length = %d, err = %v", len(private), err)
	}
	public, err := os.ReadFile(publicPath)
	if err != nil || len(public) != ed25519.PublicKeySize || string(private[ed25519.SeedSize:]) != string(public) {
		t.Fatalf("public key does not match generated private key: length=%d, err=%v", len(public), err)
	}
	privateInfo, err := os.Stat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if privateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("private key permissions = %v", privateInfo.Mode().Perm())
	}
	publicInfo, err := os.Stat(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if publicInfo.Mode().Perm() != 0o644 {
		t.Fatalf("public key permissions = %v", publicInfo.Mode().Perm())
	}
	if err := run([]string{privatePath, publicPath}); err == nil {
		t.Fatal("refused overwrite of existing signing keys")
	}
}
