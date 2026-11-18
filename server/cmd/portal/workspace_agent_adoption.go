package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
	"morde/server/internal/storage/workspacefs"
	"morde/server/internal/store/postgres"
)

type agentWorkspaceAdoptionOptions struct {
	ownerID          string
	workspaceID      string
	configPath       string
	expectedRevision int64
	manifestSHA256   string
	apply            bool
	serviceStopped   bool
	sourceAudited    bool
}

func parseAgentWorkspaceAdoptionFlags(args []string, stderr io.Writer) (agentWorkspaceAdoptionOptions, error) {
	flags := flag.NewFlagSet("workspace adopt-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	ownerID := flags.String("owner", "", "owner ID for the selected source Workspace")
	workspaceID := flags.String("workspace", "", "explicit source Workspace ID")
	configPath := flags.String("config", defaultConfigPath, "path to portal.yaml")
	expectedRevision := flags.Int64("expected-revision", -1, "revision to adopt; required with --apply")
	manifestSHA256 := flags.String("manifest-sha256", "", "verified manifest tree SHA-256; required with --apply")
	apply := flags.Bool("apply", false, "switch the stable Agent Workspace mapping after preflight")
	serviceStopped := flags.Bool("service-stopped", false, "confirm Portal and all owner Hosts are stopped")
	sourceAudited := flags.Bool("source-audited", false, "confirm the exact Host/volume source was inventoried and any legacy import completed")
	if err := flags.Parse(args); err != nil {
		return agentWorkspaceAdoptionOptions{}, err
	}
	if flags.NArg() != 0 {
		return agentWorkspaceAdoptionOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if !domain.ValidUserID(*ownerID) {
		return agentWorkspaceAdoptionOptions{}, fmt.Errorf("--owner must be a valid user ID")
	}
	if !domain.ValidWorkspaceID(*workspaceID) {
		return agentWorkspaceAdoptionOptions{}, fmt.Errorf("--workspace must be a valid workspace ID")
	}
	if *expectedRevision < -1 {
		return agentWorkspaceAdoptionOptions{}, fmt.Errorf("--expected-revision must be non-negative")
	}
	if *manifestSHA256 != "" {
		decoded, err := hex.DecodeString(*manifestSHA256)
		if err != nil || len(decoded) != 32 || strings.ToLower(*manifestSHA256) != *manifestSHA256 {
			return agentWorkspaceAdoptionOptions{}, fmt.Errorf("--manifest-sha256 must be a lowercase SHA-256 digest")
		}
	}
	if *apply {
		if *expectedRevision < 0 || *manifestSHA256 == "" {
			return agentWorkspaceAdoptionOptions{}, fmt.Errorf("--apply requires --expected-revision and --manifest-sha256 from a successful preflight")
		}
		if !*serviceStopped || !*sourceAudited {
			return agentWorkspaceAdoptionOptions{}, fmt.Errorf("--apply requires --service-stopped and --source-audited confirmations")
		}
	} else if *serviceStopped || *sourceAudited {
		return agentWorkspaceAdoptionOptions{}, fmt.Errorf("--service-stopped and --source-audited are only valid with --apply")
	}
	return agentWorkspaceAdoptionOptions{
		ownerID: *ownerID, workspaceID: *workspaceID, configPath: *configPath,
		expectedRevision: *expectedRevision, manifestSHA256: *manifestSHA256,
		apply: *apply, serviceStopped: *serviceStopped, sourceAudited: *sourceAudited,
	}, nil
}

func adoptAgentWorkspace(ctx context.Context, args []string, stderr, stdout io.Writer) (retErr error) {
	options, err := parseAgentWorkspaceAdoptionFlags(args, stderr)
	if err != nil {
		return err
	}
	cfg, err := config.Load(options.configPath)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	currentVersion, latestVersion, err := schemaVersions(ctx, pool)
	if err != nil {
		return err
	}
	if currentVersion != latestVersion {
		return fmt.Errorf("database schema version %d does not match latest migration version %d", currentVersion, latestVersion)
	}
	if err := postgres.CheckSchema(ctx, pool); err != nil {
		return err
	}
	store := postgres.New(pool)
	unlock, err := store.AcquireAgentWorkspaceInitializationLock(ctx, options.ownerID)
	if err != nil {
		return fmt.Errorf("lock owner Agent Workspace mapping for adoption: %w", err)
	}
	defer func() {
		if err := unlock(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release owner Agent Workspace adoption lock: %w", err))
		}
	}()

	current, err := workspacecurrent.Open(cfg.WorkspaceSyncNode.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("open Workspace current directories: %w", err)
	}
	defer func() { _ = current.Close() }()
	blocks, err := workspacefs.OpenExisting(cfg.WorkspaceStorage.RootDir)
	if err != nil {
		return fmt.Errorf("open Workspace block storage: %w", err)
	}
	defer func() { _ = blocks.Close() }()

	source, err := store.GetWorkspace(ctx, options.workspaceID)
	if err != nil {
		return err
	}
	if source.OwnerID != options.ownerID || source.State != workspace.StateReady || source.DeletedAt != nil || source.ReadOnly {
		return domain.NewError(domain.CodeWorkspaceConflict, "selected source Workspace is not an adoptable owner Workspace")
	}
	snapshot, err := inspectAgentWorkspaceAdoptionSource(ctx, store, current, blocks, source)
	if err != nil {
		return err
	}
	if options.expectedRevision >= 0 && options.expectedRevision != source.Revision {
		return fmt.Errorf("selected Workspace revision is %d, expected %d; rerun preflight", source.Revision, options.expectedRevision)
	}
	if options.manifestSHA256 != "" && options.manifestSHA256 != snapshot.manifestSHA256 {
		return fmt.Errorf("selected Workspace manifest changed; rerun preflight")
	}
	if existing, _, err := store.GetAgentWorkspace(ctx, options.ownerID); err == nil && existing.ID != source.ID {
		if existing.Revision != 0 || existing.UsedBytes != 0 {
			return domain.NewError(domain.CodeWorkspaceConflict, "current Agent Workspace contains data and cannot be replaced")
		}
		currentSnapshot, err := inspectAgentWorkspaceAdoptionSource(ctx, store, current, blocks, existing)
		if err != nil {
			return fmt.Errorf("verify current empty Agent Workspace before adoption: %w", err)
		}
		if len(currentSnapshot.files) != 0 || len(currentSnapshot.emptyDirectories) != 0 {
			return domain.NewError(domain.CodeWorkspaceConflict, "current Agent Workspace is not empty and cannot be replaced")
		}
	} else if err != nil && domain.CodeOf(err) != domain.CodeNotFound {
		return err
	}
	volume, err := store.GetWorkspaceVolumeRegistry(ctx, source.ID)
	if err != nil {
		return fmt.Errorf("read source Workspace volume inventory: %w", err)
	}
	var legacyVolumeName, importState string
	if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT volume_name FROM workspace_legacy_volumes WHERE workspace_id=$1), ''),
COALESCE((SELECT import_state FROM workspace_legacy_volumes WHERE workspace_id=$1), '')`, source.ID).Scan(&legacyVolumeName, &importState); err != nil {
		return fmt.Errorf("check registered legacy Host volume: %w", err)
	}
	if volume.State != app.WorkspaceVolumeLegacy || legacyVolumeName == "" ||
		(importState != "imported" && importState != "verified" && importState != "not_required") {
		return domain.NewError(domain.CodeWorkspaceConflict, "source Host volume is missing, pending, or not verified; import and verify the exact legacy volume before adoption")
	}
	if source.UsedBytes != snapshot.totalBytes {
		return domain.NewError(domain.CodeWorkspaceConflict, "source Workspace manifest byte count differs from stored usage")
	}
	bindings, err := store.GetWorkspaceBindingsByWorkspace(ctx, source.ID)
	if err != nil {
		return fmt.Errorf("read source Workspace Host binding history: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "Agent Workspace adoption preflight: owner=%s source_workspace=%s revision=%d files=%d bytes=%d manifest_sha256=%s volume_state=%s legacy_volume=%s import_state=%s legacy_host_id=%s bindings=%d\n",
		options.ownerID, source.ID, source.Revision, len(snapshot.files), snapshot.totalBytes, snapshot.manifestSHA256,
		volume.State, legacyVolumeName, importState, volume.LegacyHostID, len(bindings)); err != nil {
		return fmt.Errorf("write Agent Workspace adoption preflight: %w", err)
	}
	for _, binding := range bindings {
		var hostState string
		if err := pool.QueryRow(ctx, `SELECT state FROM hosts WHERE id=$1 AND owner_id=$2`, binding.HostID, options.ownerID).Scan(&hostState); err != nil {
			return fmt.Errorf("read source Host state: %w", err)
		}
		if _, err := fmt.Fprintf(stdout, "  host_binding: host=%s host_state=%s binding_state=%s sync_state=%s desired_revision=%d applied_revision=%d conflicts=%d\n",
			binding.HostID, hostState, binding.State, binding.SyncState, binding.DesiredRevision, binding.AppliedRevision, len(binding.SyncConflicts)); err != nil {
			return fmt.Errorf("write Agent Workspace Host binding inventory: %w", err)
		}
	}
	if !options.apply {
		_, err := fmt.Fprintln(stdout, "preflight complete; no mapping changed")
		return err
	}
	if !options.serviceStopped || !options.sourceAudited {
		return fmt.Errorf("offline and source-audit confirmations are required for adoption")
	}
	if err := store.AdoptExistingWorkspaceAsAgentWorkspace(ctx, options.ownerID, source.ID, source.Revision, snapshot.totalBytes); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Agent Workspace adopted: owner=%s workspace=%s revision=%d manifest_sha256=%s\n",
		options.ownerID, source.ID, source.Revision, snapshot.manifestSHA256)
	return err
}

type agentWorkspaceAdoptionSnapshot struct {
	files            []workspace.File
	emptyDirectories []string
	directories      []workspace.Directory
	totalBytes       int64
	manifestSHA256   string
}

func inspectAgentWorkspaceAdoptionSource(ctx context.Context, store *postgres.Store, current *workspacecurrent.Store, blocks *workspacefs.Store,
	value workspace.Workspace) (agentWorkspaceAdoptionSnapshot, error) {
	snapshot := agentWorkspaceAdoptionSnapshot{files: make([]workspace.File, 0), emptyDirectories: make([]string, 0), directories: make([]workspace.Directory, 0)}
	var after string
	for {
		page, err := store.GetWorkspaceManifestPage(ctx, value.ID, value.Revision, "", after, 1000)
		if err != nil {
			return agentWorkspaceAdoptionSnapshot{}, fmt.Errorf("read source Workspace manifest: %w", err)
		}
		if page.WorkspaceID != value.ID || page.Revision != value.Revision {
			return agentWorkspaceAdoptionSnapshot{}, errors.New("source Workspace manifest changed during adoption preflight")
		}
		snapshot.files = append(snapshot.files, page.Files...)
		snapshot.emptyDirectories = append(snapshot.emptyDirectories, page.EmptyDirectories...)
		snapshot.directories = append(snapshot.directories, page.Directories...)
		if len(snapshot.files)+len(snapshot.emptyDirectories)+len(snapshot.directories) > workspace.MaxSyncFiles {
			return agentWorkspaceAdoptionSnapshot{}, errors.New("source Workspace manifest exceeds the supported file count")
		}
		if page.NextCursor == nil {
			break
		}
		if *page.NextCursor <= after {
			return agentWorkspaceAdoptionSnapshot{}, errors.New("source Workspace manifest cursor did not advance")
		}
		after = *page.NextCursor
	}
	for _, file := range snapshot.files {
		if file.SizeBytes < 0 || file.SizeBytes > math.MaxInt64-snapshot.totalBytes {
			return agentWorkspaceAdoptionSnapshot{}, errors.New("source Workspace byte count overflow")
		}
		snapshot.totalBytes += file.SizeBytes
		for _, chunk := range file.Chunks {
			if err := blocks.VerifyBlock(value.ID, chunk.SHA256, chunk.SizeBytes); err != nil {
				return agentWorkspaceAdoptionSnapshot{}, fmt.Errorf("verify source Workspace block %s: %w", chunk.SHA256, err)
			}
		}
	}
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(snapshot.files, snapshot.emptyDirectories, snapshot.directories)
	if err != nil {
		return agentWorkspaceAdoptionSnapshot{}, fmt.Errorf("hash source Workspace manifest: %w", err)
	}
	snapshot.manifestSHA256 = manifestSHA
	matches, err := current.MatchesManifest(ctx, value.OwnerID, value.ID, snapshot.files, snapshot.emptyDirectories, snapshot.directories)
	if err != nil {
		return agentWorkspaceAdoptionSnapshot{}, fmt.Errorf("verify source Workspace directory: %w", err)
	}
	if !matches {
		return agentWorkspaceAdoptionSnapshot{}, errors.New("source Workspace directory differs from its selected manifest revision")
	}
	return snapshot, nil
}
