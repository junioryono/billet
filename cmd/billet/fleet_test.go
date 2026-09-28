package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFleetSource builds a billet checkout whose action scripts record, one line
// each, the step, the mode and reach they were given, whether a secret reached
// them, what an earlier step exported, the first PATH entry and RUNNER_TEMP. A
// file named fail-<script> beside them makes that script exit 3.
func fakeFleetSource(t *testing.T) (string, string) {
	t.Helper()

	src := t.TempDir()
	log := filepath.Join(t.TempDir(), "steps.log")
	action := filepath.Join(src, "actions", "converge-fleet")

	for _, dir := range []string{action, filepath.Join(src, "ansible_collections", "junioryono", "billet")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(src, "ansible_collections", "junioryono", "billet", "galaxy.yml"),
		[]byte("version: 0.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	extra := map[string]string{
		"prepare.sh":         `echo "FROM_PREPARE=exported" >>"$GITHUB_ENV"`,
		"install-ansible.sh": `echo "/opt/fake-venv/bin" >>"$GITHUB_PATH"`,
	}

	for _, name := range append(append([]string{}, fleetSteps...), fleetFinally...) {
		body := "#!/usr/bin/env bash\n" +
			`secret=no; [ -n "${BILLET_SSH_PRIVATE_KEY:-}" ] && secret=yes` + "\n" +
			`printf '%s mode=%s reach=%s secret=%s exported=%s path=%s temp=%s holder=%s\n' ` +
			`"` + name + `" "${BILLET_MODE:-}" "${BILLET_REACH:-}" "$secret" "${FROM_PREPARE:-}" ` +
			`"${PATH%%:*}" "${RUNNER_TEMP:-}" "${BILLET_CONVERGE_GUARD_HOLDER:-}" >>` + shellQuote(log) + "\n" +
			extra[name] + "\n" +
			`[ -f "$(dirname "$0")/fail-` + name + `" ] && exit 3` + "\n" +
			"exit 0\n"

		// Run as `bash <file>`, never exec'd, so no fork can make it ETXTBSY.
		if err := os.WriteFile(filepath.Join(action, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return src, log
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readSteps(t *testing.T, log string) []string {
	t.Helper()

	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

func stepNamed(t *testing.T, lines []string, name string) string {
	t.Helper()

	for _, line := range lines {
		if strings.HasPrefix(line, name+" ") {
			return line
		}
	}

	t.Fatalf("%s never ran; steps: %q", name, lines)

	return ""
}

// A LAPTOP CONVERGE RUNS THE ACTION'S OWN STEPS IN THE ACTION'S ORDER, carries
// what one step exports to the next as a runner does, gives the SSH key to the
// converge step alone, and leaves no RUNNER_TEMP behind.
func TestFleetConvergeRunsTheActionsStepsAsARunnerWould(t *testing.T) {
	t.Parallel()

	src, log := fakeFleetSource(t)
	key := filepath.Join(t.TempDir(), "key")

	if err := os.WriteFile(key, []byte("KEY MATERIAL"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runFleetConverge(t.Context(), src, "source "+src, fleetOptions{
		inventory: "fleet/inventory.yml", check: true, sshKeyFile: key, holder: "fleet-test-1",
	})
	if err != nil {
		t.Fatalf("runFleetConverge: %v", err)
	}

	lines := readSteps(t, log)

	var order []string
	for _, line := range lines {
		order = append(order, strings.Fields(line)[0])
	}

	want := "prepare.sh install-ansible.sh converge.sh release-guard.sh cleanup.sh"
	if strings.Join(order, " ") != want {
		t.Fatalf("steps ran as %q, want %q", order, want)
	}

	converge := stepNamed(t, lines, "converge.sh")
	for _, fragment := range []string{"mode=check", "reach=none", "secret=yes", "exported=exported",
		"path=/opt/fake-venv/bin", "holder=fleet-test-1"} {
		if !strings.Contains(converge, fragment) {
			t.Errorf("converge.sh saw %q, missing %q", converge, fragment)
		}
	}

	for _, name := range []string{"prepare.sh", "install-ansible.sh", "release-guard.sh", "cleanup.sh"} {
		if line := stepNamed(t, lines, name); strings.Contains(line, "secret=yes") {
			t.Errorf("%s was handed the SSH key: %q", name, line)
		}
	}

	temp := strings.TrimPrefix(strings.Fields(converge)[6], "temp=")
	if temp == "" {
		t.Fatalf("converge.sh had no RUNNER_TEMP: %q", converge)
	}

	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Errorf("RUNNER_TEMP %s is still there after the converge: %v", temp, err)
	}
}

// A FAILED CONVERGE STILL RELEASES ITS GUARD AND CLEANS UP, and a failed
// preparation runs nothing that converges.
func TestFleetConvergeReleasesTheGuardHoweverItEnds(t *testing.T) {
	t.Parallel()

	for _, failing := range []string{"prepare.sh", "converge.sh"} {
		src, log := fakeFleetSource(t)

		if err := os.WriteFile(filepath.Join(src, "actions", "converge-fleet", "fail-"+failing), nil, 0o644); err != nil {
			t.Fatal(err)
		}

		err := runFleetConverge(t.Context(), src, "source "+src, fleetOptions{inventory: "inv.yml", holder: "fleet-test-2"})
		if err == nil || !strings.Contains(err.Error(), failing) {
			t.Errorf("a failing %s returned %v; want an error naming it", failing, err)
		}

		lines := readSteps(t, log)
		stepNamed(t, lines, "release-guard.sh")
		stepNamed(t, lines, "cleanup.sh")

		if failing == "prepare.sh" {
			for _, line := range lines {
				if strings.HasPrefix(line, "converge.sh ") || strings.HasPrefix(line, "install-ansible.sh ") {
					t.Errorf("a failed preparation still ran %q", line)
				}
			}
		}
	}
}

// A HOLDER THE GUARD WOULD REFUSE IS REFUSED BEFORE ANY STEP RUNS.
func TestFleetConvergeRefusesAHolderTheGuardWouldRefuse(t *testing.T) {
	t.Parallel()

	src, log := fakeFleetSource(t)

	err := runFleetConverge(t.Context(), src, "source "+src, fleetOptions{inventory: "inv.yml", holder: "a/b"})
	if err == nil || !strings.Contains(err.Error(), "slash") {
		t.Fatalf("holder a/b returned %v; want the guard's refusal", err)
	}

	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Errorf("a step ran before the holder was refused: %v", err)
	}
}

// A DEVELOPMENT BUILD MUST NAME A RELEASE, and -ref takes a release only, so no
// moving ref and nothing git would read as an option reaches the clone.
func TestFleetSourceRefusesAnythingButARelease(t *testing.T) {
	t.Parallel()

	if _, _, err := fleetSource(t.Context(), fleetOptions{inventory: "inv.yml"}); err == nil ||
		!strings.Contains(err.Error(), "-ref vX.Y.Z") {
		t.Errorf("a development build with no -ref returned %v; want a refusal naming -ref", err)
	}

	for _, ref := range []string{"main", "v1.2", "--upload-pack=x", "v1.2.3-rc1"} {
		if _, _, err := fleetSource(t.Context(), fleetOptions{inventory: "inv.yml", ref: ref}); err == nil ||
			!strings.Contains(err.Error(), "not a release") {
			t.Errorf("-ref %q returned %v; want not a release", ref, err)
		}
	}
}

// THE RELEASE IS CLONED ONCE AND THEN SERVED FROM THE CACHE, and a directory
// without the marker is fetched again rather than trusted.
func TestFetchFleetSourceClonesTheTagOnceAndReusesIt(t *testing.T) {
	origin, _ := fakeFleetSource(t)

	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"-c", "user.email=t@example.test", "-c", "user.name=t", "add", "-A"},
		{"-c", "user.email=t@example.test", "-c", "user.name=t", "commit", "--quiet", "-m", "fixture"},
		{"tag", "v9.9.9"},
	} {
		if out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", origin}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	saved := fleetRepository
	fleetRepository = origin

	t.Cleanup(func() { fleetRepository = saved })

	root := t.TempDir()

	dir, err := fetchFleetSource(t.Context(), root, "v9.9.9")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if err := checkFleetSource(dir); err != nil {
		t.Fatalf("the fetched checkout: %v", err)
	}

	// The origin disappears: a second fetch must be the cache's.
	fleetRepository = filepath.Join(t.TempDir(), "gone")

	again, err := fetchFleetSource(t.Context(), root, "v9.9.9")
	if err != nil || again != dir {
		t.Fatalf("second fetch = %q, %v; want the cached %q", again, err, dir)
	}

	if err := os.Remove(filepath.Join(dir, fleetSourceMarker)); err != nil {
		t.Fatal(err)
	}

	if _, err := fetchFleetSource(t.Context(), root, "v9.9.9"); err == nil {
		t.Error("a checkout without its marker was trusted instead of fetched again")
	}
}

func TestParseGitHubEnvReadsBothForms(t *testing.T) {
	t.Parallel()

	got, err := parseGitHubEnv("A=1\nB<<EOF\nline one\nline two\nEOF\nA=2\n")
	if err != nil {
		t.Fatal(err)
	}

	if got["A"] != "2" || got["B"] != "line one\nline two" {
		t.Errorf("parsed %q", got)
	}

	for _, bad := range []string{"not an assignment\n", "B<<EOF\nnever closed\n", "1A=x\n"} {
		if _, err := parseGitHubEnv(bad); err == nil {
			t.Errorf("parsed %q without an error", bad)
		}
	}
}
