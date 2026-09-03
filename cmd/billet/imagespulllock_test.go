package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/durablefile"
)

// kernelLockProbe asks, from inside the operation under test, whether the kernel
// directory lock is held right now.
//
// ASKED WHILE THE PULL IS RUNNING, WHICH IS THE ONLY TIME THE ANSWER MEANS ANYTHING.
// The property is that no reap can run between the install and the publish, and every
// observation made after cmdImagesPull returns is compatible with a pull that took no
// lock at all.
//
// IT DISTINGUISHES CONTENTION FROM EVERY OTHER FAILURE. A probe that counted any
// error as "held" would report success on a host where the lock could not be placed at
// all, which is the exact opposite of what it claims to prove.
type kernelLockProbe struct {
	t       *testing.T
	dir     string
	tried   int
	refused int
}

func (p *kernelLockProbe) observe() {
	p.t.Helper()

	p.tried++

	lock, err := takeKernelDirLock(p.t.Context(), p.dir, "probe the kernel directory lock")
	if err != nil {
		if errors.Is(err, errKernelLockBusy) {
			p.refused++

			return
		}

		p.t.Errorf("the probe could not tell whether the lock was held: %v", err)

		return
	}

	if releaseErr := lock.release(); releaseErr != nil {
		p.t.Errorf("release the probe lock: %v", releaseErr)
	}
}

// held reports whether every observation this probe made found the lock taken.
func (p *kernelLockProbe) held() bool { return p.tried > 0 && p.refused == p.tried }

// A PULL HOLDS THE KERNEL DIRECTORY FROM BEFORE THE INSTALL THROUGH THE PUBLISH.
//
// This is the reap-versus-pull race. A concurrent `billet images reap` decides
// what to delete from the generations that exist at the moment it looks, and until
// ImportGeneration returns nothing names the kernel the pull has just installed --
// so the reap correctly concludes it is an orphan, unlinks it, and the generation
// is published naming a file that is gone. Every node then resolves a verified
// generation and cannot boot the exact kernel it was verified against.
//
// THE SPAN IS WHAT IS ASSERTED, NOT THE ACQUISITION, and it takes two probes because
// either end alone is satisfiable by the bug. A pull that locks only around the
// install passes an import-time check never being made; one that releases before the
// import passes an install-time check alone. Both mutations are real edits somebody
// could make, and each kills exactly one of these assertions.
func TestAPullHoldsTheKernelDirectoryLockAcrossTheInstallAndThePublish(t *testing.T) {
	shortKernelLockWait(t)

	fixture := newPullFixture(t)

	install := &kernelLockProbe{t: t, dir: fixture.kernelDir}
	publish := &kernelLockProbe{t: t, dir: fixture.kernelDir}

	fixture.publisher.onImport = publish.observe

	usePullSeams(t, fixture.publisher, durablefile.Installer{
		// THE MODE CHANGE, because it happens inside Install -- after the staged file
		// exists and before it is renamed into place, which is the middle of the
		// interval a reap must not run in. The hook does the real work as well as
		// observing, so this is not also a test of a broken install.
		SetMode: func(f *os.File, mode fs.FileMode) error {
			install.observe()

			return f.Chmod(mode)
		},
	}, currentRunner)

	if err := fixture.pull(t); err != nil {
		t.Fatalf("images pull: %v", err)
	}

	if !install.held() {
		t.Errorf("the kernel directory was not locked while the kernel was being installed "+
			"(%d observations, %d refused); a reap running now deletes a kernel nothing "+
			"names yet", install.tried, install.refused)
	}

	if !publish.held() {
		t.Errorf("the kernel directory was not locked while the generation was being "+
			"published (%d observations, %d refused); the window a reap must not run in "+
			"ends when the generation names the kernel, not when the file is written",
			publish.tried, publish.refused)
	}
}

