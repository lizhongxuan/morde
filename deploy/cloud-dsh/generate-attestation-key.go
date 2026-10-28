package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: generate-attestation-key <private-key-path> <public-key-path>")
	}
	for _, path := range args {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return errors.New("key paths must be clean absolute file paths")
		}
		parent, err := os.Lstat(filepath.Dir(path))
		if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0o022 != 0 {
			return errors.New("key parent directory must be a trusted non-writable directory")
		}
	}
	if args[0] == args[1] {
		return errors.New("private and public key paths must be different")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate Guardian attestation key: %w", err)
	}
	if err := writeNew(args[0], private, 0o600); err != nil {
		return fmt.Errorf("write Guardian private key: %w", err)
	}
	if err := writeNew(args[1], public, 0o644); err != nil {
		_ = os.Remove(args[0])
		return fmt.Errorf("write Guardian public key: %w", err)
	}
	return nil
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	committed = true
	return nil
}
