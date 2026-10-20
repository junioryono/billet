package images

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/imagesource"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/store/ceph"
)

func TestACompletionInstallsOnlyWhatTheDeploymentAccepts(t *testing.T) {
	m := &imagesource.Manifest{Components: []imagesource.Component{
		{Name: "google-chrome", Space: 1}, {Name: "android-sdk", Space: 2},
		{Name: "some-future-thing", Space: 3}, {Name: "codeql", Space: 4},
	}}

	plan := planCompletion(m, []config.ImageComponent{
		config.ComponentCodeQL, config.ComponentGoogleChrome, config.ComponentFirefox,
	})

	if want := []string{"android-sdk", "codeql", "google-chrome", "some-future-thing"}; !slices.Equal(plan.LeftOut, want) {
		t.Errorf("LeftOut = %v, want %v", plan.LeftOut, want)
	}
	if want := []string{"google-chrome", "codeql"}; !slices.Equal(plan.names(), want) {
		t.Errorf("Install = %v, want %v, in the manifest's order", plan.names(), want)
	}
}

const reportSecret = "s3cr3t"

func completionReport(lines ...string) string {
	return strings.Join(append([]string{"jit=" + reportSecret, "prepared=0", "unmounted=0", "fsck=0"},
		lines...), "\n")
}

func TestACompletionReportIsAcceptedOnlyWhenEveryComponentInstalledCleanly(t *testing.T) {
	plan := completionPlan{
		LeftOut: []string{"codeql", "google-chrome"},
		Install: []imagesource.Component{{Name: "google-chrome"}, {Name: "codeql"}},
	}

	got, err := checkCompletionReport(completionReport(
		"installed=google-chrome 155.0.8059.84", "installed=codeql 2.26.4"), reportSecret, plan)
	if err != nil {
		t.Fatalf("a clean report was refused: %v", err)
	}
	if got["google-chrome"] != "155.0.8059.84" || got["codeql"] != "2.26.4" || len(got) != 2 {
		t.Fatalf("installed = %v", got)
	}

	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"another run's secret": {strings.Replace(completionReport("installed=google-chrome 1",
			"installed=codeql 1"), "jit="+reportSecret, "jit=other", 1), "did not carry this run's secret"},
		"not prepared": {strings.Replace(completionReport("installed=google-chrome 1",
			"installed=codeql 1"), "prepared=0", "prepared=1", 1), "could not be mounted and prepared"},
		"not unmounted": {strings.Replace(completionReport("installed=google-chrome 1",
			"installed=codeql 1"), "unmounted=0", "unmounted=32", 1), "could not be unmounted"},
		"fsck fixed something": {strings.Replace(completionReport("installed=google-chrome 1",
			"installed=codeql 1"), "fsck=0", "fsck=1", 1), "did not check clean"},
		"fsck skipped": {strings.Replace(completionReport("installed=google-chrome 1",
			"installed=codeql 1"), "fsck=0", "fsck=skipped", 1), "did not check clean"},
		"a step reported twice": {completionReport("fsck=0", "installed=google-chrome 1",
			"installed=codeql 1"), "did not check clean"},
		"one failed":   {completionReport("installed=google-chrome 1", "failed=codeql"), "codeql did not install"},
		"one missing":  {completionReport("installed=codeql 1"), "google-chrome did not install"},
		"both missing": {completionReport(), "codeql, google-chrome did not install"},
		"one not asked for": {completionReport("installed=google-chrome 1", "installed=codeql 1",
			"installed=firefox 1"), "a component this pull did not ask for"},
		"a failure not asked for": {completionReport("installed=google-chrome 1", "installed=codeql 1",
			"failed=firefox"), "a failure of a component this pull did not ask for"},
		"installed twice": {completionReport("installed=google-chrome 1", "installed=google-chrome 2",
			"installed=codeql 1"), "google-chrome installed twice"},
		"no version": {completionReport("installed=google-chrome", "installed=codeql 1"),
			"google-chrome installed with no version"},
		"a version with text in it": {completionReport("installed=google-chrome rm -rf /",
			"installed=codeql 1"), "google-chrome installed with no version"},
		"an unknown field": {completionReport("installed=google-chrome 1", "installed=codeql 1",
			"note=hello"), "a field the contract does not name"},
		"a line that is not a field": {completionReport("installed=google-chrome 1",
			"installed=codeql 1", "hello"), "not key=value"},
		"too long": {completionReport(slices.Repeat([]string{"failed=codeql"}, maxReportLines+1)...),
			"lines, past the"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := checkCompletionReport(tc.body, reportSecret, plan)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
			// NOTHING THE GUEST WROTE IS QUOTED.
			for _, guest := range []string{"rm -rf", "hello", "firefox"} {
				if strings.Contains(err.Error(), guest) {
					t.Fatalf("the refusal quotes the guest's %q: %v", guest, err)
				}
			}
		})
	}
}

