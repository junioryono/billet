package tart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

const (
	verifiedRef    = "ghcr.io/acme/macos:26"
	verifiedTier   = verifiedRef + "@verified"
	verifiedLease  = "billet-0190c8a0-0000-7000-8000-000000000001"
	verifiedLeaseB = "billet-0190c8a0-0000-7000-8000-000000000002"
	verifiedLeaseC = "billet-0190c8a0-0000-7000-8000-000000000003"
)

// verifiedStub is a stub whose guest answers every probe question.
func verifiedStub(t *testing.T) (*stub, *Provider) {
	t.Helper()

	s := newStub(t)
	p := newProvider(t, s)
	p.stateDir = filepath.Join(filepath.Dir(s.home), "state")
	p.probeWindow = 2 * time.Second
	p.stopWindow = 2 * time.Second
	p.yieldWait = 3 * time.Second
	p.yieldRetry = 5 * time.Millisecond
	p.yieldPoll = 5 * time.Millisecond

	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(s.guestHome, "actions-runner", "bin", "Runner.Listener"),
		"#!/bin/sh\nprintf '2.337.0\\n'\n")

	for _, tool := range []string{"docker", "xcodebuild"} {
		writeExecutable(t, filepath.Join(s.guestBin, tool), "#!/bin/sh\nexit 0\n")
	}

	setRegistry(t, s, p, "manifest one")

	return s, p
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := forkSafeWriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// setRegistry makes the tag name a manifest and the pull store those bytes.
func setRegistry(t *testing.T, s *stub, p *Provider, manifest string) {
	t.Helper()

	if err := os.WriteFile(s.pullManifest, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	digest := digestOf(manifest)
	p.registry = func(context.Context, string) (string, error) { return digest, nil }
}

func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))

	return "sha256:" + hex.EncodeToString(sum[:])
}

func verifyOpts() VerifyOptions {
	return VerifyOptions{
		Image:   verifiedTier,
		Command: []string{"./actions-runner/run.sh"},
		Window: func(context.Context, string) (RunnerAnswer, error) {
			return RunnerAnswer{}, nil
		},
	}
}

func mustVerify(t *testing.T, p *Provider) *VerifiedRecord {
	t.Helper()

	rec, err := p.Verify(t.Context(), verifyOpts())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	return rec
}

func verifiedSpec(name string) provider.Spec {
	spec := validSpec(name)
	spec.Image = verifiedTier

	return spec
}

func envLines(t *testing.T, s *stub, command string) []string {
	t.Helper()

	raw, err := os.ReadFile(s.envLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	var out []string

	for line := range strings.Lines(string(raw)) {
		fields := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(fields) == 3 && strings.HasPrefix(fields[2], command) {
			out = append(out, strings.TrimSpace(line))
		}
	}

	return out
}

// A REMOTE IMAGE IS PULLED IN A STORE OF ITS OWN, by digest, and nothing in the shared
// store moves: a pull there could replace a digest another deployment pins. Once its
// base has moved, the private store is gone, cache and all.
func TestAVerificationPullsInAPrivateStoreByDigest(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	rec := mustVerify(t, p)

	pulls := envLines(t, s, "pull")
	if len(pulls) != 1 {
		t.Fatalf("pulls = %v, want one", pulls)
	}

	want := "ghcr.io/acme/macos@" + rec.Digest
	store := p.privateStore(verifiedRef)

	if pulls[0] != store+"|1|pull "+want {
		t.Fatalf("pull = %q, want it in %s, unpruned, by digest %s", pulls[0], store, want)
	}

	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the private store is still there: %v", err)
	}

	if rec.Digest != digestOf("manifest one") || rec.Runner != "2.337.0" || rec.GuestOS != guestLinux {
		t.Fatalf("recorded %+v", rec)
	}

	if !strings.HasPrefix(rec.Base, basePrefix+testOwner+"-") {
		t.Fatalf("base %q is outside this deployment's names", rec.Base)
	}

	if _, ok := provider.LeaseOf(rec.Base); ok {
		t.Fatalf("base %q reads as a lease, so a sweep would treat it as compute", rec.Base)
	}

	if _, err := os.Stat(p.vmDir(rec.Base)); err != nil {
		t.Fatalf("the base is not in the shared store: %v", err)
	}

	// PROVED GONE, before anything was recorded.
	if _, err := os.Stat(p.vmDir(p.probeName(verifiedRef))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe survived: %v", err)
	}
}

// tart re-points what it was asked for at whatever the registry returns, so a clean exit
// is not the bytes asked for.
func TestAManifestThatIsNotTheDigestAskedForRefuses(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	p.registry = func(context.Context, string) (string, error) { return digestOf("what the tag named"), nil }

	_, err := p.Verify(t.Context(), verifyOpts())
	if err == nil || !strings.Contains(err.Error(), "hashes to "+digestOf("manifest one")) {
		t.Fatalf("Verify = %v, want the mismatch named", err)
	}

	assertNoBases(t, p)

	if lines := envLines(t, s, "clone"); len(lines) != 0 {
		t.Fatalf("cloned an image it could not vouch for: %v", lines)
	}
}

func TestAPullThatCachedNothingRefuses(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	if err := os.WriteFile(s.pullStoresNothing, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil || !strings.Contains(err.Error(), "holds 0 manifests") {
		t.Fatalf("Verify = %v", err)
	}
}

func assertNoBases(t *testing.T, p *Provider) {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(p.home, "vms"))
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), basePrefix) {
			t.Fatalf("%s was left in the shared store", entry.Name())
		}
	}
}

// A STACKED VM'S LAYERS STAY IN ITS PRIVATE STORE, which is deleted, so moving one would
// leave a base with nothing beneath its overlay.
func TestAStackedImageIsRefused(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	if err := os.WriteFile(s.cloneStacked, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil || !strings.Contains(err.Error(), "stacked") {
		t.Fatalf("Verify = %v", err)
	}

	assertNoBases(t, p)
}

// `tart run` WOULD RESUME A SAVED SUSPENSION, and the probe would prove a resume rather
// than the cold boot a job makes. A local VM is the one that can carry it.
func TestAVerifiedBaseCarriesNoSavedSuspension(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	if err := os.WriteFile(s.cloneSuspended, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	rec := mustVerify(t, p)
	if _, err := os.Stat(filepath.Join(p.vmDir(rec.Base), baseSuspended)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the base kept its suspension: %v", err)
	}
}

// A LOCAL VM IS CLONED IN THE SHARED STORE WITH NO PRUNING, and has no digest to record.
func TestALocalVMIsVerifiedInTheSharedStore(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	p.registry = func(context.Context, string) (string, error) {
		t.Error("a local VM has no registry to ask")

		return "", errors.New("asked")
	}

	opts := verifyOpts()
	opts.Image = "my-vm@verified"

	rec, err := p.Verify(t.Context(), opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if rec.Digest != "" || rec.Image != "my-vm" {
		t.Fatalf("recorded %+v", rec)
	}

	clones := envLines(t, s, "clone my-vm")
	if len(clones) != 1 || !strings.HasPrefix(clones[0], s.home+"|1|") {
		t.Fatalf("clones = %v, want one in the shared store, unpruned", clones)
	}

	if lines := envLines(t, s, "pull"); len(lines) != 0 {
		t.Fatalf("pulled a local VM: %v", lines)
	}
}

// THE LAUNCH'S ONE SOURCE IS THE BASE: `tart fqn` would read `@verified` as a name, and
// a clone of the remote reference could fetch whatever the registry serves.
func TestALaunchClonesTheNewestBaseAndNothingElse(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	first := mustVerify(t, p)

	setRegistry(t, s, p, "manifest two")

	if err := os.WriteFile(s.imageBytes, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := mustVerify(t, p)
	if second.Generation <= first.Generation || second.Base == first.Base {
		t.Fatalf("records %+v and %+v", first, second)
	}

	if err := os.Truncate(s.envLog, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	clones := envLines(t, s, "clone")
	if want := "clone " + second.Base + " " + verifiedLease + stagingSuffix; len(clones) != 1 ||
		!strings.HasSuffix(clones[0], "|1|"+want) {
		t.Fatalf("clones = %v, want %q, unpruned", clones, want)
	}

	if lines := envLines(t, s, "fqn"); len(lines) != 0 {
		t.Fatalf("asked tart about the reference: %v", lines)
	}
}

func TestALaunchWithNoRecordRefusesNamingTheCommand(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)

	_, err := p.Launch(t.Context(), verifiedSpec(verifiedLease))
	if err == nil || !strings.Contains(err.Error(), "billet images verify "+verifiedRef) {
		t.Fatalf("Launch = %v", err)
	}

	if lines := envLines(t, s, "clone"); len(lines) != 0 {
		t.Fatalf("cloned with no record: %v", lines)
	}
}

func TestALaunchOfARemovedBaseRefuses(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	if err := os.RemoveAll(p.vmDir(rec.Base)); err != nil {
		t.Fatal(err)
	}

	_, err := p.Launch(t.Context(), verifiedSpec(verifiedLease))
	if err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("Launch = %v", err)
	}
}

// A BASE IS AN ORDINARY LOCAL VM TO TART, so a change to it is caught by its files'
// identities before a job boots a copy of what nobody probed.
func TestABaseThatChangedIsMarkedInvalidAndNotBooted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(t *testing.T, s *stub, dir string)
	}{
		{"before the clone", func(t *testing.T, _ *stub, dir string) {
			t.Helper()

			if err := os.WriteFile(filepath.Join(dir, baseDisk), []byte("rewritten"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"rewritten in place at its size", func(t *testing.T, _ *stub, dir string) {
			t.Helper()

			path := filepath.Join(dir, baseConfig)

			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = f.Close() }()

			// A change time moves in the kernel's own ticks, so this sleep is what a
			// same-size rewrite in the same tick would otherwise hide.
			time.Sleep(20 * time.Millisecond)

			if _, err := f.WriteAt([]byte("{"), 0); err != nil {
				t.Fatal(err)
			}

			if after, err := f.Stat(); err != nil || after.Size() != info.Size() {
				t.Fatalf("the rewrite changed the size: %v", err)
			}
		}},
		{"replaced by a symlink", func(t *testing.T, _ *stub, dir string) {
			t.Helper()

			path := filepath.Join(dir, baseNVRAM)
			if err := os.Rename(path, path+".moved"); err != nil {
				t.Fatal(err)
			}

			if err := os.Symlink(path+".moved", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced by a file of its size", func(t *testing.T, _ *stub, dir string) {
			t.Helper()

			path := filepath.Join(dir, baseNVRAM)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, p := verifiedStub(t)
			rec := mustVerify(t, p)

			tc.change(t, s, p.vmDir(rec.Base))

			if err := os.Truncate(s.envLog, 0); err != nil {
				t.Fatal(err)
			}

			_, err := p.Launch(t.Context(), verifiedSpec(verifiedLease))
			if err == nil || !strings.Contains(err.Error(), "changed since it was verified") {
				t.Fatalf("Launch = %v", err)
			}

			if clones := envLines(t, s, "clone"); len(clones) != 0 {
				t.Fatalf("copied a base it could already tell had changed: %v", clones)
			}

			if _, err := os.Stat(p.vmDir(verifiedLease)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a clone took the lease's name: %v", err)
			}

			_, err = p.Launch(t.Context(), verifiedSpec(verifiedLeaseB))
			if err == nil || !strings.Contains(err.Error(), "no verified base") {
				t.Fatalf("the next launch = %v, want the invalid base never resolved", err)
			}
		})
	}
}

// A clone that changes the base under it is held to the same rule: the identities are
// read again after the clone completes.
func TestACloneThatChangedItsBaseIsDiscardedBeforeItBoots(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	rec := mustVerify(t, p)

	// Wraps the stub: on the launch's clone, rewrite the base's disk after copying.
	wrapped := s.bin + ".real"
	if err := os.Rename(s.bin, wrapped); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, s.bin, "#!/bin/sh\n\""+wrapped+"\" \"$@\"\nrc=$?\nif [ \"$1\" = clone ]; then "+
		"printf changed > \""+filepath.Join(p.vmDir(rec.Base), baseDisk)+"\"; fi\nexit $rc\n")

	_, err := p.Launch(t.Context(), verifiedSpec(verifiedLease))
	if err == nil || !strings.Contains(err.Error(), "changed since it was verified") {
		t.Fatalf("Launch = %v", err)
	}

	if _, err := os.Stat(p.vmDir(verifiedLease + stagingSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the clone was kept: %v", err)
	}
}

// TWO BASES BY DISTINCT CONTENT, so verifying the same bytes again does not push the
// previous image out, and a rollback target survives. A regenerated MAC is not content.
func TestBasesAreKeptByDistinctContent(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)

	setImage := func(content string) *VerifiedRecord {
		t.Helper()
		setRegistry(t, s, p, "manifest "+content)

		if err := os.WriteFile(s.imageBytes, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		return mustVerify(t, p)
	}

	a := setImage("a")
	b := setImage("b")
	again := setImage("b")

	if b.ContentKey != again.ContentKey {
		t.Fatalf("one image has two content keys: %s, %s", b.ContentKey, again.ContentKey)
	}

	assertBases(t, p, a.Base, again.Base)

	c := setImage("c")
	assertBases(t, p, again.Base, c.Base)
}

func assertBases(t *testing.T, p *Provider, want ...string) {
	t.Helper()

	images, err := p.VerifiedImages(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	var got []string

	for i := range images {
		for j := range images[i].Records {
			got = append(got, images[i].Records[j].Base)
		}
	}

	slices.Sort(got)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Fatalf("records name %v, want %v", got, want)
	}

	entries, err := os.ReadDir(filepath.Join(p.home, "vms"))
	if err != nil {
		t.Fatal(err)
	}

	var bases []string

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), basePrefix) {
			bases = append(bases, entry.Name())
		}
	}

	slices.Sort(bases)

	if !slices.Equal(bases, want) {
		t.Fatalf("the store holds %v, want %v", bases, want)
	}
}

// A MAC THAT CLASHED IS REGENERATED BY A CLONE, and that clone is not new content.
func TestAConfigFieldACloneRegeneratesIsNotContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for name, body := range map[string]string{baseDisk: "d", baseNVRAM: "n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	hash := func(config string) (string, string) {
		t.Helper()

		if err := os.WriteFile(filepath.Join(dir, baseConfig), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}

		exact, key, err := hashContent(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}

		return exact, key
	}

	exactA, keyA := hash(`{"os":"linux","cpuCount":4,"macAddress":"02:00:00:00:00:01","machineIdentifier":"AA"}`)
	exactB, keyB := hash(`{"machineIdentifier":"BB","macAddress":"02:00:00:00:00:02","cpuCount":4,"os":"linux"}`)

	if exactA == exactB {
		t.Fatal("the exact hash is blind to the configuration")
	}

	if keyA != keyB {
		t.Fatal("what a clone regenerates counted as new content")
	}

	if _, keyC := hash(`{"os":"linux","cpuCount":8,"macAddress":"02:00:00:00:00:01"}`); keyC == keyA {
		t.Fatal("a configuration change is not new content")
	}
}

