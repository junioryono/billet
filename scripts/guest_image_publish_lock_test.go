package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// THE BUILD'S PUBLISH LOCK IS REMOVED WITHOUT BLOCKLISTING ITS LOCKER (#845).
//
// The locker an advisory lock records is the `rbd lock add` that took it, which
// exited at once, so Ceph's default blocklist on a `lock rm` fences nothing and
// costs an OSD map epoch. Both removals the build makes, its own release and the
// break of a holder past STALE_AFTER, are run here with rbd recorded, and each must
// name the option, the lock and the locker.
func TestTheBuildRemovesItsPublishLockWithoutABlocklist(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed here; the build reads the lock's holder with it")
	}

	const image = "pool/.publish-lock"

	staleCookie := "billet-build-deadhost-9-" + strconv.FormatInt(time.Now().Add(-7*time.Hour).Unix(), 10)

	for _, tc := range []struct {
		name, call, holder, locker, cookie string
	}{
		{"its own release", "release_publish_lock", "billet-build-here-1-100", "client.7", "billet-build-here-1-100"},
		{"a holder past the stale bound", "take_publish_lock", staleCookie, "client.9", "billet-build-here-1-100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			log := filepath.Join(dir, "rbd.log")
			adds := filepath.Join(dir, "adds")

			// rbd as the cluster answers: `lock ls` names the holder, and the first
			// `lock add` finds it held (16) while any later one takes the lock.
			rbd := `rbd() {
	printf '%s\n' "$*" >>` + shellQuote(log) + `
	case "$*" in
	*"lock ls"*) printf '[{"id":"%s","locker":"%s","address":"192.0.2.1:0/1"}]' "$HOLDER" "$LOCKER" ;;
	*"lock add"*)
		n=$(cat ` + shellQuote(adds) + ` 2>/dev/null || echo 0)
		echo $((n + 1)) >` + shellQuote(adds) + `
		[ "$n" -gt 0 ] || return 16 ;;
	esac
}`
			script := strings.Join([]string{
				"set -u",
				"CEPH_USER=billet LOCK_IMAGE=" + image + " LOCK_COOKIE=" + tc.cookie + " STALE_AFTER=21600",
				"IMAGE_POOL=pool IMAGE_NAME=ubuntu",
				"HOLDER=" + tc.holder + " LOCKER=" + tc.locker,
				"install_publish_traps() { :; }",
				rbd,
				guestImageFunction(t, "take_publish_lock"),
				guestImageFunction(t, "release_publish_lock"),
				tc.call,
				"echo completed",
			}, "\n")

			output, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			if err != nil || !strings.Contains(string(output), "completed") {
				t.Fatalf("%s did not complete: %v\n%s", tc.call, err, output)
			}

			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}

			want := "--id billet --rbd_blocklist_on_break_lock false lock rm " + image + " " + tc.holder + " " + tc.locker
			removals := 0

			for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
				if !strings.Contains(line, "lock rm") {
					continue
				}
				removals++

				if line != want {
					t.Errorf("the build removed the lock as\n  rbd %s\nwant\n  rbd %s", line, want)
				}
			}

			if removals != 1 {
				t.Errorf("the build removed the lock %d times, want once; rbd ran:\n%s", removals, raw)
			}
		})
	}
}
