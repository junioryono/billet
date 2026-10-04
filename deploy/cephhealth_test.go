package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const cephHealthTemplate = "../ansible_collections/junioryono/billet/roles/host/templates/ceph-health.sh.j2"

// cephHealthScript renders the role's Ceph health script with a private state
// directory, and with ceph and mail as functions: ceph prints the file named by
// HEALTH, and mail appends its subject to the file named by MAILED.
func cephHealthScript(t *testing.T, stateDir string) string {
	t.Helper()

	body, err := os.ReadFile(cephHealthTemplate)
	if err != nil {
		t.Fatal(err)
	}

	script := string(body)
	for from, to := range map[string]string{
		"{{ billet_alert_from | quote }}":  "from@example.com",
		"{{ billet_alert_email | quote }}": "to@example.com",
		"state_dir=/var/lib/billet/health": "state_dir='" + stateDir + "'",
	} {
		if !strings.Contains(script, from) {
			t.Fatalf("the template no longer contains %q", from)
		}

		script = strings.ReplaceAll(script, from, to)
	}

	stubs := `ceph() { cat "$HEALTH"; [ ! -e "$HEALTH.fail" ]; }
mail() { while [ $# -gt 0 ]; do if [ "$1" = -s ]; then printf '%s\n' "$2" >>"$MAILED"; fi; shift; done; cat >/dev/null; }
hostname() { printf 'node-1\n'; }
`
	path := filepath.Join(t.TempDir(), "health.sh")
	if err := os.WriteFile(path, []byte(stubs+script), 0o700); err != nil {
		t.Fatal(err)
	}

	return path
}

const (
	healthOK       = "HEALTH_OK\n"
	healthNearfull = `HEALTH_WARN 2 nearfull osd(s); 3 pool(s) nearfull
[WRN] OSD_NEARFULL: 2 nearfull osd(s)
    osd.0 is near full
[WRN] POOL_NEARFULL: 3 pool(s) nearfull
    pool 'billet-cache' is nearfull
`
	healthNearfullMoved = `HEALTH_WARN 2 nearfull osd(s); 2 pool(s) nearfull
[WRN] OSD_NEARFULL: 2 nearfull osd(s)
    osd.1 is near full
[WRN] POOL_NEARFULL: 2 pool(s) nearfull
    pool 'billet-images' is nearfull
`
	healthBackfillfull = `HEALTH_WARN 2 backfillfull osd(s); 3 pool(s) backfillfull
[WRN] OSD_BACKFILLFULL: 2 backfillfull osd(s)
    osd.0 is backfill full
[WRN] POOL_BACKFILLFULL: 3 pool(s) backfillfull
    pool 'billet-cache' is backfillfull
`
	healthDown = `HEALTH_WARN 1 osds down
[WRN] OSD_DOWN: 1 osds down
    osd.1 is down
`
	healthNameless = "HEALTH_WARN something ceph names no check for\n"
	// stateMarker is the first line of a state this version writes.
	stateMarker   = "# billet ceph health checks seen since the last HEALTH_OK\n"
	healthGarbage = "monclient: hunting for new mon\n"
	healthEmpty   = ""
)

