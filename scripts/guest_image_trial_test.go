package scripts_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/store/ceph"
)

// trialFixture stands in for what a trial completion asks of the builder: mount,
// umount and findmnt keep the mounts in a file, df answers from a counter the
// completer grows as it installs, timeout runs its command, and chroot runs a
// completer that reports what each component's marker says.
type trialFixture struct {
	dir, fakes, mounts, used, report, script string
}

func newTrialFixture(t *testing.T) trialFixture {
	t.Helper()

	dir := t.TempDir()
	fx := trialFixture{
		dir:    dir,
		fakes:  filepath.Join(dir, "fakes"),
		mounts: filepath.Join(dir, "mounts"),
		used:   filepath.Join(dir, "used"),
		report: filepath.Join(dir, "report"),
	}
	if err := os.MkdirAll(fx.fakes, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{
		// A TARGET ALREADY MOUNTED IS REFUSED, so a copy left mounted under the next
		// one is seen rather than stacked over.
		"mount":   "#!/bin/sh\nfor last; do :; done\ngrep -qxF -- \"$last\" " + fx.mounts + " 2>/dev/null && { echo \"already mounted: $last\" >&2; exit 32; }\necho \"$last\" >>" + fx.mounts + "\n",
		"umount":  "#!/bin/sh\nfor target; do :; done\n[ -e " + dir + "/stuck ] && grep -qxF -- \"$target\" " + dir + "/stuck && exit 1\ngrep -vxF -- \"$target\" " + fx.mounts + " >" + fx.mounts + ".new || true\nmv " + fx.mounts + ".new " + fx.mounts + "\n",
		"findmnt": "#!/bin/sh\ncat " + fx.mounts + " 2>/dev/null || true\n",
		"df":      "#!/bin/sh\necho Used\ncat " + fx.used + "\n",
		"timeout": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
		"chroot":  "#!/bin/sh\nshift 2\nexec " + filepath.Join(dir, "completer") + " \"$@\"\n",
		// THE COMPLETER GROWS THE DISK by the component's size, then reports.
		"completer-body": "",
	} {
		if file == "completer-body" {
			continue
		}
		writeExecutable(t, filepath.Join(fx.fakes, file), body)
	}
	// THE DISK IS REWRITTEN AT ONCE, never truncated and refilled, as a real df
	// never answers with nothing while the sampler reads it.
	writeExecutable(t, filepath.Join(dir, "completer"), "#!/bin/sh\nname=$1\n"+
		"grow=$(cat "+dir+"/grow-$name 2>/dev/null || echo 100000000)\n"+
		"echo $(( $(cat "+fx.used+") + grow )) >"+fx.used+".new && mv "+fx.used+".new "+fx.used+"\n"+
		// HELD LONG ENOUGH FOR THE SAMPLER, as a real install's minutes are.
		"sleep 1\n"+
		"if [ -e "+dir+"/fail-$name ]; then echo \"failed=$name\" >\"$BILLET_COMPLETION_REPORT\"; "+
		"else echo \"installed=$name 1.2.3\" >\"$BILLET_COMPLETION_REPORT\"; fi\n"+
		// THE DISK SHRINKS AGAIN, as a completer cleaning up does: the peak is what counts.
		"echo $(( $(cat "+fx.used+") - grow / 2 )) >"+fx.used+".new && mv "+fx.used+".new "+fx.used+"\n")
	if err := os.WriteFile(fx.used, []byte("1000000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// THE COMPONENT CHECK, stood in for: its own tests are its own.
	scriptDir := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(scriptDir, "check-guest-component.sh"),
		"#!/bin/sh\n[ -e "+dir+"/incomplete-$2 ] && exit 1\nexit 0\n")

	fx.script = "#!/usr/bin/env bash\nset -euo pipefail\n" +
		"SCRIPT_DIR=" + scriptDir + "\nWORK=" + filepath.Join(dir, "work") + "\n" +
		"COMPONENTS='android-sdk codeql '\nTRIAL_DIR=''\nTRIAL_PID=''\n" +
		"TRIAL_TIMEOUT=1m\nTRIAL_POLL=0\nTRIAL_REPORT=" + fx.report + "\nTRIAL_PATH=" + fx.fakes + ":/usr/bin:/bin\n" +
		guestImageFunction(t, "fs_used") + "\n" +
		guestImageFunction(t, "unmount_under") + "\n" +
		guestImageFunction(t, "trial_completion") + "\n" +
		"mkdir -p \"$WORK\"\ntrial_completion " + filepath.Join(dir, "image.ext4") + "\n"

	return fx
}

func (fx trialFixture) run(t *testing.T) (string, error) {
	t.Helper()

	path := filepath.Join(fx.dir, "trial.sh")
	if err := forkSafeWriteFile(path, []byte(fx.script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", path)
	cmd.Env = append(os.Environ(), "PATH="+fx.fakes+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()

	return string(out), err
}

// A TRIAL COMPLETES EACH COMPONENT ON ITS OWN COPY AND MEASURES WHAT IT NEEDED:
// the most the disk grew while the completer ran, a quarter more, in whole MiB,
// and never less than 64 MiB; and it leaves nothing mounted.
func TestATrialCompletionMeasuresEachComponentOnItsOwnCopy(t *testing.T) {
	t.Parallel()

	fx := newTrialFixture(t)
	if err := os.WriteFile(filepath.Join(fx.dir, "grow-android-sdk"), []byte("8000000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.dir, "grow-codeql"), []byte("1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := fx.run(t); err != nil {
		t.Fatalf("the trial failed: %v\n%s", err, out)
	}

	body, err := os.ReadFile(filepath.Join(fx.dir, "work", "components.json"))
	if err != nil {
		t.Fatalf("no components.json: %v", err)
	}
	var components []struct {
		Name  string `json:"name"`
		Space int64  `json:"space"`
	}
	if err := json.Unmarshal(body, &components); err != nil {
		t.Fatal(err)
	}
	// 8e9 * 5/4 = 1e10, rounded up to a MiB; 1000 bytes is under the 64 MiB floor.
	want := []struct {
		Name  string `json:"name"`
		Space int64  `json:"space"`
	}{{"android-sdk", 10000269312}, {"codeql", 67108864}}
	if !slices.Equal(components, want) {
		t.Fatalf("the trial measured %+v, want %+v", components, want)
	}

	if mounts, err := os.ReadFile(fx.mounts); err != nil || len(bytes.TrimSpace(mounts)) != 0 {
		t.Fatalf("the trial left mounts behind (%v): %q", err, mounts)
	}
}

// A COMPONENT THAT IS NOT PROVED REFUSES THE BUILD, and writes no
// components.json: one the completer reports failed, one whose completed copy
// lacks what it should, and a copy that will not unmount.
func TestATrialCompletionRefusesWhatItCannotProve(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		marker, says string
	}{
		"the completer reports it failed": {"fail-codeql", "did not install codeql"},
		"the completed copy lacks it":     {"incomplete-android-sdk", "does not carry what it should"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fx := newTrialFixture(t)
			if err := os.WriteFile(filepath.Join(fx.dir, tc.marker), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := fx.run(t)
			if err == nil || !strings.Contains(out, tc.says) {
				t.Fatalf("answered %v, want a refusal saying %q:\n%s", err, tc.says, out)
			}
			if _, statErr := os.Stat(filepath.Join(fx.dir, "work", "components.json")); !os.IsNotExist(statErr) {
				t.Fatalf("a refused trial wrote components.json (%v)", statErr)
			}
		})
	}

	fx := newTrialFixture(t)
	stuck := filepath.Join(fx.dir, "work", "trial", "merged", "dev")
	if err := os.WriteFile(filepath.Join(fx.dir, "stuck"), []byte(stuck+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := fx.run(t)
	if err == nil || !strings.Contains(out, "something is still mounted under") {
		t.Fatalf("a copy that would not unmount answered %v:\n%s", err, out)
	}
}

// unmount_under TAKES DOWN EVERY MOUNT AT OR UNDER A DIRECTORY, deepest first,
// and only those: a sibling that merely shares the name's start is not under it.
func TestUnmountUnderTakesDownOnlyWhatIsUnderTheDirectory(t *testing.T) {
	t.Parallel()

	fx := newTrialFixture(t)
	mounts := "/w/trial/lower\n/w/trial/merged\n/w/trial/merged/proc\n/w/trial-other\n/elsewhere\n"
	if err := os.WriteFile(fx.mounts, []byte(mounts), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(fx.dir, "calls")
	writeExecutable(t, filepath.Join(fx.fakes, "umount"),
		"#!/bin/sh\nfor target; do :; done\necho \"$target\" >>"+calls+"\ngrep -vxF -- \"$target\" "+fx.mounts+" >"+fx.mounts+".new || true\nmv "+fx.mounts+".new "+fx.mounts+"\n")
	script := "#!/usr/bin/env bash\nset -euo pipefail\n" + guestImageFunction(t, "unmount_under") + "\nunmount_under /w/trial\n"
	path := filepath.Join(fx.dir, "unmount.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", path)
	cmd.Env = append(os.Environ(), "PATH="+fx.fakes+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unmount_under: %v\n%s", err, out)
	}
	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/w/trial/merged/proc\n/w/trial/merged\n/w/trial/lower\n"; string(got) != want {
		t.Fatalf("unmounted %q, want %q, deepest first and nothing outside", got, want)
	}
}

// A MOUNT TABLE THAT CANNOT BE READ IS NOT A DIRECTORY WITH NOTHING MOUNTED IN
// IT: unmount_under refuses, and unmounts nothing it could not see.
func TestUnmountUnderRefusesAMountTableItCannotRead(t *testing.T) {
	t.Parallel()

	fx := newTrialFixture(t)
	calls := filepath.Join(fx.dir, "calls")
	writeExecutable(t, filepath.Join(fx.fakes, "findmnt"), "#!/bin/sh\necho /w/trial/merged\nexit 1\n")
	writeExecutable(t, filepath.Join(fx.fakes, "umount"), "#!/bin/sh\necho \"$*\" >>"+calls+"\n")
	script := "#!/usr/bin/env bash\nset -euo pipefail\n" + guestImageFunction(t, "unmount_under") +
		"\nif unmount_under /w/trial; then echo CLEARED; fi\n"
	path := filepath.Join(fx.dir, "unmount.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", path)
	cmd.Env = append(os.Environ(), "PATH="+fx.fakes+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "CLEARED") || !strings.Contains(string(out), "could not read the mount table") {
		t.Fatalf("an unreadable mount table answered %v:\n%s", err, out)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("unmount_under unmounted from a table it could not read (%v)", err)
	}

	// AND THE PROOF AFTERWARDS: a table read once and then not again proves nothing.
	reads := filepath.Join(fx.dir, "reads")
	writeExecutable(t, filepath.Join(fx.fakes, "findmnt"),
		"#!/bin/sh\nif [ -e "+reads+" ]; then exit 1; fi\n: >"+reads+"\necho /w/trial/merged\n")
	proof, err := runWithFakes(t, path, fx)
	if err != nil || strings.Contains(proof, "CLEARED") || !strings.Contains(proof, "to prove nothing is mounted") {
		t.Fatalf("a mount table unreadable for the proof answered %v:\n%s", err, proof)
	}
}

// runWithFakes runs the script at path with the fixture's stand-ins first on PATH.
func runWithFakes(t *testing.T, path string, fx trialFixture) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "bash", path)
	cmd.Env = append(os.Environ(), "PATH="+fx.fakes+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()

	return string(out), err
}

// THE TRIAL'S OVERLAY COPIES UP ONLY WHAT IS WRITTEN: without metacopy, the
// completer's chmod of /opt copies the whole toolcache up and the trial measures
// that (24 GiB for Firefox, 2026-10-20).
func TestTheTrialsOverlayCopiesUpMetadataAlone(t *testing.T) {
	t.Parallel()

	trial := guestImageFunction(t, "trial_completion")
	if !strings.Contains(trial, `-o "lowerdir=$dir/lower,upperdir=$dir/upper,workdir=$dir/work,metacopy=on" "$dir/merged"`) {
		t.Fatal("the trial's overlay is mounted without metacopy=on")
	}
}

// A GENERATION PUBLISHED STRAIGHT FROM THE BUILD IS RECORDED BEFORE IT EXISTS: a
// crash between the two must leave a record for a generation never made, never a
// generation with no record, which reads as one that left nothing out.
func TestAGenerationPublishedStraightFromTheBuildIsRecordedBeforeItIsMade(t *testing.T) {
	t.Parallel()

	publish := guestImageFunction(t, "publish")
	record := strings.Index(publish, `image-meta set "$IMAGE_NAME" "billet.components.$gen"`)
	snapshot := strings.Index(publish, `snap create "$IMAGE_NAME@$gen"`)
	if record < 0 || snapshot < 0 || record > snapshot {
		t.Fatalf("publish records the components at %d and makes the snapshot at %d; the record must come first",
			record, snapshot)
	}
}

// A GENERATION PUBLISHED STRAIGHT FROM THE BUILD RECORDS WHAT IT LEFT OUT in the
// form ceph.SetComponents writes and Components reads, strict: everything left
// out, sorted, and nothing installed.
func TestTheBuildsComponentsRecordIsTheStoresOwn(t *testing.T) {
	t.Parallel()

	script := "#!/usr/bin/env bash\nset -euo pipefail\nCOMPONENTS='microsoft-edge codeql android-sdk '\n" +
		guestImageFunction(t, "components_record") + "\ncomponents_record\n"
	out, err := exec.CommandContext(t.Context(), "bash", "-c", script).Output()
	if err != nil {
		t.Fatalf("components_record: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	var record ceph.GenerationComponents
	if err := dec.Decode(&record); err != nil {
		t.Fatalf("the store cannot read the build's record %q: %v", out, err)
	}
	if !slices.Equal(record.LeftOut, []string{"android-sdk", "codeql", "microsoft-edge"}) ||
		record.Installed == nil || len(record.Installed) != 0 {
		t.Fatalf("the record is %+v, want everything left out, sorted, and nothing installed", record)
	}
	if want := `{"left_out":["android-sdk","codeql","microsoft-edge"],"installed":{}}`; strings.TrimSpace(string(out)) != want {
		t.Fatalf("the record is %s, want the bytes ceph.SetComponents writes, %s", out, want)
	}
}
