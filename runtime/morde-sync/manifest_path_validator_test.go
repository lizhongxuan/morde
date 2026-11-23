package mordesync

import "testing"

func TestManifestPathValidatorDeletedDirectoryTree(t *testing.T) {
	var deleted ManifestPathValidator
	if err := deleted.Add("Old/deep/file.txt"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"Old", "Old/deep"} {
		if err := deleted.AddDirectory(path); err != nil {
			t.Fatalf("record deleted parent directory %q: %v", path, err)
		}
	}
	if err := deleted.AddDirectory("old/deep"); err == nil {
		t.Fatal("accepted a case-folded directory collision")
	}
	if err := deleted.AddDirectory("Old/deep/file.txt"); err == nil {
		t.Fatal("accepted a deleted file as a directory")
	}
	if err := deleted.AddEmptyDirectory("Old"); err == nil {
		t.Fatal("an empty manifest directory must still reject descendants")
	}
}
