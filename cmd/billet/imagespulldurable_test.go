package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/imagesource"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/runnerrelease"
)

// NOTHING REMOTE HAPPENS UNTIL THE KERNEL IS DURABLE, and this is the test that can
// see it.
//
// `billet images pull` installs the kernel locally and then commits Ceph metadata
// naming that exact filename. The local install used to sync the file, chmod it
// AFTER that sync, rename it, and return — with nothing flushing the containing
// directory, which fsync(2) says is what makes the entry survive. A power loss in
// between leaves a complete, remotely visible generation whose kernel is gone: every
// node resolves a verified generation and cannot boot the kernel it was verified
// against, and re-pulling repairs the local half while the cluster keeps advertising
// a pair that never existed.
//
// EVERY LOCAL STEP IS FAILED IN TURN, and the assertion is not that an error came
// back — it is that ImportGeneration was NEVER REACHED. An error alone is satisfied
// by a command that published first and complained afterwards, which is the failure
// this is about.
func TestAPullDoesNotPublishAGenerationBeforeTheKernelIsDurable(t *testing.T) {
	boom := errors.New("the disk said no")

	for _, tc := range []struct {
		name string
		fail func(*durablefile.Installer, *pullFixture)
	}{
		{"the mode change", func(i *durablefile.Installer, _ *pullFixture) {
			i.SetMode = func(*os.File, fs.FileMode) error { return boom }
		}},
		{"the file sync", func(i *durablefile.Installer, _ *pullFixture) {
			i.SyncFile = func(*os.File) error { return boom }
		}},
		{"the rename", func(i *durablefile.Installer, _ *pullFixture) {
			i.Rename = func(string, string) error { return boom }
		}},
		// SCOPED TO THE KERNEL DIRECTORY. Failing every SyncDir aborts inside
		// MkdirAll, flushing an ANCESTOR, and never reaches the sync after the
		// rename -- so a mutation to the install's own final flush would survive.
		// That is the same defect a review found in the repair-path test.
		{"the directory sync", func(i *durablefile.Installer, f *pullFixture) {
			i.SyncDir = func(dir string) error {
				if dir == f.kernelDir {
					return boom
				}

				return nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// NOT PARALLEL: these swap package-level seams, which is what makes them
			// tests of the real command rather than of a copy of it.
			fixture := newPullFixture(t)

			var installer durablefile.Installer

			tc.fail(&installer, fixture)
			usePullSeams(t, fixture.publisher, installer, currentRunner)

			err := fixture.pull(t)
			if err == nil {
				t.Fatal("the pull reported success with a kernel that was never committed")
			}

			if !strings.Contains(err.Error(), "kernel") {
				t.Errorf("the failure does not say the kernel is the problem: %v", err)
			}

			if got := fixture.publisher.calls(); got != 0 {
				t.Fatalf("ImportGeneration was reached %d times after a local durability "+
					"failure; the cluster would then advertise a kernel this host never "+
					"committed", got)
			}
		})
	}
}

// AND THE ORDINARY PULL DOES PUBLISH, naming the kernel it just installed.
//
// The refusals above are only meaningful beside this: a command that never reached
// ImportGeneration at all would pass every one of them.
func TestAPullPublishesTheGenerationItInstalledTheKernelFor(t *testing.T) {
	fixture := newPullFixture(t)

	usePullSeams(t, fixture.publisher, durablefile.Installer{}, currentRunner)

	if err := fixture.pull(t); err != nil {
		t.Fatalf("images pull: %v", err)
	}

	if got := fixture.publisher.calls(); got != 1 {
		t.Fatalf("ImportGeneration was reached %d times, want once", got)
	}

	last := fixture.publisher.last()

	if last.kernel != kernelFileName(fixture.manifest) {
		t.Errorf("the generation names kernel %q, and the installed file is %q",
			last.kernel, kernelFileName(fixture.manifest))
	}

	installed := filepath.Join(fixture.kernelDir, last.kernel)

	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("the published generation names a kernel that is not there: %v", err)
	}

	// READABLE BY WHATEVER BOOTS IT. A staged file is created 0600, and the mode is
	// set before the sync precisely so it is committed with the file.
	if info.Mode().Perm() != 0o644 {
		t.Errorf("the installed kernel is %v, want 0644", info.Mode().Perm())
	}
}