// UNPROMOTE IS A ROLLBACK THAT HOLDS: no older base of the same bytes keeps serving, and
// a tag that still names the digest does not bring it back.
func TestAnUnpromotedDigestStaysWithdrawn(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	good := mustVerify(t, p)

	setRegistry(t, s, p, "manifest bad")

	if err := os.WriteFile(s.imageBytes, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	bad := mustVerify(t, p)

	dropped, err := p.Unpromote(t.Context(), verifiedRef, bad.Base)
	if err != nil || !slices.Equal(dropped, []string{bad.Base}) {
		t.Fatalf("Unpromote = %v, %v", dropped, err)
	}

	assertBases(t, p, good.Base)

	due, err := p.ImageDue(t.Context(), verifiedTier)
	if err != nil || due.Verify {
		t.Fatalf("the refresh would verify a withdrawn digest: %+v, %v", due, err)
	}

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil || !strings.Contains(err.Error(), "--digest") {
		t.Fatalf("Verify of a withdrawn digest = %v, want it to name how to take it back", err)
	}

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch on the rollback target: %v", err)
	}

	opts := verifyOpts()
	opts.Digest = bad.Digest

	back, err := p.Verify(t.Context(), opts)
	if err != nil {
		t.Fatalf("Verify by hand: %v", err)
	}

	if due, err := p.ImageDue(t.Context(), verifiedTier); err != nil || due.Verify ||
		strings.Contains(due.Why, "withdrawn") {
		t.Fatalf("after the withdrawal was cleared: %+v, %v", due, err)
	}

	if back.Digest != bad.Digest {
		t.Fatalf("recorded %+v", back)
	}

	// A CLEARED WITHDRAWAL SERVES AGAIN: a launch resolves the base it was taken back as.
	if err := os.Truncate(s.envLog, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLeaseB)); err != nil {
		t.Fatalf("Launch of what was taken back: %v", err)
	}

	if clones := envLines(t, s, "clone "+back.Base); len(clones) != 1 {
		t.Fatalf("clones = %v, want the base taken back", clones)
	}
}

// EVERY RECORD OF THE DIGEST GOES, so verifying the same bytes twice leaves no second
// base to serve after one is unpromoted.
func TestUnpromoteRemovesEveryRecordOfItsDigest(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	// A record of the same digest with other content, as two pulls of one digest whose
	// chunks differed would leave.
	if err := p.withRecords(t.Context(), func(st *verifiedState) (bool, error) {
		twin := *rec
		twin.Base, twin.Generation, twin.ContentKey = rec.Base+"-twin", rec.Generation-1, "other"
		st.verified.Records = append(st.verified.Records, twin)

		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Unpromote(t.Context(), verifiedRef, rec.Base); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err == nil ||
		!strings.Contains(err.Error(), "no verified base") {
		t.Fatalf("Launch = %v", err)
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || len(images) != 1 || len(images[0].Records) != 0 {
		t.Fatalf("images = %+v, %v, want no record of that digest left", images, err)
	}
}

func TestUnpromoteRefusesABaseTheImageDoesNotRecord(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	if _, err := p.Unpromote(t.Context(), "ghcr.io/acme/other:1", rec.Base); err == nil {
		t.Fatal("unpromoted another image's base")
	}

	assertBases(t, p, rec.Base)
}

// AN UNPROMOTE IS NEVER UNDONE BY A PROBE ALREADY IN FLIGHT.
func TestAVerificationStartedBeforeAnUnpromoteCommitsNothing(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	first := mustVerify(t, p)

	setRegistry(t, s, p, "manifest two")

	opts := verifyOpts()
	opts.Window = func(ctx context.Context, _ string) (RunnerAnswer, error) {
		_, err := p.Unpromote(ctx, verifiedRef, first.Base)

		return RunnerAnswer{}, err
	}

	if _, err := p.Verify(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "unpromoted while") {
		t.Fatalf("Verify = %v", err)
	}

	assertBases(t, p)
}

// THE RUNNER'S WINDOW: the verification refuses one proved expired, and a launch compares
// the stored date with the clock, needing neither the network nor a refresh.
func TestAnExpiredRunnerIsRefusedAtVerification(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	opts := verifyOpts()
	opts.Window = func(context.Context, string) (RunnerAnswer, error) {
		return RunnerAnswer{Expired: true}, nil
	}

	if _, err := p.Verify(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "past GitHub's window") {
		t.Fatalf("Verify = %v", err)
	}

	assertNoBases(t, p)
}

func TestALaunchRefusesARunnerPastItsStoredDeadline(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	opts := verifyOpts()
	opts.Window = func(context.Context, string) (RunnerAnswer, error) {
		return RunnerAnswer{Deadline: now.Add(time.Hour)}, nil
	}

	if _, err := p.Verify(t.Context(), opts); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch inside the window: %v", err)
	}

	p.now = func() time.Time { return now.Add(time.Hour) }

	_, err := p.Launch(t.Context(), verifiedSpec(verifiedLeaseB))
	if err == nil || !strings.Contains(err.Error(), "past GitHub's window") ||
		!strings.Contains(err.Error(), "billet images verify "+verifiedRef) {
		t.Fatalf("Launch at the deadline = %v", err)
	}
}

// COULD-NOT-TELL NEVER ERASES A DATE, and neither does the base going away.
func TestAStoredDeadlineSurvivesCouldNotTellAndAnUnpromote(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	deadline := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	opts := verifyOpts()
	opts.Window = func(context.Context, string) (RunnerAnswer, error) {
		return RunnerAnswer{Deadline: deadline}, nil
	}

	first, err := p.Verify(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	unknown, err := p.RefreshDeadlines(t.Context(), func(context.Context, string) (RunnerAnswer, error) {
		return RunnerAnswer{}, errors.New("GitHub did not answer")
	})
	if err != nil || !slices.Equal(unknown, []string{"2.337.0"}) {
		t.Fatalf("RefreshDeadlines = %v, %v", unknown, err)
	}

	if _, err := p.Unpromote(t.Context(), verifiedRef, first.Base); err != nil {
		t.Fatal(err)
	}

	setRegistry(t, s, p, "manifest two")

	if _, err := p.Verify(t.Context(), verifyOpts()); err != nil {
		t.Fatal(err)
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(images) != 1 || !images[0].Deadlines["2.337.0"].Equal(deadline) {
		t.Fatalf("images = %+v, want the date kept", images)
	}
}

// NEVER QUOTE THE GUEST: a third party built this image, and what it prints is its own.
func TestAProbeFailureQuotesNothingTheGuestSaid(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	p.probeWindow = 200 * time.Millisecond

	writeExecutable(t, filepath.Join(s.guestHome, "actions-runner", "bin", "Runner.Listener"),
		"#!/bin/sh\necho GUESTWROTETHIS\necho GUESTWROTETHIS >&2\nexit 1\n")

	_, err := p.Verify(t.Context(), verifyOpts())
	if err == nil || !strings.Contains(err.Error(), "runner answering --version") {
		t.Fatalf("Verify = %v", err)
	}

	if strings.Contains(err.Error(), "GUESTWROTETHIS") {
		t.Fatalf("quoted the guest: %v", err)
	}

	assertNoBases(t, p)
}

func TestARunnerAnswerThatIsNotAVersionIsNeitherRecordedNorQuoted(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)

	writeExecutable(t, filepath.Join(s.guestHome, "actions-runner", "bin", "Runner.Listener"),
		"#!/bin/sh\necho '2.337.0; GUESTWROTETHIS'\n")

	_, err := p.Verify(t.Context(), verifyOpts())
	if err == nil || !strings.Contains(err.Error(), "not a version") {
		t.Fatalf("Verify = %v", err)
	}

	if strings.Contains(err.Error(), "GUESTWROTETHIS") {
		t.Fatalf("quoted the guest: %v", err)
	}
}

// THE ANSWERS A JOB DEPENDS ON differ by operating system, and each is asked.
func TestEachCheckOfAGuestStopsAVerification(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		os, tool, args string
	}{
		{"linux", "docker", "version"},
		{"linux", "docker", "buildx"},
		{"linux", "docker", "compose"},
		{"darwin", "xcodebuild", "-version"},
	} {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			t.Parallel()

			s, p := verifiedStub(t)
			p.probeWindow = 200 * time.Millisecond
			setMacOS(t, s, p, tc.os)

			writeExecutable(t, filepath.Join(s.guestBin, tc.tool),
				"#!/bin/sh\nif [ \"$1\" = "+tc.args+" ]; then exit 1; fi\nexit 0\n")

			if _, err := p.Verify(t.Context(), verifyOpts()); err == nil ||
				!strings.Contains(err.Error(), "did not answer") {
				t.Fatalf("Verify = %v", err)
			}

			assertNoBases(t, p)
		})
	}
}

// setMacOS makes clones report an operating system, with the seams a macOS admission
// reads: no other guest on the host and a limit of two.
func setMacOS(t *testing.T, s *stub, p *Provider, guestOS string) {
	t.Helper()

	if err := os.WriteFile(s.cloneOS, []byte(guestOS), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(s.bin)
	writeExecutable(t, filepath.Join(dir, "ps"),
		"#!/bin/sh\ncat \""+filepath.Join(dir, "ps.out")+"\" 2>/dev/null\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "lsof"),
		"#!/bin/sh\ncat \""+filepath.Join(dir, "lsof-")+"$(basename \"$4\")\" 2>/dev/null\n")
	p.ps = filepath.Join(dir, "ps")
	p.lsof = filepath.Join(dir, "lsof")
	p.macOSLimit = func() (int, bool, error) { return 2, true, nil }
	// tart for a pid `ps.out` shows as a tart command, and otherwise the program it names.
	p.processPath = func(pid int) (string, error) {
		raw, err := os.ReadFile(filepath.Join(dir, "ps.out"))
		if err != nil {
			return "", err
		}

		for line := range strings.Lines(string(raw)) {
			if field, command, ok := strings.Cut(strings.TrimSpace(line), " "); ok && field == strconv.Itoa(pid) {
				if strings.HasPrefix(command, "/bin/sh") || strings.HasPrefix(command, "grep") ||
					strings.HasPrefix(command, "vim") {
					first, _, _ := strings.Cut(command, " ")

					return first, nil
				}

				return "/opt/homebrew/libexec/tart.app/Contents/MacOS/tart", nil
			}
		}

		return "", syscall.ESRCH
	}
}

func hold(t *testing.T, s *stub) {
	t.Helper()

	if err := os.WriteFile(s.runHolds, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { killHeld(s) })
}

// killHeld ends the processes `tart run` left, for a test's cleanup.
func killHeld(s *stub) {
	paths, err := filepath.Glob(filepath.Join(s.home, "vms", "*", ".held"))
	if err != nil {
		return
	}

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			continue
		}

		// Ended already is what most of these are.
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			continue
		}
	}
}

