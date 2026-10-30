package workspace

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	mordesync "morde-sync"
	"unicode/utf8"
)

const (
	MaxSyncBlockBytes    int64 = 4 << 20
	MaxSyncFileBytes     int64 = 300 << 20
	MaxSyncFiles               = 100_000
	MaxSyncManifestBytes       = 16 << 20
	MaxSyncScopeItems          = 10_000
	MaxSyncPathBytes           = 4096
)

const syncIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func NewSyncSessionID() (string, error) {
	var id [8]byte
	id[0] = 'p'
	max := big.NewInt(int64(len(syncIDAlphabet)))
	for i := 1; i < len(id); i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate workspace sync session id: %w", err)
		}
		id[i] = syncIDAlphabet[n.Int64()]
	}
	return string(id[:]), nil
}

type SyncDirection string

const (
	SyncUpload        SyncDirection = "upload"
	SyncDownload      SyncDirection = "download"
	SyncBidirectional SyncDirection = "bidirectional"
)

func (d SyncDirection) Valid() bool {
	return d == SyncUpload || d == SyncDownload || d == SyncBidirectional
}

type SyncPolicy string

const (
	SyncFillMissing  SyncPolicy = "fill_missing"
	SyncCompareMerge SyncPolicy = "compare_merge"
	SyncLive         SyncPolicy = "live"
)

func (p SyncPolicy) Valid() bool {
	return p == SyncFillMissing || p == SyncCompareMerge || p == SyncLive
}

type SyncState string

const (
	SyncPlanned      SyncState = "planned"
	SyncTransferring SyncState = "transferring"
	SyncVerifying    SyncState = "verifying"
	SyncApplying     SyncState = "applying"
	SyncCompleted    SyncState = "completed"
	SyncConflict     SyncState = "conflict"
	SyncCancelled    SyncState = "cancelled"
	SyncFailed       SyncState = "failed"
	SyncExpired      SyncState = "expired"
)

type SyncStage string

const (
	SyncStageScan     SyncStage = "scan"
	SyncStageCompare  SyncStage = "compare"
	SyncStageTransfer SyncStage = "transfer"
	SyncStageVerify   SyncStage = "verify"
	SyncStageApply    SyncStage = "apply"
	SyncStageComplete SyncStage = "complete"
)

type BlockRole string

const (
	BlockUpload   BlockRole = "upload"
	BlockDownload BlockRole = "download"
)

type Chunk struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// BlockReference identifies one immutable content block referenced by any
// persisted workspace revision.
type BlockReference struct {
	WorkspaceID string
	SHA256      string
	SizeBytes   int64
}

type File struct {
	Path      string  `json:"path"`
	SizeBytes int64   `json:"size_bytes"`
	SHA256    string  `json:"sha256"`
	Mode      uint32  `json:"mode"`
	Chunks    []Chunk `json:"chunks"`
}

type ManifestPage struct {
	WorkspaceID string  `json:"workspace_id"`
	Revision    int64   `json:"revision"`
	Files       []File  `json:"files"`
	NextCursor  *string `json:"next_cursor"`
}

type ChangeAction string

const (
	ChangeUpsert ChangeAction = "upsert"
	ChangeDelete ChangeAction = "delete"
)

type Change struct {
	Revision int64        `json:"revision"`
	Path     string       `json:"path"`
	Action   ChangeAction `json:"action"`
	File     *File        `json:"file"`
}

type ChangePage struct {
	Items      []Change `json:"items"`
	NextCursor string   `json:"next_cursor"`
}

type ChangeCursor struct {
	Revision int64  `json:"revision"`
	Path     string `json:"path"`
}

func EncodeChangeCursor(cursor ChangeCursor) string {
	value, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(value)
}

func DecodeChangeCursor(value string) (ChangeCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return ChangeCursor{}, errors.New("invalid workspace changes cursor")
	}
	var cursor ChangeCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.Revision < 0 || (cursor.Path != "" && ValidateRelativePath(cursor.Path) != nil) {
		return ChangeCursor{}, errors.New("invalid workspace changes cursor")
	}
	return cursor, nil
}

type ManifestCursor struct {
	Revision int64  `json:"revision"`
	Prefix   string `json:"prefix"`
	After    string `json:"after"`
}

func EncodeManifestCursor(cursor ManifestCursor) string {
	value, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(value)
}

