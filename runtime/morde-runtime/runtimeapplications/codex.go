package runtimeapplications

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"morde-runtime/contract"
)

const (
	codexBinaryPath          = "/opt/nodejs/22/bin/codex"
	codexHome                = "/home/gem"
	codexTimeout             = 2 * time.Second
	maxVersionBytes          = 256
	arg0CleanupWarningPrefix = "WARNING: failed to clean up stale arg0 temp dirs: Cross-device link (os error 18)"
)

type commandSpec struct {
	path          string
	args          []string
	dir           string
	env           []string
	uid           uint32
	gid           uint32
	captureStdout bool
	captureStderr bool
}

type commandOutcome struct {
	stdout          []byte
	stderr          []byte
	exitCode        int
	truncated       bool
	stderrTruncated bool
	err             error
}

type commandRunner func(context.Context, commandSpec) commandOutcome

type codexProbe struct {
	binaryPath string
	timeout    time.Duration
	run        commandRunner
}

// ProbeCodex reports only a fixed application identity, validated CLI version,
// and the closed login-state enum. Process output is never returned to callers.
func ProbeCodex(ctx context.Context) contract.ApplicationStatusItem {
	if ctx == nil {
		ctx = context.Background()
	}
	return (codexProbe{binaryPath: codexBinaryPath, timeout: codexTimeout, run: runCodexProcess}).probe(ctx)
}

func (p codexProbe) probe(ctx context.Context) contract.ApplicationStatusItem {
	item := contract.ApplicationStatusItem{ID: "codex", Installed: true, LoginStatus: contract.ApplicationLoginUnknown}
	info, err := os.Lstat(p.binaryPath)
	if errors.Is(err, os.ErrNotExist) {
		return contract.ApplicationStatusItem{ID: "codex", Installed: false, LoginStatus: contract.ApplicationLoginNotApplicable}
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return item
	}
	if p.run == nil || p.timeout <= 0 {
		return item
	}
	versionResult := p.runBounded(ctx, []string{"--version"}, true)
	if versionResult.err != nil || versionResult.exitCode != 0 || versionResult.truncated {
		return item
	}
	version := parseVersion(versionResult.stdout)
	if version == "" {
		return item
	}
	item.Version = version

	loginResult := p.runBounded(ctx, []string{"login", "status"}, false)
	item.LoginStatus = parseLoginStatus(loginResult)
	return item
}

func (p codexProbe) runBounded(parent context.Context, args []string, capture bool) commandOutcome {
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	result := p.run(ctx, commandSpec{
		path: p.binaryPath, args: args, dir: codexHome,
		env: []string{"HOME=/home/gem", "PATH=/opt/nodejs/22/bin:/usr/bin:/bin", "USER=gem", "LOGNAME=gem"},
		uid: 1000, gid: 1000, captureStdout: capture, captureStderr: !capture,
	})
	if err := ctx.Err(); err != nil {
		return commandOutcome{err: err}
	}
	return result
}

func parseLoginStatus(result commandOutcome) contract.ApplicationLoginStatus {
	if result.err != nil || result.stderrTruncated {
		return contract.ApplicationLoginUnknown
	}
	switch result.exitCode {
	case 0:
		if len(result.stderr) == 0 {
			return contract.ApplicationLoginLoggedIn
		}
	case 1:
		if isNotLoggedInDiagnostic(result.stderr) {
			return contract.ApplicationLoginNotLoggedIn
		}
	}
	return contract.ApplicationLoginUnknown
}

func isNotLoggedInDiagnostic(stderr []byte) bool {
	message := string(stderr)
	if strings.HasSuffix(message, "\n") {
		message = strings.TrimSuffix(message, "\n")
	}
	lines := strings.Split(message, "\n")
	for index := range lines {
		lines[index] = strings.TrimSuffix(lines[index], "\r")
	}
	if len(lines) == 1 {
		return lines[0] == "Not logged in"
	}
	return len(lines) == 2 && lines[0] == arg0CleanupWarningPrefix && lines[1] == "Not logged in"
}

func parseVersion(output []byte) string {
	if len(output) == 0 || len(output) > maxVersionBytes {
		return ""
	}
	line := string(output)
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if strings.ContainsAny(line, "\r\n") {
		return ""
	}
	const prefix = "codex-cli "
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	version := strings.TrimPrefix(line, prefix)
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return ""
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return ""
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return ""
			}
		}
	}
	return version
}