// A MACOS GUEST HOLDS A SLOT FOR AS LONG AS ITS PROCESS LIVES, through billet closing
// its own copy, and a third start past the limit refuses.
func TestAMacOSGuestHoldsASlotThroughItsOwnProcess(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	hold(t, s)

	spec := func(name string) provider.Spec {
		out := validSpec(name)
		out.Command = []string{"./run.sh"}

		return out
	}

	if _, err := p.Launch(t.Context(), spec(verifiedLease)); err != nil {
		t.Fatalf("first: %v", err)
	}

	if _, err := p.Launch(t.Context(), spec(verifiedLeaseB)); err != nil {
		t.Fatalf("second: %v", err)
	}

	assertSlot(t, p, 1, verifiedLease)
	assertSlot(t, p, 2, verifiedLeaseB)

	_, err := p.Launch(t.Context(), spec(verifiedLeaseC))
	if !errors.Is(err, ErrNoSlot) {
		t.Fatalf("third = %v, want no slot", err)
	}

	if _, serr := os.Stat(p.vmDir(verifiedLeaseC + stagingSuffix)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the refused guest's clone was kept: %v", serr)
	}

	if _, serr := os.Stat(p.vmDir(verifiedLeaseC)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the refused guest took a lease name: %v", serr)
	}

	// THE GUEST'S END RELEASES IT, with no elapsed time in the proof.
	if _, err := p.Destroy(t.Context(), verifiedLease); err != nil {
		t.Fatal(err)
	}

	waitSlotFree(t, p, 1)

	if _, err := p.Launch(t.Context(), spec(verifiedLeaseC)); err != nil {
		t.Fatalf("after a guest ended: %v", err)
	}
}

func assertSlot(t *testing.T, p *Provider, i int, name string) {
	t.Helper()

	f, err := os.Open(p.slotPath(i))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = f.Close() }()

	if free, err := tryFlock(f, syscall.LOCK_EX); err != nil || free {
		t.Fatalf("slot %d is not held: free=%v, %v", i, free, err)
	}

	if got := slotIdentity(f); got != name {
		t.Fatalf("slot %d names %q, want %q", i, got, name)
	}
}

func waitSlotFree(t *testing.T, p *Provider, i int) {
	t.Helper()

	f, err := os.Open(p.slotPath(i))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = f.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	if err := waitFlock(ctx, f, syscall.LOCK_EX, 20*time.Second); err != nil {
		t.Fatalf("slot %d never came free: %v", i, err)
	}
}

// A LINUX GUEST TAKES NO SLOT AND CARRIES A TOKEN, which is how the census tells it from
// a macOS guest no slot accounts for.
func TestALinuxGuestHoldsATokenAndNoSlot(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	hold(t, s)

	if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	f, err := os.Open(p.billetPath(tokenPrefix + verifiedLease))
	if err != nil {
		t.Fatalf("the guest has no token: %v", err)
	}

	defer func() { _ = f.Close() }()

	if free, err := tryFlock(f, syscall.LOCK_EX); err != nil || free {
		t.Fatalf("the guest's process does not hold its token: free=%v, %v", free, err)
	}

	names, err := filepath.Glob(p.billetPath(slotPrefix + "*"))
	if err != nil || len(names) != 0 {
		t.Fatalf("a Linux guest made slots: %v, %v", names, err)
	}
}

func TestATokenWhoseGuestEndedIsSwept(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// The stub's `tart run` has exited, so nothing holds the first token.
	if _, err := p.Launch(t.Context(), validSpec(verifiedLeaseB)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if _, err := os.Stat(p.billetPath(tokenPrefix + verifiedLease)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the ended guest's token is still there: %v", err)
	}
}

// A GUEST NO SLOT ACCOUNTS FOR COUNTS AS MACOS, an operator's Linux VM included, since
// billet cannot tell. One holding a token is billet's Linux guest and one holding a slot
// is already counted.
func TestTheCensusCountsWhatNoSlotOrTokenAccountsFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		ps    string
		lsof  map[string]string
		admit bool
	}{
		{name: "nothing else", ps: "", admit: true},
		{name: "a tart run nobody coordinated", ps: "111 /opt/tart run someone --no-graphics\n222 /opt/tart run other\n"},
		{name: "a macOS install", ps: "111 tart create vm --from-ipsw latest\n222 tart run other\n"},
		{
			name: "under a path holding a space",
			ps:   "111 /Applications/My Tools/tart.app/Contents/MacOS/tart run a --no-graphics\n222 tart run b\n",
		},
		{name: "one is fine under a limit of two", ps: "111 tart run someone\n", admit: true},
		{
			name: "a token holder is a Linux guest", ps: "111 tart run a\n222 tart run b\n",
			lsof: map[string]string{tokenPrefix + "a": "111\n", tokenPrefix + "b": "222\n"}, admit: true,
		},
		{
			name: "another program's arguments are not a guest",
			ps:   "111 /bin/sh -c tart run x\n222 grep tart run\n333 /bin/sh -c /opt/tart run y\n444 vim /a/tart run\n", admit: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, p := verifiedStub(t)
			setMacOS(t, s, p, guestDarwin)

			dir := filepath.Dir(s.bin)
			if err := os.WriteFile(filepath.Join(dir, "ps.out"), []byte(tc.ps), 0o600); err != nil {
				t.Fatal(err)
			}

			for name, pids := range tc.lsof {
				if err := os.MkdirAll(p.billetPath(""), 0o700); err != nil {
					t.Fatal(err)
				}

				// A token someone holds is not swept, so the file stays for the census.
				token, err := openLockFile(p.billetPath(name))
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { _ = token.Close() })

				if _, err := tryFlock(token, syscall.LOCK_EX); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(dir, "lsof-"+name), []byte(pids), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			_, err := p.Launch(t.Context(), validSpec(verifiedLease))
			if tc.admit && err != nil {
				t.Fatalf("Launch: %v", err)
			}

			if !tc.admit && !errors.Is(err, ErrNoSlot) {
				t.Fatalf("Launch = %v, want no slot", err)
			}
		})
	}
}

