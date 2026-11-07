//go:build linux

package runtime_desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"morde-runtime/runtimecore"
)

const (
	maxCommandOutput = 1 << 20
)

var dimensionsPattern = regexp.MustCompile(`(?m)^\s*dimensions:\s*([0-9]+)x([0-9]+) pixels`)

type executablePaths struct {
	xdpyinfo string
	xdotool  string
	importer string
}

type linuxBackend struct {
	displayName string
	paths       executablePaths
}

func newSystemBackend() (desktopBackend, error) {
	display := os.Getenv("DISPLAY")
	if display == "" || len(display) > 256 || strings.IndexByte(display, 0) >= 0 {
		return nil, ErrDesktopUnavailable
	}
	paths := executablePaths{xdpyinfo: "/usr/bin/xdpyinfo", xdotool: "/usr/bin/xdotool", importer: "/usr/bin/import"}
	for _, path := range []string{paths.xdpyinfo, paths.xdotool, paths.importer} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || filepath.Clean(path) != path {
			return nil, ErrDesktopUnavailable
		}
	}
	return &linuxBackend{displayName: display, paths: paths}, nil
}

func (b *linuxBackend) probe(ctx context.Context) (displaySize, error) {
	size, err := b.display(ctx)
	if err != nil {
		return displaySize{}, err
	}
	geometry, err := b.run(ctx, b.paths.xdotool, 64, "getdisplaygeometry")
	if err != nil {
		return displaySize{}, err
	}
	parts := strings.Fields(string(geometry))
	if len(parts) != 2 {
		return displaySize{}, ErrDesktopUnavailable
	}
	width, widthErr := strconv.Atoi(parts[0])
	height, heightErr := strconv.Atoi(parts[1])
	if widthErr != nil || heightErr != nil || width != size.width || height != size.height {
		return displaySize{}, ErrDesktopUnavailable
	}
	formats, err := b.run(ctx, b.paths.importer, maxCommandOutput, "-list", "format")
	if err != nil || !bytes.Contains(formats, []byte("JPEG")) {
		return displaySize{}, ErrDesktopUnavailable
	}
	return size, nil
}

func (b *linuxBackend) display(ctx context.Context) (displaySize, error) {
	output, err := b.run(ctx, b.paths.xdpyinfo, maxCommandOutput, "-display", b.displayName)
	if err != nil {
		return displaySize{}, err
	}
	match := dimensionsPattern.FindSubmatch(output)
	if len(match) != 3 {
		return displaySize{}, ErrDesktopUnavailable
	}
	width, widthErr := strconv.Atoi(string(match[1]))
	height, heightErr := strconv.Atoi(string(match[2]))
	if widthErr != nil || heightErr != nil || !validDisplaySize(displaySize{width: width, height: height}) {
		return displaySize{}, ErrDesktopUnavailable
	}
	return displaySize{width: width, height: height}, nil
}

func (b *linuxBackend) screenshot(ctx context.Context) ([]byte, error) {
	for _, quality := range []string{"75", "60", "45", "30"} {
		data, err := b.run(ctx, b.paths.importer, maxScreenshotBytes, "-window", "root", "-resize", "1280x720>", "-strip", "-quality", quality, "jpeg:-")
		if errors.Is(err, errCommandOutputTooLarge) {
			continue
		}
		if err != nil {
			return nil, ErrDesktopUnavailable
		}
		if len(data) == 0 {
			return nil, ErrDesktopUnavailable
		}
		config, decodeErr := jpeg.DecodeConfig(bytes.NewReader(data))
		if decodeErr == nil && config.Width > 0 && config.Height > 0 && config.Width <= 1280 && config.Height <= 720 && len(data) <= maxScreenshotBytes {
			return data, nil
		}
		return nil, ErrDesktopUnavailable
	}
	return nil, ErrDesktopUnavailable
}

func (b *linuxBackend) listWindows(ctx context.Context) ([]window, error) {
	if _, err := b.display(ctx); err != nil {
		return nil, err
	}
	idsOutput, err := b.run(ctx, b.paths.xdotool, 64<<10, "search", "--onlyvisible", "--maxdepth", "2", "--name", ".*")
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, err
		}
		return []window{}, nil
	}
	lines := strings.Fields(string(idsOutput))
	if len(lines) > maxDesktopWindows {
		return nil, runtimecore.ErrResultTooLarge
	}
	windows := make([]window, 0, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, rawID := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		decimalID, err := strconv.ParseUint(rawID, 10, 32)
		if err != nil || decimalID == 0 {
			return nil, ErrDesktopUnavailable
		}
		id := fmt.Sprintf("0x%x", decimalID)
		if seen[id] {
			continue
		}
		seen[id] = true
		nameOutput, err := b.run(ctx, b.paths.xdotool, 4<<10, "getwindowname", rawID)
		if err != nil {
			continue // The visible window may have closed after enumeration.
		}
		geometryOutput, err := b.run(ctx, b.paths.xdotool, 1024, "getwindowgeometry", "--shell", rawID)
		if err != nil {
			continue
		}
		geometry, err := parseGeometry(geometryOutput)
		if err != nil {
			return nil, ErrDesktopUnavailable
		}
		title := strings.TrimSuffix(string(nameOutput), "\n")
		if !utf8.ValidString(title) {
			title = strings.ToValidUTF8(title, "�")
		}
		if len(title) > maxWindowTitleBytes {
			title = truncateUTF8(title, maxWindowTitleBytes)
		}
		windows = append(windows, window{
			ID: id, Title: title, X: geometry.x, Y: geometry.y, Width: geometry.width, Height: geometry.height,
		})
	}
	activeID, _ := b.runAllowNoWindow(ctx, b.paths.xdotool, 64, "getactivewindow")
	active := strings.TrimSpace(string(activeID))
	for i := range windows {
		id, _ := strconv.ParseUint(windows[i].ID[2:], 16, 32)
		windows[i].Active = active == strconv.FormatUint(id, 10)
	}
	return windows, nil
}

