package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

func (s *Store) RegisterWorkspaceSyncFolder(ctx context.Context, principal identity.Principal, hostID string, value workspace.SyncFolder,
	idempotencyKeyHash, requestHash []byte) (workspace.SyncFolder, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(principal.UserID) || value.OwnerID != principal.UserID ||
		!workspace.ValidSyncFolderID(value.FolderID) || value.Kind != workspace.SyncFolderKindProject ||
		workspace.ValidateSyncProjectFolderName(value.ProjectName) != nil || !norm.NFC.IsNormalString(value.ProjectName) || value.RelativePath != "project/"+value.ProjectName ||
		len(idempotencyKeyHash) != 32 || len(requestHash) != 32 {
		return workspace.SyncFolder{}, domain.InvalidArgument("workspace_sync_folder", "workspace sync folder registration is invalid")
	}
	var result workspace.SyncFolder
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, principal.UserID, true); err != nil {
			return err
		}
		if err := requireApprovedWorkspaceSyncOwner(ctx, tx, principal.UserID); err != nil {
			return err
		}
		if err := requireActivePortalSession(ctx, tx, principal); err != nil {
			return err
		}
		workspaceID, err := lockHostSyncFolderWorkspace(ctx, tx, principal.UserID, hostID, true)
		if err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, true); err != nil {
			return err
		}
		var savedRequestHash []byte
		var savedWorkspaceID, savedFolderID string
		err = tx.QueryRow(ctx, `SELECT request_hash,workspace_id,folder_id
FROM workspace_sync_folder_registration_requests
WHERE owner_id=$1 AND host_id=$2 AND idempotency_key_hash=$3`, principal.UserID, hostID, idempotencyKeyHash).
			Scan(&savedRequestHash, &savedWorkspaceID, &savedFolderID)
		if err == nil {
			if !bytes.Equal(savedRequestHash, requestHash) || savedWorkspaceID != workspaceID {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for another Workspace sync folder")
			}
			result, err = scanWorkspaceSyncFolder(tx.QueryRow(ctx, workspaceSyncFolderSelect+`WHERE f.workspace_id=$1 AND f.owner_id=$2 AND f.folder_id=$3`,
				savedWorkspaceID, principal.UserID, savedFolderID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find Workspace sync folder registration receipt: %w", err)
		}
		var activeSync bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_sync_sessions
WHERE workspace_id=$1 AND state IN ('planned','transferring','verifying','applying')
) OR EXISTS (
SELECT 1 FROM workspace_sync_receive_reservations
WHERE workspace_id=$1 AND state IN ('reserved','aborting')
)`, workspaceID).Scan(&activeSync); err != nil {
			return fmt.Errorf("check active Workspace sync before folder registration: %w", err)
		}
		if activeSync {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace synchronization must settle before registering a project folder")
		}
		if err := ensureNoPendingWorkspaceSyncCommit(ctx, tx, workspaceID); err != nil {
			return err
		}
		var hasRoot bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_sync_folders WHERE workspace_id=$1 AND owner_id=$2 AND folder_kind='workspace_root'
)`, workspaceID, principal.UserID).Scan(&hasRoot); err != nil {
			return fmt.Errorf("check legacy root Workspace sync folder: %w", err)
		}
		if hasRoot {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace already has a legacy root sync folder")
		}
		rows, err := tx.Query(ctx, `SELECT project_name FROM workspace_sync_folders
WHERE workspace_id=$1 AND owner_id=$2 AND folder_kind='project' FOR UPDATE`, workspaceID, principal.UserID)
		if err != nil {
			return fmt.Errorf("list existing Workspace project names: %w", err)
		}
		nameKey := cases.Fold().String(value.ProjectName)
		for rows.Next() {
			var existingName string
			if err := rows.Scan(&existingName); err != nil {
				rows.Close()
				return fmt.Errorf("read existing Workspace project name: %w", err)
			}
			if cases.Fold().String(norm.NFC.String(existingName)) == nameKey {
				rows.Close()
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace already has a project folder with this name")
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read existing Workspace project names: %w", err)
		}
		rows.Close()
		value.WorkspaceID = workspaceID
		value.State = "pending_node"
		if err := tx.QueryRow(ctx, `INSERT INTO workspace_sync_folders
(workspace_id,owner_id,folder_id,folder_kind,project_name,project_name_folded,relative_path,state)
VALUES ($1,$2,$3,'project',$4,$5,$6,'pending_node')
RETURNING created_at,updated_at`, workspaceID, principal.UserID, value.FolderID, value.ProjectName, nameKey, value.RelativePath).
			Scan(&value.CreatedAt, &value.UpdatedAt); err != nil {
			return fmt.Errorf("register Workspace project sync folder: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_sync_folder_registration_requests
(owner_id,host_id,workspace_id,idempotency_key_hash,request_hash,folder_id)
VALUES ($1,$2,$3,$4,$5,$6)`, principal.UserID, hostID, workspaceID, idempotencyKeyHash, requestHash, value.FolderID); err != nil {
			return fmt.Errorf("record Workspace sync folder registration: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target)
VALUES ($1,'session','workspace.sync_folder.register',$2)`, principal.UserID, value.FolderID); err != nil {
			return fmt.Errorf("audit Workspace sync folder registration: %w", err)
		}
		result = value
		return nil
	})
	return result, err
}

func (s *Store) ListWorkspaceSyncFolders(ctx context.Context, principal identity.Principal, hostID string) ([]workspace.SyncFolder, error) {
	items := make([]workspace.SyncFolder, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedWorkspaceSyncOwner(ctx, tx, principal.UserID); err != nil {
			return err
		}
		if err := requireActivePortalSession(ctx, tx, principal); err != nil {
			return err
		}
		workspaceID, err := lockHostSyncFolderWorkspace(ctx, tx, principal.UserID, hostID, false)
		if err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, workspaceSyncFolderSelect+`WHERE f.workspace_id=$1 AND f.owner_id=$2 ORDER BY f.folder_kind,f.project_name,f.folder_id`, workspaceID, principal.UserID)
		if err != nil {
			return fmt.Errorf("list Workspace sync folders: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			row, err := scanWorkspaceSyncFolder(rows)
			if err != nil {
				return err
			}
			items = append(items, row)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read Workspace sync folders: %w", err)
		}
		return nil
	})
	return items, err
}