// AND IT IS RELEASED, ON EVERY PATH.
//
// A lock a pull keeps is a reap that can never run on this host again until somebody
// restarts something -- and on the success path it would also be held across the
// verification that follows, which boots a microVM for minutes and takes the Ceph
// publish lock.
//
// THE FAILING CASES ARE HERE TOO, because the release on those paths is a deferred
// one and deleting it leaves the success path green. Each injected failure is one the
// durability tests already prove stops the pull; what is asserted here is only what
// it leaves behind.
func TestAPullReleasesTheKernelDirectoryLock(t *testing.T) {
	boom := errors.New("the disk said no")

	for _, tc := range []struct {
		name    string
		fail    func(*durablefile.Installer, *pullFixture)
		refused bool
	}{
		{name: "a pull that succeeded", fail: func(*durablefile.Installer, *pullFixture) {}},
		// ONE FAILING CASE, NOT EVERY LOCAL STEP. Each of them returns before
		// ImportGeneration and therefore leaves through the same deferred release, so a
		// second would exercise nothing this one does not -- and the pulls here are not
		// free: this package already sits close to go test's default per-package
		// timeout under -race -covermode=atomic.
		{
			name: "a pull whose install failed",
			fail: func(i *durablefile.Installer, _ *pullFixture) {
				i.Rename = func(string, string) error { return boom }
			},
			refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortKernelLockWait(t)

			fixture := newPullFixture(t)

			var installer durablefile.Installer

			tc.fail(&installer, fixture)
			usePullSeams(t, fixture.publisher, installer, currentRunner)

			// THE OUTCOME IS ASSERTED, THOUGH IT IS NOT THE SUBJECT. Discarding it
			// makes the success case pass against a pull that failed BEFORE it ever
			// took the lock -- the reacquisition below then succeeds for the one reason
			// this test must not accept -- and makes the failure case pass without ever
			// reaching the injected failure it is named for.
			err := fixture.pull(t)

			switch {
			case tc.refused && !errors.Is(err, boom):
				t.Fatalf("the pull did not fail the way this case injects: %v", err)
			case !tc.refused && err != nil:
				t.Fatalf("images pull: %v", err)
			}

			after, err := takeKernelDirLock(t.Context(), fixture.kernelDir, "collect kernels")
			if err != nil {
				t.Fatalf("the kernel directory is still locked after the pull returned, so no "+
					"reap can ever run on this host again: %v", err)
			}

			if err := after.release(); err != nil {
				t.Errorf("release: %v", err)
			}
		})
	}
}

// A PULL INSTALLS THE KERNEL WHERE THE NODE IS CONFIGURED TO LOOK FOR IT.
//
// --kernel-dir used to default to the CONSTANT /var/lib/billet/kernels while the
// reaper, configuredKernelName and the LAUNCH all resolved node.firecracker.kernel_dir.
// A host that set that key had its kernels installed where the launch does not look --
// so every microVM on the new generation failed to start -- and where the reaper does
// not look either, so they accumulated forever. The ansible host role runs this command
// with no --kernel-dir, which is how the packaged upgrade path reached it.
//
// THE FLAG IS OMITTED ENTIRELY, which is the whole point: every other test in this
// package passes --kernel-dir, so none of them could see this.
func TestAPullInstallsTheKernelWhereTheNodeIsConfiguredToLookForIt(t *testing.T) {
	fixture := newPullFixture(t)

	configured := filepath.Join(t.TempDir(), "configured-kernels")
	configurePullKernelDir(t, fixture.cfgPath, configured)

	usePullSeams(t, fixture.publisher, durablefile.Installer{}, currentRunner)

	if err := cmdImagesPull(t.Context(), []string{
		"--config", fixture.cfgPath,
		"--from", fixture.from,
		"--staging-dir", fixture.staging,
		"--skip-signature-verification",
		"ubuntu-2404-x64",
	}); err != nil {
		t.Fatalf("images pull: %v", err)
	}

	// WHERE THE LAUNCH WILL LOOK. The provider joins a generation's recorded kernel
	// name to node.firecracker.kernel_dir, so this is the only directory that boots.
	if _, err := os.Stat(filepath.Join(configured, kernelFileName(fixture.manifest))); err != nil {
		t.Errorf("the kernel is not in the directory the node config names, so the launch "+
			"will not find it: %v", err)
	}

	// AND NOTHING HERE LOOKS AT THE REAL /var/lib/billet/kernels, deliberately. The
	// obvious second assertion -- that the kernel is NOT in the built-in default --
	// reads as the stronger half and is the weaker one: it makes correct code fail on
	// a host that happens to hold a file of that name, and on an ordinary machine the
	// default is not writable, so a pull that ignored the config would die at the
	// mkdir and never reach the assertion at all. That is an environmental kill rather
	// than a proved one.
	//
	// The assertion above is what holds the property, and it was verified by mutation
	// against a WRITABLE wrong directory, so no permission error could stand in for it.
	if got := fixture.publisher.last().kernel; got != kernelFileName(fixture.manifest) {
		t.Errorf("the generation names kernel %q, want %q", got, kernelFileName(fixture.manifest))
	}
}

// configurePullKernelDir adds a kernel_dir to a fixture's config.
//
// THE SUBSTITUTION IS ASSERTED BEFORE IT IS MADE. A scripted edit that matches nothing
// reports success and leaves the subject untouched, which here would silently turn the
// test into one about the default it exists to remove.
func configurePullKernelDir(t *testing.T, cfgPath, dir string) {
	t.Helper()

	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}

	const anchor = "    bridge: br0\n"

	if !strings.Contains(string(body), anchor) {
		t.Fatalf("the fixture config no longer contains %q, so this test would silently "+
			"configure nothing", anchor)
	}

	updated := strings.Replace(string(body), anchor, "    kernel_dir: "+dir+"\n"+anchor, 1)

	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}
}
