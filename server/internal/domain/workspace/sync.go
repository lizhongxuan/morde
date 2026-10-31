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
	WorkspaceID      string   `json:"workspace_id"`
	Revision         int64    `json:"revision"`
	Files            []File   `json:"files"`
	EmptyDirectories []string `json:"empty_directories"`
	NextCursor       *string  `json:"next_cursor"`
}

type ChangeAction string

const (
	ChangeUpsert          ChangeAction = "upsert"
	ChangeDelete          ChangeAction = "delete"
	ChangeDirectoryUpsert ChangeAction = "directory_upsert"
	ChangeDirectoryDelete ChangeAction = "directory_delete"
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
	Direction          SyncDirection `json:"direction"`
	Policy             SyncPolicy    `json:"policy"`
	BaseRevision       int64         `json:"base_revision"`
	Scope              []string      `json:"scope,omitempty"`
	Files              []File        `json:"files"`
	EmptyDirectories   []string      `json:"empty_directories,omitempty"`
	DeletedPaths       []string      `json:"deleted_paths,omitempty"`
	DeletedDirectories []string      `json:"deleted_directories,omitempty"`
	AckCursor          *string       `json:"ack_cursor,omitempty"`
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
	return DownloadActionsForTreePlan(files, deletedPaths, nil, nil)
}

func DownloadActionsForTreePlan(files []File, deletedPaths, emptyDirectories, deletedDirectories []string) []DownloadAction {
	actions := make([]DownloadAction, 0, len(files)+len(deletedPaths)+len(emptyDirectories)+len(deletedDirectories))
	for _, file := range files {
		digest := file.SHA256
		actions = append(actions, DownloadAction{
			Path: file.Path, Action: ChangeUpsert, SHA256: &digest, SizeBytes: file.SizeBytes,
		})
	}
	for _, path := range deletedPaths {
		actions = append(actions, DownloadAction{Path: path, Action: ChangeDelete})
	}
	for _, directory := range emptyDirectories {
		actions = append(actions, DownloadAction{Path: directory, Action: ChangeDirectoryUpsert})
	}
	for _, directory := range deletedDirectories {
		actions = append(actions, DownloadAction{Path: directory, Action: ChangeDirectoryDelete})
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
	case ChangeDirectoryUpsert, ChangeDirectoryDelete:
		if value.SHA256 != nil || value.SizeBytes != 0 {
			return errors.New("download directory action metadata is invalid")
		}
	default:
		return errors.New("download action kind is invalid")
	}
	return nil
}