func (s *Store) GetWorkspaceSyncFolder(ctx context.Context, principal identity.Principal, workspaceID, folderID string) (workspace.SyncFolder, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidSyncFolderID(folderID) {
		return workspace.SyncFolder{}, domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
	}
	var result workspace.SyncFolder
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, principal.UserID, false); err != nil {
			return err
		}
		if err := requireApprovedWorkspaceSyncOwner(ctx, tx, principal.UserID); err != nil {
			return err
		}
		switch principal.Kind {
		case identity.KindSession:
			if err := requireActivePortalSession(ctx, tx, principal); err != nil {
				return err
			}
		case identity.KindDevice:
			if _, err := lockWorkspaceSyncDeviceCredentials(ctx, tx, principal.UserID, principal.ClientInstallationID, principal.TokenID, workspaceID); err != nil {
				return err
			}
		case identity.KindClient:
			if err := requireWorkspaceSyncClientInstallation(ctx, tx, principal, false); err != nil {
				return err
			}
		default:
			return domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
		}
		var folderKind string
		if err := tx.QueryRow(ctx, `SELECT folder_kind FROM workspace_sync_folders
WHERE workspace_id=$1 AND owner_id=$2 AND folder_id=$3`, workspaceID, principal.UserID, folderID).Scan(&folderKind); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
		} else if err != nil {
			return fmt.Errorf("read Workspace sync folder kind: %w", err)
		}
		if folderKind == string(workspace.SyncFolderKindProject) {
			if err := lockWorkspaceSyncProjectRegistrationHost(ctx, tx, principal.UserID, workspaceID, folderID); err != nil {
				return err
			}
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, false); err != nil {
			return err
		}
		value, err := scanWorkspaceSyncFolder(tx.QueryRow(ctx, workspaceSyncFolderSelect+
			`WHERE f.workspace_id=$1 AND f.owner_id=$2 AND f.folder_id=$3`, workspaceID, principal.UserID, folderID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace sync folder not found")
		}
		if err != nil {
			return err
		}
		if value.Kind == workspace.SyncFolderKindProject {
			if err := requireWorkspaceSyncProjectHostBinding(ctx, tx, value.OwnerID, value.WorkspaceID, value.FolderID); err != nil {
				return err
			}
		}
		result = value
		return nil
	})
	return result, err
}

func lockHostSyncFolderWorkspace(ctx context.Context, tx pgx.Tx, ownerID, hostID string, exclusive bool) (string, error) {
	lock := "FOR SHARE"
	if exclusive {
		lock = "FOR UPDATE"
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM hosts WHERE id=$1 AND owner_id=$2 AND deleted_at IS NULL `+lock, hostID, ownerID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return "", fmt.Errorf("lock Host for Workspace sync folder: %w", err)
	}
	if state != string(host.StateRunning) && state != string(host.StateStopped) {
		return "", domain.NewError(domain.CodeWorkspaceBusy, "host must be running or stopped to register a sync folder")
	}
	var workspaceID string
	err = tx.QueryRow(ctx, `SELECT workspace_id FROM workspace_bindings
WHERE host_id=$1 AND owner_id=$2 AND state='active' FOR SHARE`, hostID, ownerID).Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.NewError(domain.CodeNotFound, "host workspace binding not found")
	}
	if err != nil {
		return "", fmt.Errorf("resolve Host Workspace for sync folder: %w", err)
	}
	return workspaceID, nil
}

const workspaceSyncFolderSelect = `SELECT f.owner_id,f.workspace_id,f.folder_id,f.folder_kind,COALESCE(f.project_name,''),
f.relative_path,f.state,f.last_upload_revision,f.created_at,f.updated_at FROM workspace_sync_folders f `

func scanWorkspaceSyncFolder(row interface{ Scan(...any) error }) (workspace.SyncFolder, error) {
	var value workspace.SyncFolder
	var lastUploadRevision pgtype.Int8
	err := row.Scan(&value.OwnerID, &value.WorkspaceID, &value.FolderID, &value.Kind, &value.ProjectName,
		&value.RelativePath, &value.State, &lastUploadRevision, &value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return workspace.SyncFolder{}, storeError("read Workspace sync folder", "Workspace sync folder", err)
	}
	if lastUploadRevision.Valid {
		value.LastUploadRevision = &lastUploadRevision.Int64
	}
	return value, nil
}

func requireWorkspaceRootSyncTarget(ctx context.Context, tx pgx.Tx, ownerID, workspaceID string) error {
	var hasProject bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_sync_folders WHERE owner_id=$1 AND workspace_id=$2 AND folder_kind='project'
)`, ownerID, workspaceID).Scan(&hasProject); err != nil {
		return fmt.Errorf("check Workspace sync target kind: %w", err)
	}
	if hasProject {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "project folder synchronization is not available yet")
	}
	return nil
}