func DecodeManifestCursor(value string) (ManifestCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return ManifestCursor{}, errors.New("invalid workspace manifest cursor")
	}
	var cursor ManifestCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.Revision < 0 || (cursor.After != "" && ValidateRelativePath(cursor.After) != nil) {
		return ManifestCursor{}, errors.New("invalid workspace manifest cursor")
	}
	return cursor, nil
}

type Conflict struct {
	Path         string  `json:"path"`
	Reason       string  `json:"reason"`
	LocalSHA256  *string `json:"local_sha256"`
	RemoteSHA256 *string `json:"remote_sha256"`
}

type CreateSyncSession struct {
	Direction    SyncDirection `json:"direction"`
	Policy       SyncPolicy    `json:"policy"`
	BaseRevision int64         `json:"base_revision"`
	Scope        []string      `json:"scope,omitempty"`
	Files        []File        `json:"files"`
	DeletedPaths []string      `json:"deleted_paths,omitempty"`
	AckCursor    *string       `json:"ack_cursor,omitempty"`
}

type TransferBlock struct {
	Chunk    `json:"chunk"`
	Role     BlockRole `json:"role"`
	Received bool      `json:"received"`
}

type DownloadAction struct {
	Path      string       `json:"path"`
	Action    ChangeAction `json:"action"`
	SHA256    *string      `json:"sha256"`
	SizeBytes int64        `json:"size_bytes"`
	Applied   bool         `json:"applied"`
}

func DownloadActionsForPlan(files []File, deletedPaths []string) []DownloadAction {
	actions := make([]DownloadAction, 0, len(files)+len(deletedPaths))
	for _, file := range files {
		digest := file.SHA256
		actions = append(actions, DownloadAction{
			Path: file.Path, Action: ChangeUpsert, SHA256: &digest, SizeBytes: file.SizeBytes,
		})
	}
	for _, path := range deletedPaths {
		actions = append(actions, DownloadAction{Path: path, Action: ChangeDelete})
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].Path < actions[j].Path })
	return actions
}

func ValidateDownloadAction(value DownloadAction) error {
	if err := ValidateRelativePath(value.Path); err != nil {
		return err
	}
	switch value.Action {
	case ChangeUpsert:
		if value.SHA256 == nil || !sha256Pattern.MatchString(*value.SHA256) || value.SizeBytes < 0 {
			return errors.New("download upsert action metadata is invalid")
		}
	case ChangeDelete:
		if value.SHA256 != nil || value.SizeBytes != 0 {
			return errors.New("download delete action metadata is invalid")
		}
	default:
		return errors.New("download action kind is invalid")
	}
	return nil
}