// THE PROCESSES OR THEIR FILES UNREADABLE REFUSES: an unread census is not an empty one.
func TestACensusThatCannotBeReadRefuses(t *testing.T) {
	t.Parallel()

	t.Run("ps", func(t *testing.T) {
		t.Parallel()

		s, p := verifiedStub(t)
		setMacOS(t, s, p, guestDarwin)
		writeExecutable(t, p.ps, "#!/bin/sh\necho denied >&2\nexit 1\n")

		if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err == nil ||
			!strings.Contains(err.Error(), "count the guests") {
			t.Fatalf("Launch = %v", err)
		}
	})

	// A PROCESS THAT ENDED BETWEEN `ps` AND THE ASK IS NO GUEST, which macOS says of a zombie too.
	t.Run("not a process that has gone", func(t *testing.T) {
		t.Parallel()

		s, p := verifiedStub(t)
		setMacOS(t, s, p, guestDarwin)
		p.macOSLimit = func() (int, bool, error) { return 1, true, nil }

		if err := os.WriteFile(filepath.Join(filepath.Dir(s.bin), "ps.out"), []byte("111 tart run x\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		p.processPath = func(int) (string, error) {
			return "", fmt.Errorf("read: %w", syscall.ESRCH)
		}

		if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err != nil {
			t.Fatalf("Launch: %v", err)
		}
	})

	t.Run("an executable", func(t *testing.T) {
		t.Parallel()

		s, p := verifiedStub(t)
		setMacOS(t, s, p, guestDarwin)

		if err := os.WriteFile(filepath.Join(filepath.Dir(s.bin), "ps.out"), []byte("111 tart run x\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		p.processPath = func(int) (string, error) { return "", syscall.EPERM }

		if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err == nil ||
			!strings.Contains(err.Error(), "read the executable") {
			t.Fatalf("Launch = %v", err)
		}
	})

	t.Run("open files", func(t *testing.T) {
		t.Parallel()

		s, p := verifiedStub(t)
		setMacOS(t, s, p, guestDarwin)

		if err := os.WriteFile(filepath.Join(filepath.Dir(s.bin), "ps.out"), []byte("111 tart run x\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		writeExecutable(t, p.lsof, "#!/bin/sh\necho 'lsof: WARNING: cannot stat' >&2\nexit 1\n")

		if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err == nil ||
			!strings.Contains(err.Error(), "see who holds") {
			t.Fatalf("Launch = %v", err)
		}
	})
}

// TWO DEPLOYMENTS SHARING A STORE CANNOT TOGETHER RUN MORE THAN THE SMALLER ALLOWS, and the
// count changes only while no slot is held and every deployment names one limit.
func TestASlotCountThatDiffersRefuses(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	hold(t, s)

	if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err != nil {
		t.Fatal(err)
	}

	p.macOSLimit = func() (int, bool, error) { return 3, true, nil }

	_, err := p.Launch(t.Context(), validSpec(verifiedLeaseB))
	if err == nil || !strings.Contains(err.Error(), "changes only while no macOS guest holds a slot") {
		t.Fatalf("with a slot held = %v", err)
	}

	if _, err := p.Destroy(t.Context(), verifiedLease); err != nil {
		t.Fatal(err)
	}

	waitSlotFree(t, p, 1)

	other := "fedcba9876543210fedcba9876543210"
	if err := writeSmallFile(p.billetPath(limitPrefix+other), "2"); err != nil {
		t.Fatal(err)
	}

	_, err = p.Launch(t.Context(), validSpec(verifiedLeaseB))
	if err == nil || !strings.Contains(err.Error(), other) {
		t.Fatalf("with another deployment's limit = %v, want it named", err)
	}

	if err := os.Remove(p.billetPath(limitPrefix + other)); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Launch(t.Context(), validSpec(verifiedLeaseB)); err != nil {
		t.Fatalf("every deployment naming one limit: %v", err)
	}

	raw, err := os.ReadFile(p.billetPath(slotsCount))
	if err != nil || strings.TrimSpace(string(raw)) != "3" {
		t.Fatalf("recorded count = %q, %v", raw, err)
	}
}

// WITH NO RECORD OF THE LIMIT, a probe refuses and a launch uses Apple's default.
func TestAnUnknownLimitRefusesAProbeAndDefaultsALaunch(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	p.macOSLimit = func() (int, bool, error) { return 0, false, nil }

	_, err := p.Verify(t.Context(), verifyOpts())
	if err == nil || !strings.Contains(err.Error(), "no record of its macOS limit") {
		t.Fatalf("Verify = %v", err)
	}

	// NOT A VERDICT ON THE IMAGE: what was pulled waits for the next verification.
	images, err := p.VerifiedImages(t.Context())
	if err != nil || len(images) != 1 || images[0].Pending == nil || images[0].Failed != nil {
		t.Fatalf("images = %+v, %v", images, err)
	}

	if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	raw, err := os.ReadFile(p.billetPath(slotsCount))
	if err != nil || strings.TrimSpace(string(raw)) != "2" {
		t.Fatalf("recorded count = %q, %v", raw, err)
	}
}

// A MACOS PROBE GIVES ITS SLOT TO A JOB, and the launch that asked for it starts. What
// holds the probe in place here is the stub's guest: its xcodebuild never answers.
func TestAProbeYieldsItsSlotToAJob(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	hold(t, s)
	p.probeWindow = 20 * time.Second
	p.yieldWait = 15 * time.Second
	p.macOSLimit = func() (int, bool, error) { return 1, true, nil }

	reached := filepath.Join(filepath.Dir(s.bin), "probe-reached")
	writeExecutable(t, filepath.Join(s.guestBin, "xcodebuild"),
		"#!/bin/sh\necho reached > \""+reached+"\"\nexit 1\n")

	verified := make(chan error, 1)

	go func() {
		_, err := p.Verify(t.Context(), verifyOpts())
		verified <- err
	}()

	waitForFile(t, reached)

	if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := <-verified; !errors.Is(err, ErrYielded) {
		t.Fatalf("Verify = %v, want it to have yielded", err)
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || len(images) != 1 || images[0].Yields != 1 || images[0].Pending == nil {
		t.Fatalf("images = %+v, %v, want the yield counted and its base kept", images, err)
	}

	if len(images[0].Records) != 0 {
		t.Fatalf("a yield recorded %+v", images[0].Records)
	}

	// THE NEXT VERIFICATION BOOTS WHAT WAS PULLED, once the job has gone.
	if _, err := p.Destroy(t.Context(), verifiedLease); err != nil {
		t.Fatal(err)
	}

	waitSlotFree(t, p, 1)

	writeExecutable(t, filepath.Join(s.guestBin, "xcodebuild"), "#!/bin/sh\nexit 0\n")

	rec, err := p.Verify(t.Context(), verifyOpts())
	if err != nil {
		t.Fatalf("Verify again: %v", err)
	}

	if rec.Base != images[0].Pending.Base {
		t.Fatalf("recorded %s, want the kept %s", rec.Base, images[0].Pending.Base)
	}

	if pulls := envLines(t, s, "pull"); len(pulls) != 1 {
		t.Fatalf("pulls = %v, want the one", pulls)
	}

	after, err := p.VerifiedImages(t.Context())
	if err != nil || after[0].Pending != nil || after[0].Yields != 0 {
		t.Fatalf("images = %+v, %v, want nothing pending", after, err)
	}

	names, err := filepath.Glob(p.billetPath(yieldPrefix + "*"))
	if err != nil || len(names) != 0 {
		t.Fatalf("the launch left its request: %v, %v", names, err)
	}
}

// --no-yield KEEPS THE PROBE, and the launch fails as any launch does.
func TestAProbeThatMayNotYieldKeepsItsSlot(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	hold(t, s)
	p.probeWindow = 20 * time.Second
	p.yieldWait = 200 * time.Millisecond
	p.macOSLimit = func() (int, bool, error) { return 1, true, nil }

	reached := filepath.Join(filepath.Dir(s.bin), "probe-reached")
	release := filepath.Join(filepath.Dir(s.bin), "probe-release")
	writeExecutable(t, filepath.Join(s.guestBin, "xcodebuild"),
		"#!/bin/sh\necho reached > \""+reached+"\"\n[ -e \""+release+"\" ]\n")

	opts := verifyOpts()
	opts.NoYield = true

	verified := make(chan error, 1)

	go func() {
		_, err := p.Verify(t.Context(), opts)
		verified <- err
	}()

	waitForFile(t, reached)

	_, err := p.Launch(t.Context(), validSpec(verifiedLease))
	if !errors.Is(err, ErrNoSlot) {
		t.Fatalf("Launch = %v, want no slot", err)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := <-verified; err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A REQUEST OLDER THAN A LAUNCH'S WAIT IS A DEAD LAUNCH'S, which a probe ignores.
func TestAStaleYieldRequestIsIgnored(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	p.yieldWait = yieldWait

	if err := os.MkdirAll(p.billetPath(""), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := p.writeYieldRequest(p.billetPath(yieldPrefix+"x"), "x"); err != nil {
		t.Fatal(err)
	}

	if !p.yieldRequested() {
		t.Fatal("a fresh request was not seen")
	}

	p.now = func() time.Time { return now.Add(yieldWait) }

	if p.yieldRequested() {
		t.Fatal("a request as old as the wait was honoured")
	}
}

// startOrphanProbe leaves a probe as a dead verifier would: its guest's process running
// and holding its slot and custody, and no verification lock held.
func startOrphanProbe(t *testing.T, s *stub, p *Provider) string {
	t.Helper()

	hold(t, s)

	probe := p.probeName(verifiedRef)

	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", probe); err != nil {
		t.Fatal(err)
	}

	if err := p.writeOwner(probe); err != nil {
		t.Fatal(err)
	}

	custody, err := openLockFile(p.custodyPath(probe))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tryFlock(custody, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	claim, err := p.claimGuest(t.Context(), probe, guestDarwin, true)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := p.startDetachedHolding(probe, append(claim.files, custody)); err != nil {
		t.Fatal(err)
	}

	claim.release()
	_ = custody.Close()

	waitForFile(t, filepath.Join(p.vmDir(probe), ".held"))

	return probe
}

// A PROBE WHOSE VERIFIER DIED keeps its slot through its guest's process. A launch that
// needs it and finds the verification lock free, which proves no verifier owns it,
// destroys it. Its processes are found as its custody file's holders.
func TestALaunchRemovesAProbeNoVerifierOwns(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	p.macOSLimit = func() (int, bool, error) { return 1, true, nil }

	probe := startOrphanProbe(t, s, p)

	if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if _, err := os.Stat(p.vmDir(probe)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe survived: %v", err)
	}
}

// IT NEVER TOUCHES A PROBE WHOSE VERIFICATION LOCK IS HELD, which it asks to yield.
func TestALaunchLeavesAProbeWithAVerifierAlone(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	p.macOSLimit = func() (int, bool, error) { return 1, true, nil }
	p.yieldWait = 200 * time.Millisecond

	probe := startOrphanProbe(t, s, p)

	verifier, err := lockPath(t.Context(), p.verificationLockPath(hashName(verifiedRef)), syscall.LOCK_EX, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer verifier.release()

	_, err = p.Launch(t.Context(), validSpec(verifiedLease))
	if !errors.Is(err, ErrNoSlot) {
		t.Fatalf("Launch = %v, want no slot", err)
	}

	if _, err := os.Stat(p.vmDir(probe)); err != nil {
		t.Fatalf("the probe was touched: %v", err)
	}
}

// DESTRUCTION IS SCOPED BY DEPLOYMENT: another deployment's probe is that deployment's.
func TestALaunchLeavesAnotherDeploymentsProbeAlone(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	removed, err := p.removeOrphanProbe(t.Context(), probePrefix+"fedcba9876543210fedcba9876543210-abc")
	if err != nil || removed {
		t.Fatalf("removed = %v, %v", removed, err)
	}
}

// ONLY WHAT IT POSITIVELY IDENTIFIES: tart or softnet, holding the file, with the same
// start before and after. A billet trying the lock, or anything else, is left alone.
func TestOnlyAProbesOwnProcessesAreSignalled(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)

	path := p.custodyPath("probe")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	lsof := filepath.Join(filepath.Dir(s.bin), "lsof-"+filepath.Base(path))
	if err := os.WriteFile(lsof, []byte("10\n20\n30\n40\n50\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	exes := map[int]string{10: "/opt/homebrew/bin/tart", 20: "/opt/homebrew/bin/softnet", 30: "/usr/local/bin/billet"}
	starts := map[int]uint64{10: 1, 20: 2, 30: 3, 50: 5}

	p.processPath = func(pid int) (string, error) {
		if exe, ok := exes[pid]; ok {
			return exe, nil
		}

		return "", fmt.Errorf("no process %d", pid)
	}
	p.processStart = func(pid int) (uint64, error) {
		start, ok := starts[pid]
		if !ok {
			return 0, errors.New("gone")
		}

		return start, nil
	}

	var signalled []int

	p.signal = func(pid int, sig syscall.Signal) error {
		signalled = append(signalled, pid)

		if sig != syscall.SIGTERM {
			t.Errorf("signal %v", sig)
		}

		return nil
	}

	if err := p.signalCustodyHolders(t.Context(), path, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(signalled, []int{10, 20}) {
		t.Fatalf("signalled %v, want tart and softnet alone", signalled)
	}
}

// A VERIFICATION REMOVES WHAT AN EARLIER ONE LEFT, under the image's lock.
func TestAVerificationRemovesWhatAnEarlierOneLeft(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestLinux)

	kept := mustVerify(t, p)

	prefix := basePrefix + testOwner + "-" + hashName(verifiedRef) + "-"

	for _, name := range []string{prefix + "90", prefix + "91" + stagingSuffix, p.probeName(verifiedRef)} {
		if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", name); err != nil {
			t.Fatal(err)
		}
	}

	other := basePrefix + testOwner + "-" + hashName("ghcr.io/other/image:1") + "-1"
	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", other); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(p.privateStore(verifiedRef), "leftover")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}

	setRegistry(t, s, p, "manifest two")
	if err := os.WriteFile(s.imageBytes, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}

	next := mustVerify(t, p)

	entries, err := os.ReadDir(filepath.Join(p.home, "vms"))
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}

	slices.Sort(got)

	want := []string{kept.Base, next.Base, other}
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Fatalf("the store holds %v, want %v", got, want)
	}
}

// A BASE ANOTHER DEPLOYMENT MARKED IS NOT THIS ONE'S TO DELETE, whatever it is named.
func TestAVerifiedVMMarkedByAnotherDeploymentIsNotDeleted(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	name := basePrefix + testOwner + "-abc-1"
	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", name); err != nil {
		t.Fatal(err)
	}

	err := os.WriteFile(filepath.Join(p.vmDir(name), ownerMarker), []byte("fedcba9876543210fedcba9876543210\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.deleteOwnedVM(t.Context(), name); err == nil {
		t.Fatal("deleted another deployment's VM")
	}

	if _, err := os.Stat(p.vmDir(name)); err != nil {
		t.Fatalf("it is gone: %v", err)
	}
}

func TestOneVerificationOfAnImageAtATime(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	held, err := lockPath(t.Context(), p.verificationLockPath(hashName(verifiedRef)), syscall.LOCK_EX, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer held.release()

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil || !strings.Contains(err.Error(), "another verification") {
		t.Fatalf("Verify = %v", err)
	}
}

func TestAStateDirectoryOfAnotherDeploymentRefuses(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	mustVerify(t, p)

	other, err := New("fedcba9876543210fedcba9876543210", WithHome(p.home), WithStateDir(p.stateDir))
	if err != nil {
		t.Fatal(err)
	}

	_, err = other.VerifiedImages(t.Context())
	if err == nil || !regexp.MustCompile(`records deployment 0123`).MatchString(err.Error()) {
		t.Fatalf("VerifiedImages = %v", err)
	}
}

func TestAReferenceThatCannotBeVerifiedRefuses(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	for name, opts := range map[string]VerifyOptions{
		"no image":   {Command: []string{"./run.sh"}},
		"an option":  {Image: "--help", Command: []string{"./run.sh"}},
		"a digest":   {Image: "ghcr.io/a/b@sha256:0", Command: []string{"./run.sh"}},
		"no command": {Image: "ghcr.io/a/b:1"},
		"a local VM with a digest": {
			Image: "my-vm", Digest: "sha256:0", Command: []string{"./run.sh"},
		},
	} {
		if _, err := p.Verify(t.Context(), opts); err == nil {
			t.Errorf("%s: verified", name)
		}
	}

	// UNTRUSTED NETWORK FLAGS OR NOTHING: a probe never falls back to tart's default NAT.
	opts := verifyOpts()
	opts.GuestUntrusted = true

	if _, err := p.Verify(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "isolation") {
		t.Fatalf("Verify = %v", err)
	}
}

// A DIGEST WITHDRAWN IS NEVER RESOLVED, even in a document naming a record of it, which no
// path of billet's writes and resolution holds against anyway.
func TestResolveSkipsAWithdrawnDigest(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	if err := p.withRecords(t.Context(), func(st *verifiedState) (bool, error) {
		st.verified.Withdrawn[verifiedRef] = []string{rec.Digest}

		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err == nil ||
		!strings.Contains(err.Error(), "no verified base") {
		t.Fatalf("Launch = %v", err)
	}
}

// A BASE MOVES IN BY A RENAME, which another filesystem cannot do. Refused by name before a
// pull of tens of gigabytes rather than by the rename after it.
func TestAPrivateStoreOnAnotherFilesystemRefuses(t *testing.T) {
	t.Parallel()

	const other = "/dev/shm"

	if _, err := os.Stat(other); err != nil {
		t.Skip("no second filesystem to stage this on")
	}

	s, p := verifiedStub(t)

	if err := sameDevice(other, filepath.Join(p.home, "vms")); err == nil {
		t.Skip(other + " is on the store's own filesystem")
	}

	state, err := os.MkdirTemp(other, "billet-tart-*") //nolint:usetesting // t.TempDir is on the filesystem this must not be
	if err != nil {
		t.Skipf("cannot write to %s: %v", other, err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(state) })

	p.stateDir = state

	_, err = p.Verify(t.Context(), verifyOpts())
	if err == nil || !strings.Contains(err.Error(), "different filesystems") {
		t.Fatalf("Verify = %v", err)
	}

	if lines := envLines(t, s, "pull"); len(lines) != 0 {
		t.Fatalf("pulled before it knew the base could move: %v", lines)
	}
}

// TART'S OWN LOCK IS THE ONE ITS CLONE HOLDS AROUND MOVING A VM IN, so a move that did not
// take it could race a clone in the store.
func TestABaseMovesUnderTartsOwnLock(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	p.storeLockWindow = 100 * time.Millisecond

	from := t.TempDir()
	to := p.vmDir("moved")

	home, err := os.Open(p.home)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = home.Close() }()

	if err := syscall.Flock(int(home.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	err = p.moveVMDirectory(t.Context(), from, to)
	if err == nil || !strings.Contains(err.Error(), "tart's own lock") {
		t.Fatalf("moved while tart held its lock: %v", err)
	}

	if _, err := os.Stat(to); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the VM moved: %v", err)
	}

	if err := syscall.Flock(int(home.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	if err := p.moveVMDirectory(t.Context(), from, to); err != nil {
		t.Fatalf("with the lock free: %v", err)
	}

	// AN OCCUPIED NAME REFUSES, since a rename would replace an empty directory silently.
	if err := p.moveVMDirectory(t.Context(), t.TempDir(), to); err == nil ||
		!strings.Contains(err.Error(), "already carries") {
		t.Fatalf("over an occupied name: %v", err)
	}
}

// THE OPERATING SYSTEM DECIDES WHETHER APPLE'S LIMIT APPLIES. No `os` is darwin, as tart's own
// decoder reads one, and an answer billet cannot read refuses rather than guess Linux.
func TestAConfigWithNoOSIsMacOS(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	for config, want := range map[string]string{
		`{"macAddress":"02:00:00:00:00:01"}`: guestDarwin,
		`{"os":"darwin"}`:                    guestDarwin,
		`{"os":"linux"}`:                     guestLinux,
		`{"os":"plan9"}`:                     "",
		`{`:                                  "",
	} {
		if err := os.MkdirAll(p.vmDir("vm"), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(p.vmDir("vm"), baseConfig), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}

		got, err := p.guestOSOf("vm")
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%s: answered %q, %v, want %q", config, got, err, want)
		}
	}

	if err := os.Remove(filepath.Join(p.vmDir("vm"), baseConfig)); err != nil {
		t.Fatal(err)
	}

	if _, err := p.guestOSOf("vm"); err == nil {
		t.Error("no configuration at all answered an operating system")
	}
}

// A READ THAT FAILED IS NOT A DIFFERENCE: it says nothing about the base, and marking one
// invalid on it would take a tier off the Mac for a passing fault.
func TestAReadThatFailedMarksNoBaseInvalid(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	// A file where the directory was answers ENOTDIR, which is not absence.
	saved := p.vmDir(rec.Base) + ".saved"
	if err := os.Rename(p.vmDir(rec.Base), saved); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p.vmDir(rec.Base), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := p.Launch(t.Context(), verifiedSpec(verifiedLease))
	if err == nil || !strings.Contains(err.Error(), "read base") {
		t.Fatalf("Launch = %v", err)
	}

	if err := os.Remove(p.vmDir(rec.Base)); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(saved, p.vmDir(rec.Base)); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLeaseB)); err != nil {
		t.Fatalf("Launch once the fault passed: %v", err)
	}
}

// A RECORD THAT CANNOT BE READ IS NOT AN ABSENT ONE. Read as absent, the next verification's
// cleanup would find every base unnamed and delete them all.
func TestARecordThatCannotBeReadDeletesNothing(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	if err := os.WriteFile(filepath.Join(p.stateDir, verifiedFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified over a record it could not read")
	}

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err == nil {
		t.Fatal("launched from a record it could not read")
	}

	if _, err := os.Stat(p.vmDir(rec.Base)); err != nil {
		t.Fatalf("the base is gone: %v", err)
	}
}

// A LAUNCH THAT RESOLVED ITS BASE COMPLETES ITS CLONE: an unpromote coming after waits under
// the base's lock, and then removes it.
func TestALaunchThatResolvedItsBaseCompletesItsClone(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	rec := mustVerify(t, p)

	if err := os.WriteFile(s.cloneBlocks, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}

	launched := make(chan error, 1)

	go func() {
		_, err := p.Launch(t.Context(), verifiedSpec(verifiedLease))
		launched <- err
	}()

	waitForFile(t, s.cloneReached)

	unpromoted := make(chan error, 1)

	go func() {
		_, err := p.Unpromote(t.Context(), verifiedRef, rec.Base)
		unpromoted <- err
	}()

	// THE RECORD GOES FIRST, which is the point the delete waits past.
	deadline := time.Now().Add(20 * time.Second)

	for {
		images, err := p.VerifiedImages(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		if len(images) == 1 && len(images[0].Records) == 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the record never went")
		}

		time.Sleep(5 * time.Millisecond)
	}

	if _, err := os.Stat(p.vmDir(rec.Base)); err != nil {
		t.Fatalf("the base went while a launch was cloning it: %v", err)
	}

	if err := os.WriteFile(s.cloneRelease, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := <-launched; err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := <-unpromoted; err != nil {
		t.Fatalf("Unpromote: %v", err)
	}

	assertBases(t, p)
}

// THE LOCK IS THE CLONE'S, not the launch's: a boot and a registration take minutes, and a
// delete must not wait them out. The guest's agent here answers only once the base is gone.
func TestALaunchLetsGoOfItsBaseOnceItHasCloned(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	rec := mustVerify(t, p)

	reached := filepath.Join(filepath.Dir(s.bin), "started")
	release := filepath.Join(filepath.Dir(s.bin), "go-on")

	if err := os.Rename(s.bin, s.bin+".real"); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, s.bin, "#!/bin/sh\n\"$0.real\" \"$@\"\nrc=$?\n"+
		"if [ \"$1\" = run ]; then echo started > \""+reached+"\"; fi\n"+
		"if [ \"$1\" = exec ]; then n=0; while [ ! -e \""+release+"\" ] && [ \"$n\" -lt 400 ]; do sleep 0.05; n=$((n + 1)); done; fi\n"+
		"exit $rc\n")

	launched := make(chan error, 1)

	go func() {
		_, err := p.Launch(t.Context(), verifiedSpec(verifiedLease))
		launched <- err
	}()

	waitForFile(t, reached)

	base, err := openLockFile(p.baseLockPath(rec.Base))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = base.Close() }()

	free, err := tryFlock(base, syscall.LOCK_EX)
	if err != nil || !free {
		t.Errorf("a booting launch still holds its base: free=%v, %v", free, err)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := <-launched; err != nil {
		t.Fatalf("Launch: %v", err)
	}
}

// UNTRUSTED WORK'S PROBE BOOTS AS UNTRUSTED WORK BOOTS: a third party's image never gets the
// default NAT because the tier that names it is isolated, and its guest proves it resolves.
func TestAnIsolatedProbeBootsIsolatedAndProvesItResolves(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	p.cfg.UntrustedIsolation = "softnet"
	p.cfg.UntrustedDNS = []string{"1.1.1.1"}

	// Whatever the host's own resolver says: the script's first question is answered here,
	// so it does nothing to the host running this test.
	writeExecutable(t, filepath.Join(s.guestBin, "getent"), "#!/bin/sh\nexit 0\n")

	opts := verifyOpts()
	opts.GuestUntrusted = true

	if _, err := p.Verify(t.Context(), opts); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	runs := envLines(t, s, "run "+p.probeName(verifiedRef))
	if len(runs) != 1 || !strings.Contains(runs[0], "--net-softnet --net-softnet-block=@host") {
		t.Fatalf("runs = %v, want the probe isolated", runs)
	}

	raw, err := os.ReadFile(s.argvLog)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "getent hosts github.com") {
		t.Fatal("the probe never asked the guest to resolve")
	}

	// AND A GUEST THAT CANNOT, whose probe never records.
	writeExecutable(t, filepath.Join(s.guestBin, "getent"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(s.guestBin, "sudo"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(s.guestBin, "resolvectl"), "#!/bin/sh\nexit 1\n")

	p.resolverWaits = 0
	p.probeWindow = 200 * time.Millisecond

	setRegistry(t, s, p, "manifest two")

	if _, err := p.Verify(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "resolving github.com") {
		t.Fatalf("Verify = %v", err)
	}
}

// THE CASE SIGNALS EXIST FOR: a guest still starting, waiting on tart's lock, where `tart
// stop` reports it stopped and deleting its directory ends nothing. Its process is found
// as its custody file's holder, asked, and then told.
func TestAProbeThatWillNotStopIsEndedThroughItsCustodyFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// ignoresTerm leaves the process running through SIGTERM.
		ignoresTerm bool
		want        []syscall.Signal
	}{
		{name: "it ends when asked", want: []syscall.Signal{syscall.SIGTERM}},
		{name: "it must be told", ignoresTerm: true, want: []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, p := verifiedStub(t)
			setMacOS(t, s, p, guestDarwin)
			p.stopWindow = 150 * time.Millisecond

			if err := os.WriteFile(s.stopNoop, nil, 0o600); err != nil {
				t.Fatal(err)
			}

			probe := startOrphanProbe(t, s, p)

			raw, err := os.ReadFile(filepath.Join(p.vmDir(probe), ".held"))
			if err != nil {
				t.Fatal(err)
			}

			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}

			lsof := filepath.Join(filepath.Dir(s.bin), "lsof-"+filepath.Base(p.custodyPath(probe)))
			if err := os.WriteFile(lsof, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			p.processPath = func(int) (string, error) { return "/opt/homebrew/bin/tart", nil }
			p.processStart = func(int) (uint64, error) { return 1, nil }

			var signals []syscall.Signal

			p.signal = func(got int, sig syscall.Signal) error {
				if got != pid {
					t.Errorf("signalled %d, want %d", got, pid)
				}

				signals = append(signals, sig)

				if sig == syscall.SIGTERM && tc.ignoresTerm {
					return nil
				}

				return syscall.Kill(got, sig)
			}

			if err := p.endProbe(t.Context(), probe); err != nil {
				t.Fatalf("endProbe: %v", err)
			}

			if !slices.Equal(signals, tc.want) {
				t.Fatalf("signals = %v, want %v", signals, tc.want)
			}

			if _, err := os.Stat(p.vmDir(probe)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the probe survived: %v", err)
			}
		})
	}
}

// A PID WHOSE START MOVED BETWEEN TWO READS IS ANOTHER PROCESS, and one that let go of the
// file is no longer the probe's.
func TestAHolderThatChangedBetweenTwoReadsIsNotSignalled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		starts []uint64
		second string
	}{
		{name: "its start moved", starts: []uint64{1, 2}, second: "10\n"},
		{name: "it let go of the file", starts: []uint64{1, 1}, second: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, p := verifiedStub(t)
			setMacOS(t, s, p, guestDarwin)

			path := p.custodyPath("probe")
			lsof := filepath.Join(filepath.Dir(s.bin), "lsof-"+filepath.Base(path))

			if err := os.WriteFile(lsof, []byte("10\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			reads := 0
			p.processStart = func(int) (uint64, error) {
				start := tc.starts[min(reads, len(tc.starts)-1)]
				reads++

				return start, nil
			}
			p.processPath = func(int) (string, error) {
				// BETWEEN THE TWO READS OF WHO HOLDS IT.
				if tc.second != "10\n" {
					if err := os.WriteFile(lsof, []byte(tc.second), 0o600); err != nil {
						t.Error(err)
					}
				}

				return "/opt/homebrew/bin/tart", nil
			}
			p.signal = func(pid int, _ syscall.Signal) error {
				t.Errorf("signalled %d", pid)

				return nil
			}

			if tc.second == "" {
				// An empty answer from the real lsof exits 1, which is how it says none.
				writeExecutable(t, p.lsof, "#!/bin/sh\nf=\""+lsof+"\"\n[ -s \"$f\" ] && cat \"$f\" && exit 0\nexit 1\n")
			}

			if err := p.signalCustodyHolders(t.Context(), path, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A DELETION NEVER RESTS ON A LOCK FILE IT JUST MADE: a probe with no custody file goes only
// on tart's own word that it is stopped.
func TestAProbeWithNoCustodyFileIsDeletedOnlyIfTartSaysStopped(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)

	probe := p.probeName(verifiedRef)

	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", probe); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(p.vmDir(probe), ".state"), []byte("running\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := p.endProbe(t.Context(), probe)
	if err == nil || !strings.Contains(err.Error(), "no custody file") {
		t.Fatalf("endProbe = %v", err)
	}

	if _, serr := os.Stat(p.vmDir(probe)); serr != nil {
		t.Fatalf("a running probe was deleted: %v", serr)
	}

	if _, err := p.run(t.Context(), "stop", probe); err != nil {
		t.Fatal(err)
	}

	if err := p.endProbe(t.Context(), probe); err != nil {
		t.Fatalf("endProbe once stopped: %v", err)
	}

	if _, serr := os.Stat(p.vmDir(probe)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the stopped probe survived: %v", serr)
	}
}

// A YIELD WHOSE PROBE MAY STILL BE ALIVE IS NOT THE CLEAN YIELD a refresh reports as nothing
// wrong.
func TestAYieldNotProvedGoneIsNotAYield(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	hold(t, s)
	p.probeWindow = 20 * time.Second
	p.stopWindow = 50 * time.Millisecond
	p.macOSLimit = func() (int, bool, error) { return 1, true, nil }
	p.signal = func(int, syscall.Signal) error { return nil }

	if err := os.WriteFile(s.stopNoop, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	reached := filepath.Join(filepath.Dir(s.bin), "probe-reached")
	writeExecutable(t, filepath.Join(s.guestBin, "xcodebuild"), "#!/bin/sh\necho reached > \""+reached+"\"\nexit 1\n")

	verified := make(chan error, 1)

	go func() {
		_, err := p.Verify(t.Context(), verifyOpts())
		verified <- err
	}()

	waitForFile(t, reached)

	if err := p.writeYieldRequest(p.billetPath(yieldPrefix+"job"), "job"); err != nil {
		t.Fatal(err)
	}

	err := <-verified
	if err == nil || !strings.Contains(err.Error(), "could not be proved gone") {
		t.Fatalf("Verify = %v", err)
	}

	if errors.Is(err, ErrYielded) {
		t.Fatalf("reported as a clean yield: %v", err)
	}
}

// A RECORD OF THE LIMIT THAT CANNOT BE READ IS NOT THE ABSENCE OF ONE, for a launch too: the
// number a start uses becomes the store's count.
func TestAnUnreadableLimitRefusesALaunch(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	p.macOSLimit = func() (int, bool, error) { return 0, false, errors.New("corrupt") }

	_, err := p.Launch(t.Context(), validSpec(verifiedLease))
	if err == nil || !strings.Contains(err.Error(), "read this node's macOS limit") {
		t.Fatalf("Launch = %v", err)
	}

	if _, serr := os.Stat(p.billetPath(slotsCount)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("a count was recorded: %v", serr)
	}
}

// BOTH WAYS: the deployment whose number the store recorded refuses too, or it would run up
// to it past what the smaller allows.
func TestADeploymentAgreeingWithTheCountStillRefusesASmallerLimitBesideIt(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)

	if _, err := p.Launch(t.Context(), validSpec(verifiedLease)); err != nil {
		t.Fatal(err)
	}

	other := "fedcba9876543210fedcba9876543210"
	if err := writeSmallFile(p.billetPath(limitPrefix+other), "1"); err != nil {
		t.Fatal(err)
	}

	_, err := p.Launch(t.Context(), validSpec(verifiedLeaseB))
	if err == nil || !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), "remove") {
		t.Fatalf("Launch = %v, want the other deployment and the remedy named", err)
	}
}

// A BASE THE RECORDS NAME IS KEPT WHEN THE COMMIT'S WRITE REPORTED FAILURE: a durable install
// reports a directory sync that failed after its rename, so the record can be there, and
// deleting would leave a launch resolving a base that is gone.
func TestABaseARecordNamesSurvivesACommitThatReportedFailure(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	// THE STATE'S FIRST WRITE PRECEDES THE RECORD, so what fails is the sync after the
	// install that first carries a record: the rename has landed and its report has not.
	p.installer.SyncDir = failOnFile(p, verifiedFile)

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("Verify reported success through a failed sync")
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(images) != 1 || len(images[0].Records) != 1 {
		t.Fatalf("images = %+v, want the installed record", images)
	}

	if _, err := os.Stat(p.vmDir(images[0].Records[0].Base)); err != nil {
		t.Fatalf("the base its record names was deleted: %v", err)
	}
}

// failOnFile fails the directory sync that follows the install of one record file, once,
// which is the moment a rename has landed and its report has not.
func failOnFile(p *Provider, name string) func(string) error {
	failed := false

	return func(dir string) error {
		raw, err := os.ReadFile(filepath.Join(p.stateDir, name))
		if err == nil && !failed && strings.Contains(string(raw), `"content_hash"`) {
			failed = true

			return errors.New("the directory would not sync")
		}

		return syncDir(dir)
	}
}

// AND A BASE NO RECORD NAMES IS REMOVED, by the same failed commit reaching no install.
func TestABaseNoRecordNamesGoesWhenACommitFailed(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	opts := verifyOpts()
	opts.Window = func(ctx context.Context, _ string) (RunnerAnswer, error) {
		return RunnerAnswer{Deadline: p.now().Add(-time.Hour)}, nil
	}

	if _, err := p.Verify(t.Context(), opts); err == nil {
		t.Fatal("Verify recorded a runner past a date it was given")
	}

	assertNoBases(t, p)
}

// A TAKE-BACK CLEARS THE WITHDRAWAL AND INSTALLS ITS RECORD AS ONE WRITE, so a failure leaves
// the digest withdrawn and no base of it serving, never withdrawal gone and the old base back.
func TestATakeBackThatCouldNotBeInstalledLeavesTheDigestWithdrawn(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	if _, err := p.Unpromote(t.Context(), verifiedRef, rec.Base); err != nil {
		t.Fatal(err)
	}

	opts := verifyOpts()
	opts.Digest = rec.Digest
	opts.Window = func(context.Context, string) (RunnerAnswer, error) {
		p.installer.Rename = func(string, string) error { return errors.New("the disk is full") }

		return RunnerAnswer{}, nil
	}

	if _, err := p.Verify(t.Context(), opts); err == nil {
		t.Fatal("reported success through a failed install")
	}

	p.installer.Rename = nil

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err == nil ||
		!strings.Contains(err.Error(), "no verified base") {
		t.Fatalf("Launch = %v", err)
	}

	due, err := p.ImageDue(t.Context(), verifiedTier)
	if err != nil || due.Verify || !strings.Contains(due.Why, "withdrawn") {
		t.Fatalf("ImageDue = %+v, %v, want the digest still withdrawn", due, err)
	}
}

// UNPROMOTE SAYS WHAT IT COULD NOT REMOVE: a retry has no record to act on, so a silent failure
// would leave tens of gigabytes behind a report that it was removed.
func TestAnUnpromoteThatCouldNotRemoveItsBaseSaysSo(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)
	p.storeLockWindow = 50 * time.Millisecond

	held, err := lockPath(t.Context(), p.baseLockPath(rec.Base), syscall.LOCK_SH, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer held.release()

	_, err = p.Unpromote(t.Context(), verifiedRef, rec.Base)
	if err == nil || !strings.Contains(err.Error(), "still on the Mac") {
		t.Fatalf("Unpromote = %v", err)
	}

	if _, err := os.Stat(p.vmDir(rec.Base)); err != nil {
		t.Fatalf("the base went under a launch's lock: %v", err)
	}

	// THE RECORD IS GONE AND THE DIGEST WITHDRAWN WHATEVER THE BASE DID, and the image's next
	// verification removes what is left.
	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err == nil ||
		!strings.Contains(err.Error(), "no verified base") {
		t.Fatalf("Launch = %v", err)
	}

	held.release()

	p.storeLockWindow = 2 * time.Minute

	opts := verifyOpts()
	opts.Digest = rec.Digest

	if _, err := p.Verify(t.Context(), opts); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if _, err := os.Stat(p.vmDir(rec.Base)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the next verification left it: %v", err)
	}
}

// A HUNG DELETE LETS GO OF THE STORE, whichever path reached it: it holds the lock every
// deployment on the Mac takes to publish a guest.
func TestAHungDeleteLetsGoOfTheStoreLock(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	rec := mustVerify(t, p)
	p.deleteWindow = 100 * time.Millisecond

	if err := os.WriteFile(s.deleteHangs, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := p.Unpromote(t.Context(), verifiedRef, rec.Base)
	if err == nil || !strings.Contains(err.Error(), "still on the Mac") {
		t.Fatalf("Unpromote = %v", err)
	}

	unlock, err := p.lockStore(t.Context(), "prove it is free")
	if err != nil {
		t.Fatalf("the store is still held: %v", err)
	}

	unlock()
}

// AN ABSENCE THAT AUTHORISES A DELETE MUST BE DURABLE: with the record document's directory not
// synced, a crash could bring back a document naming what cleanup removed.
func TestCleanupDeletesNothingOnARemovalThatMayNotBeDurable(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	mustVerify(t, p)

	left := basePrefix + testOwner + "-" + hashName(verifiedRef) + "-90"
	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", left); err != nil {
		t.Fatal(err)
	}

	p.installer.SyncDir = func(string) error { return errors.New("the directory would not sync") }

	setRegistry(t, s, p, "manifest two")

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified with a directory that would not sync")
	}

	if _, err := os.Stat(p.vmDir(left)); err != nil {
		t.Fatalf("a base went on an absence that may not last: %v", err)
	}
}

// A DOCUMENT THE READER WOULD REFUSE IS NEVER INSTALLED, or one write would stop every verified
// launch on the Mac.
func TestARecordPastWhatTheReaderTakesIsNotInstalled(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	err := p.withRecords(t.Context(), func(st *verifiedState) (bool, error) {
		st.verified.Deadlines[strings.Repeat("x", recordLimit)] = time.Now()

		return true, nil
	})
	if err == nil || !strings.Contains(err.Error(), "past the") {
		t.Fatalf("withRecords = %v", err)
	}

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch from the document still there: %v", err)
	}

	if _, err := os.Stat(p.vmDir(rec.Base)); err != nil {
		t.Fatal(err)
	}
}

// CLEANUP IS BOUNDED, because one that hangs on a cancelled caller's context holds the store
// lock against every deployment on the Mac.
func TestCleanupAfterACancelledCallerHasItsOwnBound(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	cleanCtx, done := cleanupContext(ctx)
	defer done()

	if cleanCtx.Err() != nil {
		t.Fatalf("cleanup ran on its caller's cancellation: %v", cleanCtx.Err())
	}

	deadline, ok := cleanCtx.Deadline()
	if !ok || time.Until(deadline) > probeTeardownWindow {
		t.Fatalf("no bound: %v, %v", deadline, ok)
	}
}

// A FAULT ELSEWHERE IN THE STORE DOES NOT WEDGE A PROBE'S CLEANUP: tart's rows vouch for this VM,
// and an unreadable lease-named directory says nothing about it.
func TestAnUnrelatedCorpseDoesNotStopAProbeBeingRemoved(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)

	probe := p.probeName(verifiedRef)

	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", probe); err != nil {
		t.Fatal(err)
	}

	// A directory with a lease's name and no `tart list` row, which the stub's `get` calls
	// unreadable, is what the full inventory refuses over.
	if err := os.MkdirAll(p.vmDir(verifiedLease), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := p.list(t.Context()); err == nil {
		t.Fatal("the stub's inventory does not refuse this, so this stages nothing")
	}

	if err := p.endProbe(t.Context(), probe); err != nil {
		t.Fatalf("endProbe: %v", err)
	}
}

// A WITHDRAWN RECORD A CRASH LEFT IS NOT A ROLLBACK TARGET, and counting it would push out the
// usable one.
func TestRetentionDoesNotCountAWithdrawnBase(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)

	setImage := func(content string) *VerifiedRecord {
		t.Helper()
		setRegistry(t, s, p, "manifest "+content)

		if err := os.WriteFile(s.imageBytes, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		return mustVerify(t, p)
	}

	good := setImage("good")
	bad := setImage("bad")

	if err := p.withRecords(t.Context(), func(st *verifiedState) (bool, error) {
		st.verified.Withdrawn[verifiedRef] = []string{bad.Digest}

		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	next := setImage("next")
	assertBases(t, p, good.Base, next.Base)
}

// A DATE ALREADY STORED ANSWERS WHEN GITHUB DOES NOT: a launch would refuse this at once.
func TestAVerificationRefusesARunnerPastAStoredDateWhenGitHubIsSilent(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	if err := p.withRecords(t.Context(), func(st *verifiedState) (bool, error) {
		st.verified.Deadlines["2.337.0"] = p.now().Add(-time.Hour)

		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	opts := verifyOpts()
	opts.Window = func(context.Context, string) (RunnerAnswer, error) {
		return RunnerAnswer{}, errors.New("GitHub did not answer")
	}

	if _, err := p.Verify(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "past GitHub's window") {
		t.Fatalf("Verify = %v", err)
	}

	assertNoBases(t, p)
}

// A STATE DIRECTORY OF ANOTHER DEPLOYMENT REFUSES BEFORE ANYTHING IS REMOVED, and each
// deployment's private store is its own.
func TestAnotherDeploymentsStateDirectoryLosesNothing(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	mustVerify(t, p)

	store := p.privateStore(verifiedRef)
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}

	other, err := New("fedcba9876543210fedcba9876543210", WithBinary(p.tart), WithHome(p.home), WithStateDir(p.stateDir))
	if err != nil {
		t.Fatal(err)
	}

	if other.privateStore(verifiedRef) == store {
		t.Fatal("two deployments share a private store")
	}

	other.registry = p.registry

	if _, err := other.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified in another deployment's state directory")
	}

	if _, err := os.Stat(store); err != nil {
		t.Fatalf("the other deployment's private store was removed: %v", err)
	}
}

// A PROCESS STILL STARTING IS WHAT TART REPORTS AS STOPPED, so with no custody file its
// process is what refuses.
func TestAProbeWithNoCustodyFileAndARunningProcessIsNotDeleted(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)

	probe := p.probeName(verifiedRef)

	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", probe); err != nil {
		t.Fatal(err)
	}

	ps := []byte("111 /opt/homebrew/Cellar/tart/2.36.0/libexec/tart.app/Contents/MacOS/tart run " + probe + " --no-graphics\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(s.bin), "ps.out"), ps, 0o600); err != nil {
		t.Fatal(err)
	}

	err := p.endProbe(t.Context(), probe)
	if err == nil || !strings.Contains(err.Error(), "its process is running") {
		t.Fatalf("endProbe = %v", err)
	}

	if _, serr := os.Stat(p.vmDir(probe)); serr != nil {
		t.Fatalf("it was deleted: %v", serr)
	}
}

// A PRIVATE STORE A CRASH LEFT GOES WITH THE NEXT VERIFICATION OF ITS IMAGE, under its lock,
// whether or not that verification pulls anything.
func TestAPrivateStoreACrashLeftIsRemovedByTheNextVerification(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)

	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", "my-vm"); err != nil {
		t.Fatal(err)
	}

	left := filepath.Join(p.privateStore("my-vm"), "cache")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}

	opts := verifyOpts()
	opts.Image = "my-vm@verified"

	if _, err := p.Verify(t.Context(), opts); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the private store is still there: %v", err)
	}

	if lines := envLines(t, s, "pull"); len(lines) != 0 {
		t.Fatalf("pulled a local VM: %v", lines)
	}
}

// THE FAILURE PATH'S ABSENCE HAS TO LAST TOO: with the document's directory not synced, a crash
// could bring back a record naming what this removed.
func TestAFailedVerificationKeepsItsBaseOnAnAbsenceThatMayNotLast(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	base := basePrefix + testOwner + "-" + hashName(verifiedRef) + "-90"
	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", base); err != nil {
		t.Fatal(err)
	}

	p.installer.SyncDir = func(string) error { return errors.New("the directory would not sync") }

	p.discardUnrecordedBase(t.Context(), verifiedRef, base)

	if _, err := os.Stat(p.vmDir(base)); err != nil {
		t.Fatalf("the base went: %v", err)
	}

	p.installer.SyncDir = nil

	p.discardUnrecordedBase(t.Context(), verifiedRef, base)

	if _, err := os.Stat(p.vmDir(base)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the base stayed once the absence was durable: %v", err)
	}
}

// A MACOS VERIFICATION FINDS OUT IT CANNOT BOOT BEFORE IT PULLS: the image is tens of gigabytes, and
// a slot or a limit it cannot have would otherwise cost them for nothing.
func TestAMacOSVerificationRefusesBeforeItPulls(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		limit func() (int, bool, error)
		ps    string
		want  string
	}{
		{
			name:  "no record of the limit",
			limit: func() (int, bool, error) { return 0, false, nil },
			want:  "no record of its macOS limit",
		},
		{
			name:  "no slot",
			limit: func() (int, bool, error) { return 1, true, nil },
			ps:    "111 tart run someone\n",
			want:  "no macOS guest slot is free",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, p := verifiedStub(t)
			setMacOS(t, s, p, guestDarwin)
			p.macOSLimit = tc.limit

			if err := os.WriteFile(filepath.Join(filepath.Dir(s.bin), "ps.out"), []byte(tc.ps), 0o600); err != nil {
				t.Fatal(err)
			}

			opts := verifyOpts()
			opts.MacOS = true

			if _, err := p.Verify(t.Context(), opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Verify = %v", err)
			}

			if pulls := envLines(t, s, "pull"); len(pulls) != 0 {
				t.Fatalf("pulled first: %v", pulls)
			}

			// NO VERDICT, so a refresh tries it again tomorrow.
			images, err := p.VerifiedImages(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			for i := range images {
				if images[i].Failed != nil {
					t.Fatalf("recorded a failure: %+v", images[i])
				}
			}
		})
	}
}

// A LOCAL VM SAYS WHAT IT IS, whatever a tier does.
func TestALocalMacOSVMIsKnownBeforeItIsCloned(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	p.macOSLimit = func() (int, bool, error) { return 0, false, nil }

	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", "my-vm"); err != nil {
		t.Fatal(err)
	}

	if err := os.Truncate(s.envLog, 0); err != nil {
		t.Fatal(err)
	}

	opts := verifyOpts()
	opts.Image = "my-vm@verified"

	if _, err := p.Verify(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "no record of its macOS limit") {
		t.Fatalf("Verify = %v", err)
	}

	if clones := envLines(t, s, "clone"); len(clones) != 0 {
		t.Fatalf("cloned first: %v", clones)
	}
}