// A RETRY REPAIRS THE HALF-COMMITTED STATE RATHER THAN CONFIRMING IT.
//
// The state a failed directory sync leaves is a kernel with the right content under
// the right name and an entry that may not survive a crash. The next pull takes the
// "already there" branch, and if that branch returns success without flushing the
// directory then the retry — the thing that is supposed to fix this — is the thing
// that certifies it.
//
// AND WHAT THE OLD IMPLEMENTATION LEFT IS REPAIRED TOO. It set the mode AFTER the
// sync that would have committed it, so a kernel recovered from that version can
// hold the right bytes at 0600 — which is not readable by whatever boots it, and is
// invisible to a check that only hashes the content.
func TestAPullRepairsAKernelThatIsAlreadyThere(t *testing.T) {
	fixture := newPullFixture(t)

	// The kernel is already installed, exactly as an interrupted run left it: right
	// bytes, mode never committed, directory entry never flushed.
	if err := os.MkdirAll(fixture.kernelDir, 0o755); err != nil {
		t.Fatalf("make the kernel directory: %v", err)
	}

	installed := filepath.Join(fixture.kernelDir, kernelFileName(fixture.manifest))

	if err := os.WriteFile(installed, fixture.kernelBytes, 0o600); err != nil {
		t.Fatalf("pre-install the kernel: %v", err)
	}

	var flushed []string

	installer := durablefile.Installer{
		SyncDir: func(dir string) error {
			flushed = append(flushed, dir)

			return nil
		},
		Rename: func(string, string) error {
			t.Error("a kernel that was already installed was copied again")

			return errors.New("must not run")
		},
	}

	usePullSeams(t, fixture.publisher, installer, currentRunner)

	if err := fixture.pull(t); err != nil {
		t.Fatalf("images pull: %v", err)
	}

	if !slices.Contains(flushed, fixture.kernelDir) {
		t.Errorf("the kernel directory was never flushed (%v); an entry from an "+
			"interrupted run is committed by this branch or by nothing", flushed)
	}

	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("stat the kernel: %v", err)
	}

	if info.Mode().Perm() != 0o644 {
		t.Errorf("the kernel is still %v; a retry left it unreadable by whatever boots it",
			info.Mode().Perm())
	}
}

// AND IT REPAIRS IN THE ONLY ORDER THAT COMMITS, which the tests above cannot see.
//
// They assert the final state and each step's failure, and a mutation that flushes
// the file BEFORE setting its mode passes all of them: the mode still ends up 0644,
// each injected failure still stops the pull, and the directory is still flushed.
// What it breaks is the thing durablefile exists for — a mode set after the sync
// that would have committed it is metadata nothing committed, so the file can come
// back 0600 and the pull publishes anyway. The repair would reintroduce, in repair
// form, exactly the defect it exists to fix.
func TestAPullRepairsInTheOrderThatCommits(t *testing.T) {
	fixture := newPullFixture(t)

	if err := os.MkdirAll(fixture.kernelDir, 0o755); err != nil {
		t.Fatalf("make the kernel directory: %v", err)
	}

	if err := os.WriteFile(filepath.Join(fixture.kernelDir, kernelFileName(fixture.manifest)),
		fixture.kernelBytes, 0o600); err != nil {
		t.Fatalf("pre-install the kernel: %v", err)
	}

	var order []string

	usePullSeams(t, fixture.publisher, durablefile.Installer{
		SetMode: func(f *os.File, mode fs.FileMode) error {
			order = append(order, "setmode")

			return f.Chmod(mode)
		},
		SyncFile: func(f *os.File) error {
			order = append(order, "syncfile")

			return f.Sync()
		},
		SyncDir: func(dir string) error {
			// ONLY THE KERNEL DIRECTORY. MkdirAll flushes every ancestor first, and
			// recording those would bury the three steps this is about.
			if dir == fixture.kernelDir {
				order = append(order, "syncdir")
			}

			return nil
		},
	}, currentRunner)

	if err := fixture.pull(t); err != nil {
		t.Fatalf("images pull: %v", err)
	}

	if got := strings.Join(order, ","); got != "setmode,syncfile,syncdir" {
		t.Errorf("the repair ran %v; the mode must be set before the sync that commits "+
			"it, and the directory flushed after both", order)
	}
}

