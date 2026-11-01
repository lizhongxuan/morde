package runtimefiles

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
)

const (
	fileCapabilityPrefix = "file."
	fileReadLimit        = runtimecore.MaxFileReadResultBytes
	fileWriteLimit       = maxWriteBytes
	listPageSize         = 128
)

type actionHandler struct {
	workspace *Workspace
	action    string
}

var _ runtimecore.ActionHandler = (*actionHandler)(nil)

func (h *actionHandler) Capability() string { return fileCapabilityPrefix + h.action }

func (h *actionHandler) Action() string { return h.action }

func (h *actionHandler) Execute(ctx context.Context, request contract.ExecuteRequest) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.Capability != h.Capability() || request.Action != h.action || !validOperationID(request.OperationID) {
		return nil, ErrInvalidRequest
	}
	write := h.action == "write"
	run, ok := h.workspace.registry.begin(request.OperationID, write)
	if !ok {
		return nil, runtimecore.ErrOperationConflict
	}
	committed := false
	defer func() { h.workspace.registry.finish(request.OperationID, run, committed) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case h.workspace.parallel <- struct{}{}:
		defer func() { <-h.workspace.parallel }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var (
		root *os.Root
		err  error
	)
	if request.WorkspaceDirectory == "" {
		// Direct handler tests exercise file semantics without the Runtime API.
		root, err = h.workspace.getRoot()
	} else {
		root, err = h.workspace.getTaskWorkspaceRoot(request.TaskID, request.WorkspaceViewID, request.WorkspaceDirectory)
	}
	if err != nil {
		return nil, err
	}
	if request.WorkspaceDirectory != "" {
		defer root.Close()
	}
	switch h.action {
	case "read":
		return executeRead(ctx, root, request.Arguments)
	case "write":
		result, didCommit, err := executeWrite(ctx, h.workspace, root, request)
		committed = didCommit
		return result, err
	case "list":
		return executeList(ctx, root, request.Arguments)
	case "find":
		return executeFind(ctx, root, request.Arguments)
	case "verify":
		return executeVerify(ctx, root, request.Arguments)
	default:
		return nil, runtimecore.ErrCapabilityUnavailable
	}
}

func (h *actionHandler) Stop(ctx context.Context, operation contract.Operation) (runtimecore.StopResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if operation.Capability != h.Capability() || operation.Action != h.action || !validOperationID(operation.OperationID) {
		return runtimecore.StopResult{}, ErrInvalidRequest
	}
	return h.workspace.registry.stop(ctx, operation.OperationID, h.action == "write")
}

type readArguments struct {
	Path      string `json:"path"`
	StartLine *int   `json:"start_line,omitempty"`
	EndLine   *int   `json:"end_line,omitempty"`
}

func executeRead(ctx context.Context, root *os.Root, body []byte) (json.RawMessage, error) {
	var args readArguments
	if err := decodeArguments(body, &args); err != nil {
		return nil, ErrInvalidRequest
	}
	startLine, endLine := 1, 0
	if args.StartLine != nil {
		if *args.StartLine < 1 {
			return nil, ErrInvalidRequest
		}
		startLine = *args.StartLine
	}
	if args.EndLine != nil {
		if *args.EndLine < 1 {
			return nil, ErrInvalidRequest
		}
		endLine = *args.EndLine
	}
	if endLine > 0 && endLine < startLine {
		return nil, ErrInvalidRequest
	}
	rel, err := cleanWorkspacePath(args.Path, false)
	if err != nil {
		return nil, err
	}
	info, err := checkPath(root, rel, false)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrFileUnavailable
	}
	file, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrFileUnavailable
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		return nil, ErrFileUnavailable
	}
	text, tooLarge, err := readLineRange(ctx, file, startLine, endLine)
	if err != nil {
		return nil, err
	}
	if tooLarge {
		return oversizedReadResult(), nil
	}
	if !utf8.Valid(text) {
		return nil, ErrFileUnavailable
	}
	return marshalResult(struct {
		Path string `json:"path"`
		Text string `json:"text"`
	}{Path: rel, Text: string(text)})
}

