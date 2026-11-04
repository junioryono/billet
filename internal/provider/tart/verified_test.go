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
func setRegistry(t *testing.T, s *stub, p *Provider, manifest string) string {
	t.Helper()

	if err := os.WriteFile(s.pullManifest, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	digest := digestOf(manifest)
	p.registry = func(context.Context, string) (string, error) { return digest, nil }

	return digest
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
	store := filepath.Join(p.stateDir, privateStores, hashName(verifiedRef))

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

			_, err := p.Launch(t.Context(), verifiedSpec(verifiedLease))
			if err == nil || !strings.Contains(err.Error(), "changed since it was verified") {
				t.Fatalf("Launch = %v", err)
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
	real := s.bin + ".real"
	if err := os.Rename(s.bin, real); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, s.bin, "#!/bin/sh\n\""+real+"\" \"$@\"\nrc=$?\nif [ \"$1\" = clone ]; then "+
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

	for _, image := range images {
		for _, r := range image.Records {
			got = append(got, r.Base)
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

	if due, err := p.ImageDue(t.Context(), verifiedTier); err != nil || due.Verify {
		t.Fatalf("after the withdrawal was cleared: %+v, %v", due, err)
	}

	if back.Digest != bad.Digest {
		t.Fatalf("recorded %+v", back)
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
	paths, _ := filepath.Glob(filepath.Join(s.home, "vms", "*", ".held"))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
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
	if !errors.Is(err, errNoSlot) {
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

	if names, _ := filepath.Glob(p.billetPath(slotPrefix + "*")); len(names) != 0 {
		t.Fatalf("a Linux guest made slots: %v", names)
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
		{name: "one is fine under a limit of two", ps: "111 tart run someone\n", admit: true},
		{
			name: "a token holder is a Linux guest", ps: "111 tart run a\n222 tart run b\n",
			lsof: map[string]string{tokenPrefix + "a": "111\n", tokenPrefix + "b": "222\n"}, admit: true,
		},
		{
			name: "another program's arguments are not a guest",
			ps:   "111 /bin/sh -c tart run x\n222 grep tart run\n", admit: true,
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

				defer func() { _ = token.Close() }()

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

			if !tc.admit && !errors.Is(err, errNoSlot) {
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

	assertNoBases(t, p)

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

	assertNoBases(t, p)

	images, err := p.VerifiedImages(t.Context())
	if err != nil || len(images) != 1 || images[0].Yields != 1 {
		t.Fatalf("images = %+v, %v, want the yield counted", images, err)
	}

	if names, _ := filepath.Glob(p.billetPath(yieldPrefix + "*")); len(names) != 0 {
		t.Fatalf("the launch left its request: %v", names)
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
	if !errors.Is(err, errNoSlot) {
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
	if !errors.Is(err, errNoSlot) {
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

	stale := filepath.Join(p.stateDir, privateStores, hashName(verifiedRef), "leftover")
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

// A DIGEST WITHDRAWN IS NEVER RESOLVED, even with its record still there: the withdrawal is
// installed first, so a crash between the two files leaves exactly this.
func TestResolveSkipsAWithdrawnDigest(t *testing.T) {
	t.Parallel()

	_, p := verifiedStub(t)
	rec := mustVerify(t, p)

	if err := p.withRecords(t.Context(), func(st *verifiedState) (bool, error) {
		st.withdrawn.Withdrawn[verifiedRef] = []string{rec.Digest}

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

	state, err := os.MkdirTemp(other, "billet-tart-*")
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
