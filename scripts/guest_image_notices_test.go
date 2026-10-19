package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE IMAGE REDISTRIBUTES BILLET AND THE CONTAINER HOOK, so the publication gate
// refuses an image that carries either without its notices. The block is run by
// bash against a mounted-image fixture, never pattern-matched.
func TestThePublicationGateRefusesAnImageWithoutItsNotices(t *testing.T) {
	t.Parallel()

	gate := readScriptFile(t, "check-guest-image.sh")
	const begin = "# THE IMAGE REDISTRIBUTES BILLET AND THE CONTAINER HOOK"
	_, after, found := strings.Cut(gate, begin)
	_, after, _ = strings.Cut(after, "\n")
	block, _, closed := strings.Cut(after, "\nfi\n")
	if !found || !closed {
		t.Fatal("check-guest-image.sh no longer has the notices block")
	}
	block += "\nfi\n"

	notices := "opt/billet/THIRD_PARTY_NOTICES"
	license := "usr/local/lib/billet/container-hook/LICENSE"

	for _, tc := range []struct {
		name    string
		files   map[string]string
		verdict string
	}{
		{"both present", map[string]string{notices: "notices\n", license: "MIT License\n"}, "PASS"},
		{"no notices", map[string]string{license: "MIT License\n"}, "FAIL"},
		{"no hook license", map[string]string{notices: "notices\n"}, "FAIL"},
		{"an empty hook license", map[string]string{notices: "notices\n", license: ""}, "FAIL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mnt := t.TempDir()
			for p, body := range tc.files {
				full := filepath.Join(mnt, p)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			script := "pass() { echo PASS; }\nfail() { echo FAIL; }\nMNT=" + mnt + "\n" + block
			out, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("the block failed to run: %v: %s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.verdict {
				t.Fatalf("the gate said %q, want %s", got, tc.verdict)
			}
		})
	}
}

// The build installs both notices from files this tree carries, so the gate above
// is checking for something the build really writes.
func TestTheBuildInstallsTheNoticesTheGateChecks(t *testing.T) {
	t.Parallel()

	build := readScriptFile(t, "build-guest-image.sh")
	for source, dest := range map[string]string{
		"internal/notices/THIRD_PARTY_NOTICES":        `"$rootfs/opt/billet/THIRD_PARTY_NOTICES"`,
		"internal/guestassets/container-hook.LICENSE": `"$rootfs/usr/local/lib/billet/container-hook/LICENSE"`,
	} {
		install := `install -m 0644 "$SCRIPT_DIR/../` + source + `" \` + "\n\t\t" + dest
		if !strings.Contains(build, install) {
			t.Errorf("build-guest-image.sh does not install %s as %s", source, dest)
		}
		if info, err := os.Stat(filepath.Join("..", source)); err != nil || info.Size() == 0 {
			t.Errorf("%s, which the build installs, is missing or empty: %v", source, err)
		}
	}
}
