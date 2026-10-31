package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"morde/server/internal/domain"
	sitedomain "morde/server/internal/domain/site"
	"morde/server/internal/domain/workspace"
)

const siteManifestPageSize int32 = 1000

// SiteRevisionManifestReader reads pages from one pinned Workspace revision.
type SiteRevisionManifestReader interface {
	GetWorkspaceManifestPage(context.Context, string, int64, string, string, int32) (workspace.ManifestPage, error)
}

// SiteArtifactStore stages one immutable release. CreateFile must create only
// regular files below private staging. Closing a file must fsync it and remove
// write bits while preserving execute bits. Commit must fsync directories and
// atomically publish the tree; Abort must discard unpublished staging.
type SiteArtifactStore interface {
	Begin(siteID sitedomain.ID, releaseID sitedomain.ReleaseID, manifestSHA256 string) (SiteArtifactWriter, error)
	Delete(siteID sitedomain.ID, releaseID sitedomain.ReleaseID, manifestSHA256 string) error
}

// SiteArtifactWriter writes files under one private staged release.
type SiteArtifactWriter interface {
	CreateFile(path string, mode uint32) (io.WriteCloser, error)
	Commit() error
	Abort() error
}

// SiteArtifactPublishedError reports a post-publish durability error after the staging lease is released.
type SiteArtifactPublishedError struct {
	Err error
}

func (e *SiteArtifactPublishedError) Error() string {
	return fmt.Sprintf("site artifact was published but durability confirmation failed: %v", e.Err)
}

func (e *SiteArtifactPublishedError) Unwrap() error { return e.Err }

// SiteRevisionMaterializer verifies and reconstructs immutable Workspace revisions.
type SiteRevisionMaterializer struct {
	manifest  SiteRevisionManifestReader
	blocks    WorkspaceBlockReader
	artifacts SiteArtifactStore
	maxBytes  int64
}

// SiteMaterialization summarizes the immutable source used for a release.
type SiteMaterialization struct {
	SiteID         sitedomain.ID
	ReleaseID      sitedomain.ReleaseID
	WorkspaceID    string
	Revision       int64
	ManifestSHA256 string
	FileCount      int64
	TotalBytes     int64
}

// NewSiteRevisionMaterializer validates the bounded materialization dependencies.
func NewSiteRevisionMaterializer(manifest SiteRevisionManifestReader, blocks WorkspaceBlockReader, artifacts SiteArtifactStore, maxBytes int64) (*SiteRevisionMaterializer, error) {
	if manifest == nil || blocks == nil || artifacts == nil || maxBytes <= 0 {
		return nil, errors.New("site revision materializer dependencies and positive byte limit are required")
	}
	return &SiteRevisionMaterializer{manifest: manifest, blocks: blocks, artifacts: artifacts, maxBytes: maxBytes}, nil
}

