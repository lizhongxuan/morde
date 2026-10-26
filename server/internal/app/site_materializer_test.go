package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	sitedomain "morde/server/internal/domain/site"
	"morde/server/internal/domain/workspace"
)

const siteMaterializerWorkspaceID = "w1234567"

type siteManifestPages map[string]workspace.ManifestPage

func (p siteManifestPages) GetWorkspaceManifestPage(_ context.Context, workspaceID string, revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	if workspaceID != siteMaterializerWorkspaceID || revision != 4 || prefix != "" || limit != siteManifestPageSize {
		return workspace.ManifestPage{}, fmt.Errorf("unexpected manifest query: %s/%d prefix=%q after=%q limit=%d", workspaceID, revision, prefix, after, limit)
	}
	page, ok := p[after]
	if !ok {
		return workspace.ManifestPage{}, fmt.Errorf("no manifest page after %q", after)
	}
	return page, nil
}

type siteMaterializerBlocks map[string][]byte

func (b siteMaterializerBlocks) PutBlock(string, string, io.Reader) (int64, error) {
	return 0, errors.New("PutBlock must not be called while materializing a release")
}

func (b siteMaterializerBlocks) Open(_ string, digest string) (io.ReadCloser, error) {
	data, ok := b[digest]
	if !ok {
		return nil, fmt.Errorf("missing block %s", digest)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b siteMaterializerBlocks) ReservationBytes(size int64) (int64, error) { return size, nil }

func (b siteMaterializerBlocks) BlockAllocatedBytes(_, digest string) (int64, error) {
	data, ok := b[digest]
	if !ok {
		return 0, fmt.Errorf("missing block %s", digest)
	}
	return int64(len(data)), nil
}

type fakeSiteArtifactStore struct {
	writer     *fakeSiteArtifactWriter
	beginCount int
	beginErr   error
	commitErr  error
	published  bool
	abortErr   error
	deleteErr  error
	deleteCall int
	deleted    []string
}

func (s *fakeSiteArtifactStore) Begin(siteID sitedomain.ID, releaseID sitedomain.ReleaseID, digest string) (SiteArtifactWriter, error) {
	s.beginCount++
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	s.writer = &fakeSiteArtifactWriter{
		siteID: siteID, releaseID: releaseID, manifestSHA256: digest,
		files: make(map[string][]byte), modes: make(map[string]uint32),
		commitErr: s.commitErr, published: s.published, abortErr: s.abortErr,
	}
	return s.writer, nil
}

func (s *fakeSiteArtifactStore) Delete(siteID sitedomain.ID, releaseID sitedomain.ReleaseID, digest string) error {
	s.deleteCall++
	s.deleted = append(s.deleted, string(siteID)+"/"+string(releaseID)+"/"+digest)
	return s.deleteErr
}

type fakeSiteArtifactWriter struct {
	siteID         sitedomain.ID
	releaseID      sitedomain.ReleaseID
	manifestSHA256 string
	files          map[string][]byte
	modes          map[string]uint32
	createErr      error
	commitErr      error
	published      bool
	abortErr       error
	committed      bool
	abortCount     int
}

type fakeSiteArtifactFile struct {
	writer *fakeSiteArtifactWriter
	path   string
	mode   uint32
	data   bytes.Buffer
}

func (w *fakeSiteArtifactWriter) CreateFile(path string, mode uint32) (io.WriteCloser, error) {
	if w.createErr != nil {
		return nil, w.createErr
	}
	return &fakeSiteArtifactFile{writer: w, path: path, mode: mode}, nil
}

func (f *fakeSiteArtifactFile) Write(data []byte) (int, error) { return f.data.Write(data) }

func (f *fakeSiteArtifactFile) Close() error {
	f.writer.files[f.path] = append([]byte(nil), f.data.Bytes()...)
	f.writer.modes[f.path] = f.mode
	return nil
}

func (w *fakeSiteArtifactWriter) Commit() error {
	if w.commitErr != nil {
		if w.published {
			w.committed = true
			return &SiteArtifactPublishedError{Err: w.commitErr}
		}
		return w.commitErr
	}
	w.committed = true
	return nil
}

func TestSiteRevisionMaterializerCleansPublishedArtifactAfterDurabilityError(t *testing.T) {
	file := siteFile("index.html", 0644, "hello")
	manifestSHA256, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	pages := siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{file}}}
	blocks := siteMaterializerBlocks{file.Chunks[0].SHA256: []byte("hello")}

	for _, deleteErr := range []error{nil, errors.New("cleanup failed")} {
		store := &fakeSiteArtifactStore{commitErr: errors.New("directory sync failed"), published: true, deleteErr: deleteErr}
		materializer, err := NewSiteRevisionMaterializer(pages, blocks, store, 64)
		if err != nil {
			t.Fatal(err)
		}
		_, err = materializer.Materialize(context.Background(), siteRelease(manifestSHA256))
		if err == nil || !strings.Contains(err.Error(), "durability confirmation failed") {
			t.Fatalf("Materialize() error = %v", err)
		}
		wantDelete := string(siteRelease(manifestSHA256).SiteID) + "/" + string(siteRelease(manifestSHA256).ID) + "/" + manifestSHA256
		if store.deleteCall != 1 || store.deleted[0] != wantDelete || store.writer.abortCount != 0 {
			t.Fatalf("published artifact cleanup = %v, calls=%d, aborts=%d", store.deleted, store.deleteCall, store.writer.abortCount)
		}
		if deleteErr != nil && !errors.Is(err, deleteErr) {
			t.Fatalf("cleanup failure was not preserved: %v", err)
		}
	}

	store := &fakeSiteArtifactStore{commitErr: errors.New("publish failed before rename")}
	materializer, err := NewSiteRevisionMaterializer(pages, blocks, store, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(context.Background(), siteRelease(manifestSHA256)); err == nil || store.deleteCall != 0 || store.writer.abortCount != 1 {
		t.Fatalf("pre-publish failure cleanup = %v, deletes=%d, aborts=%d", err, store.deleteCall, store.writer.abortCount)
	}
}