// THE GUEST'S HALF OF THE CONTRACT IS EXECUTED, against stand-ins for the commands
// it runs, so what it reports for each step is what each step did.
func TestTheCompletionCommandReportsEachStepAsItHappened(t *testing.T) {
	for name, tc := range map[string]struct {
		env        []string
		wantPosted []string
		wantRan    []string
		notRan     []string
	}{
		"everything succeeds": {
			wantPosted: []string{"jit=" + reportSecret, "prepared=0", "unmounted=0", "fsck=0",
				"installed=google-chrome 155.0.8059.84", "failed=codeql"},
			wantRan: []string{"mount /dev/vdz", "resize2fs /dev/vdz", "mount --bind /proc",
				"mount --bind /run", "chroot", "/opt/billet/complete/run google-chrome codeql",
				"umount -R", "e2fsck -f -y /dev/vdz"},
		},
		"the image does not mount": {
			env:        []string{"FAIL=mount"},
			wantPosted: []string{"prepared=1"},
			notRan:     []string{"chroot", "mount --bind"},
		},
		"the filesystem does not check clean": {
			env:        []string{"FSCK_STATUS=4"},
			wantPosted: []string{"prepared=0", "unmounted=0", "fsck=4"},
		},
		"the completer leaves a mount behind": {
			env:        []string{"UMOUNT_STATUS=32"},
			wantPosted: []string{"prepared=0", "unmounted=32", "fsck=skipped"},
			notRan:     []string{"e2fsck"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			log := filepath.Join(dir, "ran")
			posted := filepath.Join(dir, "posted")
			stub(t, bin, "sudo", `exec "$@"`)
			stub(t, bin, "mount", `echo "mount $*" >>"$LOG"; [ "${FAIL:-}" = mount ] && exit 1; exit 0`)
			stub(t, bin, "umount", `echo "umount $*" >>"$LOG"; exit "${UMOUNT_STATUS:-0}"`)
			stub(t, bin, "resize2fs", `echo "resize2fs $*" >>"$LOG"`)
			stub(t, bin, "e2fsck", `echo "e2fsck $*" >>"$LOG"; exit "${FSCK_STATUS:-0}"`)
			stub(t, bin, "chroot", `echo "chroot $*" >>"$LOG"
printf 'installed=google-chrome 155.0.8059.84\nfailed=codeql\nnoise\n' >"$BILLET_COMPLETION_REPORT"`)
			stub(t, bin, "curl", `cat >"$POSTED"`)

			script := completionScript("127.0.0.1:7719", []string{"google-chrome", "codeql"},
				"/dev/vdz", filepath.Join(dir, "mnt"), filepath.Join(dir, "report"))

			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
			cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "LOG=" + log, "POSTED=" + posted,
				"ACTIONS_RUNNER_INPUT_JITCONFIG=" + reportSecret}, tc.env...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the command failed: %v\n%s", err, out)
			}

			body, err := os.ReadFile(posted)
			if err != nil {
				t.Fatalf("the command posted nothing: %v", err)
			}
			ran, err := os.ReadFile(log)
			if err != nil {
				t.Fatalf("the command ran nothing: %v", err)
			}
			for _, want := range tc.wantPosted {
				if !slices.Contains(strings.Split(strings.TrimSpace(string(body)), "\n"), want) {
					t.Errorf("the report lacks %q:\n%s", want, body)
				}
			}
			if strings.Contains(string(body), "noise") {
				t.Errorf("the report carried a line the contract does not name:\n%s", body)
			}
			for _, want := range tc.wantRan {
				if !strings.Contains(string(ran), want) {
					t.Errorf("the command did not run %q:\n%s", want, ran)
				}
			}
			for _, unwanted := range tc.notRan {
				if strings.Contains(string(ran), unwanted) {
					t.Errorf("the command ran %q:\n%s", unwanted, ran)
				}
			}
		})
	}
}