// AND A KERNEL AN OPERATOR HAS HARDENED IS STILL THAT KERNEL.
//
// The repair opens the installed file to hash it, set its mode and flush it — and
// asking for WRITE access it does not need refuses a correct artifact. MEASURED on
// Linux: opening O_RDWR on a 0444 file fails with permission denied, while a chmod
// and an fsync both succeed through a read-only descriptor, because a chmod needs
// ownership rather than write permission. A gate that refuses a correct artifact is
// the failure ADR-005 names: the next thing anybody does is delete the check.
func TestAPullAcceptsAKernelLeftReadOnly(t *testing.T) {
	fixture := newPullFixture(t)

	if err := os.MkdirAll(fixture.kernelDir, 0o755); err != nil {
		t.Fatalf("make the kernel directory: %v", err)
	}

	installed := filepath.Join(fixture.kernelDir, kernelFileName(fixture.manifest))

	if err := os.WriteFile(installed, fixture.kernelBytes, 0o444); err != nil {
		t.Fatalf("pre-install the kernel: %v", err)
	}

	usePullSeams(t, fixture.publisher, durablefile.Installer{}, currentRunner)

	if err := fixture.pull(t); err != nil {
		t.Fatalf("a pull refused a kernel it could read, hash and chmod: %v", err)
	}

	if got := fixture.publisher.calls(); got != 1 {
		t.Fatalf("ImportGeneration was reached %d times, want once", got)
	}

	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("stat the kernel: %v", err)
	}

	if info.Mode().Perm() != 0o644 {
		t.Errorf("the kernel is %v; the repair sets the mode it must boot with",
			info.Mode().Perm())
	}
}

// A KERNEL WHOSE MODE IS ALREADY RIGHT IS NOT CHMOD'ED AT ALL.
//
// A chmod to the mode a file already has changes nothing, so its failure proves
// nothing about the artifact — and it CAN fail: MEASURED on a read-only bind mount,
// chmod to the identical mode returns EROFS while both fsyncs succeed. Attempting it
// unconditionally refuses a correct kernel on a read-only kernel directory over an
// operation that was a no-op, which is the failure ADR-005 names.
//
// THE HOOK RECORDS AND THE TEST ASSERTS IT WAS NEVER CALLED. Returning an error and
// checking the pull succeeded proves only that the error was not propagated -- a
// mutation calling SetModeOn unconditionally and discarding what it returns passes
// that. The property is that it was not called at all.
func TestAPullDoesNotChmodAKernelWhoseModeIsAlreadyRight(t *testing.T) {
	fixture := newPullFixture(t)

	if err := os.MkdirAll(fixture.kernelDir, 0o755); err != nil {
		t.Fatalf("make the kernel directory: %v", err)
	}

	installed := filepath.Join(fixture.kernelDir, kernelFileName(fixture.manifest))

	if err := os.WriteFile(installed, fixture.kernelBytes, 0o644); err != nil {
		t.Fatalf("pre-install the kernel: %v", err)
	}

	chmoded := false

	usePullSeams(t, fixture.publisher, durablefile.Installer{
		SetMode: func(*os.File, fs.FileMode) error {
			chmoded = true

			return errors.New("read-only file system")
		},
	}, currentRunner)

	if err := fixture.pull(t); err != nil {
		t.Fatalf("a pull chmod'ed a kernel that was already 0644, and was refused: %v", err)
	}

	if chmoded {
		t.Error("the pull chmod'ed a kernel that was already 0644; on a read-only mount " +
			"that call fails and refuses a correct artifact over a no-op")
	}

	if got := fixture.publisher.calls(); got != 1 {
		t.Errorf("ImportGeneration was reached %d times, want once", got)
	}
}

