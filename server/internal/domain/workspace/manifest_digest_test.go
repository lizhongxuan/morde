package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestManifestSHA256UsesStableCanonicalContent(t *testing.T) {
	emptyDigest, err := ManifestSHA256(nil)
	if err != nil || emptyDigest != "04494e25afb7b2208ed0b53183188a9779b3c2658673cf4d389fdbca92c32549" {
		t.Fatalf("empty v1 manifest digest = %q, %v", emptyDigest, err)
	}
	contentDigest := sha256.Sum256([]byte("x"))
	file := File{
		Path: "src/a.txt", SizeBytes: 1, SHA256: hex.EncodeToString(contentDigest[:]), Mode: 0644,
		Chunks: []Chunk{{SHA256: hex.EncodeToString(contentDigest[:]), SizeBytes: 1}},
	}
	emptySHA256 := sha256.Sum256(nil)
	other := File{Path: "README.md", SizeBytes: 0, SHA256: hex.EncodeToString(emptySHA256[:]), Mode: 0644, Chunks: []Chunk{}}
	readmeDigest, err := ManifestSHA256([]File{other})
	if err != nil || readmeDigest != "120e3d4d424b7d017c9d20da3182ed86aa6f4a49650eb8f0e50db31fcd2df00b" {
		t.Fatalf("README manifest digest = %q, %v", readmeDigest, err)
	}

	first, err := ManifestSHA256([]File{file, other})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ManifestSHA256([]File{other, file})
	if err != nil || first != second {
		t.Fatalf("manifest digest depends on input order: %q, %q, %v", first, second, err)
	}

	file.Mode = 0600
	changed, err := ManifestSHA256([]File{file, other})
	if err != nil || changed == first {
		t.Fatalf("manifest digest did not change with file metadata: %q, %v", changed, err)
	}
}

func TestManifestSHA256RejectsInvalidAndDuplicateEntries(t *testing.T) {
	emptySHA256 := sha256.Sum256(nil)
	file := File{SizeBytes: 0, SHA256: hex.EncodeToString(emptySHA256[:]), Mode: 0644, Chunks: []Chunk{}}
	if _, err := ManifestSHA256([]File{
		{Path: "B.txt", SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Chunks: file.Chunks},
		{Path: "a.txt", SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Chunks: file.Chunks},
		{Path: "b.txt", SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Chunks: file.Chunks},
	}); err == nil {
		t.Fatal("accepted case-folded duplicate paths that sort non-adjacently")
	}
	file.Path = "../escape"
	if _, err := ManifestSHA256([]File{file}); err == nil {
		t.Fatal("accepted invalid manifest path")
	}
}

func TestManifestSHA256RejectsCaseFoldedDirectoryCollisions(t *testing.T) {
	emptyDigest := sha256.Sum256(nil)
	first := File{Path: "Workspace/a.txt", SizeBytes: 0, SHA256: hex.EncodeToString(emptyDigest[:]), Mode: 0644, Chunks: []Chunk{}}
	second := first
	second.Path = "workspace/b.txt"
	if _, err := ManifestSHA256([]File{first, second}); err == nil {
		t.Fatal("accepted directory names that collide on case-insensitive filesystems")
	}
}

func TestManifestSHA256StreamMatchesSliceAndRejectsInvalidStreams(t *testing.T) {
	emptySHA256 := sha256.Sum256(nil)
	other := File{Path: "README.md", SHA256: hex.EncodeToString(emptySHA256[:]), Mode: 0644, Chunks: []Chunk{}}
	contentSHA256 := sha256.Sum256([]byte("x"))
	file := File{
		Path: "src/a.txt", SizeBytes: 1, SHA256: hex.EncodeToString(contentSHA256[:]), Mode: 0644,
		Chunks: []Chunk{{SHA256: hex.EncodeToString(contentSHA256[:]), SizeBytes: 1}},
	}
	want, err := ManifestSHA256([]File{file, other})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ManifestSHA256Stream(func(yield func(File) error) error {
		for _, value := range []File{other, file} {
			if err := yield(value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || got != want {
		t.Fatalf("streamed manifest digest = %q, %v; want %q", got, err, want)
	}
	if _, err := ManifestSHA256Stream(func(yield func(File) error) error {
		if err := yield(file); err != nil {
			return err
		}
		return yield(other)
	}); err == nil {
		t.Fatal("accepted files yielded out of path order")
	}
	if _, err := ManifestSHA256Stream(func(yield func(File) error) error {
		_ = yield(File{Path: "../escape"})
		return nil
	}); err == nil {
		t.Fatal("accepted a stream that ignored a validation error")
	}
	readErr := errors.New("manifest source failed")
	if _, err := ManifestSHA256Stream(func(func(File) error) error { return readErr }); !errors.Is(err, readErr) {
		t.Fatalf("stream read error = %v, want %v", err, readErr)
	}
}

func TestManifestSHA256RejectsOversizedCanonicalManifest(t *testing.T) {
	emptySHA256 := sha256.Sum256(nil)
	base := strings.Repeat(strings.Repeat("a", 240)+"/", 16)
	files := make([]File, 4300)
	for i := range files {
		files[i] = File{
			Path: base + fmt.Sprintf("f%05d", i), SHA256: hex.EncodeToString(emptySHA256[:]), Mode: 0644, Chunks: []Chunk{},
		}
	}
	if _, err := ManifestSHA256(files); err == nil {
		t.Fatal("accepted a canonical manifest larger than the byte limit")
	}
}