type SyncSession struct {
	ID                         string           `json:"id"`
	WorkspaceID                string           `json:"workspace_id"`
	OwnerID                    string           `json:"-"`
	ReplicaID                  string           `json:"-"`
	Direction                  SyncDirection    `json:"direction"`
	Policy                     SyncPolicy       `json:"policy"`
	State                      SyncState        `json:"state"`
	BaseRevision               int64            `json:"base_revision"`
	TargetRevision             int64            `json:"target_revision"`
	PinnedRevision             int64            `json:"-"`
	Stage                      SyncStage        `json:"stage"`
	PlannedBytes               int64            `json:"planned_bytes"`
	ConfirmedBytes             int64            `json:"confirmed_bytes"`
	TransferBlocks             []TransferBlock  `json:"transfer_blocks"`
	DownloadActions            []DownloadAction `json:"download_actions"`
	Conflicts                  []Conflict       `json:"conflicts"`
	Files                      []File           `json:"-"`
	UploadFiles                []File           `json:"-"`
	DownloadFiles              []File           `json:"-"`
	Scope                      []string         `json:"-"`
	DeletedPaths               []string         `json:"-"`
	EmptyDirectories           []string         `json:"-"`
	UploadEmptyDirectories     []string         `json:"-"`
	DownloadEmptyDirectories   []string         `json:"-"`
	DeletedDirectories         []string         `json:"-"`
	UploadDeletedDirectories   []string         `json:"-"`
	DownloadDeletedDirectories []string         `json:"-"`
	UploadDeletedPaths         []string         `json:"-"`
	DownloadDeletedPaths       []string         `json:"-"`
	AckCursor                  *string          `json:"-"`
	CreatedAt                  time.Time        `json:"created_at"`
	UpdatedAt                  time.Time        `json:"updated_at"`
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

type DeviceSyncProgressSummary struct {
	Revision       int64                `json:"revision"`
	UsedBytes      int64                `json:"used_bytes"`
	QuotaBytes     int64                `json:"quota_bytes"`
	RemainingBytes int64                `json:"remaining_bytes"`
	QuotaStatus    SyncQuotaStatus      `json:"quota_status"`
	Items          []DeviceSyncProgress `json:"items"`
}

type SyncQuotaStatus string

const (
	SyncQuotaAvailable SyncQuotaStatus = "available"
	SyncQuotaFull      SyncQuotaStatus = "full"
	SyncQuotaOver      SyncQuotaStatus = "over_quota"
)

func SyncQuotaStatusForUsage(usedBytes, quotaBytes int64) SyncQuotaStatus {
	switch {
	case usedBytes > quotaBytes:
		return SyncQuotaOver
	case usedBytes == quotaBytes:
		return SyncQuotaFull
	default:
		return SyncQuotaAvailable
	}
}

type SyncPlan struct {
	UploadFiles                []File
	DownloadFiles              []File
	UploadDeletedPaths         []string
	DownloadDeletedPaths       []string
	UploadEmptyDirectories     []string
	DownloadEmptyDirectories   []string
	UploadDeletedDirectories   []string
	DownloadDeletedDirectories []string
	Conflicts                  []Conflict
	UploadBlocks               []Chunk
	DownloadBlocks             []Chunk
	RequiredBlocks             []Chunk
	PlannedBytes               int64
}

// PlanSync compares explicit client state with an immutable base and the
// current server snapshot. Missing paths are never interpreted as deletions;
// only DeletedPaths can request a local deletion.
func PlanSync(baseFiles, currentFiles []File, request CreateSyncSession) SyncPlan {
	return PlanSyncTree(baseFiles, currentFiles, nil, nil, request)
}

// PlanSyncTree plans files and empty leaf directories against the same
// immutable snapshots. Directories have no content blocks and do not affect
// file quota or byte counts.
func PlanSyncTree(baseFiles, currentFiles []File, baseEmptyDirectories, currentEmptyDirectories []string, request CreateSyncSession) SyncPlan {
	base := fileMap(baseFiles)
	current := fileMap(currentFiles)
	client := fileMap(request.Files)
	localDeleted := stringSet(request.DeletedPaths)
	plan := SyncPlan{
		UploadFiles: []File{}, DownloadFiles: []File{},
		UploadDeletedPaths: []string{}, DownloadDeletedPaths: []string{},
		Conflicts: []Conflict{}, UploadBlocks: []Chunk{}, DownloadBlocks: []Chunk{}, RequiredBlocks: []Chunk{},
		UploadEmptyDirectories: []string{}, DownloadEmptyDirectories: []string{},
		UploadDeletedDirectories: []string{}, DownloadDeletedDirectories: []string{},
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
	planEmptyDirectoryChanges(&plan, baseEmptyDirectories, currentEmptyDirectories, request)
	coalesceTreePlan(&plan, baseFiles, currentFiles, baseEmptyDirectories, currentEmptyDirectories, request)

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
	sort.Strings(plan.UploadEmptyDirectories)
	sort.Strings(plan.DownloadEmptyDirectories)
	sort.Strings(plan.UploadDeletedDirectories)
	sort.Strings(plan.DownloadDeletedDirectories)
	return plan
}

func planEmptyDirectoryChanges(plan *SyncPlan, baseDirectories, currentDirectories []string, request CreateSyncSession) {
	base := pathSet(baseDirectories)
	current := pathSet(currentDirectories)
	local := pathSet(request.EmptyDirectories)
	localDeleted := stringSet(request.DeletedDirectories)
	keys := make(map[string]struct{}, len(base)+len(current)+len(local)+len(localDeleted))
	for key := range base {
		keys[key] = struct{}{}
	}
	for key := range current {
		keys[key] = struct{}{}
	}
	for key := range local {
		keys[key] = struct{}{}
	}
	for key := range localDeleted {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		if !inScope(key, request.Scope) {
			continue
		}
		basePath, baseOK := base[key]
		remotePath, remoteOK := current[key]
		localPath, localOK := local[key]
		localDelete := localDeleted[key]
		localChanged := localDelete && baseOK || localOK && (!baseOK || localPath != basePath)
		remoteChanged := baseOK != remoteOK || (baseOK && remoteOK && basePath != remotePath)
		if request.Direction == SyncUpload || request.Direction == SyncBidirectional {
			if localChanged {
				if request.Policy == SyncFillMissing {
					if !remoteOK && localOK {
						plan.UploadEmptyDirectories = append(plan.UploadEmptyDirectories, localPath)
					}
				} else if remoteChanged && !sameOptionalPath(localPath, localOK, remotePath, remoteOK) {
					appendConflictOnce(plan, Conflict{Path: canonicalDirectoryPath(key, basePath, baseOK, remotePath, remoteOK, localPath, localOK), Reason: "type_changed"})
				} else if localDelete {
					if remoteOK {
						plan.UploadDeletedDirectories = append(plan.UploadDeletedDirectories, remotePath)
					}
				} else if localOK && (!remoteOK || localPath != remotePath) {
					plan.UploadEmptyDirectories = append(plan.UploadEmptyDirectories, localPath)
				}
			}
		}
		if request.Direction == SyncDownload || request.Direction == SyncBidirectional {
			if request.Policy == SyncFillMissing {
				if !localOK && remoteOK {
					plan.DownloadEmptyDirectories = append(plan.DownloadEmptyDirectories, remotePath)
				}
			} else if remoteChanged {
				if localChanged && !sameOptionalPath(localPath, localOK, remotePath, remoteOK) {
					appendConflictOnce(plan, Conflict{Path: canonicalDirectoryPath(key, basePath, baseOK, remotePath, remoteOK, localPath, localOK), Reason: "type_changed"})
				} else if remoteOK && (!localOK || localPath != remotePath) {
					plan.DownloadEmptyDirectories = append(plan.DownloadEmptyDirectories, remotePath)
				} else if !remoteOK && localOK {
					plan.DownloadDeletedDirectories = append(plan.DownloadDeletedDirectories, canonicalDirectoryPath(key, basePath, baseOK, remotePath, remoteOK, localPath, localOK))
				}
			} else if request.Policy == SyncLive && !localChanged && remoteOK && !localOK {
				plan.DownloadEmptyDirectories = append(plan.DownloadEmptyDirectories, remotePath)
			}
		}
	}
	conflictPaths := make(map[string]struct{}, len(plan.Conflicts))
	for _, conflict := range plan.Conflicts {
		conflictPaths[strings.ToLower(conflict.Path)] = struct{}{}
	}
	withoutConflicts := func(paths []string) []string {
		result := paths[:0]
		for _, value := range paths {
			if _, conflicted := conflictPaths[strings.ToLower(value)]; !conflicted {
				result = append(result, value)
			}
		}
		return result
	}
	plan.UploadFiles = filterFilesWithoutConflicts(plan.UploadFiles, conflictPaths)
	plan.DownloadFiles = filterFilesWithoutConflicts(plan.DownloadFiles, conflictPaths)
	plan.UploadDeletedPaths = withoutConflicts(plan.UploadDeletedPaths)
	plan.DownloadDeletedPaths = withoutConflicts(plan.DownloadDeletedPaths)
	plan.UploadEmptyDirectories = withoutConflicts(plan.UploadEmptyDirectories)
	plan.DownloadEmptyDirectories = withoutConflicts(plan.DownloadEmptyDirectories)
	plan.UploadDeletedDirectories = withoutConflicts(plan.UploadDeletedDirectories)
	plan.DownloadDeletedDirectories = withoutConflicts(plan.DownloadDeletedDirectories)
}

func filterFilesWithoutConflicts(files []File, conflictPaths map[string]struct{}) []File {
	result := files[:0]
	for _, file := range files {
		if _, conflicted := conflictPaths[strings.ToLower(file.Path)]; !conflicted {
			result = append(result, file)
		}
	}
	return result
}

func filterStringsWithoutConflicts(paths []string, conflictPaths map[string]struct{}) []string {
	result := paths[:0]
	for _, value := range paths {
		if _, conflicted := conflictPaths[strings.ToLower(value)]; !conflicted {
			result = append(result, value)
		}
	}
	return result
}

func coalesceTreePlan(plan *SyncPlan, baseFiles, currentFiles []File, baseDirectories, currentDirectories []string, request CreateSyncSession) {
	// A type replacement is one desired tree mutation. Prefer the explicitly
	// present local kind for uploads and the committed remote kind for downloads.
	localFiles, localDirs := fileMap(request.Files), pathSet(request.EmptyDirectories)
	localDeletedFiles, localDeletedDirs := stringSet(request.DeletedPaths), stringSet(request.DeletedDirectories)
	remoteFiles, remoteDirs := fileMap(currentFiles), pathSet(currentDirectories)
	baseFileMap, baseDirMap := fileMap(baseFiles), pathSet(baseDirectories)
	filter := func(paths []string, keep func(string) bool) []string {
		result := paths[:0]
		for _, value := range paths {
			if keep(strings.ToLower(value)) {
				result = append(result, value)
			}
		}
		return result
	}
	filterFiles := func(files []File, keep func(File) bool) []File {
		result := files[:0]
		for _, file := range files {
			if keep(file) {
				result = append(result, file)
			}
		}
		return result
	}
	plan.UploadFiles = filterFiles(plan.UploadFiles, func(file File) bool { return localFiles[strings.ToLower(file.Path)].Path != "" })
	plan.UploadEmptyDirectories = filter(plan.UploadEmptyDirectories, func(key string) bool { return localDirs[key] != "" })
	plan.UploadDeletedPaths = filter(plan.UploadDeletedPaths, func(key string) bool {
		return localFiles[key].Path == "" && localDirs[key] == "" && localDeletedFiles[key]
	})
	plan.UploadDeletedDirectories = filter(plan.UploadDeletedDirectories, func(key string) bool {
		return localFiles[key].Path == "" && localDirs[key] == "" && localDeletedDirs[key]
	})
	plan.DownloadFiles = filterFiles(plan.DownloadFiles, func(file File) bool { return remoteFiles[strings.ToLower(file.Path)].Path != "" })
	plan.DownloadEmptyDirectories = filter(plan.DownloadEmptyDirectories, func(key string) bool { return remoteDirs[key] != "" })
	plan.DownloadDeletedPaths = filter(plan.DownloadDeletedPaths, func(key string) bool {
		return remoteFiles[key].Path == "" && remoteDirs[key] == "" && baseFileMap[key].Path != ""
	})
	plan.DownloadDeletedDirectories = filter(plan.DownloadDeletedDirectories, func(key string) bool {
		return remoteFiles[key].Path == "" && remoteDirs[key] == "" && baseDirMap[key] != ""
	})
	_ = localDeletedFiles
	_ = localDeletedDirs
	// For cross-kind concurrent edits, report a single type conflict and remove
	// both directions' mutations for that path.
	baseKinds := treeKindMap(baseFiles, baseDirectories)
	remoteKinds := treeKindMap(currentFiles, currentDirectories)
	localKinds := treeKindMap(request.Files, request.EmptyDirectories)
	keys := make(map[string]struct{}, len(baseKinds)+len(remoteKinds)+len(localKinds))
	for k := range baseKinds {
		keys[k] = struct{}{}
	}
	for k := range remoteKinds {
		keys[k] = struct{}{}
	}
	for k := range localKinds {
		keys[k] = struct{}{}
	}
	for key := range keys {
		baseKind, remoteKind, localKind := baseKinds[key], remoteKinds[key], localKinds[key]
		localChanged := (localDeletedFiles[key] && baseKind == "file") || (localDeletedDirs[key] && baseKind == "directory")
		if localKind != "" && localKind != baseKind {
			localChanged = true
		}
		remoteChanged := remoteKind != baseKind
		if localChanged && remoteChanged && localKind != remoteKind {
			pathValue := key
			if file, ok := remoteFiles[key]; ok {
				pathValue = file.Path
			} else if dir := remoteDirs[key]; dir != "" {
				pathValue = dir
			} else if file, ok := localFiles[key]; ok {
				pathValue = file.Path
			} else if dir := localDirs[key]; dir != "" {
				pathValue = dir
			}
			appendConflictOnce(plan, Conflict{Path: pathValue, Reason: "type_changed"})
			plan.UploadFiles = filterFiles(plan.UploadFiles, func(file File) bool { return strings.ToLower(file.Path) != key })
			plan.UploadEmptyDirectories = filter(plan.UploadEmptyDirectories, func(value string) bool { return value != key })
			plan.UploadDeletedPaths = filter(plan.UploadDeletedPaths, func(value string) bool { return value != key })
			plan.UploadDeletedDirectories = filter(plan.UploadDeletedDirectories, func(value string) bool { return value != key })
			plan.DownloadFiles = filterFiles(plan.DownloadFiles, func(file File) bool { return strings.ToLower(file.Path) != key })
			plan.DownloadEmptyDirectories = filter(plan.DownloadEmptyDirectories, func(value string) bool { return value != key })
			plan.DownloadDeletedPaths = filter(plan.DownloadDeletedPaths, func(value string) bool { return value != key })
			plan.DownloadDeletedDirectories = filter(plan.DownloadDeletedDirectories, func(value string) bool { return value != key })
		}
	}
	conflictPaths := make(map[string]struct{}, len(plan.Conflicts))
	for _, conflict := range plan.Conflicts {
		conflictPaths[strings.ToLower(conflict.Path)] = struct{}{}
	}
	plan.UploadFiles = filterFilesWithoutConflicts(plan.UploadFiles, conflictPaths)
	plan.DownloadFiles = filterFilesWithoutConflicts(plan.DownloadFiles, conflictPaths)
	plan.UploadDeletedPaths = filterStringsWithoutConflicts(plan.UploadDeletedPaths, conflictPaths)
	plan.DownloadDeletedPaths = filterStringsWithoutConflicts(plan.DownloadDeletedPaths, conflictPaths)
	plan.UploadEmptyDirectories = filterStringsWithoutConflicts(plan.UploadEmptyDirectories, conflictPaths)
	plan.DownloadEmptyDirectories = filterStringsWithoutConflicts(plan.DownloadEmptyDirectories, conflictPaths)
	plan.UploadDeletedDirectories = filterStringsWithoutConflicts(plan.UploadDeletedDirectories, conflictPaths)
	plan.DownloadDeletedDirectories = filterStringsWithoutConflicts(plan.DownloadDeletedDirectories, conflictPaths)
}

func pathSet(paths []string) map[string]string {
	result := make(map[string]string, len(paths))
	for _, value := range paths {
		result[strings.ToLower(value)] = value
	}
	return result
}

func sameOptionalPath(a string, aOK bool, b string, bOK bool) bool {
	return aOK == bOK && (!aOK || a == b)
}

func canonicalDirectoryPath(key, base string, baseOK bool, remote string, remoteOK bool, local string, localOK bool) string {
	if baseOK {
		return base
	}
	if remoteOK {
		return remote
	}
	if localOK {
		return local
	}
	return key
}

func appendConflictOnce(plan *SyncPlan, conflict Conflict) {
	if !containsConflict(plan.Conflicts, conflict.Path) {
		plan.Conflicts = append(plan.Conflicts, conflict)
	}
}

func treeKindMap(files []File, directories []string) map[string]string {
	result := make(map[string]string, len(files)+len(directories))
	for _, file := range files {
		result[strings.ToLower(file.Path)] = "file"
	}
	for _, directory := range directories {
		result[strings.ToLower(directory)] = "directory"
	}
	return result
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
	if len(value.Files)+len(value.EmptyDirectories) > MaxSyncFiles || len(value.Scope) > MaxSyncScopeItems ||
		len(value.DeletedPaths) > MaxSyncFiles || len(value.DeletedDirectories) > MaxSyncFiles ||
		len(value.DeletedPaths)+len(value.DeletedDirectories) > MaxSyncFiles {
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
	seen := make(map[string]string, len(value.Files)+len(value.EmptyDirectories))
	seenFiles := make(map[string]string, len(value.Files))
	seenDirectories := make(map[string]string, len(value.EmptyDirectories))
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
		seenFiles[folded] = file.Path
		if len(value.Scope) > 0 && !inScope(strings.ToLower(file.Path), value.Scope) {
			return fmt.Errorf("file path %q is outside the requested sync scope", file.Path)
		}
		if total > int64(^uint64(0)>>1)-file.SizeBytes {
			return errors.New("workspace file size overflows int64")
		}
		total += file.SizeBytes
	}
	for _, directory := range value.EmptyDirectories {
		if err := ValidateRelativePath(directory); err != nil {
			return fmt.Errorf("empty directory %q: %w", directory, err)
		}
		folded := strings.ToLower(directory)
		if _, exists := seen[folded]; exists {
			return fmt.Errorf("duplicate or case-folded path collision %q", directory)
		}
		if err := paths.AddEmptyDirectory(directory); err != nil {
			return err
		}
		seen[folded] = directory
		seenDirectories[folded] = directory
		if len(value.Scope) > 0 && !inScope(strings.ToLower(directory), value.Scope) {
			return fmt.Errorf("empty directory path %q is outside the requested sync scope", directory)
		}
	}
	seenDeleted := make(map[string]struct{}, len(value.DeletedPaths))
	var deletedTree mordesync.ManifestPathValidator
	for _, filePath := range value.DeletedPaths {
		if err := ValidateRelativePath(filePath); err != nil {
			return fmt.Errorf("deleted path %q: %w", filePath, err)
		}
		if err := deletedTree.Add(filePath); err != nil {
			return err
		}
		folded := strings.ToLower(filePath)
		if _, exists := seenFiles[folded]; exists {
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
	seenDeletedDirectories := make(map[string]struct{}, len(value.DeletedDirectories))
	for _, directory := range value.DeletedDirectories {
		if err := ValidateRelativePath(directory); err != nil {
			return fmt.Errorf("deleted directory %q: %w", directory, err)
		}
		if err := deletedTree.AddEmptyDirectory(directory); err != nil {
			return err
		}
		folded := strings.ToLower(directory)
		if _, exists := seenDirectories[folded]; exists {
			return fmt.Errorf("directory %q is both present and deleted", directory)
		}
		if _, exists := seenDeletedDirectories[folded]; exists {
			return fmt.Errorf("duplicate deleted directory %q", directory)
		}
		if _, exists := seenDeleted[folded]; exists {
			return fmt.Errorf("path %q is both a file and directory deletion", directory)
		}
		if len(value.Scope) > 0 && !inScope(strings.ToLower(directory), value.Scope) {
			return fmt.Errorf("deleted directory %q is outside the requested sync scope", directory)
		}
		seenDeletedDirectories[folded] = struct{}{}
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