// AND EACH OF THE REPAIR'S OWN STEPS FAILING IS STILL A FAILED PULL.
//
// FAILING ONE STEP RATHER THAN ALL OF THEM, and that is not fussiness: the first
// version of this failed EVERY SyncDir, which — once MkdirAll was added ahead of
// the reuse branch — aborted at the first ancestor flush and never reached the
// branch the test is named for. MEASURED by mutation: deleting the reuse branch's
// directory sync left it green. A hook that fails everything cannot say which
// thing it was about.
func TestAPullDoesNotPublishWhenTheExistingKernelCannotBeRepaired(t *testing.T) {
	boom := errors.New("the disk said no")

	for _, tc := range []struct {
		name string
		fail func(*durablefile.Installer, *pullFixture)
	}{
		{"its directory entry", func(i *durablefile.Installer, f *pullFixture) {
			i.SyncDir = func(dir string) error {
				if dir == f.kernelDir {
					return boom
				}

				return nil
			}
		}},
		{"its mode", func(i *durablefile.Installer, _ *pullFixture) {
			i.SetMode = func(*os.File, fs.FileMode) error { return boom }
		}},
		{"the file itself", func(i *durablefile.Installer, _ *pullFixture) {
			i.SyncFile = func(*os.File) error { return boom }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPullFixture(t)

			if err := os.MkdirAll(fixture.kernelDir, 0o755); err != nil {
				t.Fatalf("make the kernel directory: %v", err)
			}

			if err := os.WriteFile(
				filepath.Join(fixture.kernelDir, kernelFileName(fixture.manifest)),
				fixture.kernelBytes, 0o600); err != nil {
				t.Fatalf("pre-install the kernel: %v", err)
			}

			var installer durablefile.Installer

			tc.fail(&installer, fixture)
			usePullSeams(t, fixture.publisher, installer, currentRunner)

			if err := fixture.pull(t); err == nil {
				t.Fatal("a pull that could not finish repairing the kernel reported success")
			}

			if got := fixture.publisher.calls(); got != 0 {
				t.Errorf("ImportGeneration was reached %d times", got)
			}
		})
	}
}

