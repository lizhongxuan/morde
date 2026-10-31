package syncnodequota

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"morde/server/internal/domain"
)

const maxUnixSocketPath = 103

type Endpoint struct {
	listener   *net.UnixListener
	server     *http.Server
	path       string
	socketInfo os.FileInfo
	closeOnce  sync.Once
	closeErr   error
}

// Listen creates an owner-bound endpoint. The socket parent is writable only
// by the Portal UID; the node GID can traverse it and connect to the socket.
func Listen(path, ownerID string, service Service, socketGroup int) (*Endpoint, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || len(path) > maxUnixSocketPath ||
		!domain.ValidUserID(ownerID) || service == nil || socketGroup < 0 {
		return nil, errors.New("invalid sync node quota socket configuration")
	}
	if err := validateSocketParent(filepath.Dir(path), socketGroup); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	handler, err := NewHandler(ownerID, service)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on sync node quota socket: %w", err)
	}
	listener.SetUnlinkOnClose(true)
	createdInfo, err := os.Lstat(path)
	if err != nil || createdInfo.Mode()&os.ModeSocket == 0 || !ownedBy(createdInfo, os.Geteuid()) {
		_ = listener.Close()
		return nil, errors.New("created sync node quota endpoint is not the expected socket")
	}
	listener.SetUnlinkOnClose(false)
	cleanup := func() {
		_ = listener.Close()
		removeSocketIfSame(path, createdInfo)
	}
	if err := os.Chown(path, -1, socketGroup); err != nil {
		cleanup()
		return nil, fmt.Errorf("set sync node quota socket group: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		cleanup()
		return nil, fmt.Errorf("restrict sync node quota socket permissions: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || !ownedBy(info, os.Geteuid()) || !hasGroup(info, socketGroup) || info.Mode().Perm() != 0o660 || !os.SameFile(createdInfo, info) {
		cleanup()
		return nil, errors.New("created sync node quota socket has unsafe ownership or permissions")
	}
	return &Endpoint{
		listener: listener, path: path, socketInfo: info,
		server: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 3 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Minute,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    16 << 10,
		},
	}, nil
}

func (e *Endpoint) Serve(ctx context.Context) error {
	if e == nil || e.listener == nil || e.server == nil || ctx == nil {
		return errors.New("sync node quota endpoint and context are required")
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- e.server.Serve(e.listener) }()
	select {
	case err := <-serveErr:
		_ = e.Close()
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.server.Shutdown(shutdownCtx); err != nil {
			_ = e.server.Close()
			_ = e.listener.Close()
			_ = e.Close()
			return err
		}
		return e.Close()
	}
}

func (e *Endpoint) Close() error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		if e.server != nil {
			if err := e.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				e.closeErr = errors.Join(e.closeErr, err)
			}
		}
		if e.listener != nil {
			if err := e.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				e.closeErr = errors.Join(e.closeErr, err)
			}
		}
		removeSocketIfSame(e.path, e.socketInfo)
	})
	return e.closeErr
}

func validateSocketParent(path string, socketGroup int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("sync node quota socket parent path must be clean and absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("sync node quota socket parent must not contain symlinks")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedBy(info, os.Geteuid()) {
		return errors.New("sync node quota socket parent must be a Portal-owned directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Gid) != socketGroup || info.Mode().Perm() != 0o710 || info.Mode()&os.ModeSetgid == 0 {
		return errors.New("sync node quota socket parent must be setgid 02710 with node-group traversal only")
	}
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		ancestor, err := os.Lstat(current)
		if err != nil || !ancestor.IsDir() || ancestor.Mode()&os.ModeSymlink != 0 {
			return errors.New("sync node quota socket ancestors must be real directories")
		}
		if ancestor.Mode().Perm()&0o022 != 0 && ancestor.Mode()&os.ModeSticky == 0 {
			return errors.New("sync node quota socket ancestors must not be writable without the sticky bit")
		}
		if current == "/" {
			break
		}
	}
	return nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing sync node quota socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || !ownedBy(info, os.Geteuid()) {
		return errors.New("sync node quota socket path is occupied by an unsafe file")
	}
	connection, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return errors.New("sync node quota socket already has a listener")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return fmt.Errorf("sync node quota socket state is unavailable: %w", dialErr)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return errors.New("sync node quota socket changed while checking stale state")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale sync node quota socket: %w", err)
	}
	return nil
}

func removeSocketIfSame(path string, expected os.FileInfo) {
	if expected == nil {
		return
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSocket == 0 || !os.SameFile(expected, current) {
		return
	}
	_ = os.Remove(path)
}

func ownedBy(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

func hasGroup(info os.FileInfo, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Gid) == gid
}