// readLineRange preserves line endings and bounds both returned bytes and the
// amount of work needed to seek to a requested line range.
func readLineRange(ctx context.Context, file *os.File, startLine, endLine int) ([]byte, bool, error) {
	reader := bufio.NewReaderSize(file, 32<<10)
	lineNo := 1
	var scanned int64
	output := make([]byte, 0, fileReadLimit)
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		line, lineErr := readBoundedLine(ctx, reader)
		scanned += int64(len(line))
		if scanned > maxReadScanBytes {
			return nil, true, nil
		}
		if lineNo >= startLine && (endLine == 0 || lineNo <= endLine) {
			if len(line) > fileReadLimit-len(output) {
				return nil, true, nil
			}
			output = append(output, line...)
		}
		if errors.Is(lineErr, io.EOF) {
			return output, false, nil
		}
		if lineErr != nil {
			if errors.Is(lineErr, errLineTooLong) {
				return nil, true, nil
			}
			return nil, false, ErrFileUnavailable
		}
		if endLine > 0 && lineNo >= endLine {
			return output, false, nil
		}
		lineNo++
	}
}

var errLineTooLong = errors.New("line exceeds bounded read size")

func readBoundedLine(ctx context.Context, reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 128)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxReadLineBytes {
			return nil, errLineTooLong
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
			return line, io.EOF
		default:
			return nil, err
		}
	}
}

func oversizedReadResult() json.RawMessage {
	// The runtime core recognizes an oversized `text` field and durably stores
	// only its `result_too_large` error result. This sentinel is never exposed.
	return json.RawMessage(`{"text":"` + strings.Repeat("x", fileReadLimit+1) + `"}`)
}

type writeArguments struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func executeWrite(ctx context.Context, workspace *Workspace, root *os.Root, request contract.ExecuteRequest) (json.RawMessage, bool, error) {
	var args writeArguments
	if err := decodeArguments(request.Arguments, &args); err != nil || !utf8.ValidString(args.Content) || len(args.Content) > fileWriteLimit {
		return nil, false, ErrInvalidRequest
	}
	rel, err := cleanWorkspacePath(args.Path, false)
	if err != nil {
		return nil, false, err
	}
	select {
	case workspace.writeGate <- struct{}{}:
		defer func() { <-workspace.writeGate }()
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	parentPath, _ := path.Split(rel)
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "."
	}
	parentInfo, err := checkPath(root, parentPath, true)
	if err != nil || !parentInfo.IsDir() {
		return nil, false, ErrUnsafePath
	}
	if existing, err := root.Lstat(rel); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return nil, false, ErrUnsafePath
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, ErrFileUnavailable
	}

	var tempName string
	var tempFile *os.File
	for attempt := 0; attempt < maxWriteTempRetries; attempt++ {
		var nonce [12]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, false, ErrFileUnavailable
		}
		tempName = tempFilePrefix + request.OperationID + "-" + hex.EncodeToString(nonce[:])
		tempFile, err = root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, false, ErrFileUnavailable
		}
	}
	if tempFile == nil {
		return nil, false, ErrFileUnavailable
	}
	committed := false
	defer func() {
		if !committed {
			_ = root.Remove(tempName)
		}
	}()
	if err := writeAllContext(ctx, tempFile, []byte(args.Content)); err != nil {
		_ = tempFile.Close()
		return nil, false, err
	}
	if err := tempFile.Chmod(0o660); err != nil {
		_ = tempFile.Close()
		return nil, false, ErrFileUnavailable
	}
	if request.WorkspaceDirectory != "" {
		if err := tempFile.Chown(int(workspace.taskViewUID), int(workspace.taskViewGID)); err != nil {
			_ = tempFile.Close()
			return nil, false, ErrFileUnavailable
		}
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return nil, false, ErrFileUnavailable
	}
	if err := tempFile.Close(); err != nil {
		return nil, false, ErrFileUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := root.Rename(tempName, rel); err != nil {
		return nil, false, ErrFileUnavailable
	}
	committed = true
	digest := sha256.Sum256([]byte(args.Content))
	result, err := marshalResult(struct {
		Path         string `json:"path"`
		BytesWritten int    `json:"bytes_written"`
		SHA256       string `json:"sha256"`
	}{Path: rel, BytesWritten: len(args.Content), SHA256: hex.EncodeToString(digest[:])})
	return result, true, err
}