// Materialize verifies the pinned revision before atomically publishing its files.
func (m *SiteRevisionMaterializer) Materialize(ctx context.Context, release sitedomain.Release) (result SiteMaterialization, retErr error) {
	if err := release.Validate(); err != nil {
		return SiteMaterialization{}, err
	}
	if release.State != sitedomain.ReleaseCandidate {
		return SiteMaterialization{}, domain.NewError(domain.CodeConflict, "only a candidate release can be materialized")
	}
	if err := ctx.Err(); err != nil {
		return SiteMaterialization{}, err
	}
	writer, err := m.artifacts.Begin(release.SiteID, release.ID, release.ManifestSHA256)
	if err != nil {
		return SiteMaterialization{}, fmt.Errorf("begin site artifact staging: %w", err)
	}
	if writer == nil {
		return SiteMaterialization{}, errors.New("site artifact store returned no staging writer")
	}
	committed := false
	defer func() {
		if !committed {
			if abortErr := writer.Abort(); abortErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("discard incomplete site artifact: %w", abortErr))
			}
		}
	}()

	var fileCount, totalBytes int64
	manifestSHA256, err := workspace.ManifestSHA256Stream(func(yield func(workspace.File) error) error {
		afterPath := ""
		lastCursor := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			page, err := m.manifest.GetWorkspaceManifestPage(ctx, release.WorkspaceID, release.WorkspaceRevision, "", afterPath, siteManifestPageSize)
			if err != nil {
				return fmt.Errorf("read pinned workspace revision page: %w", err)
			}
			if page.WorkspaceID != release.WorkspaceID || page.Revision != release.WorkspaceRevision ||
				len(page.Files)+len(page.EmptyDirectories) > int(siteManifestPageSize) {
				return errors.New("workspace revision page identity or size is invalid")
			}
			if _, err := workspace.ManifestTreeSHA256(page.Files, page.EmptyDirectories); err != nil {
				return errors.New("workspace revision page paths are invalid")
			}
			pagePaths := make([]string, 0, len(page.Files)+len(page.EmptyDirectories))
			for _, file := range page.Files {
				pagePaths = append(pagePaths, file.Path)
			}
			pagePaths = append(pagePaths, page.EmptyDirectories...)
			sort.Strings(pagePaths)
			if page.NextCursor != nil && len(pagePaths) == 0 {
				return errors.New("workspace revision cursor did not advance")
			}
			pageLastPath := afterPath
			for _, path := range pagePaths {
				if path <= afterPath {
					return errors.New("workspace revision pages are not in strict path order")
				}
				pageLastPath = path
			}
			for _, file := range page.Files {
				if file.SizeBytes > m.maxBytes-totalBytes {
					return domain.NewError(domain.CodeCapacityExceeded, "site release exceeds its byte limit")
				}
				if err := yield(file); err != nil {
					return err
				}
				if err := materializeSiteFile(ctx, m.blocks, release.WorkspaceID, writer, file); err != nil {
					return fmt.Errorf("materialize workspace file %q: %w", file.Path, err)
				}
				fileCount++
				totalBytes += file.SizeBytes
			}
			if page.NextCursor == nil {
				return nil
			}
			cursor, err := workspace.DecodeManifestCursor(*page.NextCursor)
			if err != nil || cursor.Revision != release.WorkspaceRevision || cursor.Prefix != "" || cursor.After != pageLastPath || cursor.After == "" || cursor.After <= lastCursor {
				return errors.New("workspace revision cursor is invalid or did not advance")
			}
			afterPath = pageLastPath
			lastCursor = cursor.After
		}
	})
	if err != nil {
		return SiteMaterialization{}, fmt.Errorf("verify and materialize workspace revision: %w", err)
	}
	if manifestSHA256 != release.ManifestSHA256 {
		return SiteMaterialization{}, domain.NewError(domain.CodeWorkspaceConflict, "site release manifest does not match its pinned Workspace revision")
	}
	if err := ctx.Err(); err != nil {
		return SiteMaterialization{}, err
	}
	if err := writer.Commit(); err != nil {
		commitErr := fmt.Errorf("publish immutable site artifact: %w", err)
		var publishedErr *SiteArtifactPublishedError
		if errors.As(err, &publishedErr) {
			committed = true
			if cleanupErr := m.artifacts.Delete(release.SiteID, release.ID, release.ManifestSHA256); cleanupErr != nil {
				commitErr = errors.Join(commitErr, fmt.Errorf("remove site artifact after failed durability confirmation: %w", cleanupErr))
			}
		}
		return SiteMaterialization{}, commitErr
	}
	committed = true
	return SiteMaterialization{
		SiteID: release.SiteID, ReleaseID: release.ID, WorkspaceID: release.WorkspaceID,
		Revision: release.WorkspaceRevision, ManifestSHA256: manifestSHA256,
		FileCount: fileCount, TotalBytes: totalBytes,
	}, nil
}

func materializeSiteFile(ctx context.Context, blocks WorkspaceBlockReader, workspaceID string, writer SiteArtifactWriter, file workspace.File) error {
	output, err := writer.CreateFile(file.Path, file.Mode)
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("site artifact writer returned no file")
	}
	hasher := sha256.New()
	var total int64
	var writeErr error
	for _, chunk := range file.Chunks {
		if err := ctx.Err(); err != nil {
			writeErr = err
			break
		}
		data, err := readVerifiedBlock(blocks, workspaceID, chunk)
		if err != nil {
			writeErr = err
			break
		}
		if _, err := hasher.Write(data); err != nil {
			writeErr = fmt.Errorf("hash workspace file: %w", err)
			break
		}
		if err := writeAll(output, data); err != nil {
			writeErr = fmt.Errorf("write staged site file: %w", err)
			break
		}
		total += int64(len(data))
	}
	closeErr := output.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return fmt.Errorf("close staged site file: %w", closeErr)
	}
	if total != file.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != file.SHA256 {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace file does not match its manifest")
	}
	return nil
}

func writeAll(output io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := output.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