func (w *fakeSiteArtifactWriter) Abort() error {
	w.abortCount++
	return w.abortErr
}

func TestSiteRevisionMaterializerStreamsPinnedRevisionAndVerifiesFiles(t *testing.T) {
	files := []workspace.File{
		siteFile("index.html", 0644, "hello"),
		siteFile("static/app.js", 0755, "world!", "wor", "ld!"),
		siteFile("zero.txt", 0644, ""),
	}
	manifestSHA256, err := workspace.ManifestSHA256(files)
	if err != nil {
		t.Fatal(err)
	}
	firstCursor := workspace.EncodeManifestCursor(workspace.ManifestCursor{Revision: 4, After: files[1].Path})
	pages := siteManifestPages{
		"":            {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: files[:2], NextCursor: &firstCursor},
		files[1].Path: {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: files[2:]},
	}
	blocks := siteMaterializerBlocks{}
	for _, data := range [][]byte{[]byte("hello"), []byte("wor"), []byte("ld!")} {
		digest := sha256.Sum256(data)
		blocks[hex.EncodeToString(digest[:])] = data
	}
	store := &fakeSiteArtifactStore{}
	materializer, err := NewSiteRevisionMaterializer(pages, blocks, store, 64)
	if err != nil {
		t.Fatal(err)
	}

	result, err := materializer.Materialize(context.Background(), siteRelease(manifestSHA256))
	if err != nil {
		t.Fatal(err)
	}
	if result.FileCount != 3 || result.TotalBytes != 11 || result.ManifestSHA256 != manifestSHA256 || result.Revision != 4 {
		t.Fatalf("materialization result = %#v", result)
	}
	if !store.writer.committed || store.writer.abortCount != 0 || store.beginCount != 1 {
		t.Fatalf("artifact writer committed=%t aborts=%d begins=%d", store.writer.committed, store.writer.abortCount, store.beginCount)
	}
	if string(store.writer.files["index.html"]) != "hello" || string(store.writer.files["static/app.js"]) != "world!" || len(store.writer.files["zero.txt"]) != 0 {
		t.Fatalf("materialized files = %#v", store.writer.files)
	}
	if store.writer.modes["static/app.js"] != 0755 {
		t.Fatalf("executable file mode = %#o", store.writer.modes["static/app.js"])
	}
}

func TestSiteRevisionMaterializerRejectsCorruptionAndAborts(t *testing.T) {
	file := siteFile("index.html", 0644, "expected")
	manifestSHA256, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	wrongChunk := []byte("corrupt!")
	digest := sha256.Sum256([]byte("expected"))
	blocks := siteMaterializerBlocks{hex.EncodeToString(digest[:]): wrongChunk}
	pages := siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{file}}}
	store := &fakeSiteArtifactStore{}
	materializer, err := NewSiteRevisionMaterializer(pages, blocks, store, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(context.Background(), siteRelease(manifestSHA256)); err == nil {
		t.Fatal("corrupted Workspace block was accepted")
	}
	if store.writer.committed || store.writer.abortCount != 1 {
		t.Fatalf("corrupt release state: committed=%t aborts=%d", store.writer.committed, store.writer.abortCount)
	}

	validBlock := []byte("expected")
	blockDigest := sha256.Sum256(validBlock)
	wrongFile := siteFile("index.html", 0644, string(validBlock))
	wrongFile.SHA256 = strings.Repeat("0", 64)
	wrongManifest, err := workspace.ManifestSHA256([]workspace.File{wrongFile})
	if err != nil {
		t.Fatal(err)
	}
	pages = siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{wrongFile}}}
	store = &fakeSiteArtifactStore{}
	materializer, err = NewSiteRevisionMaterializer(pages, siteMaterializerBlocks{hex.EncodeToString(blockDigest[:]): validBlock}, store, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(context.Background(), siteRelease(wrongManifest)); err == nil || !strings.Contains(err.Error(), "file does not match") {
		t.Fatalf("incorrect whole-file digest error = %v", err)
	}
	if store.writer.committed || store.writer.abortCount != 1 {
		t.Fatalf("bad file digest release state: committed=%t aborts=%d", store.writer.committed, store.writer.abortCount)
	}
}