func writeAllContext(ctx context.Context, file *os.File, data []byte) error {
	const chunkSize = 32 << 10
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		written, err := file.Write(data[offset:end])
		if err != nil || written != end-offset {
			return ErrFileUnavailable
		}
		offset = end
	}
	return nil
}

type listArguments struct {
	Path string `json:"path,omitempty"`
}

type entryMetadata struct {
	Name    string    `json:"name"`
	Kind    string    `json:"kind"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modified_at"`
}

func executeList(ctx context.Context, root *os.Root, body []byte) (json.RawMessage, error) {
	var args listArguments
	if err := decodeArguments(body, &args); err != nil {
		return nil, ErrInvalidRequest
	}
	rel, err := cleanWorkspacePath(args.Path, true)
	if err != nil {
		return nil, err
	}
	info, err := checkPath(root, rel, true)
	if err != nil || !info.IsDir() {
		return nil, ErrFileUnavailable
	}
	dirRoot, err := root.OpenRoot(rel)
	if err != nil {
		return nil, ErrFileUnavailable
	}
	defer dirRoot.Close()
	dir, err := dirRoot.Open(".")
	if err != nil {
		return nil, ErrFileUnavailable
	}
	defer dir.Close()
	entries := make([]entryMetadata, 0, 64)
	truncated := false
	scanned := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, readErr := dir.ReadDir(listPageSize)
		for _, entry := range batch {
			scanned++
			if scanned > maxListScanned {
				truncated = true
				break
			}
			if strings.HasPrefix(entry.Name(), tempFilePrefix) {
				continue
			}
			if len(entries) == maxListEntries {
				truncated = true
				break
			}
			child := joinWorkspacePath(rel, entry.Name())
			meta, err := entryInfo(root, child, entry.Name())
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, ErrFileUnavailable
			}
			entries = append(entries, meta)
		}
		if truncated || errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, ErrFileUnavailable
		}
	}
	return marshalResult(struct {
		Path      string          `json:"path"`
		Entries   []entryMetadata `json:"entries"`
		Truncated bool            `json:"truncated"`
	}{Path: rel, Entries: entries, Truncated: truncated})
}

type findArguments struct {
	Path  string `json:"path,omitempty"`
	Query string `json:"query"`
}

type verifyArguments struct {
	Files []verifyFileArgument `json:"files"`
}

type verifyFileArgument struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type verifiedFile struct {
	path    string
	args    verifyFileArgument
	info    os.FileInfo
	missing bool
}

type verifyFileResult struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Matches bool   `json:"matches"`
}

func executeVerify(ctx context.Context, root *os.Root, body []byte) (json.RawMessage, error) {
	var args verifyArguments
	if err := decodeUniqueArguments(body, &args); err != nil || len(args.Files) == 0 || len(args.Files) > maxVerifyFiles {
		return nil, ErrInvalidRequest
	}

	files := make([]verifiedFile, 0, len(args.Files))
	var expectedTotal int64
	for _, file := range args.Files {
		if file.Size < 0 || file.Size > maxVerifyFileBytes || !isLowerSHA256(file.SHA256) {
			return nil, ErrInvalidRequest
		}
		if expectedTotal > maxVerifyTotalBytes-file.Size {
			return nil, ErrInvalidRequest
		}
		expectedTotal += file.Size
		rel, err := cleanWorkspacePath(file.Path, false)
		if err != nil {
			return nil, err
		}
		file.Path = rel
		info, err := checkPath(root, rel, false)
		if errors.Is(err, fs.ErrNotExist) {
			files = append(files, verifiedFile{path: rel, args: file, missing: true})
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxVerifyFileBytes {
			return nil, ErrFileUnavailable
		}
		files = append(files, verifiedFile{path: rel, args: file, info: info})
	}

	results := make([]verifyFileResult, 0, len(files))
	var workTotal int64
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file.missing {
			results = append(results, verifyFileResult{Path: file.path})
			continue
		}
		if workTotal > maxVerifyTotalBytes-file.info.Size() {
			return nil, ErrFileUnavailable
		}
		opened, err := openVerifiedFile(root, file.path)
		if errors.Is(err, fs.ErrNotExist) {
			results = append(results, verifyFileResult{Path: file.path})
			continue
		}
		if err != nil {
			return nil, err
		}
		initial, err := opened.Stat()
		if err != nil || !initial.Mode().IsRegular() || initial.Size() < 0 || initial.Size() > maxVerifyFileBytes {
			_ = opened.Close()
			return nil, ErrFileUnavailable
		}
		if workTotal > maxVerifyTotalBytes-initial.Size() {
			_ = opened.Close()
			return nil, ErrFileUnavailable
		}
		digest, size, err := hashFileContext(ctx, opened, maxVerifyFileBytes, maxVerifyTotalBytes-workTotal)
		final, statErr := opened.Stat()
		closeErr := opened.Close()
		if err != nil || statErr != nil || closeErr != nil || !final.Mode().IsRegular() || final.Size() != size || size != initial.Size() || !final.ModTime().Equal(initial.ModTime()) {
			if err != nil {
				return nil, err
			}
			return nil, ErrFileUnavailable
		}
		workTotal += size
		results = append(results, verifyFileResult{
			Path: file.path, Size: size, SHA256: digest,
			Matches: size == file.args.Size && digest == file.args.SHA256,
		})
	}
	return marshalResult(struct {
		Files []verifyFileResult `json:"files"`
	}{Files: results})
}