// AND SOMETHING THAT IS NOT THAT KERNEL IS NEVER REUSED. The name carries the
// digest, so the name is a claim about the bytes — and a file under it that fails
// the claim must stop the pull rather than be overwritten or believed.
func TestAPullRefusesAKernelPathThatIsNotThatKernel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		place func(t *testing.T, path string, fixture *pullFixture)
		says  string
	}{
		{
			name: "different content under the right name",
			place: func(t *testing.T, path string, _ *pullFixture) {
				t.Helper()

				if err := os.WriteFile(path, []byte("not that kernel"), 0o644); err != nil {
					t.Fatalf("write the impostor: %v", err)
				}
			},
			says: "hashes to",
		},
		{
			// A FIFO IS NOT A KERNEL EITHER, and it is the one that does not merely
			// give a wrong answer: MEASURED, opening a FIFO for reading with no
			// writer NEVER RETURNS, so the check that would reject it is never
			// reached and the pull hangs with nothing able to cancel it — a
			// filesystem open takes no context. O_NONBLOCK is what makes the
			// rejection reachable.
			name: "a fifo",
			place: func(t *testing.T, path string, _ *pullFixture) {
				t.Helper()

				if err := syscall.Mkfifo(path, 0o644); err != nil {
					t.Skipf("this filesystem does not do fifos: %v", err)
				}
			},
			says: "not a regular file",
		},
		{
			// A SYMLINK IS NOT A KERNEL, and this runs as root: following one would
			// hash whatever it points at and then chmod that file instead.
			name: "a symlink to the right content",
			place: func(t *testing.T, path string, fixture *pullFixture) {
				t.Helper()

				target := filepath.Join(t.TempDir(), "elsewhere")

				if err := os.WriteFile(target, fixture.kernelBytes, 0o644); err != nil {
					t.Fatalf("write the target: %v", err)
				}

				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("link: %v", err)
				}
			},
			says: "ordinary file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPullFixture(t)

			if err := os.MkdirAll(fixture.kernelDir, 0o755); err != nil {
				t.Fatalf("make the kernel directory: %v", err)
			}

			tc.place(t, filepath.Join(fixture.kernelDir, kernelFileName(fixture.manifest)),
				fixture)

			usePullSeams(t, fixture.publisher, durablefile.Installer{}, currentRunner)

			// BOUNDED, BECAUSE ONE OF THESE CASES IS ABOUT HANGING. Without this the
			// fifo case does not fail — it stops, and takes the package's whole
			// timeout with it, reported ten minutes later as something else.
			err := pullWithin(t, fixture, 30*time.Second)
			if err == nil {
				t.Fatal("a pull accepted something that is not the kernel its name claims")
			}

			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say what is wrong (%q): %v", tc.says, err)
			}

			if got := fixture.publisher.calls(); got != 0 {
				t.Errorf("ImportGeneration was reached %d times", got)
			}
		})
	}
}

// THE DIRECTORY'S OWN NAME IS COMMITTED TOO, and this is the same defect one level
// up.
//
// Flushing the kernel directory commits the entries INSIDE it. It says nothing
// about the entry naming the directory in its parent — and on a fresh host nothing
// else creates /var/lib/billet/kernels, so a crash after the generation is published
// can take the whole directory away and leave precisely the remote-record-with-no-
// local-kernel state the rest of this file is about.
func TestAPullCommitsTheKernelDirectoryItself(t *testing.T) {
	fixture := newPullFixture(t)

	var flushed []string

	usePullSeams(t, fixture.publisher, durablefile.Installer{
		SyncDir: func(dir string) error {
			flushed = append(flushed, dir)

			return nil
		},
	}, currentRunner)

	if err := fixture.pull(t); err != nil {
		t.Fatalf("images pull: %v", err)
	}

	parent := filepath.Dir(fixture.kernelDir)

	if !slices.Contains(flushed, parent) {
		t.Errorf("the parent of the kernel directory was never flushed (%v), so the entry "+
			"naming the directory is not durable and the kernel inside it can vanish with "+
			"it", flushed)
	}

	// AND EVERY LEVEL ABOVE IT, because the same argument applies to the parent's
	// own name: a directory this pull created two levels down is no more durable
	// than the shallowest entry nothing flushed.
	if !slices.Contains(flushed, filepath.Dir(parent)) {
		t.Errorf("only one level above the kernel directory was flushed (%v)", flushed)
	}
}

// AND A DIRECTORY THAT CANNOT BE COMMITTED STOPS THE PULL, before anything remote.
func TestAPullDoesNotPublishWhenTheKernelDirectoryCannotBeCommitted(t *testing.T) {
	fixture := newPullFixture(t)

	parent := filepath.Dir(fixture.kernelDir)

	usePullSeams(t, fixture.publisher, durablefile.Installer{
		SyncDir: func(dir string) error {
			if dir == parent {
				return errors.New("the disk said no")
			}

			return nil
		},
	}, currentRunner)

	if err := fixture.pull(t); err == nil {
		t.Fatal("a pull whose kernel directory was never committed reported success")
	}

	if got := fixture.publisher.calls(); got != 0 {
		t.Fatalf("ImportGeneration was reached %d times", got)
	}
}