// A BASE WAITING FOR ANOTHER DIGEST IS NOT WHAT THIS ONE BOOTS, and goes.
func TestAPendingBaseOfAnotherDigestGoes(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	p.macOSLimit = func() (int, bool, error) { return 0, false, nil }

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified with no limit")
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || len(images) != 1 || images[0].Pending == nil {
		t.Fatalf("images = %+v, %v", images, err)
	}

	waiting := images[0].Pending.Base

	p.macOSLimit = func() (int, bool, error) { return 2, true, nil }

	setRegistry(t, s, p, "manifest two")

	rec, err := p.Verify(t.Context(), verifyOpts())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if rec.Base == waiting {
		t.Fatal("booted the base of another digest")
	}

	assertBases(t, p, rec.Base)
}

// A BASE WAITING WHOSE FILES CHANGED IS NOT WHAT WAS PULLED.
func TestAPendingBaseThatChangedIsNotBooted(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	p.macOSLimit = func() (int, bool, error) { return 0, false, nil }

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified with no limit")
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || len(images) != 1 || images[0].Pending == nil {
		t.Fatalf("images = %+v, %v", images, err)
	}

	waiting := images[0].Pending.Base

	if err := os.WriteFile(filepath.Join(p.vmDir(waiting), baseDisk), []byte("rewritten"), 0o600); err != nil {
		t.Fatal(err)
	}

	p.macOSLimit = func() (int, bool, error) { return 2, true, nil }

	rec, err := p.Verify(t.Context(), verifyOpts())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if rec.Base == waiting {
		t.Fatal("booted a base that changed")
	}
}

