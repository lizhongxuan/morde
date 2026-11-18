package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
)

type agentWorkspaceFileProviderStub struct {
	value workspace.Workspace
	owner string
}

func (s *agentWorkspaceFileProviderStub) EnsureAgentWorkspace(_ context.Context, ownerID string) (workspace.Workspace, error) {
	s.owner = ownerID
	return s.value, nil
}

type agentWorkspaceDirectoryStub struct {
	entries []workspace.DirectoryEntry
	opened  string
}

func (s *agentWorkspaceDirectoryStub) ReadDirectory(_, _, _ string) ([]workspace.DirectoryEntry, error) {
	return s.entries, nil
}

func (s *agentWorkspaceDirectoryStub) OpenRegularFile(_, _, relativePath string) (*os.File, os.FileInfo, error) {
	s.opened = relativePath
	file, err := os.Open(filepath.Join(os.TempDir(), "agent-workspace-test-file"))
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	return file, info, err
}

func TestAgentWorkspaceFilesListIsOwnerScopedAndPaged(t *testing.T) {
	provider := &agentWorkspaceFileProviderStub{value: workspace.Workspace{ID: "w1234567", OwnerID: "u1234567", State: workspace.StateReady}}
	current := &agentWorkspaceDirectoryStub{entries: []workspace.DirectoryEntry{
		{Name: "a.txt", Kind: "file", SizeBytes: 1, ModifiedAt: time.Unix(1, 0).UTC()},
		{Name: "b.txt", Kind: "file", SizeBytes: 2, ModifiedAt: time.Unix(2, 0).UTC()},
		{Name: "c", Kind: "directory", ModifiedAt: time.Unix(3, 0).UTC()},
	}}
	service, err := NewAgentWorkspaceFilesService(provider, current)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.List(context.Background(), identity.Principal{UserID: "u1234567", Kind: identity.KindSession}, "w1234567", "", "a.txt", 1)
	if err != nil || provider.owner != "u1234567" || len(page.Entries) != 1 || page.Entries[0].Name != "b.txt" || page.NextCursor != "b.txt" {
		t.Fatalf("List() = %#v, %v; owner=%q", page, err, provider.owner)
	}
	if _, err := service.List(context.Background(), identity.Principal{UserID: "u1234567", Kind: identity.KindSession}, "w7654321", "", "", 10); err == nil {
		t.Fatal("List() accepted a different Workspace ID")
	}
	if _, err := service.List(context.Background(), identity.Principal{UserID: "u1234567", Kind: identity.KindClient}, "w1234567", "", "", 10); err == nil {
		t.Fatal("List() accepted a non-session principal")
	}
}

func TestAgentWorkspaceFilesRejectsUnsafeFilePath(t *testing.T) {
	provider := &agentWorkspaceFileProviderStub{value: workspace.Workspace{ID: "w1234567", OwnerID: "u1234567", State: workspace.StateReady}}
	current := &agentWorkspaceDirectoryStub{}
	service, err := NewAgentWorkspaceFilesService(provider, current)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.Open(context.Background(), identity.Principal{UserID: "u1234567", Kind: identity.KindSession}, "w1234567", "../secret")
	if err == nil || current.opened != "" {
		t.Fatalf("Open() = %v, reader path=%q; wanted unsafe path rejection", err, current.opened)
	}
}

func TestAgentWorkspaceFilesAndHostUseTheSameDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Workspace file access uses Linux openat2 for containment")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current, err := workspacecurrent.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Close() })
	ownerID, workspaceID := "u1234567", "w1234567"
	principal := identity.Principal{UserID: ownerID, Kind: identity.KindSession}
	provider := &agentWorkspaceFileProviderStub{value: workspace.Workspace{ID: workspaceID, OwnerID: ownerID, State: workspace.StateReady}}
	service, err := NewAgentWorkspaceFilesService(provider, current)
	if err != nil {
		t.Fatal(err)
	}
	rootMetadata := []workspace.Directory{{Path: ".", MetadataKnown: true}}

	// Agent materialization is visible at the path mounted by the Host.
	agentContent := []byte("written by Agent")
	agentDigest := sha256.Sum256(agentContent)
	agentFile := workspace.File{Kind: workspace.FileKindRegular, Path: "agent.txt", SizeBytes: int64(len(agentContent)),
		SHA256: hex.EncodeToString(agentDigest[:]), Mode: 0o600, MetadataKnown: true,
		Chunks: []workspace.Chunk{{SHA256: hex.EncodeToString(agentDigest[:]), SizeBytes: int64(len(agentContent))}}}
	if err := current.Materialize(context.Background(), ownerID, workspaceID, 1, []workspace.File{agentFile}, nil, rootMetadata,
		directWorkspaceBlocks{agentFile.SHA256: agentContent}); err != nil {
		t.Fatalf("materialize Agent file: %v", err)
	}
	hostMountPath, err := current.WorkspacePath(ownerID, workspaceID)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			t.Skip("the Linux kernel does not provide openat2")
		}
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(hostMountPath, agentFile.Path)); err != nil || string(got) != string(agentContent) {
		t.Fatalf("Host view of Agent file = %q, %v", got, err)
	}

	// A Host write appears in the Portal reader without a sync or rematerialization.
	hostContent := []byte("written by Host")
	if err := os.WriteFile(filepath.Join(hostMountPath, "host.txt"), hostContent, 0o600); err != nil {
		t.Fatalf("write Host file: %v", err)
	}
	page, err := service.List(context.Background(), principal, workspaceID, "", "", 10)
	if err != nil || len(page.Entries) != 2 {
		t.Fatalf("Portal listing after Host write = %#v, %v", page, err)
	}
	file, info, err := service.Open(context.Background(), principal, workspaceID, "host.txt")
	if err != nil {
		t.Fatalf("open Host-written file through Agent Workspace service: %v", err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil || string(got) != string(hostContent) || info.Size() != int64(len(hostContent)) {
		t.Fatalf("Agent view of Host file = %q info=%#v, %v", got, info, err)
	}
}

type directWorkspaceBlocks map[string][]byte

func (b directWorkspaceBlocks) Open(_ string, digest string) (io.ReadCloser, error) {
	content, ok := b[digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}