func stub(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeCompletionStore answers which generation is verified, and records which
// completion roots were removed.
type fakeCompletionStore struct {
	verified ceph.Generation
	found    bool
	removed  []string
}

func (f *fakeCompletionStore) NewestVerifiedForContract(context.Context, string, string,
) (ceph.Generation, bool, error) {
	return f.verified, f.found, nil
}

func (f *fakeCompletionStore) RemoveCompletionRoot(_ context.Context, name string) error {
	f.removed = append(f.removed, name)

	return nil
}

// completionPull is a sideloaded schema 3 pull on a deployment accepting accept,
// with every completion seam replaced; it returns what the backend launched.
type completionPull struct {
	fixture  *pullFixture
	backend  *launchRecorder
	store    *fakeCompletionStore
	detached []string
	grownTo  int64
}

func newCompletionPull(t *testing.T, accept string, report string, store *fakeCompletionStore) *completionPull {
	t.Helper()

	fixture := newPullFixture(t, leavingOut)
	part := fixture.manifest.RootfsMultipart.Parts[0].Name
	writeAsset(t, filepath.Join(fixture.from, part), []byte("a root filesystem, for the purposes of this test"))

	cfg, err := os.ReadFile(fixture.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(cfg), "  firecracker:\n",
		"  lock_dir: "+t.TempDir()+"\n  firecracker:\n", 1) + "images:\n  accept: [" + accept + "]\n"
	if err := os.WriteFile(fixture.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	usePullSeams(t, fixture.publisher, durablefile.Installer{}, currentRunner)

	run := &completionPull{fixture: fixture, backend: &launchRecorder{report: report}, store: store}
	fakeGuestListener(t, run.backend)

	prov, attach, open := newCompletionProvider, attachLoop, openCompletionStore
	t.Cleanup(func() { newCompletionProvider, attachLoop, openCompletionStore = prov, attach, open })
	newCompletionProvider = func(*config.Config, string) (provider.Provider, error) { return run.backend, nil }
	openCompletionStore = func(*config.Config) (completionStore, error) { return store, nil }
	attachLoop = func(_ context.Context, path string) (string, func(context.Context) error, error) {
		info, err := os.Stat(path)
		if err != nil {
			return "", nil, err
		}
		run.grownTo = info.Size()

		return "/dev/loop7", func(context.Context) error {
			run.detached = append(run.detached, "/dev/loop7")

			return nil
		}, nil
	}

	return run
}

func (r *completionPull) pull(t *testing.T) error {
	t.Helper()

	return cmdImagesPull(t.Context(), processEnv(), []string{
		"--config", r.fixture.cfgPath,
		"--from", r.fixture.from,
		"--staging-dir", r.fixture.staging,
		"--kernel-dir", r.fixture.kernelDir,
		"--skip-signature-verification",
		"--completion-wait", "1m",
		"ubuntu-2404-x64",
	})
}

// A PULL OF AN IMAGE THAT LEAVES CHROME OUT, ON A DEPLOYMENT THAT ACCEPTS IT,
// installs it through a completion microVM booted from the newest verified
// generation, with the image attached as its second disk, and imports the image
// recording what it left out and the version installed.
func TestAPullCompletesAnImageBeforeImportingIt(t *testing.T) {
	store := &fakeCompletionStore{verified: ceph.Generation{Name: "g20260901000000"}, found: true}
	run := newCompletionPull(t, "google-chrome", completionReportFor("installed=google-chrome 155.0.8059.84"), store)

	if err := run.pull(t); err != nil {
		t.Fatalf("images pull: %v", err)
	}

	if len(run.backend.launched) != 1 {
		t.Fatalf("launched %d microVMs, want one", len(run.backend.launched))
	}
	spec := run.backend.launched[0]
	if spec.Image != "ubuntu-2404-x64@g20260901000000" || spec.Trust != provider.TrustTrusted ||
		len(spec.Volumes) != 1 || spec.Volumes[0].Device != "/dev/loop7" ||
		spec.VCPU != completionVCPU || spec.Memory != completionMemory {
		t.Fatalf("launched %+v", spec)
	}
	if !strings.Contains(strings.Join(spec.Command, " "), "/opt/billet/complete/run google-chrome") {
		t.Fatalf("the microVM was not asked to complete google-chrome: %v", spec.Command)
	}
	if len(run.backend.destroyed) == 0 || len(run.detached) != 1 {
		t.Fatalf("destroyed %v and detached %v, want the microVM destroyed and the image detached",
			run.backend.destroyed, run.detached)
	}

	content := int64(len("a root filesystem, for the purposes of this test"))
	if run.grownTo != content+600<<20 {
		t.Fatalf("the image was attached at %d bytes, want it grown by google-chrome's space to %d",
			run.grownTo, content+600<<20)
	}

	if got := run.fixture.publisher.calls(); got != 1 {
		t.Fatalf("ImportGeneration was reached %d times, want once", got)
	}
	got := run.fixture.publisher.last().components
	if got == nil || !slices.Equal(got.LeftOut, []string{"google-chrome"}) ||
		got.Installed["google-chrome"] != "155.0.8059.84" {
		t.Fatalf("the generation records %+v", got)
	}
	if len(store.removed) != 0 {
		t.Fatalf("a completion booted from a real generation removed %v", store.removed)
	}
}

// A COMPLETION THAT FAILS IMPORTS NOTHING, so the generation every tier boots is
// unchanged.
func TestAPullWhoseCompletionFailsImportsNothing(t *testing.T) {
	store := &fakeCompletionStore{verified: ceph.Generation{Name: "g20260901000000"}, found: true}
	run := newCompletionPull(t, "google-chrome", completionReportFor("failed=google-chrome"), store)

	err := run.pull(t)
	if err == nil || !strings.Contains(err.Error(), "google-chrome did not install") {
		t.Fatalf("got %v, want the failed completion refused", err)
	}
	if got := run.fixture.publisher.calls(); got != 0 {
		t.Fatalf("ImportGeneration was reached %d times after a failed completion", got)
	}
	if len(run.backend.destroyed) == 0 || len(run.detached) != 1 {
		t.Fatalf("destroyed %v and detached %v after a failed completion",
			run.backend.destroyed, run.detached)
	}
}

// ON A SITE WITH NO VERIFIED GENERATION, the completion boots a staging copy of the
// image as published, imported under the completion root's name before the image
// is grown, and removes it afterwards.
func TestAFirstPullCompletesOnAStagingCopy(t *testing.T) {
	store := &fakeCompletionStore{}
	run := newCompletionPull(t, "google-chrome", completionReportFor("installed=google-chrome 155.0.8059.84"), store)

	if err := run.pull(t); err != nil {
		t.Fatalf("images pull: %v", err)
	}

	if got := run.fixture.publisher.calls(); got != 2 {
		t.Fatalf("ImportGeneration was reached %d times, want the staging copy and the image", got)
	}
	staging := run.fixture.publisher.seen[0]
	if staging.image != ceph.CompletionRoot("ubuntu-2404-x64") || staging.components != nil {
		t.Fatalf("the first import was %+v, want the staging copy with no components", staging)
	}
	if spec := run.backend.launched[0]; spec.Image != ceph.CompletionRoot("ubuntu-2404-x64")+"@gen-20260831-000000" {
		t.Fatalf("the completion booted %s, want the staging copy", spec.Image)
	}
	if want := []string{ceph.CompletionRoot("ubuntu-2404-x64"), ceph.CompletionRoot("ubuntu-2404-x64")}; !slices.Equal(store.removed, want) {
		t.Fatalf("removed %v, want any leftover before staging and the staging copy after", store.removed)
	}
	if run.fixture.publisher.last().components == nil {
		t.Fatal("the image was imported without its components record")
	}
}

// A DEPLOYMENT THAT ACCEPTS NOTHING IMPORTS THE IMAGE WITHOUT THE COMPONENTS, with
// no completion, and records what it left out so a later acceptance completes it.
func TestAPullThatAcceptsNothingImportsTheImageAsPublished(t *testing.T) {
	store := &fakeCompletionStore{verified: ceph.Generation{Name: "g20260901000000"}, found: true}
	run := newCompletionPull(t, "", "", store)

	if err := run.pull(t); err != nil {
		t.Fatalf("images pull: %v", err)
	}
	if len(run.backend.launched) != 0 || len(run.detached) != 0 {
		t.Fatalf("a deployment that accepts nothing launched %v", run.backend.launched)
	}
	got := run.fixture.publisher.last().components
	if got == nil || !slices.Equal(got.LeftOut, []string{"google-chrome"}) || len(got.Installed) != 0 {
		t.Fatalf("the generation records %+v, want google-chrome left out and nothing installed", got)
	}
}

// completionReportFor is a report from a completion that prepared, unmounted and
// checked the image cleanly, under the secret the launch recorder substitutes.
func completionReportFor(lines ...string) string {
	return strings.Join(append([]string{"jit=SECRET", "prepared=0", "unmounted=0", "fsck=0"}, lines...), "\n")
}