func openVerifiedFile(root *os.Root, rel string) (*os.File, error) {
	if _, err := checkPath(root, rel, false); err != nil {
		return nil, err
	}
	file, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrFileUnavailable
	}
	return file, nil
}

func hashFileContext(ctx context.Context, file *os.File, fileLimit, remainingTotal int64) (string, int64, error) {
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		read, err := file.Read(buffer)
		if read > 0 {
			total += int64(read)
			if total > fileLimit || total > remainingTotal {
				return "", total, ErrFileUnavailable
			}
			if _, writeErr := hash.Write(buffer[:read]); writeErr != nil {
				return "", total, ErrFileUnavailable
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if read == 0 && err == nil {
			return "", total, ErrFileUnavailable
		}
		if err != nil {
			return "", total, ErrFileUnavailable
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), total, nil
}

func isLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

// decodeUniqueArguments rejects duplicate object keys before applying the
// normal strict decoder, so callers cannot supply ambiguous verification data.
func decodeUniqueArguments(body []byte, target any) error {
	if len(body) == 0 || len(body) > runtimecore.MaxRequestBytes {
		return ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := consumeUniqueJSONValue(decoder, 0); err != nil {
		return ErrInvalidRequest
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrInvalidRequest
	}
	return decodeArguments(body, target)
}

func consumeUniqueJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return ErrInvalidRequest
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrInvalidRequest
			}
			if _, exists := seen[key]; exists {
				return ErrInvalidRequest
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalidRequest
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalidRequest
		}
	}
	return nil
}

