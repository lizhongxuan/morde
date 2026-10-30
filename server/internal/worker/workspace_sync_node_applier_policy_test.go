package worker

import (
	"encoding/json"
	"testing"

	"morde/server/internal/domain/workspace"
)

func TestConfigureSyncthingFolderAppliesMordeSingleFileLimit(t *testing.T) {
	got, err := configureSyncthingFolder(json.RawMessage(`{"id":"old","path":"old","type":"sendreceive","devices":[]}`), workspace.SyncNodeFolder{
		WorkspaceID: "w1234567", FolderID: "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatal(err)
	}
	var configured struct {
		MaxFileBytes int64 `json:"mordeMaxSingleFileBytes"`
	}
	if err := json.Unmarshal(got, &configured); err != nil {
		t.Fatal(err)
	}
	if configured.MaxFileBytes != 314_572_800 || configured.MaxFileBytes != workspace.MaxSyncFileBytes {
		t.Fatalf("Morde single-file limit = %d, want %d", configured.MaxFileBytes, workspace.MaxSyncFileBytes)
	}
}