// AN UNPROMOTE HOLDS FOR WHAT WAS WAITING TOO: a base of that digest no longer boots.
func TestUnpromoteTakesAPendingBaseOfItsDigest(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	if err := p.withRecords(t.Context(), func(st *verifiedState) (bool, error) {
		waiting := *rec
		waiting.Base = rec.Base + "-waiting"
		st.verified.Pending[verifiedRef] = &waiting

		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", rec.Base+"-waiting"); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Unpromote(t.Context(), verifiedRef, rec.Base); err != nil {
		t.Fatal(err)
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || images[0].Pending != nil {
		t.Fatalf("images = %+v, %v", images, err)
	}

	if _, err := os.Stat(p.vmDir(rec.Base + "-waiting")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the base that was waiting stayed: %v", err)
	}
}

// THE REFRESH DOES NOT PULL WHAT FAILED AGAIN TOMORROW, for a week, and an operator is not held to
// that. A digest the tag has moved past is a new question.
func TestADigestThatFailedWaitsBeforeTheRefreshTriesAgain(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	p.probeWindow = 200 * time.Millisecond

	writeExecutable(t, filepath.Join(s.guestHome, "actions-runner", "bin", "Runner.Listener"), "#!/bin/sh\nexit 1\n")

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified a guest that would not answer")
	}

	due, err := p.ImageDue(t.Context(), verifiedTier)
	if err != nil || due.Verify || !strings.Contains(due.Why, "failed its verification") {
		t.Fatalf("ImageDue = %+v, %v", due, err)
	}

	p.now = func() time.Time { return now.Add(failedRetry) }

	if due, err := p.ImageDue(t.Context(), verifiedTier); err != nil || !due.Verify {
		t.Fatalf("a week on: %+v, %v", due, err)
	}

	p.now = func() time.Time { return now }

	setRegistry(t, s, p, "manifest two")

	if due, err := p.ImageDue(t.Context(), verifiedTier); err != nil || !due.Verify {
		t.Fatalf("a digest the tag moved to: %+v, %v", due, err)
	}

	setRegistry(t, s, p, "manifest one")

	writeExecutable(t, filepath.Join(s.guestHome, "actions-runner", "bin", "Runner.Listener"), "#!/bin/sh\nprintf '2.337.0\\n'\n")
	p.probeWindow = 2 * time.Second

	if _, err := p.Verify(t.Context(), verifyOpts()); err != nil {
		t.Fatalf("an operator's own try: %v", err)
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || images[0].Failed != nil {
		t.Fatalf("images = %+v, %v, want the failure forgotten", images, err)
	}
}

// A PULL THAT DID NOT FINISH IS THE NETWORK'S, not the image's, so tomorrow tries again.
func TestAPullThatDidNotFinishIsNoVerdict(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)

	if err := os.WriteFile(s.pullFails, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified through a failed pull")
	}

	if due, err := p.ImageDue(t.Context(), verifiedTier); err != nil || !due.Verify {
		t.Fatalf("ImageDue = %+v, %v", due, err)
	}
}

// AN EMPTY MARKER IS A CRASH BETWEEN ITS CREATION AND ITS WRITE, which would otherwise wedge every
// later verification of the image.
func TestAnEmptyMarkerInOurOwnNamesIsAMissingOne(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	name := basePrefix + testOwner + "-abc-1"
	if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", name); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(p.vmDir(name), ownerMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.deleteOwnedVM(t.Context(), name); err != nil {
		t.Fatalf("deleteOwnedVM: %v", err)
	}

	if _, err := os.Stat(p.vmDir(name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("it stayed: %v", err)
	}
}

// A PROBE NO VERIFICATION OWNS IS NAMED: a local VM's may never see another verification, and a
// Linux one takes no slot and is reserved nothing. One that has a verifier, or another
// deployment's, is not this report's.
func TestOrphanProbesAreNamed(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)

	orphan := p.probeName("ghcr.io/a/orphan:1")
	owned := p.probeName("ghcr.io/a/owned:1")
	foreign := probePrefix + "fedcba9876543210fedcba9876543210-abc"

	for _, name := range []string{orphan, owned, foreign} {
		if _, err := p.run(t.Context(), "clone", "ghcr.io/x/y", name); err != nil {
			t.Fatal(err)
		}
	}

	held, err := lockPath(t.Context(), p.verificationLockPath(hashName("ghcr.io/a/owned:1")), syscall.LOCK_EX, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer held.release()

	got, err := p.OrphanProbes(t.Context())
	if err != nil || !slices.Equal(got, []string{orphan}) {
		t.Fatalf("OrphanProbes = %v, %v, want %s", got, err, orphan)
	}
}

// WHAT A CHECK SAYS OF A BASE COMES FROM ITS FILES, and reading them marks nothing.
func TestAReportReadsABasesFilesAndMarksNothing(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	images, err := p.VerifiedImages(t.Context())
	if err != nil || images[0].BaseProblems[rec.Base] != "" {
		t.Fatalf("images = %+v, %v", images, err)
	}

	if err := os.RemoveAll(p.vmDir(rec.Base)); err != nil {
		t.Fatal(err)
	}

	images, err = p.VerifiedImages(t.Context())
	if err != nil || !strings.Contains(images[0].BaseProblems[rec.Base], "gone") {
		t.Fatalf("images = %+v, %v", images, err)
	}

	if images[0].Records[0].Invalid != "" {
		t.Fatalf("a report marked it invalid: %+v", images[0].Records[0])
	}
}

// A BASE THAT WAITED AND THEN FAILED ITS PROBE GOES, and the document stops naming it.
func TestAPendingBaseThatFailsItsProbeGoes(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	p.macOSLimit = func() (int, bool, error) { return 0, false, nil }

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified with no limit")
	}

	p.macOSLimit = func() (int, bool, error) { return 2, true, nil }
	p.probeWindow = 200 * time.Millisecond

	writeExecutable(t, filepath.Join(s.guestHome, "actions-runner", "bin", "Runner.Listener"), "#!/bin/sh\nexit 1\n")

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified a guest that would not answer")
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || images[0].Pending != nil || images[0].Failed == nil {
		t.Fatalf("images = %+v, %v", images, err)
	}

	assertNoBases(t, p)
}

