package scripts_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// THE STICKY DISK'S POST STEP SAYS WHAT BECAME OF ITS WRITE, published or not, so a
// job's log is evidence the acceptance matrix's two-writer readers judge by: a
// publication names its generation, and a discard names the node's reason, a cas loss
// as itself. It is executed here under node against a cache endpoint that answers the
// commit, with the disk's own commands faked.
func TestTheStickyDiskPostStepSaysWhatBecameOfItsWrite(t *testing.T) {
	t.Parallel()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("this test runs the action's post step, which needs node")
	}
	cleanup, err := filepath.Abs("../actions/stickydisk/cleanup.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		answer map[string]any
		says   string
	}{
		"published": {map[string]any{"published": true, "generation": "g-42"},
			"billet published this sticky-disk update as generation g-42\n"},
		"superseded": {map[string]any{"published": false, "reason": "superseded: another job published this key"},
			"::warning::billet discarded this sticky-disk update (superseded: another job published this key); the job result is unchanged\n"},
		"unavailable": {map[string]any{"published": false},
			"::warning::billet discarded this sticky-disk update (cache unavailable); the job result is unchanged\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// HANDED OVER, not shared: the handler runs on the server's goroutine.
			asked := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")
				if err := json.NewEncoder(w).Encode(c.answer); err != nil {
					t.Errorf("answer the commit: %v", err)
				}
			}))
			t.Cleanup(srv.Close)
			bin := t.TempDir()
			for tool, body := range map[string]string{
				"sudo":   `exec "$@"`,
				"sync":   "exit 0",
				"umount": "exit 0",
				"blkid":  `case "$*" in *TYPE*) echo ext4 ;; *UUID*) echo fs-1 ;; esac`,
				"e2fsck": "exit 0",
			} {
				if err := forkSafeWriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), nodeBin, cleanup)
			cmd.Env = []string{
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"STATE_mounted=true", "STATE_path=/mnt/matrix", "STATE_endpoint=" + srv.URL,
				"STATE_token=tok", "STATE_slot=3", "STATE_device=/dev/vdd",
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the post step: %v\n%s", err, out)
			}
			if string(out) != c.says {
				t.Fatalf("the post step said %q, want %q", out, c.says)
			}
			select {
			case got := <-asked:
				if got != "POST /v1/volumes/3/commit Bearer tok" {
					t.Fatalf("the post step asked %q", got)
				}
			default:
				t.Fatal("the post step asked the node nothing")
			}
			if len(asked) != 0 {
				t.Fatalf("the post step asked the node %d more times", len(asked))
			}
		})
	}
}
