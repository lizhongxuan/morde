package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"morde/server/internal/dshguardian"
)

func main() {
	if err := run(); err != nil {
		log.Printf("dsh guardian stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("dsh-guardian", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	socket := flags.String("socket", "/run/morde/dsh-guardian.sock", "private Unix socket for the Portal guardian client")
	socketMode := flags.Int("socket-mode", 0o600, "guardian Unix socket permissions (0600 or 0660)")
	stateDir := flags.String("state-dir", "/var/lib/morde/dsh-guardian", "private durable lease directory")
	dockerSocket := flags.String("docker-socket", "/var/run/docker.sock", "local Docker Engine Unix socket")
	attestationKeyFile := flags.String("attestation-key-file", "", "protected raw Ed25519 private key for stop attestations")
	interval := flags.Duration("sweep-interval", time.Second, "lease expiry sweep interval")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *interval <= 0 || *attestationKeyFile == "" || (*socketMode != 0o600 && *socketMode != 0o660) {
		return errors.New("no positional arguments are accepted; sweep-interval must be positive, socket-mode must be 0600 or 0660, and attestation-key-file is required")
	}
	attestationKey, err := dshguardian.LoadAttestationPrivateKey(*attestationKeyFile)
	if err != nil {
		return fmt.Errorf("load dsh guardian attestation key: %w", err)
	}
	service, err := dshguardian.Open(*stateDir, *dockerSocket, attestationKey)
	for i := range attestationKey {
		attestationKey[i] = 0
	}
	if err != nil {
		return fmt.Errorf("initialize dsh guardian: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("dsh guardian listening on restricted Unix socket %s", *socket)
	err = service.Serve(ctx, *socket, os.FileMode(*socketMode), *interval, func(sweepErr error) {
		log.Printf("dsh guardian retrying lease enforcement: %v", sweepErr)
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
