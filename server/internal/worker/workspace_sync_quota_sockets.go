package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"morde/server/internal/domain"
	"morde/server/internal/syncnodequota"
)

const workspaceSyncNodeQuotaGroupID = 65532

type WorkspaceSyncQuotaSocketManager interface {
	EnsureOwner(context.Context, string) error
	Close() error
}

type WorkspaceSyncNodeQuotaSockets struct {
	root    string
	groupID int
	service syncnodequota.Service
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	owners  map[string]*syncnodequota.Endpoint
}

func NewWorkspaceSyncNodeQuotaSockets(root string, service syncnodequota.Service) (*WorkspaceSyncNodeQuotaSockets, error) {
	if !cleanDockerWorkspaceSyncPath(root) || service == nil {
		return nil, errors.New("workspace sync quota socket root and service are required")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !workspaceSyncQuotaPathOwnedBy(info, os.Geteuid()) {
		return nil, errors.New("workspace sync quota socket root must be a protected Portal-owned directory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &WorkspaceSyncNodeQuotaSockets{
		root: root, groupID: workspaceSyncNodeQuotaGroupID, service: service, ctx: ctx, cancel: cancel,
		owners: make(map[string]*syncnodequota.Endpoint),
	}, nil
}

func (m *WorkspaceSyncNodeQuotaSockets) EnsureOwner(ctx context.Context, ownerID string) error {
	if m == nil || ctx == nil || !domain.ValidUserID(ownerID) {
		return errors.New("workspace sync quota socket owner is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.owners[ownerID]; exists {
		return nil
	}
	ownerHash := sha256.Sum256([]byte(ownerID))
	ownerDir := filepath.Join(m.root, hex.EncodeToString(ownerHash[:]))
	if err := os.Mkdir(ownerDir, 0o2710); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Workspace sync quota owner directory: %w", err)
	}
	info, err := os.Lstat(ownerDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !workspaceSyncQuotaPathOwnedBy(info, os.Geteuid()) {
		return errors.New("workspace sync quota owner directory is unsafe")
	}
	if err := os.Chown(ownerDir, -1, m.groupID); err != nil {
		return fmt.Errorf("set Workspace sync quota owner directory group: %w", err)
	}
	if err := os.Chmod(ownerDir, 0o2710); err != nil {
		return fmt.Errorf("protect Workspace sync quota owner directory: %w", err)
	}
	info, err = os.Lstat(ownerDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o710 || info.Mode()&os.ModeSetgid == 0 || !workspaceSyncQuotaPathOwnedBy(info, os.Geteuid()) || !workspaceSyncQuotaPathHasGroup(info, m.groupID) {
		return errors.New("workspace sync quota owner directory permissions are unsafe")
	}
	endpoint, err := syncnodequota.Listen(filepath.Join(ownerDir, "quota.sock"), ownerID, m.service, m.groupID)
	if err != nil {
		return fmt.Errorf("listen on Workspace sync quota socket: %w", err)
	}
	m.owners[ownerID] = endpoint
	go func() {
		if err := endpoint.Serve(m.ctx); err != nil {
			m.mu.Lock()
			if m.owners[ownerID] == endpoint {
				delete(m.owners, ownerID)
			}
			m.mu.Unlock()
		}
	}()
	return nil
}

func (m *WorkspaceSyncNodeQuotaSockets) Close() error {
	if m == nil {
		return nil
	}
	m.cancel()
	m.mu.Lock()
	endpoints := make([]*syncnodequota.Endpoint, 0, len(m.owners))
	for _, endpoint := range m.owners {
		endpoints = append(endpoints, endpoint)
	}
	m.owners = make(map[string]*syncnodequota.Endpoint)
	m.mu.Unlock()
	var closeErr error
	for _, endpoint := range endpoints {
		closeErr = errors.Join(closeErr, endpoint.Close())
	}
	return closeErr
}

func workspaceSyncQuotaPathOwnedBy(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

func workspaceSyncQuotaPathHasGroup(info os.FileInfo, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Gid) == gid
}