// ONLY WHAT IS THE IMAGE'S IS A VERDICT: a guest that never came up, a clone a full disk refused and
// a commit that failed are this host's, and tomorrow's to try again.
func TestAFailureOfTheHostIsNoVerdictOnTheImage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, s *stub, p *Provider)
	}{
		{"a guest whose agent never came up", func(t *testing.T, s *stub, p *Provider) {
			t.Helper()
			p.probeWindow = 100 * time.Millisecond

			if err := os.WriteFile(s.execFails, []byte("100000"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a commit that failed", func(t *testing.T, _ *stub, p *Provider) {
			t.Helper()

			installs := 0
			p.installer.Rename = func(from, to string) error {
				installs++
				// The first install takes the generation; the next is the commit.
				if installs == 2 {
					return errors.New("the disk is full")
				}

				return os.Rename(from, to)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, p := verifiedStub(t)
			tc.stage(t, s, p)

			if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
				t.Fatal("verified")
			}

			p.installer.Rename = nil

			if err := os.Remove(s.execFails); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			due, err := p.ImageDue(t.Context(), verifiedTier)
			if err != nil || !due.Verify {
				t.Fatalf("ImageDue = %+v, %v, want tomorrow to try again", due, err)
			}
		})
	}
}

// WHAT IS THE IMAGE'S IS: bytes that are not what was asked for, a stacked image, and a runner
// past GitHub's window would each cost the same pull tomorrow for the same answer.
func TestAFailureOfTheImageIsAVerdict(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, s *stub, p *Provider, opts *VerifyOptions)
	}{
		{"a manifest that is not the digest", func(t *testing.T, _ *stub, p *Provider, _ *VerifyOptions) {
			t.Helper()
			p.registry = func(context.Context, string) (string, error) { return digestOf("another"), nil }
		}},
		{"a stacked image", func(t *testing.T, s *stub, _ *Provider, _ *VerifyOptions) {
			t.Helper()

			if err := os.WriteFile(s.cloneStacked, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a runner past its window", func(t *testing.T, _ *stub, _ *Provider, opts *VerifyOptions) {
			t.Helper()
			opts.Window = func(context.Context, string) (RunnerAnswer, error) { return RunnerAnswer{Expired: true}, nil }
		}},
		{"a toolchain that does not answer", func(t *testing.T, s *stub, p *Provider, _ *VerifyOptions) {
			t.Helper()
			p.probeWindow = 200 * time.Millisecond
			writeExecutable(t, filepath.Join(s.guestBin, "docker"), "#!/bin/sh\nexit 1\n")
		}},
		{"a date already stored", func(t *testing.T, _ *stub, p *Provider, _ *VerifyOptions) {
			t.Helper()

			if err := p.withRecords(t.Context(), func(st *verifiedState) (bool, error) {
				st.verified.Deadlines["2.337.0"] = p.now().Add(-time.Hour)

				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, p := verifiedStub(t)
			opts := verifyOpts()
			tc.stage(t, s, p, &opts)

			if _, err := p.Verify(t.Context(), opts); err == nil {
				t.Fatal("verified")
			}

			due, err := p.ImageDue(t.Context(), verifiedTier)
			if err != nil {
				t.Fatal(err)
			}

			if due.Verify || !strings.Contains(due.Why, "failed its verification") {
				t.Fatalf("ImageDue = %+v, want it left alone", due)
			}
		})
	}
}

// A NODE KEEPS WANTING WHAT IT LAUNCHED OR AN OPERATOR VERIFIED, for as long as GitHub's own window,
// so an image nothing uses stops costing tens of gigabytes per move of its tag.
func TestAnImageIsWantedWhileSomethingUsesIt(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	wanted := func() bool {
		t.Helper()

		images, err := p.VerifiedImages(t.Context())
		if err != nil || len(images) != 1 {
			t.Fatalf("images = %+v, %v", images, err)
		}

		return images[0].Wanted
	}

	mustVerify(t, p)

	if !wanted() {
		t.Fatal("an image just verified is not wanted")
	}

	p.now = func() time.Time { return now.Add(refreshInterest - time.Minute) }

	if _, err := p.Launch(t.Context(), verifiedSpec(verifiedLease)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	p.now = func() time.Time { return now.Add(2*refreshInterest - 2*time.Minute) }

	if !wanted() {
		t.Fatal("a launch did not keep it wanted")
	}

	p.now = func() time.Time { return now.Add(2 * refreshInterest) }

	if wanted() {
		t.Fatal("an image nothing used for a month is still wanted")
	}
}

// WHAT A VERIFICATION WAS ASKED WITH OUTLIVES IT, a failed or yielded one included, because a node
// whose file names no tier has nowhere else to read it.
func TestAVerificationThatNeverFinishedKeepsItsQuestion(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	p.probeWindow = 200 * time.Millisecond

	writeExecutable(t, filepath.Join(s.guestHome, "custom", "bin", "Runner.Listener"), "#!/bin/sh\nexit 1\n")

	opts := verifyOpts()
	opts.Command = []string{"./custom/run.sh"}
	opts.MacOS = false

	if _, err := p.Verify(t.Context(), opts); err == nil {
		t.Fatal("verified")
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || len(images) != 1 {
		t.Fatalf("images = %+v, %v", images, err)
	}

	if !images[0].Wanted || !slices.Equal(images[0].Command, opts.Command) || len(images[0].Records) != 0 {
		t.Fatalf("image = %+v", images[0])
	}
}

// AN UNPROMOTE WHILE A PROBE RAN SAYS THAT DIGEST IS NOT WANTED: its yield keeps no base of it.
func TestAYieldAfterAnUnpromoteKeepsNothing(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	hold(t, s)

	first := mustVerify(t, p)

	p.probeWindow = 20 * time.Second

	if err := os.WriteFile(s.imageBytes, []byte("another copy"), 0o600); err != nil {
		t.Fatal(err)
	}

	reached := filepath.Join(filepath.Dir(s.bin), "probe-reached")
	writeExecutable(t, filepath.Join(s.guestBin, "xcodebuild"), "#!/bin/sh\necho reached > \""+reached+"\"\nexit 1\n")

	verified := make(chan error, 1)

	go func() {
		opts := verifyOpts()
		opts.Digest = first.Digest
		_, err := p.Verify(t.Context(), opts)
		verified <- err
	}()

	waitForFile(t, reached)

	if _, err := p.Unpromote(t.Context(), verifiedRef, first.Base); err != nil {
		t.Fatal(err)
	}

	if err := p.writeYieldRequest(p.billetPath(yieldPrefix+"job"), "job"); err != nil {
		t.Fatal(err)
	}

	if err := <-verified; !errors.Is(err, ErrYielded) {
		t.Fatalf("Verify = %v", err)
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || images[0].Pending != nil {
		t.Fatalf("images = %+v, %v, want nothing kept", images, err)
	}

	assertNoBases(t, p)
}

// AN IMAGE NOTHING USES DOES NOT KEEP ITSELF WANTED by being verified by the refresh each time its
// tag moves.
func TestTheRefreshsOwnVerificationIsNoSignOfInterest(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	mustVerify(t, p)

	p.now = func() time.Time { return now.Add(refreshInterest - time.Minute) }

	setRegistry(t, s, p, "manifest two")

	opts := verifyOpts()
	opts.Automatic = true

	if _, err := p.Verify(t.Context(), opts); err != nil {
		t.Fatal(err)
	}

	p.now = func() time.Time { return now.Add(refreshInterest) }

	images, err := p.VerifiedImages(t.Context())
	if err != nil || images[0].Wanted {
		t.Fatalf("images = %+v, %v, want interest not renewed", images, err)
	}
}

// WHAT A BASE PROVED IT IS REACHES THE NEXT VERIFICATION, whatever it was asked with: the next
// move of the tag asks for a slot before it pulls, on a node whose file names no tier.
func TestAVerifiedMacOSImageIsKnownToTheNextVerification(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)

	opts := verifyOpts()
	opts.MacOS = false

	if _, err := p.Verify(t.Context(), opts); err != nil {
		t.Fatal(err)
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || !images[0].MacOS {
		t.Fatalf("images = %+v, %v", images, err)
	}
}

// A BASE WAITING SAYS WHAT IT IS TOO.
func TestAWaitingBaseSaysWhatItIs(t *testing.T) {
	t.Parallel()

	s, p := verifiedStub(t)
	setMacOS(t, s, p, guestDarwin)
	p.macOSLimit = func() (int, bool, error) { return 0, false, nil }

	if _, err := p.Verify(t.Context(), verifyOpts()); err == nil {
		t.Fatal("verified with no limit")
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || images[0].Pending == nil || !images[0].MacOS {
		t.Fatalf("images = %+v, %v", images, err)
	}
}

// A FIRST VERIFICATION A REGISTRY OUTAGE STOPPED IS STILL WANTED: on a node whose file names no tier,
// the refresh has nowhere else to learn of it, and tries it again from what this kept.
func TestAFirstVerificationARegistryStoppedIsStillWanted(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	p.registry = func(context.Context, string) (string, error) { return "", errors.New("the registry is down") }

	opts := verifyOpts()
	opts.Command = []string{"./custom/run.sh"}
	opts.GuestUntrusted = false

	if _, err := p.Verify(t.Context(), opts); err == nil {
		t.Fatal("verified with no registry")
	}

	images, err := p.VerifiedImages(t.Context())
	if err != nil || len(images) != 1 || !images[0].Wanted || !slices.Equal(images[0].Command, opts.Command) {
		t.Fatalf("images = %+v, %v", images, err)
	}
}

// WHAT IS STACKED IS THE IMAGE'S. WHAT THIS HOST COULD NOT READ IS NOT.
func TestOnlyAProvedLayoutIsAVerdictOnTheImage(t *testing.T) {
	t.Parallel()

	stacked := t.TempDir()
	if err := os.WriteFile(filepath.Join(stacked, baseOverlay), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := refuseStacked(stacked); !isImageVerdict(err) {
		t.Errorf("a stacked VM: %v", err)
	}

	if err := refuseStacked(t.TempDir()); !isImageVerdict(err) {
		t.Errorf("a VM with no disk of its own: %v", err)
	}

	unreadable := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(unreadable, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := refuseStacked(unreadable)
	if err == nil || isImageVerdict(err) {
		t.Errorf("a directory that could not be read: %v", err)
	}
}

// A DISK AN INTERRUPTED PULL FILLED DOES NOT STOP THE CLEANUP THAT WOULD FREE IT: the record is
// written again once that has run, and if it still cannot be, nothing goes on.
func TestARecordThatCannotBeWrittenFirstStillReachesTheCleanup(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		fails int
		ok    bool
	}{
		{name: "the cleanup freed the room", fails: 1, ok: true},
		{name: "it did not", fails: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, p := verifiedStub(t)

			left := filepath.Join(p.privateStore(verifiedRef), "cache")
			if err := os.MkdirAll(left, 0o700); err != nil {
				t.Fatal(err)
			}

			failures := 0
			p.installer.Rename = func(from, to string) error {
				if failures < tc.fails {
					failures++

					return errors.New("no space left on device")
				}

				return os.Rename(from, to)
			}

			_, err := p.Verify(t.Context(), verifyOpts())
			if (err == nil) != tc.ok {
				t.Fatalf("Verify = %v, want success %v", err, tc.ok)
			}

			if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the private store is still there: %v", err)
			}
		})
	}
}
