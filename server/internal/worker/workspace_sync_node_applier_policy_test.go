package worker

import (
	"encoding/json"
	"testing"

	"morde/server/internal/domain/workspace"
)

func TestConfigureSyncthingFolderAppliesMordeSingleFileLimit(t *testing.T) {
	got, err := configureSyncthingFolder(json.RawMessage(`{"id":"old","path":"old","type":"sendreceive","devices":[]}`), workspace.SyncNodeFolder{
		WorkspaceID: "w1234567", FolderID: "morde-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}, workspaceSyncNodeQuotaSocket)
	if err != nil {
		t.Fatal(err)
	}
	var configured struct {
		MaxFileBytes int64  `json:"mordeMaxSingleFileBytes"`
		QuotaSocket  string `json:"mordeQuotaSocket"`
		WorkspaceID  string `json:"mordeWorkspaceId"`
	}
	if err := json.Unmarshal(got, &configured); err != nil {
		t.Fatal(err)
	}
	if configured.MaxFileBytes != workspace.MaxSyncFileBytes || workspace.MaxSyncFileBytes != 314_572_800 {
		t.Fatalf("Morde single-file limit = %d, want %d", configured.MaxFileBytes, workspace.MaxSyncFileBytes)
	}
	if configured.QuotaSocket != workspaceSyncNodeQuotaSocket || configured.WorkspaceID != "w1234567" {
		t.Fatalf("Morde receive hook = socket %q Workspace %q", configured.QuotaSocket, configured.WorkspaceID)
	}
}