// ONE EMAIL PER PROBLEM, AND ONLY FOR A PROBLEM. On 2026-10-03 one nearfull
// episode sent three emails, because the script compared ceph's detail text:
// nearfull, then backfillfull, then nearfull again on the way down. A check is
// now mailed once, when it first appears since health was last OK; its numbers
// moving, a milder check returning, and recovery send nothing.
func TestTheCephHealthAlertMailsEachNewProblemOnce(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("the script uses GNU mv -T, as the hosts it is installed on do")
	}

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	healthFile := filepath.Join(dir, "health")
	mailed := filepath.Join(dir, "mailed")
	script := cephHealthScript(t, stateDir)

	mails := func() []string {
		body, err := os.ReadFile(mailed)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}

		return strings.Split(strings.TrimSpace(string(body)), "\n")
	}

	steps := []struct {
		health string
		fail   bool
		want   int
		why    string
	}{
		{healthOK, false, 0, "healthy sends nothing"},
		{healthNearfull, false, 1, "nearfull appears"},
		{healthNearfull, false, 1, "the same nearfull an hour later"},
		{healthNearfullMoved, false, 1, "nearfull with other numbers and other pools"},
		{healthBackfillfull, false, 2, "backfillfull, a check not seen this episode"},
		{healthNearfull, false, 2, "back down to nearfull, already seen"},
		{healthDown, false, 3, "an OSD down, a new problem"},
		{healthOK, false, 3, "recovery sends nothing"},
		{healthNearfull, false, 4, "nearfull again after recovery is a new episode"},
		{healthOK, false, 4, "recovered"},
		{healthNameless, false, 5, "a warning that names no check is mailed under its status"},
		{healthNameless, false, 5, "and only once"},
		{healthOK, false, 5, "recovered"},
		{healthGarbage, true, 6, "ceph failing is mailed as unreadable"},
		{healthGarbage, true, 6, "and failing again is not mailed again"},
		{healthEmpty, false, 6, "an empty answer is the same unreadable"},
		{healthGarbage, false, 6, "and so is one that is not a health report"},
	}

	for i, step := range steps {
		if err := os.WriteFile(healthFile, []byte(step.health), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Remove(healthFile + ".fail"); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if step.fail {
			if err := os.WriteFile(healthFile+".fail", nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		cmd := exec.CommandContext(t.Context(), "/bin/sh", script)
		cmd.Env = append(os.Environ(), "HEALTH="+healthFile, "MAILED="+mailed)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("step %d (%s): %v\n%s", i, step.why, err, out)
		}

		if got := len(mails()); got != step.want {
			t.Fatalf("step %d (%s): %d email(s) in all, want %d: %q", i, step.why, got, step.want, mails())
		}
	}

	subjects := mails()
	if !strings.Contains(subjects[0], "OSD_NEARFULL") || !strings.Contains(subjects[1], "OSD_BACKFILLFULL") ||
		strings.Contains(subjects[1], "NEARFULL ") || !strings.Contains(subjects[2], "OSD_DOWN") ||
		!strings.Contains(subjects[4], "HEALTH_WARN") || !strings.Contains(subjects[5], "CEPH_UNREADABLE") {
		t.Errorf("subjects %q do not name the checks that were new", subjects)
	}
}

// A STATE WRITTEN BY THE OLD SCRIPT HOLDS THE DETAIL TEXT, and what it reported
// counts as seen, so the converge that installs this version sends nothing for a
// problem already mailed, and the state is carried into the new form.
func TestTheCephHealthAlertReadsTheOldStateAsSeen(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("the script uses GNU mv -T, as the hosts it is installed on do")
	}

	for _, tc := range []struct {
		name, old, now, state string
	}{
		{"named checks", healthNearfull, healthNearfullMoved, stateMarker + "OSD_NEARFULL\nPOOL_NEARFULL\n"},
		{"a nameless warning", healthNameless, healthNameless, stateMarker + "HEALTH_WARN\n"},
		{"a failed ceph", "error connecting to the cluster\n", healthGarbage, stateMarker + "CEPH_UNREADABLE\n"},
		// AN UPPERCASE WORD ALONE was an earlier version's detail for a failure, and
		// without the marker it would have read as a name this version wrote.
		{"a lone uppercase diagnostic", "ERROR\n", healthGarbage, stateMarker + "CEPH_UNREADABLE\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			stateDir := filepath.Join(dir, "state")
			stateFile := filepath.Join(stateDir, "ceph.last")
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stateFile, []byte(tc.old), 0o600); err != nil {
				t.Fatal(err)
			}

			healthFile := filepath.Join(dir, "health")
			mailed := filepath.Join(dir, "mailed")
			script := cephHealthScript(t, stateDir)
			run := func(health string) {
				if err := os.WriteFile(healthFile, []byte(health), 0o600); err != nil {
					t.Fatal(err)
				}

				cmd := exec.CommandContext(t.Context(), "/bin/sh", script)
				cmd.Env = append(os.Environ(), "HEALTH="+healthFile, "MAILED="+mailed)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
			}

			run(tc.now)

			if _, err := os.Stat(mailed); !os.IsNotExist(err) {
				body, readErr := os.ReadFile(mailed)
				t.Fatalf("a problem the old script already mailed was mailed again (%v): %q (read: %v)",
					err, body, readErr)
			}

			if state, err := os.ReadFile(stateFile); err != nil || string(state) != tc.state {
				t.Fatalf("the carried state is %q (%v), want %q", state, err, tc.state)
			}

			// AND A NEW PROBLEM STILL MAILS, so silence above was the state's doing.
			run(healthDown)

			if body, err := os.ReadFile(mailed); err != nil || !strings.Contains(string(body), "OSD_DOWN") {
				t.Fatalf("a new problem after the carried state mailed %q (%v)", body, err)
			}
		})
	}
}
