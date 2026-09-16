package retirement

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRetiredAccountReaderRequiresTrustedRecord(t *testing.T) {
	for _, scenario := range []string{"recorded", "missing", "malformed", "writable", "symlink", "hardlink", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			root := useRoot(t)
			want := ServiceAccount{User: "billet", UID: 123, Group: "billet", GID: 456}
			if err := WriteServiceAccount(want); err != nil {
				t.Fatal(err)
			}
			path := ServiceAccountPath()
			var err error
			switch scenario {
			case "missing":
				err = os.Remove(path)
			case "malformed":
				err = os.WriteFile(path, []byte(`{"user":"billet"}`), 0o644)
			case "writable":
				err = os.Chmod(path, 0o666)
			case "symlink":
				if err := os.Rename(path, filepath.Join(root, "target")); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink("target", path)
			case "hardlink":
				err = os.Link(path, filepath.Join(root, "other"))
			case "oversized":
				err = os.WriteFile(path, make([]byte, maxAccountBytes+1), 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReadRetiredServiceAccount()
			if scenario == "recorded" {
				if err != nil || got != want {
					t.Fatalf("recorded 0644 account refused: %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("untrusted %s account admitted: %+v", scenario, got)
			}
		})
	}
}
