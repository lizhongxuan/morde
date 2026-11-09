//go:build !linux

package runtime_desktop

func newSystemBackend() (desktopBackend, error) { return nil, ErrDesktopUnavailable }