// AN IMAGE IS REFUSED ON WHAT GITHUB SAYS, NOT ON HOW OLD THE FILE IS.
//
// Import used to refuse at built_at + 30 days, which is not evidence about github in
// either direction. Both halves are asserted here because fixing one of them alone
// leaves the other: an image built four months ago whose runner is still the current
// release must import, and a fresh image whose runner github has stopped queueing to
// must not.
func TestImportJudgesTheRunnerRatherThanTheImagesAge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		builtDays int
		fresh     runnerrelease.Freshness
		refused   bool
	}{
		{
			name:      "an old image whose runner is still current",
			builtDays: 120,
			fresh: runnerrelease.Freshness{
				Installed: fixtureRunner, Latest: fixtureRunner, InstalledKnown: true, HistoryComplete: true,
			},
		},
		{
			name:      "a fresh image whose runner github refuses",
			builtDays: 1,
			fresh: runnerrelease.Freshness{
				Installed: fixtureRunner, Latest: "2.400.0", InstalledKnown: true, HistoryComplete: true,
				FirstNewer:          "2.337.0",
				FirstNewerPublished: time.Now().UTC().Add(-90 * 24 * time.Hour),
			},
			refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPullFixture(t, builtDaysAgo(tc.builtDays))

			usePullSeams(t, fixture.publisher, durablefile.Installer{},
				func(context.Context, *http.Client, string) (runnerrelease.Freshness, error) {
					return tc.fresh, nil
				})

			err := fixture.pull(t)

			if tc.refused {
				if err == nil {
					t.Fatal("an image whose runner github has stopped queueing to was imported")
				}

				if !strings.Contains(err.Error(), "--allow-stale") {
					t.Errorf("the refusal does not say how to override it: %v", err)
				}

				if got := fixture.publisher.calls(); got != 0 {
					t.Errorf("ImportGeneration was reached %d times for a refused image", got)
				}

				return
			}

			if err != nil {
				t.Fatalf("an image whose runner is the current release was refused: %v", err)
			}

			if got := fixture.publisher.calls(); got != 1 {
				t.Errorf("ImportGeneration was reached %d times, want once", got)
			}
		})
	}
}

// AND A LOOKUP THAT CANNOT BE MADE IS NOT A REFUSAL. A machine with no egress must
// still be able to import; the alternative is a node that cannot be repaired during
// the outage that is stopping it from asking.
func TestImportProceedsWhenGitHubCannotBeAsked(t *testing.T) {
	fixture := newPullFixture(t)

	usePullSeams(t, fixture.publisher, durablefile.Installer{},
		func(context.Context, *http.Client, string) (runnerrelease.Freshness, error) {
			return runnerrelease.Freshness{}, errors.New("dial tcp: no route to host")
		})

	if err := fixture.pull(t); err != nil {
		t.Fatalf("a pull on a machine that cannot reach github was refused: %v", err)
	}

	if got := fixture.publisher.calls(); got != 1 {
		t.Errorf("ImportGeneration was reached %d times, want once", got)
	}
}

// fixtureRunner is the runner version the fixture image bakes.
const fixtureRunner = "2.336.0"

// currentRunner is the answer for a fleet with nothing to do, so a test about
// durability is not also a test about freshness.
func currentRunner(
	context.Context, *http.Client, string,
) (runnerrelease.Freshness, error) {
	return runnerrelease.Freshness{
		Installed: fixtureRunner, Latest: fixtureRunner, InstalledKnown: true, HistoryComplete: true,
	}, nil
}

