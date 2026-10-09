package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

// The bridge a tap is on is read from its master link, the host's own record,
// and anything else is no answer.
func TestTapBridgeReadsTheMasterLink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	for _, dev := range []string{"bt-17", "bt-18", "billet1"} {
		if err := os.MkdirAll(filepath.Join(root, dev), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Symlink("../billet1", filepath.Join(root, "bt-17", "master")); err != nil {
		t.Fatal(err)
	}

	if got := tapBridge(root, "bt-17"); got != "billet1" {
		t.Errorf("tapBridge(bt-17) = %q, want billet1", got)
	}

	for _, tap := range []string{"bt-18", "bt-99", "", "../billet1", "bt.17"} {
		if got := tapBridge(root, tap); got != "" {
			t.Errorf("tapBridge(%q) = %q, want no answer", tap, got)
		}
	}
}