func TestSiteRevisionMaterializerRejectsFileDirectoryCollisionAndStaleCursor(t *testing.T) {
	first := siteFile("src", 0644, "x")
	child := siteFile("src/main.go", 0644, "y")
	manifestSHA256 := strings.Repeat("a", 64)
	blocks := siteMaterializerBlocks{}
	for _, data := range [][]byte{[]byte("x"), []byte("y")} {
		digest := sha256.Sum256(data)
		blocks[hex.EncodeToString(digest[:])] = data
	}
	pages := siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{first, child}}}
	store := &fakeSiteArtifactStore{}
	materializer, err := NewSiteRevisionMaterializer(pages, blocks, store, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(context.Background(), siteRelease(manifestSHA256)); err == nil || !strings.Contains(err.Error(), "conflicts with") {
		t.Fatalf("file/directory collision error = %v", err)
	}
	if store.writer.committed || store.writer.abortCount != 1 {
		t.Fatalf("collision release state: committed=%t aborts=%d", store.writer.committed, store.writer.abortCount)
	}

	manifestSHA256, err = workspace.ManifestSHA256([]workspace.File{first})
	if err != nil {
		t.Fatal(err)
	}
	staleCursor := workspace.EncodeManifestCursor(workspace.ManifestCursor{Revision: 4, After: "wrong"})
	pages = siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{first}, NextCursor: &staleCursor}}
	store = &fakeSiteArtifactStore{}
	materializer, err = NewSiteRevisionMaterializer(pages, blocks, store, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(context.Background(), siteRelease(manifestSHA256)); err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("stale cursor error = %v", err)
	}
	if store.writer.committed || store.writer.abortCount != 1 {
		t.Fatalf("cursor release state: committed=%t aborts=%d", store.writer.committed, store.writer.abortCount)
	}
}

func TestSiteRevisionMaterializerRejectsLimitAndReportsAbortFailure(t *testing.T) {
	file := siteFile("index.html", 0644, "too large")
	manifestSHA256, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	pages := siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{file}}}
	store := &fakeSiteArtifactStore{}
	materializer, err := NewSiteRevisionMaterializer(pages, siteMaterializerBlocks{}, store, 1)
	if err != nil {
		t.Fatal(err)
	}
	store.writer = nil
	if _, err := materializer.Materialize(context.Background(), siteRelease(manifestSHA256)); err == nil || store.writer.abortCount != 1 {
		t.Fatalf("oversized release err=%v writer=%#v", err, store.writer)
	}
	store.abortErr = errors.New("disk cleanup failed")
	if _, err := materializer.Materialize(context.Background(), siteRelease(manifestSHA256)); err == nil || !strings.Contains(err.Error(), "discard incomplete site artifact") {
		t.Fatalf("abort failure was not reported: %v", err)
	}
}

func TestSiteRevisionMaterializerDoesNotStartForCancelledOrNonCandidateRelease(t *testing.T) {
	store := &fakeSiteArtifactStore{}
	materializer, err := NewSiteRevisionMaterializer(siteManifestPages{}, siteMaterializerBlocks{}, store, 64)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := materializer.Materialize(ctx, siteRelease(strings.Repeat("a", 64))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled materialization error = %v", err)
	}
	completed := siteRelease(strings.Repeat("a", 64))
	completed.State = sitedomain.ReleaseHealthy
	if _, err := materializer.Materialize(context.Background(), completed); err == nil {
		t.Fatal("healthy release was accepted for materialization")
	}
	if store.beginCount != 0 {
		t.Fatalf("artifact staging began %d times for rejected requests", store.beginCount)
	}
}

func siteRelease(manifestSHA256 string) sitedomain.Release {
	return sitedomain.Release{
		ID: "rel_0123456789abcdef", SiteID: "site_0123456789abcdef", WorkspaceID: siteMaterializerWorkspaceID,
		WorkspaceRevision: 4, ManifestSHA256: manifestSHA256, ServicePort: 3000, State: sitedomain.ReleaseCandidate,
	}
}

func siteFile(path string, mode uint32, data string, chunks ...string) workspace.File {
	fileDigest := sha256.Sum256([]byte(data))
	file := workspace.File{Path: path, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(fileDigest[:]), Mode: mode, Chunks: []workspace.Chunk{}}
	if data == "" {
		return file
	}
	if len(chunks) == 0 {
		chunks = []string{data}
	}
	for _, value := range chunks {
		chunkDigest := sha256.Sum256([]byte(value))
		file.Chunks = append(file.Chunks, workspace.Chunk{SHA256: hex.EncodeToString(chunkDigest[:]), SizeBytes: int64(len(value))})
	}
	return file
}