// usePullSeams swaps the package-level seams the command reaches through, and puts
// them back.
//
// THE REAL COMMAND, WITH THREE THINGS REPLACED: the cluster, the durability
// primitive's steps, and the question asked of github. Everything else — the config
// load, the sideload staging, the digest checks, the ORDER of the install and the
// import — is production's.
func usePullSeams(
	t *testing.T,
	publisher generationPublisher,
	installer durablefile.Installer,
	resolve func(context.Context, *http.Client, string) (runnerrelease.Freshness, error),
) {
	t.Helper()

	openStore, oldInstaller, oldResolve := openGenerationPublisher, kernelInstaller, resolveRunnerFreshness

	t.Cleanup(func() {
		openGenerationPublisher, kernelInstaller, resolveRunnerFreshness = openStore, oldInstaller, oldResolve
	})

	openGenerationPublisher = func(*config.Config) (generationPublisher, error) {
		return publisher, nil
	}
	kernelInstaller = installer
	resolveRunnerFreshness = resolve
}

// recordingPublisher stands in for the cluster and judges nothing.
//
// IT ONLY RECORDS. A fake that reimplemented the rule under test would be the thing
// the assertions were about; what is asserted here is whether production reached it
// at all, and with what.
type recordingPublisher struct {
	mu    sync.Mutex
	seen  []publishedGeneration
	fails bool

	// onImport runs INSIDE the import, before anything is recorded.
	//
	// A SEAM FOR ASKING WHAT IS TRUE AT THIS INSTANT rather than afterwards. What the
	// kernel directory lock guarantees is about the moment the generation is
	// published -- the pull must still hold it then -- and a check made after
	// cmdImagesPull returns cannot see that at all.
	onImport func()
}

type publishedGeneration struct {
	image, rawPath, runnerVersion, kernel, guestContract string
}

func (p *recordingPublisher) ImportGeneration(
	_ context.Context,
	image, rawPath, runnerVersion, kernel, guestContract string,
	_ time.Time,
) (string, error) {
	// BEFORE THE LOCK THIS FAKE TAKES FOR ITS OWN BOOKKEEPING, so a hook is free to
	// call back into anything.
	if p.onImport != nil {
		p.onImport()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.seen = append(p.seen, publishedGeneration{
		image: image, rawPath: rawPath, runnerVersion: runnerVersion,
		kernel: kernel, guestContract: guestContract,
	})

	if p.fails {
		return "", errors.New("the cluster refused")
	}

	return "gen-20260831-000000", nil
}

func (p *recordingPublisher) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.seen)
}

func (p *recordingPublisher) last() publishedGeneration {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.seen) == 0 {
		return publishedGeneration{}
	}

	return p.seen[len(p.seen)-1]
}

// pullFixture is a sideloadable image on disk plus the config that imports it.
type pullFixture struct {
	cfgPath     string
	from        string
	staging     string
	kernelDir   string
	manifest    *imagesource.Manifest
	kernelBytes []byte
	publisher   *recordingPublisher
}

func (f *pullFixture) pull(t *testing.T) error {
	t.Helper()

	return f.pullWith(t.Context())
}

// pullWith is the pull with its context supplied, for a caller that must not touch t.
func (f *pullFixture) pullWith(ctx context.Context) error {
	return cmdImagesPull(ctx, []string{
		"--config", f.cfgPath,
		"--from", f.from,
		"--staging-dir", f.staging,
		"--kernel-dir", f.kernelDir,
		"--skip-signature-verification",
		"ubuntu-2404-x64",
	})
}