func (b *linuxBackend) activate(ctx context.Context, id string) error {
	number, _ := strconv.ParseUint(id[2:], 16, 32)
	_, err := b.run(ctx, b.paths.xdotool, 64, "windowactivate", "--sync", strconv.FormatUint(number, 10))
	return err
}

func (b *linuxBackend) click(ctx context.Context, id string, x, y int, button string) error {
	if err := b.requireActiveWindow(ctx, id); err != nil {
		return err
	}
	number, err := windowIDDecimal(id)
	if err != nil {
		return err
	}
	buttonNumber := map[string]string{"left": "1", "middle": "2", "right": "3"}[button]
	_, err = b.run(ctx, b.paths.xdotool, 64,
		"mousemove", "--sync", "--window", number, strconv.Itoa(x), strconv.Itoa(y),
		"click", buttonNumber)
	return err
}

func (b *linuxBackend) typeText(ctx context.Context, id, text string) error {
	if err := b.requireActiveWindow(ctx, id); err != nil {
		return err
	}
	_, err := b.run(ctx, b.paths.xdotool, 64, "type", "--clearmodifiers", "--delay", "0", "--", text)
	return err
}

func (b *linuxBackend) pressKey(ctx context.Context, id, key string) error {
	if err := b.requireActiveWindow(ctx, id); err != nil {
		return err
	}
	keysyms := map[string]string{
		"Enter": "Return", "Tab": "Tab", "Escape": "Escape", "BackSpace": "BackSpace", "Delete": "Delete",
		"Home": "Home", "End": "End", "Page_Up": "Page_Up", "Page_Down": "Page_Down",
		"ArrowUp": "Up", "ArrowDown": "Down", "ArrowLeft": "Left", "ArrowRight": "Right",
	}
	keysym, ok := keysyms[key]
	if !ok {
		return ErrDesktopUnavailable
	}
	_, err := b.run(ctx, b.paths.xdotool, 64, "key", "--clearmodifiers", keysym)
	return err
}

func (b *linuxBackend) runAllowNoWindow(ctx context.Context, path string, limit int, args ...string) ([]byte, error) {
	output, err := b.run(ctx, path, limit, args...)
	if err == nil {
		return output, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil
	}
	return nil, err
}

func (b *linuxBackend) requireActiveWindow(ctx context.Context, id string) error {
	wanted, err := windowIDDecimal(id)
	if err != nil {
		return runtimecore.ErrInvalidRequest
	}
	output, err := b.run(ctx, b.paths.xdotool, 64, "getactivewindow")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != wanted {
		return errDesktopFocusChanged
	}
	// shortcut: focus can change before XTEST sends, replace with atomic targeted input if this race affects users.
	return nil
}

func windowIDDecimal(id string) (string, error) {
	if !validWindowID(id) {
		return "", runtimecore.ErrInvalidRequest
	}
	number, err := strconv.ParseUint(id[2:], 16, 32)
	if err != nil {
		return "", runtimecore.ErrInvalidRequest
	}
	return strconv.FormatUint(number, 10), nil
}

var errCommandOutputTooLarge = errors.New("desktop command output exceeded limit")

type cappedOutput struct {
	buffer   bytes.Buffer
	limit    int
	tooLarge bool
}

func (o *cappedOutput) Write(data []byte) (int, error) {
	remaining := o.limit - o.buffer.Len()
	if remaining > len(data) {
		remaining = len(data)
	}
	if remaining > 0 {
		_, _ = o.buffer.Write(data[:remaining])
	}
	if remaining < len(data) {
		o.tooLarge = true
	}
	return len(data), nil
}

func (b *linuxBackend) run(ctx context.Context, path string, limit int, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Env = []string{"DISPLAY=" + b.displayName, "LC_ALL=C"}
	if authority := os.Getenv("XAUTHORITY"); authority != "" {
		command.Env = append(command.Env, "XAUTHORITY="+authority)
	}
	stdout := &cappedOutput{limit: limit}
	stderr := &cappedOutput{limit: 4096}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if stdout.tooLarge {
		return stdout.buffer.Bytes(), errCommandOutputTooLarge
	}
	return stdout.buffer.Bytes(), nil
}

func validDisplaySize(size displaySize) bool {
	return size.width > 0 && size.height > 0 && size.width <= maxDisplayWidth && size.height <= maxDisplayHeight && int64(size.width)*int64(size.height) <= maxDisplayPixels
}

func truncateUTF8(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