func (s *Store) CreateWorkspaceSyncDeviceChallenge(ctx context.Context, principal identity.Principal, value workspace.SyncDeviceChallenge) error {
	if !domain.ValidWorkspaceSyncDeviceChallengeID(value.ID) || !domain.ValidWorkspaceID(value.WorkspaceID) ||
		!domain.ValidUserID(value.OwnerID) || !domain.ValidClientInstallationID(value.ClientInstallationID) ||
		value.OwnerID != principal.UserID || value.ClientInstallationID != principal.ClientInstallationID ||
		(value.FolderID == "" && (value.FolderKind != workspace.SyncFolderKindWorkspaceRoot || value.RelativePath != ".")) ||
		(value.FolderID != "" && (!workspace.ValidSyncFolderID(value.FolderID) || value.FolderKind != workspace.SyncFolderKindProject ||
			!strings.HasPrefix(value.RelativePath, "project/") || strings.Count(value.RelativePath, "/") != 1 ||
			workspace.ValidateRelativePath(value.RelativePath) != nil ||
			workspace.ValidateSyncProjectFolderName(strings.TrimPrefix(value.RelativePath, "project/")) != nil)) ||
		value.DeviceID == "" || len(value.CertificateDER) == 0 || len(value.CertificateDER) > app.MaxWorkspaceSyncDeviceCertificateDER ||
		len(value.CertificateSHA256) != 32 || len(value.Nonce) != 32 || !value.ExpiresAt.After(time.Now()) {
		return domain.InvalidArgument("challenge", "workspace sync device challenge is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, principal.UserID, false); err != nil {
			return err
		}
		if err := requireApprovedWorkspaceSyncOwner(ctx, tx, principal.UserID); err != nil {
			return err
		}
		// Serialize this installation's count-and-insert before checking its token and Workspace.
		if err := requireWorkspaceSyncClientInstallation(ctx, tx, principal, true); err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, value.WorkspaceID, false); err != nil {
			return err
		}
		if value.FolderID == "" {
			var existingKind string
			err := tx.QueryRow(ctx, `SELECT folder_kind FROM workspace_sync_folders WHERE workspace_id=$1 AND owner_id=$2`,
				value.WorkspaceID, principal.UserID).Scan(&existingKind)
			if err == nil && existingKind != string(workspace.SyncFolderKindWorkspaceRoot) {
				return domain.NewError(domain.CodeWorkspaceUnavailable, "project folder synchronization is not available yet")
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check Workspace sync folder for legacy root challenge: %w", err)
			}
		} else {
			var storedKind, storedPath string
			if err := tx.QueryRow(ctx, `SELECT folder_kind,relative_path FROM workspace_sync_folders
WHERE workspace_id=$1 AND owner_id=$2 AND folder_id=$3`, value.WorkspaceID, principal.UserID, value.FolderID).
				Scan(&storedKind, &storedPath); err != nil {
				return storeError("check Workspace project sync folder for device challenge", "Workspace sync folder", err)
			}
			if storedKind != string(value.FolderKind) || storedPath != value.RelativePath {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync folder target changed")
			}
		}
		if _, err := tx.Exec(ctx, `WITH expired AS (
  SELECT id FROM workspace_sync_device_challenges WHERE expires_at<=clock_timestamp()
  ORDER BY expires_at LIMIT 256 FOR UPDATE SKIP LOCKED
)
DELETE FROM workspace_sync_device_challenges c USING expired WHERE c.id=expired.id`); err != nil {
			return fmt.Errorf("clean expired Workspace sync device challenges: %w", err)
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_device_challenges
WHERE owner_id=$1 AND client_installation_id=$2 AND expires_at>clock_timestamp()`, principal.UserID, principal.ClientInstallationID).Scan(&pending); err != nil {
			return fmt.Errorf("count pending Workspace sync device challenges: %w", err)
		}
		if pending >= app.MaxPendingWorkspaceSyncDeviceChallengesPerInstallation {
			return domain.NewError(domain.CodeRateLimited, "too many pending Workspace sync device challenges")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_sync_device_challenges
(id,owner_id,workspace_id,folder_id,folder_kind,relative_path,client_installation_id,device_id,certificate_der,certificate_sha256,nonce,expires_at)
VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,$12)`, value.ID, value.OwnerID, value.WorkspaceID,
			value.FolderID, string(value.FolderKind), value.RelativePath, value.ClientInstallationID, value.DeviceID, value.CertificateDER,
			value.CertificateSHA256, value.Nonce, value.ExpiresAt); err != nil {
			return fmt.Errorf("create workspace sync device challenge: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target)
VALUES ($1,'client','workspace.sync_device.challenge',$2)`, value.OwnerID, value.ID); err != nil {
			return fmt.Errorf("audit workspace sync device challenge: %w", err)
		}
		return nil
	})
}

func (s *Store) GetWorkspaceSyncDeviceChallenge(ctx context.Context, principal identity.Principal, workspaceID, challengeID string) (workspace.SyncDeviceChallenge, error) {
	var value workspace.SyncDeviceChallenge
	var folderID pgtype.Text
	err := s.pool.QueryRow(ctx, `SELECT id,owner_id,workspace_id,folder_id,folder_kind,relative_path,client_installation_id,device_id,
certificate_der,certificate_sha256,nonce,expires_at
FROM workspace_sync_device_challenges
WHERE id=$1 AND owner_id=$2 AND workspace_id=$3 AND client_installation_id=$4`,
		challengeID, principal.UserID, workspaceID, principal.ClientInstallationID).Scan(
		&value.ID, &value.OwnerID, &value.WorkspaceID, &folderID, &value.FolderKind, &value.RelativePath, &value.ClientInstallationID, &value.DeviceID,
		&value.CertificateDER, &value.CertificateSHA256, &value.Nonce, &value.ExpiresAt)
	if err != nil {
		return workspace.SyncDeviceChallenge{}, storeError("get workspace sync device challenge", "workspace sync device challenge", err)
	}
	if folderID.Valid {
		value.FolderID = folderID.String
		var storedKind, storedPath string
		if err := s.pool.QueryRow(ctx, `SELECT folder_kind,relative_path FROM workspace_sync_folders
WHERE workspace_id=$1 AND owner_id=$2 AND folder_id=$3`, value.WorkspaceID, value.OwnerID, value.FolderID).
			Scan(&storedKind, &storedPath); err != nil {
			return workspace.SyncDeviceChallenge{}, storeError("verify Workspace sync challenge folder target", "Workspace sync folder", err)
		}
		if storedKind != string(value.FolderKind) || storedPath != value.RelativePath {
			return workspace.SyncDeviceChallenge{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync folder target changed")
		}
	}
	return value, nil
}

func (s *Store) GetWorkspaceSyncDeviceRegistration(ctx context.Context, principal identity.Principal, workspaceID, folderID string,
	idempotencyKeyHash, requestHash []byte) (workspace.SyncDeviceBinding, bool, error) {
	if !domain.ValidWorkspaceID(workspaceID) || (folderID != "" && !workspace.ValidSyncFolderID(folderID)) || len(idempotencyKeyHash) != 32 || len(requestHash) != 32 {
		return workspace.SyncDeviceBinding{}, false, domain.InvalidArgument("registration", "workspace sync device registration lookup is invalid")
	}
	var result workspace.SyncDeviceBinding
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, principal.UserID, false); err != nil {
			return err
		}
		if err := requireApprovedWorkspaceSyncOwner(ctx, tx, principal.UserID); err != nil {
			return err
		}
		if err := requireWorkspaceSyncClientInstallation(ctx, tx, principal, false); err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, false); err != nil {
			return err
		}
		var savedRequestHash []byte
		var bindingID string
		var receiptFolderID string
		err := tx.QueryRow(ctx, `SELECT r.request_hash,r.binding_id,r.folder_id FROM workspace_sync_device_registration_requests r
JOIN workspace_sync_folders f ON f.workspace_id=r.workspace_id AND f.owner_id=r.owner_id AND f.folder_id=r.folder_id
WHERE r.owner_id=$1 AND r.workspace_id=$2 AND r.idempotency_key_hash=$3
  AND (($4='' AND f.folder_kind='workspace_root') OR r.folder_id=$4)`,
			principal.UserID, workspaceID, idempotencyKeyHash, folderID).Scan(&savedRequestHash, &bindingID, &receiptFolderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find Workspace sync device registration receipt: %w", err)
		}
		if !bytes.Equal(savedRequestHash, requestHash) {
			return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for another device registration")
		}
		result, err = scanWorkspaceSyncDeviceBinding(tx.QueryRow(ctx, workspaceSyncDeviceBindingSelect+`
WHERE b.id=$1 AND b.owner_id=$2 AND b.workspace_id=$3 AND b.folder_id=$4`, bindingID, principal.UserID, workspaceID, receiptFolderID))
		if err == nil {
			found = true
		}
		return err
	})
	return result, found, err
}

func (s *Store) RegisterWorkspaceSyncDevice(ctx context.Context, principal identity.Principal, workspaceID, challengeID string,
	idempotencyKeyHash, requestHash []byte, bindingID, folderID string) (workspace.SyncDeviceBinding, error) {
	if !domain.ValidWorkspaceID(workspaceID) || !domain.ValidWorkspaceSyncDeviceChallengeID(challengeID) || !domain.ValidWorkspaceSyncDeviceBindingID(bindingID) ||
		len(idempotencyKeyHash) != 32 || len(requestHash) != 32 || !workspace.ValidSyncFolderID(folderID) {
		return workspace.SyncDeviceBinding{}, domain.InvalidArgument("registration", "workspace sync device registration is invalid")
	}
	var result workspace.SyncDeviceBinding
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, principal.UserID, false); err != nil {
			return err
		}
		if err := requireApprovedWorkspaceSyncOwner(ctx, tx, principal.UserID); err != nil {
			return err
		}
		if err := requireWorkspaceSyncClientInstallation(ctx, tx, principal, false); err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, true); err != nil {
			return err
		}
		var existingBindingID string
		var savedRequestHash []byte
		var receiptFolderID string
		err := tx.QueryRow(ctx, `SELECT r.request_hash,r.binding_id,r.folder_id FROM workspace_sync_device_registration_requests r
JOIN workspace_sync_folders f ON f.workspace_id=r.workspace_id AND f.owner_id=r.owner_id AND f.folder_id=r.folder_id
WHERE r.owner_id=$1 AND r.workspace_id=$2 AND r.idempotency_key_hash=$3
  AND (r.folder_id=$4 OR ($5::boolean AND f.folder_kind='workspace_root'))`,
			principal.UserID, workspaceID, idempotencyKeyHash, folderID, folderID != "").Scan(&savedRequestHash, &existingBindingID, &receiptFolderID)
		if err == nil {
			if !bytes.Equal(savedRequestHash, requestHash) {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for another device registration")
			}
			result, err = scanWorkspaceSyncDeviceBinding(tx.QueryRow(ctx, workspaceSyncDeviceBindingSelect+`
WHERE b.id=$1 AND b.owner_id=$2 AND b.workspace_id=$3 AND b.folder_id=$4`, existingBindingID, principal.UserID, workspaceID, receiptFolderID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find workspace sync device registration request: %w", err)
		}

		var deviceID, challengeKind, challengePath string
		var challengeFolderID pgtype.Text
		var certificateSHA256, nonce []byte
		var expiresAt time.Time
		if err := tx.QueryRow(ctx, `SELECT folder_id,folder_kind,relative_path,device_id,certificate_sha256,nonce,expires_at
FROM workspace_sync_device_challenges WHERE id=$1 AND owner_id=$2 AND workspace_id=$3 AND client_installation_id=$4 FOR UPDATE`,
			challengeID, principal.UserID, workspaceID, principal.ClientInstallationID).Scan(
			&challengeFolderID, &challengeKind, &challengePath, &deviceID, &certificateSHA256, &nonce, &expiresAt); err != nil {
			return storeError("lock workspace sync device challenge", "workspace sync device challenge", err)
		}
		if !expiresAt.After(time.Now()) {
			return domain.NewError(domain.CodeTransferExpired, "workspace sync device challenge expired")
		}
		var targetFolderID string
		if challengeFolderID.Valid {
			if challengeFolderID.String != folderID || challengeKind != string(workspace.SyncFolderKindProject) ||
				workspace.ValidateRelativePath(challengePath) != nil || strings.Count(challengePath, "/") != 1 {
				return domain.NewError(domain.CodeForbidden, "device challenge is bound to another sync folder")
			}
			var storedKind, storedPath string
			if err := tx.QueryRow(ctx, `SELECT folder_kind,relative_path FROM workspace_sync_folders
WHERE workspace_id=$1 AND owner_id=$2 AND folder_id=$3 FOR UPDATE`, workspaceID, principal.UserID, folderID).
				Scan(&storedKind, &storedPath); err != nil {
				return storeError("lock Workspace sync folder for device registration", "Workspace sync folder", err)
			}
			if storedKind != challengeKind || storedPath != challengePath {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace sync folder target changed")
			}
		} else if challengeKind != string(workspace.SyncFolderKindWorkspaceRoot) || challengePath != "." {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace root sync challenge is invalid")
		}
		if challengeFolderID.Valid {
			// Project targets remain metadata-only until the node path and receive materialization are project-aware.
			return domain.NewError(domain.CodeWorkspaceUnavailable, "project folder synchronization is not available yet")
		}
		// Resolve and validate the challenged folder before creating a device identity.
		// In particular, a root challenge may predate project metadata registration.
		var storedFolderID, storedKind, storedPath string
		err = tx.QueryRow(ctx, `SELECT folder_id,folder_kind,relative_path FROM workspace_sync_folders
WHERE workspace_id=$1 AND owner_id=$2 AND folder_kind='workspace_root' FOR UPDATE`, workspaceID, principal.UserID).
			Scan(&storedFolderID, &storedKind, &storedPath)
		if errors.Is(err, pgx.ErrNoRows) {
			var hasProject bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_sync_folders WHERE workspace_id=$1 AND owner_id=$2 AND folder_kind='project')`, workspaceID, principal.UserID).Scan(&hasProject); err != nil {
				return fmt.Errorf("check project sync folders before legacy root registration: %w", err)
			}
			if hasProject {
				return domain.NewError(domain.CodeWorkspaceUnavailable, "project folder synchronization is not available yet")
			}
			if challengeFolderID.Valid {
				return domain.NewError(domain.CodeWorkspaceConflict, "registered project sync folder was removed")
			}
			storedFolderID = folderID
			storedKind, storedPath = string(workspace.SyncFolderKindWorkspaceRoot), "."
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_sync_folders
(workspace_id,owner_id,folder_id,folder_kind,relative_path)
VALUES ($1,$2,$3,'workspace_root','.')`, workspaceID, principal.UserID, storedFolderID); err != nil {
				return fmt.Errorf("create workspace sync folder binding: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("get workspace sync folder binding: %w", err)
		}
		if challengeFolderID.Valid {
			if storedFolderID != challengeFolderID.String || storedKind != string(workspace.SyncFolderKindProject) || storedPath != challengePath {
				return domain.NewError(domain.CodeWorkspaceConflict, "device challenge is bound to another Workspace sync folder")
			}
		} else if storedKind != string(workspace.SyncFolderKindWorkspaceRoot) || storedPath != "." {
			return domain.NewError(domain.CodeWorkspaceUnavailable, "project folder synchronization is not available yet")
		}
		targetFolderID = storedFolderID

		// A root request may have been retried with a newly generated candidate
		// folder ID. Resolve its receipt against the stable root folder instead.
		if targetFolderID != folderID {
			var savedHash []byte
			var savedBindingID string
			err := tx.QueryRow(ctx, `SELECT request_hash,binding_id FROM workspace_sync_device_registration_requests
WHERE owner_id=$1 AND workspace_id=$2 AND folder_id=$3 AND idempotency_key_hash=$4`,
				principal.UserID, workspaceID, targetFolderID, idempotencyKeyHash).Scan(&savedHash, &savedBindingID)
			if err == nil {
				if !bytes.Equal(savedHash, requestHash) {
					return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for another device registration")
				}
				result, err = scanWorkspaceSyncDeviceBinding(tx.QueryRow(ctx, workspaceSyncDeviceBindingSelect+
					`WHERE b.id=$1 AND b.owner_id=$2 AND b.workspace_id=$3 AND b.folder_id=$4`, savedBindingID, principal.UserID, workspaceID, targetFolderID))
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("find root Workspace sync device registration receipt: %w", err)
			}
		}

		var identityID string
		var identityOwner, identityInstallation string
		var existingHash []byte
		err = tx.QueryRow(ctx, `SELECT id,owner_id,client_installation_id,certificate_sha256
FROM workspace_sync_device_identities WHERE device_id=$1 AND revoked_at IS NULL FOR UPDATE`, deviceID).
			Scan(&identityID, &identityOwner, &identityInstallation, &existingHash)
		if err == nil {
			if identityOwner != principal.UserID || identityInstallation != principal.ClientInstallationID || !bytes.Equal(existingHash, certificateSHA256) {
				return domain.NewError(domain.CodeConflict, "Syncthing device is already paired to another installation")
			}
		} else if errors.Is(err, pgx.ErrNoRows) {
			identityID, err = domain.NewWorkspaceSyncDeviceIdentityID()
			if err != nil {
				return err
			}
			command, err := tx.Exec(ctx, `INSERT INTO workspace_sync_device_identities
(id,owner_id,client_installation_id,device_id,certificate_sha256)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (device_id) WHERE revoked_at IS NULL DO NOTHING`,
				identityID, principal.UserID, principal.ClientInstallationID, deviceID, certificateSHA256)
			if err != nil {
				return fmt.Errorf("register workspace sync device identity: %w", err)
			}
			if command.RowsAffected() != 1 {
				err := tx.QueryRow(ctx, `SELECT id,owner_id,client_installation_id,certificate_sha256
FROM workspace_sync_device_identities WHERE device_id=$1 AND revoked_at IS NULL FOR UPDATE`, deviceID).
					Scan(&identityID, &identityOwner, &identityInstallation, &existingHash)
				if errors.Is(err, pgx.ErrNoRows) || err == nil &&
					(identityOwner != principal.UserID || identityInstallation != principal.ClientInstallationID || !bytes.Equal(existingHash, certificateSHA256)) {
					return domain.NewError(domain.CodeConflict, "Syncthing device is already paired to another installation")
				}
				if err != nil {
					return fmt.Errorf("read concurrently registered Workspace sync device identity: %w", err)
				}
			}
		} else {
			return fmt.Errorf("find workspace sync device identity: %w", err)
		}

		folderUpdate, err := tx.Exec(ctx, `UPDATE workspace_sync_folders SET state='pending_node',updated_at=clock_timestamp()
WHERE workspace_id=$1 AND folder_id=$2 AND state IN ('revoking','revoked')
  AND NOT EXISTS (SELECT 1 FROM workspace_sync_device_bindings b WHERE b.workspace_id=$1 AND b.folder_id=$2 AND b.state='revoking')`, workspaceID, targetFolderID)
		if err != nil {
			return fmt.Errorf("reopen Workspace sync folder after device registration: %w", err)
		}
		desiredStateChanged := folderUpdate.RowsAffected() != 0

		var existingBindingState string
		err = tx.QueryRow(ctx, `SELECT id,state FROM workspace_sync_device_bindings
WHERE workspace_id=$1 AND folder_id=$2 AND device_identity_id=$3 AND state<>'revoked' FOR UPDATE`, workspaceID, targetFolderID, identityID).Scan(&existingBindingID, &existingBindingState)
		if errors.Is(err, pgx.ErrNoRows) {
			existingBindingID = bindingID
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_sync_device_bindings
(id,owner_id,workspace_id,folder_id,client_installation_id,device_identity_id,state)
VALUES ($1,$2,$3,$4,$5,$6,'pending_node')`, existingBindingID, principal.UserID, workspaceID, targetFolderID, principal.ClientInstallationID, identityID); err != nil {
				return fmt.Errorf("create workspace sync device binding: %w", err)
			}
			desiredStateChanged = true
		} else if err != nil {
			return fmt.Errorf("find workspace sync device binding: %w", err)
		} else if existingBindingState == string(workspace.SyncDeviceBindingRevoking) {
			return domain.NewError(domain.CodeConflict, "device revocation is pending Workspace sync node reconciliation")
		}
		if desiredStateChanged {
			if _, err := bumpWorkspaceSyncNodeDesiredRevision(ctx, tx, principal.UserID); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `INSERT INTO workspace_sync_device_registration_requests
(owner_id,workspace_id,folder_id,idempotency_key_hash,request_hash,binding_id)
VALUES ($1,$2,$3,$4,$5,$6)`, principal.UserID, workspaceID, targetFolderID, idempotencyKeyHash, requestHash, existingBindingID); err != nil {
			return fmt.Errorf("record workspace sync device registration request: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_sync_device_challenges WHERE id=$1 AND owner_id=$2 AND workspace_id=$3 AND client_installation_id=$4`,
			challengeID, principal.UserID, workspaceID, principal.ClientInstallationID); err != nil {
			return fmt.Errorf("consume workspace sync device challenge: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target)
VALUES ($1,'client','workspace.sync_device.register',$2)`, principal.UserID, existingBindingID); err != nil {
			return fmt.Errorf("audit workspace sync device registration: %w", err)
		}
		result, err = scanWorkspaceSyncDeviceBinding(tx.QueryRow(ctx, workspaceSyncDeviceBindingSelect+`
WHERE b.id=$1 AND b.owner_id=$2 AND b.workspace_id=$3 AND b.folder_id=$4`, existingBindingID, principal.UserID, workspaceID, targetFolderID))
		return err
	})
	return result, err
}

func (s *Store) RevokeWorkspaceSyncDevice(ctx context.Context, principal identity.Principal, workspaceID, bindingID string, disableRelay func(context.Context) error) error {
	return s.withWorkspaceSyncOwnerTx(ctx, principal.UserID, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, principal.UserID, true); err != nil {
			return err
		}
		if err := requireApprovedWorkspaceSyncOwner(ctx, tx, principal.UserID); err != nil {
			return err
		}
		if err := requireActivePortalSession(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, true); err != nil {
			return err
		}
		var deviceID, installationID, bindingState, bindingFolderID, bindingFolderKind string
		err := tx.QueryRow(ctx, `SELECT i.device_id,b.client_installation_id,b.state,b.folder_id,f.folder_kind
FROM workspace_sync_device_bindings b
JOIN workspace_sync_device_identities i ON i.id=b.device_identity_id AND i.owner_id=b.owner_id AND i.client_installation_id=b.client_installation_id
JOIN workspace_sync_folders f ON f.workspace_id=b.workspace_id AND f.owner_id=b.owner_id AND f.folder_id=b.folder_id
WHERE b.id=$1 AND b.owner_id=$2 AND b.workspace_id=$3 FOR UPDATE OF b`,
			bindingID, principal.UserID, workspaceID).Scan(&deviceID, &installationID, &bindingState, &bindingFolderID, &bindingFolderKind)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace sync device binding not found")
		}
		if err != nil {
			return fmt.Errorf("find Workspace sync device binding for revocation: %w", err)
		}
		if (bindingState == "active" || bindingState == "pending_node") && disableRelay != nil {
			if err := disableRelay(ctx); err != nil {
				return fmt.Errorf("disable Workspace sync relay before device revocation: %w", err)
			}
		}
		binding, err := tx.Exec(ctx, `UPDATE workspace_sync_device_bindings SET state='revoking'
WHERE id=$1 AND owner_id=$2 AND workspace_id=$3 AND folder_id=$4 AND state IN ('active','pending_node')`, bindingID, principal.UserID, workspaceID, bindingFolderID)
		if err != nil {
			return fmt.Errorf("revoke workspace sync device binding: %w", err)
		}
		if binding.RowsAffected() != 0 {
			if _, err := bumpWorkspaceSyncNodeDesiredRevision(ctx, tx, principal.UserID); err != nil {
				return err
			}
		}
		challenges, err := tx.Exec(ctx, `DELETE FROM workspace_sync_device_challenges
WHERE owner_id=$1 AND workspace_id=$2 AND client_installation_id=$3 AND device_id=$4
  AND (($5='workspace_root' AND folder_id IS NULL) OR folder_id=$6)`,
			principal.UserID, workspaceID, installationID, deviceID, bindingFolderKind, bindingFolderID)
		if err != nil {
			return fmt.Errorf("invalidate pending Workspace sync device challenges: %w", err)
		}
		if binding.RowsAffected() != 0 {
			if err := refreshWorkspaceSyncFolderStates(ctx, tx, principal.UserID, workspaceID); err != nil {
				return fmt.Errorf("update workspace sync folder state after device revocation: %w", err)
			}
		}
		if err := cancelWorkspaceSyncSessions(ctx, tx, principal.UserID, installationID, workspaceID, true); err != nil {
			return err
		}
		if binding.RowsAffected() == 0 && challenges.RowsAffected() == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target)
VALUES ($1,'session','workspace.sync_device.revoke',$2)`, principal.UserID, bindingID); err != nil {
			return fmt.Errorf("audit workspace sync device revocation: %w", err)
		}
		return nil
	}, nil, nil)
}