func executeFind(ctx context.Context, root *os.Root, body []byte) (json.RawMessage, error) {
	var args findArguments
	if err := decodeArguments(body, &args); err != nil || !utf8.ValidString(args.Query) || strings.TrimSpace(args.Query) == "" || len(args.Query) > 256 {
		return nil, ErrInvalidRequest
	}
	rel, err := cleanWorkspacePath(args.Path, true)
	if err != nil {
		return nil, err
	}
	info, err := checkPath(root, rel, true)
	if err != nil || !info.IsDir() {
		return nil, ErrFileUnavailable
	}
	query := strings.ToLower(args.Query)
	type directory struct {
		path  string
		depth int
	}
	stack := []directory{{path: rel}}
	matches := make([]entryMetadata, 0, 32)
	scanned := 0
	truncated := false
	for len(stack) > 0 && !truncated {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		dirRoot, err := root.OpenRoot(current.path)
		if err != nil {
			return nil, ErrFileUnavailable
		}
		dir, err := dirRoot.Open(".")
		if err != nil {
			_ = dirRoot.Close()
			return nil, ErrFileUnavailable
		}
		for {
			if err := ctx.Err(); err != nil {
				_ = dir.Close()
				_ = dirRoot.Close()
				return nil, err
			}
			batch, readErr := dir.ReadDir(listPageSize)
			for _, entry := range batch {
				if scanned >= maxFindEntries {
					truncated = true
					break
				}
				scanned++
				if strings.HasPrefix(entry.Name(), tempFilePrefix) {
					continue
				}
				child := joinWorkspacePath(current.path, entry.Name())
				meta, err := entryInfo(root, child, entry.Name())
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					_ = dir.Close()
					_ = dirRoot.Close()
					return nil, ErrFileUnavailable
				}
				if strings.Contains(strings.ToLower(child), query) {
					if len(matches) == maxFindMatches {
						truncated = true
						break
					}
					matches = append(matches, meta)
				}
				if meta.Kind == "directory" {
					if current.depth >= maxFindDepth || len(stack) >= maxFindDirectories {
						truncated = true
						break
					}
					stack = append(stack, directory{path: child, depth: current.depth + 1})
				}
			}
			if truncated || errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				_ = dir.Close()
				_ = dirRoot.Close()
				return nil, ErrFileUnavailable
			}
		}
		_ = dir.Close()
		_ = dirRoot.Close()
	}
	return marshalResult(struct {
		Path      string          `json:"path"`
		Query     string          `json:"query"`
		Matches   []entryMetadata `json:"matches"`
		Truncated bool            `json:"truncated"`
	}{Path: rel, Query: args.Query, Matches: matches, Truncated: truncated})
}

func entryInfo(root *os.Root, rel, name string) (entryMetadata, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return entryMetadata{}, err
	}
	kind := "other"
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		kind = "symlink"
	case info.IsDir():
		kind = "directory"
	case info.Mode().IsRegular():
		kind = "file"
	}
	return entryMetadata{Name: name, Kind: kind, Size: info.Size(), ModTime: info.ModTime().UTC()}, nil
}

func cleanWorkspacePath(raw string, allowRoot bool) (string, error) {
	if !utf8.ValidString(raw) || strings.IndexByte(raw, 0) >= 0 || strings.Contains(raw, `\`) || strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) || len(raw) >= 2 && isASCIILetter(raw[0]) && raw[1] == ':' {
		return "", ErrUnsafePath
	}
	if raw == "" {
		if allowRoot {
			return ".", nil
		}
		return "", ErrUnsafePath
	}
	for _, component := range strings.Split(raw, "/") {
		if component == ".." || component == "" || strings.HasPrefix(component, tempFilePrefix) {
			return "", ErrUnsafePath
		}
	}
	clean := path.Clean(raw)
	if clean == "." {
		if allowRoot {
			return clean, nil
		}
		return "", ErrUnsafePath
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrUnsafePath
	}
	return clean, nil
}

func checkPath(root *os.Root, rel string, allowRoot bool) (os.FileInfo, error) {
	clean, err := cleanWorkspacePath(rel, allowRoot)
	if err != nil {
		return nil, err
	}
	if clean == "." {
		info, err := root.Stat(".")
		if err != nil {
			return nil, ErrFileUnavailable
		}
		return info, nil
	}
	parts := strings.Split(clean, "/")
	for index := range parts {
		current := strings.Join(parts[:index+1], "/")
		info, err := root.Lstat(current)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fs.ErrNotExist
			}
			return nil, ErrFileUnavailable
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrUnsafePath
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, ErrUnsafePath
		}
		if index == len(parts)-1 {
			return info, nil
		}
	}
	return nil, ErrUnsafePath
}

func joinWorkspacePath(parent, name string) string {
	if parent == "." {
		return name
	}
	return path.Join(parent, name)
}

func decodeArguments(body []byte, target any) error {
	if len(body) == 0 || len(body) > runtimecore.MaxRequestBytes {
		return ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrInvalidRequest
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrInvalidRequest
	}
	return nil
}

func marshalResult(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > runtimecore.MaxOperationResultBytes {
		return nil, ErrFileUnavailable
	}
	return json.RawMessage(encoded), nil
}

func validOperationID(value string) bool {
	if len(value) != 8 || value[0] != 'o' {
		return false
	}
	for _, char := range value[1:] {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func isASCIILetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