// pullWithin runs the pull and fails if it does not finish.
//
// A HANG IS A FAILURE WITH A NAME. cmdImagesPull opens the kernel path directly, and
// a filesystem open takes no context — so a path that blocks cannot be cancelled by
// the test's own context and would otherwise surface as the package timing out, ten
// minutes later, reported as something else.
//
// THE GOROUTINE IS LEAKED ON THE FAILURE PATH, deliberately: nothing can stop a
// blocked open, the channel is buffered so it cannot block on the way out, and the
// only run that reaches it is one where the subject is already broken. On the
// passing path there is nothing left behind.
func pullWithin(t *testing.T, fixture *pullFixture, within time.Duration) error {
	t.Helper()

	// EVERYTHING THE GOROUTINE NEEDS IS TAKEN FIRST. On the failure path the test
	// ends while the goroutine is still blocked, so it must not reach for anything on
	// t afterwards.
	ctx := t.Context()
	done := make(chan error, 1)

	go func() { done <- fixture.pullWith(ctx) }()

	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("the pull did not finish within %v; opening the kernel path blocked, and "+
			"nothing can cancel it", within)

		return nil
	}
}

type fixtureOption func(*imagesource.Manifest)

func builtDaysAgo(days int) fixtureOption {
	return func(m *imagesource.Manifest) {
		m.BuiltAt = time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Truncate(time.Second)
	}
}

// newPullFixture writes a real sideload directory: a manifest, an uncompressed root
// filesystem and a kernel, with the digests the manifest publishes.
//
// UNCOMPRESSED DELIBERATELY, so the import path does not need the zstd binary and
// the test is about the seam rather than about what is installed on the machine
// running it.
func newPullFixture(t *testing.T, opts ...fixtureOption) *pullFixture {
	t.Helper()

	dir := t.TempDir()
	from := filepath.Join(dir, "sideload")

	if err := os.MkdirAll(from, 0o700); err != nil {
		t.Fatalf("make the sideload directory: %v", err)
	}

	rootfs := []byte("a root filesystem, for the purposes of this test")
	kernel := []byte("a kernel, for the purposes of this test")

	writeAsset(t, filepath.Join(from, "rootfs.img"), rootfs)
	writeAsset(t, filepath.Join(from, "vmlinux-billet"), kernel)

	manifest := &imagesource.Manifest{
		Schema:        imagesource.SchemaV1,
		GuestContract: firecracker.GuestContract,
		Arch:          hostArch(),
		RunnerVersion: fixtureRunner,
		BuiltAt:       time.Now().UTC().Truncate(time.Second),
		Rootfs: imagesource.Asset{
			Name: "rootfs.img", SHA256: digest(rootfs), Size: int64(len(rootfs)),
		},
		Kernel: imagesource.Asset{
			Name: "vmlinux-billet", SHA256: digest(kernel), Size: int64(len(kernel)),
			Version: "6.1.155",
		},
	}

	for _, opt := range opts {
		opt(manifest)
	}

	if err := manifest.Validate(); err != nil {
		t.Fatalf("the fixture manifest is not valid: %v", err)
	}

	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("render the fixture manifest: %v", err)
	}

	writeAsset(t, filepath.Join(from, imagesource.ManifestName), body)

	return &pullFixture{
		cfgPath:     writePullConfig(t, dir),
		from:        from,
		staging:     filepath.Join(dir, "staging"),
		kernelDir:   filepath.Join(dir, "kernels"),
		manifest:    manifest,
		kernelBytes: kernel,
		publisher:   &recordingPublisher{},
	}
}

func writeAsset(t *testing.T, path string, body []byte) {
	t.Helper()

	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:])
}

// writePullConfig is a firecracker node with a cluster, which is what the import
// path requires before it will look at anything.
func writePullConfig(t *testing.T, dir string) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	cfgPath := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf(`server:
  listen: 127.0.0.1:7717
  state_dir: %s
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 1
  private_key_path: %s
node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: firecracker
  state_dir: %s
  firecracker:
    kernel_image: /var/lib/billet/vmlinux
    bridge: br0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
tiers:
  - label: billet-4vcpu
    provider: firecracker
    vcpu: 4
    memory: 16GiB
    image: ubuntu-2404-x64
`, filepath.Join(dir, "server"), keyPath, filepath.Join(dir, "node"))

	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return cfgPath
}