func (s *Store) ListWorkspaceSyncDeviceBindings(ctx context.Context, principal identity.Principal, workspaceID string) ([]workspace.SyncDeviceBinding, error) {
	items := make([]workspace.SyncDeviceBinding, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireApprovedWorkspaceSyncOwner(ctx, tx, principal.UserID); err != nil {
			return err
		}
		if err := requireActivePortalSession(ctx, tx, principal); err != nil {
			return err
		}
		if err := lockReadyWorkspaceForSyncDevice(ctx, tx, principal.UserID, workspaceID, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, workspaceSyncDeviceBindingSelect+`WHERE b.owner_id=$1 AND b.workspace_id=$2 ORDER BY f.folder_kind,f.project_name,b.created_at,b.id`,
			principal.UserID, workspaceID)
		if err != nil {
			return fmt.Errorf("list workspace sync device bindings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanWorkspaceSyncDeviceBinding(rows)
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read workspace sync device bindings: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) ListWorkspaceSyncNodeBindings(ctx context.Context, ownerID, workspaceID string) ([]workspace.SyncDeviceBinding, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	rows, err := s.pool.Query(ctx, workspaceSyncDeviceBindingSelect+`WHERE b.owner_id=$1 AND b.workspace_id=$2 AND b.state='active' AND f.folder_kind='workspace_root' ORDER BY b.created_at,b.id`, ownerID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace sync node bindings: %w", err)
	}
	defer rows.Close()
	items := make([]workspace.SyncDeviceBinding, 0)
	for rows.Next() {
		item, err := scanWorkspaceSyncDeviceBinding(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read workspace sync node bindings: %w", err)
	}
	return items, nil
}

const workspaceSyncDeviceBindingSelect = `SELECT b.id,b.owner_id,b.workspace_id,b.client_installation_id,
i.device_id,i.certificate_sha256,f.folder_id,f.folder_kind,f.relative_path,b.state,b.created_at,b.revoked_at
FROM workspace_sync_device_bindings b
JOIN workspace_sync_device_identities i ON i.id=b.device_identity_id AND i.owner_id=b.owner_id AND i.client_installation_id=b.client_installation_id
JOIN workspace_sync_folders f ON f.workspace_id=b.workspace_id AND f.owner_id=b.owner_id AND f.folder_id=b.folder_id `

func scanWorkspaceSyncDeviceBinding(row interface{ Scan(...any) error }) (workspace.SyncDeviceBinding, error) {
	var value workspace.SyncDeviceBinding
	var revokedAt pgtype.Timestamptz
	err := row.Scan(&value.ID, &value.OwnerID, &value.WorkspaceID, &value.ClientInstallationID,
		&value.DeviceID, &value.CertificateSHA256, &value.FolderID, &value.FolderKind, &value.RelativePath,
		&value.State, &value.CreatedAt, &revokedAt)
	if err != nil {
		return workspace.SyncDeviceBinding{}, storeError("read workspace sync device binding", "workspace sync device binding", err)
	}
	if revokedAt.Valid {
		at := revokedAt.Time
		value.RevokedAt = &at
	}
	return value, nil
}

func requireApprovedWorkspaceSyncOwner(ctx context.Context, tx pgx.Tx, ownerID string) error {
	var approved bool
	err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1 FOR SHARE`, ownerID).Scan(&approved)
	if err != nil || !approved {
		return identity.ErrInvalidCredential
	}
	return nil
}

func requireWorkspaceSyncClientInstallation(ctx context.Context, tx pgx.Tx, principal identity.Principal, exclusive bool) error {
	var active bool
	lock := "FOR SHARE"
	if exclusive {
		lock = "FOR UPDATE"
	}
	err := tx.QueryRow(ctx, `SELECT platform='desktop' AND revoked_at IS NULL AND expires_at>clock_timestamp()
FROM client_installations WHERE id=$1 AND owner_id=$2 `+lock, principal.ClientInstallationID, principal.UserID).Scan(&active)
	if err != nil || !active {
		return identity.ErrInvalidCredential
	}
	err = tx.QueryRow(ctx, `SELECT kind='client' AND revoked_at IS NULL AND expires_at>clock_timestamp()
FROM tokens WHERE id=$1 AND owner_id=$2 AND client_installation_id=$3 FOR SHARE`,
		principal.TokenID, principal.UserID, principal.ClientInstallationID).Scan(&active)
	if err != nil || !active {
		return identity.ErrInvalidCredential
	}
	return nil
}

func requireActivePortalSession(ctx context.Context, tx pgx.Tx, principal identity.Principal) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT kind='session' AND client_installation_id IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp()
FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`, principal.TokenID, principal.UserID).Scan(&active)
	if err != nil || !active {
		return identity.ErrInvalidCredential
	}
	return nil
}

func lockReadyWorkspaceForSyncDevice(ctx context.Context, tx pgx.Tx, ownerID, workspaceID string, exclusive bool) error {
	lock := "FOR SHARE"
	if exclusive {
		lock = "FOR UPDATE"
	}
	var ready bool
	err := tx.QueryRow(ctx, `SELECT state='ready' AND deleted_at IS NULL FROM workspaces
WHERE id=$1 AND owner_id=$2 `+lock, workspaceID, ownerID).Scan(&ready)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !ready {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if err != nil {
		return fmt.Errorf("lock workspace for sync device: %w", err)
	}
	if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, workspaceID); err != nil {
		return err
	}
	return nil
}

func bumpWorkspaceSyncNodeDesiredRevision(ctx context.Context, tx pgx.Tx, ownerID string) (int64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `INSERT INTO workspace_sync_node_revisions(owner_id,desired_revision,applied_revision)
VALUES ($1,1,0)
ON CONFLICT (owner_id) DO UPDATE SET
  desired_revision=workspace_sync_node_revisions.desired_revision+1,
  updated_at=clock_timestamp()
RETURNING desired_revision`, ownerID).Scan(&revision)
	if err != nil {
		return 0, fmt.Errorf("bump Workspace sync node desired revision: %w", err)
	}
	return revision, nil
}

func refreshWorkspaceSyncFolderStates(ctx context.Context, tx pgx.Tx, ownerID, workspaceID string) error {
	_, err := tx.Exec(ctx, `UPDATE workspace_sync_folders f SET state=CASE
  WHEN EXISTS (SELECT 1 FROM workspace_sync_device_bindings b WHERE b.workspace_id=f.workspace_id AND b.folder_id=f.folder_id AND b.state='revoking') THEN 'revoking'
  WHEN EXISTS (SELECT 1 FROM workspace_sync_device_bindings b WHERE b.workspace_id=f.workspace_id AND b.folder_id=f.folder_id AND b.state='active') THEN 'active'
  WHEN EXISTS (SELECT 1 FROM workspace_sync_device_bindings b WHERE b.workspace_id=f.workspace_id AND b.folder_id=f.folder_id AND b.state='pending_node') THEN 'pending_node'
  ELSE 'revoked' END,
  updated_at=clock_timestamp()
WHERE f.owner_id=$1 AND ($2='' OR f.workspace_id=$2) AND f.state IS DISTINCT FROM CASE
  WHEN EXISTS (SELECT 1 FROM workspace_sync_device_bindings b WHERE b.workspace_id=f.workspace_id AND b.folder_id=f.folder_id AND b.state='revoking') THEN 'revoking'
  WHEN EXISTS (SELECT 1 FROM workspace_sync_device_bindings b WHERE b.workspace_id=f.workspace_id AND b.folder_id=f.folder_id AND b.state='active') THEN 'active'
  WHEN EXISTS (SELECT 1 FROM workspace_sync_device_bindings b WHERE b.workspace_id=f.workspace_id AND b.folder_id=f.folder_id AND b.state='pending_node') THEN 'pending_node'
  ELSE 'revoked' END`, ownerID, workspaceID)
	if err != nil {
		return fmt.Errorf("refresh Workspace sync folder states: %w", err)
	}
	return nil
}

func revokeWorkspaceSyncDevicesForInstallation(ctx context.Context, tx pgx.Tx, ownerID, installationID string) error {
	bindings, err := tx.Exec(ctx, `UPDATE workspace_sync_device_bindings SET state='revoking'
WHERE owner_id=$1 AND client_installation_id=$2 AND state IN ('active','pending_node')`, ownerID, installationID)
	if err != nil {
		return fmt.Errorf("mark Workspace sync bindings revoking for installation: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_sync_device_identities SET revoked_at=COALESCE(revoked_at,clock_timestamp())
WHERE owner_id=$1 AND client_installation_id=$2 AND revoked_at IS NULL`, ownerID, installationID); err != nil {
		return fmt.Errorf("revoke Workspace sync identities for installation: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_sync_device_challenges WHERE owner_id=$1 AND client_installation_id=$2`, ownerID, installationID); err != nil {
		return fmt.Errorf("invalidate Workspace sync challenges for installation: %w", err)
	}
	if err := refreshWorkspaceSyncFolderStates(ctx, tx, ownerID, ""); err != nil {
		return err
	}
	if err := cancelWorkspaceSyncSessions(ctx, tx, ownerID, installationID, "", false); err != nil {
		return err
	}
	if bindings.RowsAffected() != 0 {
		if _, err := bumpWorkspaceSyncNodeDesiredRevision(ctx, tx, ownerID); err != nil {
			return err
		}
	}
	return nil
}

func revokeWorkspaceSyncDevicesForOwner(ctx context.Context, tx pgx.Tx, ownerID string) error {
	bindings, err := tx.Exec(ctx, `UPDATE workspace_sync_device_bindings SET state='revoking'
WHERE owner_id=$1 AND state IN ('active','pending_node')`, ownerID)
	if err != nil {
		return fmt.Errorf("mark Workspace sync bindings revoking for account: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_sync_device_identities SET revoked_at=COALESCE(revoked_at,clock_timestamp())
WHERE owner_id=$1 AND revoked_at IS NULL`, ownerID); err != nil {
		return fmt.Errorf("revoke Workspace sync identities for account: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_sync_device_challenges WHERE owner_id=$1`, ownerID); err != nil {
		return fmt.Errorf("invalidate Workspace sync challenges for account: %w", err)
	}
	if err := refreshWorkspaceSyncFolderStates(ctx, tx, ownerID, ""); err != nil {
		return err
	}
	if err := cancelWorkspaceSyncSessions(ctx, tx, ownerID, "", "", false); err != nil {
		return err
	}
	if bindings.RowsAffected() != 0 {
		if _, err := bumpWorkspaceSyncNodeDesiredRevision(ctx, tx, ownerID); err != nil {
			return err
		}
	}
	return nil
}

func cancelWorkspaceSyncSessions(ctx context.Context, tx pgx.Tx, ownerID, installationID, workspaceID string, preserveOtherBindings bool) error {
	_, err := tx.Exec(ctx, `UPDATE workspace_sync_sessions s
SET state='cancelled',stage='complete',updated_at=clock_timestamp()
WHERE s.owner_id=$1 AND ($2='' OR s.workspace_id=$2)
  AND s.state IN ('planned','transferring','verifying','applying')
  AND EXISTS (
    SELECT 1 FROM tokens t
    WHERE t.owner_id=s.owner_id AND t.kind='device' AND ($3='' OR t.client_installation_id=$3)
      AND s.replica_id='device:' || t.id
  )
  AND (NOT $4::boolean OR NOT EXISTS (
    SELECT 1 FROM workspace_sync_device_bindings b
    JOIN workspace_sync_device_identities i ON i.id=b.device_identity_id AND i.owner_id=b.owner_id AND i.client_installation_id=b.client_installation_id
    WHERE b.owner_id=s.owner_id AND b.workspace_id=s.workspace_id
      AND ($3='' OR b.client_installation_id=$3) AND b.state='active' AND i.revoked_at IS NULL
  ))`, ownerID, workspaceID, installationID, preserveOtherBindings)
	if err != nil {
		return fmt.Errorf("cancel Workspace sync sessions after device revocation: %w", err)
	}
	return nil
}

func (s *Store) ListWorkspaceSyncNodeOwnersToReconcile(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT owner_id FROM workspace_sync_node_revisions
WHERE desired_revision>applied_revision ORDER BY owner_id`)
	if err != nil {
		return nil, fmt.Errorf("list Workspace sync node owners to reconcile: %w", err)
	}
	defer rows.Close()
	owners := make([]string, 0)
	for rows.Next() {
		var ownerID string
		if err := rows.Scan(&ownerID); err != nil {
			return nil, fmt.Errorf("read Workspace sync node owner to reconcile: %w", err)
		}
		owners = append(owners, ownerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Workspace sync node owners to reconcile: %w", err)
	}
	return owners, nil
}

func (s *Store) ListWorkspaceSyncNodeOwners(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT owner_id FROM workspace_sync_node_revisions ORDER BY owner_id`)
	if err != nil {
		return nil, fmt.Errorf("list Workspace sync node owners: %w", err)
	}
	defer rows.Close()
	owners := make([]string, 0)
	for rows.Next() {
		var ownerID string
		if err := rows.Scan(&ownerID); err != nil {
			return nil, fmt.Errorf("read Workspace sync node owner: %w", err)
		}
		owners = append(owners, ownerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Workspace sync node owners: %w", err)
	}
	return owners, nil
}

func (s *Store) ReconcileWorkspaceSyncNode(ctx context.Context, ownerID string, apply func(context.Context, workspace.SyncNodeDesiredState) error) error {
	return s.reconcileWorkspaceSyncNode(ctx, ownerID, apply, nil, nil)
}

func (s *Store) ReconcileWorkspaceSyncNodeWithRelayGate(ctx context.Context, ownerID string,
	apply func(context.Context, workspace.SyncNodeDesiredState) error,
	enableRelay func(context.Context, workspace.SyncNodeDesiredState) error,
	disableRelay func(context.Context) error) error {
	if enableRelay == nil || disableRelay == nil {
		return domain.InvalidArgument("relay_gate", "Workspace sync relay gate callbacks are required")
	}
	return s.reconcileWorkspaceSyncNode(ctx, ownerID, apply, enableRelay, disableRelay)
}

func (s *Store) reconcileWorkspaceSyncNode(ctx context.Context, ownerID string,
	apply func(context.Context, workspace.SyncNodeDesiredState) error,
	enableRelay func(context.Context, workspace.SyncNodeDesiredState) error,
	disableRelay func(context.Context) error) error {
	if !domain.ValidUserID(ownerID) {
		return domain.InvalidArgument("owner_id", "Workspace sync node owner is invalid")
	}
	if apply == nil {
		return domain.InvalidArgument("apply", "Workspace sync node apply callback is required")
	}
	var enableState workspace.SyncNodeDesiredState
	return s.withWorkspaceSyncOwnerTx(ctx, ownerID, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, ownerID, true); err != nil {
			return err
		}
		if err := ensureNoWorkspaceSyncReceiveForOwner(ctx, tx, ownerID); err != nil {
			return err
		}
		if enableRelay != nil {
			if err := disableRelay(ctx); err != nil {
				return fmt.Errorf("close Workspace sync relay gate before applying owner state: %w", err)
			}
		}
		var desiredRevision, appliedRevision int64
		err := tx.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_sync_node_revisions
WHERE owner_id=$1 FOR UPDATE`, ownerID).Scan(&desiredRevision, &appliedRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock Workspace sync node revision: %w", err)
		}

		desired, err := workspaceSyncNodeDesiredState(ctx, tx, ownerID, desiredRevision)
		if err != nil {
			return err
		}
		if err := apply(ctx, desired); err != nil {
			return fmt.Errorf("apply Workspace sync node desired state: %w", err)
		}

		if _, err := tx.Exec(ctx, `UPDATE workspace_sync_device_bindings b SET state='revoked',revoked_at=COALESCE(revoked_at,clock_timestamp())
WHERE b.owner_id=$1 AND (b.state='revoking' OR b.state IN ('pending_node','active') AND EXISTS (
  SELECT 1 FROM workspace_sync_device_identities i WHERE i.id=b.device_identity_id AND i.revoked_at IS NOT NULL
))`, ownerID); err != nil {
			return fmt.Errorf("finalize Workspace sync device revocations: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_sync_device_bindings b SET state='active'
WHERE b.owner_id=$1 AND b.state='pending_node' AND EXISTS (
  SELECT 1 FROM workspace_sync_device_identities i WHERE i.id=b.device_identity_id AND i.revoked_at IS NULL
)
AND EXISTS (
  SELECT 1 FROM workspace_sync_folders f
  WHERE f.workspace_id=b.workspace_id AND f.owner_id=b.owner_id AND f.folder_id=b.folder_id AND f.folder_kind='workspace_root'
)`, ownerID); err != nil {
			return fmt.Errorf("activate Workspace sync device bindings: %w", err)
		}
		if err := refreshWorkspaceSyncFolderStates(ctx, tx, ownerID, ""); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE workspace_sync_node_revisions SET applied_revision=$2,updated_at=clock_timestamp()
WHERE owner_id=$1 AND desired_revision=$2 AND applied_revision=$3`, ownerID, desiredRevision, appliedRevision)
		if err != nil {
			return fmt.Errorf("advance applied Workspace sync node revision: %w", err)
		}
		if command.RowsAffected() != 1 {
			return errors.New("workspace sync node revision changed during reconciliation")
		}
		if enableRelay != nil && len(desired.Folders) > 0 {
			enableState = desired
		}
		return nil
	}, func(afterCommitCtx context.Context) error {
		if enableRelay == nil || len(enableState.Folders) == 0 {
			return nil
		}
		return enableRelay(afterCommitCtx, enableState)
	}, disableRelay)
}

func workspaceSyncNodeDesiredState(ctx context.Context, tx pgx.Tx, ownerID string, revision int64) (workspace.SyncNodeDesiredState, error) {
	desired := workspace.SyncNodeDesiredState{OwnerID: ownerID, Revision: revision, Folders: make([]workspace.SyncNodeFolder, 0)}
	rows, err := tx.Query(ctx, `SELECT f.workspace_id,f.folder_id,i.device_id
FROM workspace_sync_folders f
JOIN workspace_sync_device_bindings b ON b.workspace_id=f.workspace_id AND b.owner_id=f.owner_id AND b.folder_id=f.folder_id
JOIN workspace_sync_device_identities i ON i.id=b.device_identity_id AND i.owner_id=b.owner_id AND i.client_installation_id=b.client_installation_id
WHERE f.owner_id=$1 AND f.folder_kind='workspace_root' AND b.state IN ('pending_node','active') AND i.revoked_at IS NULL
ORDER BY f.workspace_id,i.device_id`, ownerID)
	if err != nil {
		return workspace.SyncNodeDesiredState{}, fmt.Errorf("read Workspace sync node desired state: %w", err)
	}
	defer rows.Close()
	indexByWorkspace := make(map[string]int)
	seenDevices := make(map[string]map[string]struct{})
	for rows.Next() {
		var workspaceID, folderID, deviceID string
		if err := rows.Scan(&workspaceID, &folderID, &deviceID); err != nil {
			return workspace.SyncNodeDesiredState{}, fmt.Errorf("read Workspace sync node desired folder: %w", err)
		}
		index, ok := indexByWorkspace[workspaceID]
		if !ok {
			index = len(desired.Folders)
			indexByWorkspace[workspaceID] = index
			seenDevices[workspaceID] = make(map[string]struct{})
			desired.Folders = append(desired.Folders, workspace.SyncNodeFolder{
				WorkspaceID: workspaceID, FolderID: folderID, DeviceIDs: make([]string, 0),
			})
		}
		if _, ok := seenDevices[workspaceID][deviceID]; !ok {
			seenDevices[workspaceID][deviceID] = struct{}{}
			desired.Folders[index].DeviceIDs = append(desired.Folders[index].DeviceIDs, deviceID)
		}
	}
	if err := rows.Err(); err != nil {
		return workspace.SyncNodeDesiredState{}, fmt.Errorf("read Workspace sync node desired folders: %w", err)
	}
	return desired, nil
}

var _ app.WorkspaceSyncDeviceStore = (*Store)(nil)