type SyncSession struct {
	ID                   string           `json:"id"`
	WorkspaceID          string           `json:"workspace_id"`
	OwnerID              string           `json:"-"`
	ReplicaID            string           `json:"-"`
	Direction            SyncDirection    `json:"direction"`
	Policy               SyncPolicy       `json:"policy"`
	State                SyncState        `json:"state"`
	BaseRevision         int64            `json:"base_revision"`
	TargetRevision       int64            `json:"target_revision"`
	PinnedRevision       int64            `json:"-"`
	Stage                SyncStage        `json:"stage"`
	PlannedBytes         int64            `json:"planned_bytes"`
	ConfirmedBytes       int64            `json:"confirmed_bytes"`
	TransferBlocks       []TransferBlock  `json:"transfer_blocks"`
	DownloadActions      []DownloadAction `json:"download_actions"`
	Conflicts            []Conflict       `json:"conflicts"`
	Files                []File           `json:"-"`
	UploadFiles          []File           `json:"-"`
	DownloadFiles        []File           `json:"-"`
	Scope                []string         `json:"-"`
	DeletedPaths         []string         `json:"-"`
	UploadDeletedPaths   []string         `json:"-"`
	DownloadDeletedPaths []string         `json:"-"`
	AckCursor            *string          `json:"-"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
}

type DeviceSyncProgress struct {
	WorkspaceID          string        `json:"workspace_id"`
	DeviceID             string        `json:"device_id"`
	SessionID            string        `json:"session_id"`
	Direction            SyncDirection `json:"direction"`
	State                SyncState     `json:"state"`
	Stage                SyncStage     `json:"stage"`
	PlannedBytes         int64         `json:"planned_bytes"`
	ConfirmedBytes       int64         `json:"confirmed_bytes"`
	DownloadActionsTotal int64         `json:"download_actions_total"`
	DownloadActionsDone  int64         `json:"download_actions_done"`
	UpdatedAt            time.Time     `json:"updated_at"`
}

type SyncPlan struct {
	UploadFiles          []File
	DownloadFiles        []File
	UploadDeletedPaths   []string
	DownloadDeletedPaths []string
	Conflicts            []Conflict
	UploadBlocks         []Chunk
	DownloadBlocks       []Chunk
	RequiredBlocks       []Chunk
	PlannedBytes         int64
}

// PlanSync compares explicit client state with an immutable base and the
// current server snapshot. Missing paths are never interpreted as deletions;
// only DeletedPaths can request a local deletion.
func PlanSync(baseFiles, currentFiles []File, request CreateSyncSession) SyncPlan {
	base := fileMap(baseFiles)
	current := fileMap(currentFiles)
	client := fileMap(request.Files)
	localDeleted := stringSet(request.DeletedPaths)
	plan := SyncPlan{
		UploadFiles: []File{}, DownloadFiles: []File{},
		UploadDeletedPaths: []string{}, DownloadDeletedPaths: []string{},
		Conflicts: []Conflict{}, UploadBlocks: []Chunk{}, DownloadBlocks: []Chunk{}, RequiredBlocks: []Chunk{},
	}
	paths := make(map[string]struct{}, len(base)+len(current)+len(client)+len(localDeleted))
	for key := range base {
		paths[key] = struct{}{}
	}
	for key := range current {
		paths[key] = struct{}{}
	}
	for key := range client {
		paths[key] = struct{}{}
	}
	for key := range localDeleted {
		paths[key] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for key := range paths {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)

	uploadByDigest := make(map[string]Chunk)
	downloadByDigest := make(map[string]Chunk)
	for _, key := range ordered {
		baseFile, baseOK := base[key]
		remoteFile, remoteOK := current[key]
		localFile, localOK := client[key]
		if !inScope(key, request.Scope) {
			continue
		}
		localDelete := localDeleted[key]
		localChanged := localDelete && baseOK || localOK && (!baseOK || !sameFile(localFile, baseFile))
		remoteChanged := baseOK != remoteOK || (baseOK && remoteOK && !sameFile(baseFile, remoteFile))
		if request.Policy != SyncFillMissing && (request.Direction == SyncDownload || request.Direction == SyncBidirectional) &&
			remoteChanged && localOK && remoteOK && localFile.Path != remoteFile.Path && strings.EqualFold(localFile.Path, remoteFile.Path) {
			// shortcut: remote case-only renames require a manual local rename to avoid cross-platform overwrite, revisit with a no-clobber rename transaction.
			plan.Conflicts = append(plan.Conflicts, makeConflict(remoteFile.Path, false, &localFile, true, &remoteFile, true))
			continue
		}

		if request.Direction == SyncUpload || request.Direction == SyncBidirectional {
			if localChanged {
				if request.Policy == SyncFillMissing {
					if _, exists := current[key]; !exists && localOK {
						plan.UploadFiles = append(plan.UploadFiles, localFile)
					}
				} else if remoteChanged && !sameOptional(localFile, localOK, remoteFile, remoteOK) {
					plan.Conflicts = append(plan.Conflicts, makeConflict(canonicalPath(key, baseFile, baseOK, remoteFile, remoteOK, localFile, localOK), localDelete || baseOK && !remoteOK, &localFile, localOK, &remoteFile, remoteOK))
				} else if localDelete {
					if remoteOK {
						plan.UploadDeletedPaths = append(plan.UploadDeletedPaths, canonicalPath(key, baseFile, baseOK, remoteFile, remoteOK, localFile, localOK))
					}
				} else if localOK && !sameOptional(localFile, true, remoteFile, remoteOK) {
					plan.UploadFiles = append(plan.UploadFiles, localFile)
				}
			}
		}

		if request.Direction == SyncDownload || request.Direction == SyncBidirectional {
			if request.Policy == SyncFillMissing {
				if !localOK && remoteOK {
					plan.DownloadFiles = append(plan.DownloadFiles, remoteFile)
				}
			} else if remoteChanged {
				if localChanged && !sameOptional(localFile, localOK, remoteFile, remoteOK) {
					if !containsConflict(plan.Conflicts, key) {
						plan.Conflicts = append(plan.Conflicts, makeConflict(canonicalPath(key, baseFile, baseOK, remoteFile, remoteOK, localFile, localOK), localDelete || !localOK, &localFile, localOK, &remoteFile, remoteOK))
					}
				} else if remoteOK && (!localOK || !sameFile(localFile, remoteFile)) {
					plan.DownloadFiles = append(plan.DownloadFiles, remoteFile)
				} else if !remoteOK && localOK {
					// A deletion is surfaced through the durable changes feed. A sync
					// plan does not infer a destination delete from an absent file list.
					plan.DownloadDeletedPaths = append(plan.DownloadDeletedPaths, canonicalPath(key, baseFile, baseOK, remoteFile, remoteOK, localFile, localOK))
				}
			} else if request.Policy == SyncLive && !localChanged && remoteOK && (!localOK || !sameFile(localFile, remoteFile)) {
				plan.DownloadFiles = append(plan.DownloadFiles, remoteFile)
			}
		}
	}

	for _, file := range plan.UploadFiles {
		for _, chunk := range file.Chunks {
			uploadByDigest[chunk.SHA256] = chunk
		}
	}
	for _, file := range plan.DownloadFiles {
		for _, chunk := range file.Chunks {
			downloadByDigest[chunk.SHA256] = chunk
		}
	}
	for digest, chunk := range uploadByDigest {
		plan.UploadBlocks = append(plan.UploadBlocks, chunk)
		_ = digest
	}
	for digest, chunk := range downloadByDigest {
		plan.DownloadBlocks = append(plan.DownloadBlocks, chunk)
		_ = digest
	}
	sort.Slice(plan.UploadBlocks, func(i, j int) bool { return plan.UploadBlocks[i].SHA256 < plan.UploadBlocks[j].SHA256 })
	sort.Slice(plan.DownloadBlocks, func(i, j int) bool { return plan.DownloadBlocks[i].SHA256 < plan.DownloadBlocks[j].SHA256 })
	all := make(map[string]Chunk, len(uploadByDigest)+len(downloadByDigest))
	for digest, chunk := range uploadByDigest {
		all[digest] = chunk
	}
	for digest, chunk := range downloadByDigest {
		all[digest] = chunk
	}
	for _, chunk := range all {
		plan.RequiredBlocks = append(plan.RequiredBlocks, chunk)
	}
	sort.Slice(plan.RequiredBlocks, func(i, j int) bool { return plan.RequiredBlocks[i].SHA256 < plan.RequiredBlocks[j].SHA256 })
	for _, chunk := range plan.RequiredBlocks {
		plan.PlannedBytes += chunk.SizeBytes
	}
	sort.Strings(plan.UploadDeletedPaths)
	sort.Strings(plan.DownloadDeletedPaths)
	return plan
}

func fileMap(files []File) map[string]File {
	result := make(map[string]File, len(files))
	for _, file := range files {
		result[strings.ToLower(file.Path)] = file
	}
	return result
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[strings.ToLower(value)] = true
	}
	return result
}

func inScope(filePath string, scope []string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, candidate := range scope {
		candidate = strings.TrimSuffix(candidate, "/")
		if strings.EqualFold(filePath, candidate) || strings.HasPrefix(strings.ToLower(filePath), strings.ToLower(candidate)+"/") {
			return true
		}
	}
	return false
}

func sameFile(a, b File) bool {
	return a.Path == b.Path && a.SizeBytes == b.SizeBytes && a.SHA256 == b.SHA256 && a.Mode == b.Mode
}

func sameOptional(a File, aOK bool, b File, bOK bool) bool {
	return aOK == bOK && (!aOK || sameFile(a, b))
}

func makeConflict(filePath string, deletion bool, local *File, localOK bool, remote *File, remoteOK bool) Conflict {
	reason := "content_changed"
	if deletion || !localOK || !remoteOK {
		reason = "delete_changed"
	} else if local.Path != remote.Path && strings.EqualFold(local.Path, remote.Path) {
		reason = "case_collision"
	}
	var localHash, remoteHash *string
	if localOK {
		localHash = &local.SHA256
	}
	if remoteOK {
		remoteHash = &remote.SHA256
	}
	return Conflict{Path: filePath, Reason: reason, LocalSHA256: localHash, RemoteSHA256: remoteHash}
}

func containsConflict(conflicts []Conflict, filePath string) bool {
	for _, conflict := range conflicts {
		if strings.EqualFold(conflict.Path, filePath) {
			return true
		}
	}
	return false
}

func canonicalPath(key string, base File, baseOK bool, remote File, remoteOK bool, local File, localOK bool) string {
	if baseOK {
		return base.Path
	}
	if remoteOK {
		return remote.Path
	}
	if localOK {
		return local.Path
	}
	return key
}

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidateRelativePath(value string) error {
	if value == "" || len(value) > MaxSyncPathBytes || !utf8.ValidString(value) || strings.ContainsRune(value, '\\') || strings.ContainsRune(value, '\x00') {
		return errors.New("path must be a valid relative POSIX path")
	}
	if path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return errors.New("path must be normalized and relative")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("path must not contain empty or traversal components")
		}
	}
	return nil
}

func ValidateCreateSyncSession(value CreateSyncSession, maxBytes int64) error {
	if !value.Direction.Valid() || !value.Policy.Valid() || value.BaseRevision < 0 {
		return errors.New("sync direction, policy, or base revision is invalid")
	}
	if len(value.Files) > MaxSyncFiles || len(value.Scope) > MaxSyncScopeItems || len(value.DeletedPaths) > MaxSyncFiles {
		return errors.New("sync manifest exceeds item limits")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode sync manifest: %w", err)
	}
	if int64(len(encoded)) > MaxSyncManifestBytes {
		return errors.New("sync manifest exceeds 16 MiB")
	}
	if maxBytes > 0 {
		var total int64
		for _, file := range value.Files {
			if file.SizeBytes < 0 || total > maxBytes-file.SizeBytes {
				return errors.New("sync manifest exceeds workspace quota")
			}
			total += file.SizeBytes
		}
	}
	seen := make(map[string]string, len(value.Files))
	var paths mordesync.ManifestPathValidator
	var total int64
	for _, file := range value.Files {
		if err := ValidateFile(file); err != nil {
			return fmt.Errorf("file %q: %w", file.Path, err)
		}
		folded := strings.ToLower(file.Path)
		if err := paths.Add(file.Path); err != nil {
			return err
		}
		seen[folded] = file.Path
		if len(value.Scope) > 0 && !inScope(strings.ToLower(file.Path), value.Scope) {
			return fmt.Errorf("file path %q is outside the requested sync scope", file.Path)
		}
		if total > int64(^uint64(0)>>1)-file.SizeBytes {
			return errors.New("workspace file size overflows int64")
		}
		total += file.SizeBytes
	}
	seenDeleted := make(map[string]struct{}, len(value.DeletedPaths))
	for _, filePath := range value.DeletedPaths {
		if err := ValidateRelativePath(filePath); err != nil {
			return fmt.Errorf("deleted path %q: %w", filePath, err)
		}
		folded := strings.ToLower(filePath)
		if _, exists := seen[folded]; exists {
			return fmt.Errorf("path %q is both present and deleted", filePath)
		}
		if _, exists := seenDeleted[folded]; exists {
			return fmt.Errorf("duplicate deleted path %q", filePath)
		}
		if len(value.Scope) > 0 && !inScope(strings.ToLower(filePath), value.Scope) {
			return fmt.Errorf("deleted path %q is outside the requested sync scope", filePath)
		}
		seenDeleted[folded] = struct{}{}
	}
	for _, scopePath := range value.Scope {
		if err := ValidateRelativePath(scopePath); err != nil {
			return fmt.Errorf("scope path %q: %w", scopePath, err)
		}
	}
	if len(value.Scope) > 0 {
		scope := make([]string, len(value.Scope))
		for i, value := range value.Scope {
			scope[i] = strings.ToLower(value)
		}
		sort.Strings(scope)
		for i := 1; i < len(scope); i++ {
			if scope[i] == scope[i-1] || strings.HasPrefix(scope[i], strings.TrimSuffix(scope[i-1], "/")+"/") {
				return errors.New("sync scope paths must be unique and non-overlapping")
			}
		}
	}
	if value.AckCursor != nil && len(*value.AckCursor) > 512 {
		return errors.New("ack cursor exceeds 512 bytes")
	}
	return nil
}

func ValidateFile(file File) error {
	if err := ValidateRelativePath(file.Path); err != nil {
		return err
	}
	if file.SizeBytes < 0 || file.Mode > 0777 || !sha256Pattern.MatchString(file.SHA256) {
		return errors.New("file size, mode, or SHA-256 is invalid")
	}
	var total int64
	for _, chunk := range file.Chunks {
		if !sha256Pattern.MatchString(chunk.SHA256) || chunk.SizeBytes < 1 || chunk.SizeBytes > MaxSyncBlockBytes || total > file.SizeBytes-chunk.SizeBytes {
			return errors.New("file chunk digest or size is invalid")
		}
		total += chunk.SizeBytes
	}
	if total != file.SizeBytes || (file.SizeBytes == 0) != (len(file.Chunks) == 0) {
		return errors.New("file chunks do not match file size")
	}
	return nil
}

func ValidSessionID(id string) bool {
	if len(id) != 8 || id[0] != 'p' {
		return false
	}
	for _, r := range id[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
