package retirement

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// useRoot points the package at a directory the test owns.
func useRoot(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	old := Root
	Root = dir
	t.Cleanup(func() { Root = old })

	return dir
}

// releaseLater releases a hold at the end of the test and fails it on an error,
// because a lock that did not release is what the next command trips over.
func releaseLater(t *testing.T, release func() error) {
	t.Helper()
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})
}

func TestTheServiceAccountRecordRoundTripsAndRefusesWhatItCannotUse(t *testing.T) {
	useRoot(t)

	if _, err := ReadServiceAccount(); !errors.Is(err, ErrNoServiceAccount) {
		t.Fatalf("an absent record must be the positive absence, got %v", err)
	}

	want := ServiceAccount{User: "billet", UID: 998, Group: "billet", GID: 997}
	if err := WriteServiceAccount(want); err != nil {
		t.Fatal(err)
	}

	got, err := ReadServiceAccount()
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}

	info, err := os.Stat(ServiceAccountPath())
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("the record must be world-readable (0644), got %v %v", info.Mode(), err)
	}

	for name, body := range map[string]string{
		"root":       `{"user":"root","uid":0,"group":"root","gid":0}`,
		"unknown":    `{"user":"billet","uid":998,"group":"billet","gid":997,"shell":"/bin/false"}`,
		"trailing":   `{"user":"billet","uid":998,"group":"billet","gid":997} {}`,
		"not-json":   `billet:998`,
		"empty-name": `{"user":"","uid":998,"group":"billet","gid":997}`,
	} {
		if err := os.WriteFile(ServiceAccountPath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := ReadServiceAccount(); err == nil || errors.Is(err, ErrNoServiceAccount) {
			t.Fatalf("%s: a record billet cannot use must refuse as such, got %v", name, err)
		}
	}
}

func TestTheStatusIsReadTyped(t *testing.T) {
	useRoot(t)

	if _, presence, err := ReadStatus(); presence != StatusAbsent || err != nil {
		t.Fatalf("no file must be absent, got %v %v", presence, err)
	}

	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if err := WriteStatus(PhaseStopped, VariantServerOnly, now); err != nil {
		t.Fatal(err)
	}

	st, presence, err := ReadStatus()
	if err != nil || presence != StatusPresent || st.Phase != PhaseStopped || st.Variant != VariantServerOnly || st.UpdatedAt != "2026-09-11T12:00:00Z" {
		t.Fatalf("got %+v %v %v", st, presence, err)
	}

	if !PhaseStopped.Closed() || PhaseIntent.Closed() || !PhaseDone.Closed() {
		t.Fatal("the authority closes at stopped, not before")
	}

	for name, body := range map[string]string{
		"unknown-phase":   `{"phase":"paused","variant":"server-only","updated_at":"2026-09-11T12:00:00Z"}`,
		"unknown-variant": `{"phase":"stopped","variant":"both","updated_at":"2026-09-11T12:00:00Z"}`,
		"bad-time":        `{"phase":"stopped","variant":"server-only","updated_at":"yesterday"}`,
		"extra-member":    `{"phase":"stopped","variant":"server-only","updated_at":"2026-09-11T12:00:00Z","x":1}`,
	} {
		if err := os.WriteFile(StatusPath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, presence, err := ReadStatus(); presence != StatusMalformed || err == nil {
			t.Fatalf("%s: must be malformed with a reason, got %v %v", name, presence, err)
		}
	}

	if err := WriteStatus("paused", VariantServerOnly, now); err == nil {
		t.Fatal("an unknown phase must not be published")
	}
}

func TestTheGlobalLockIsCreatedOnlyByAPrivilegedCallerAndExcludes(t *testing.T) {
	useRoot(t)

	if _, err := Acquire(t.Context(), AcquireOptions{}); !errors.Is(err, ErrNoGlobalLock) {
		t.Fatalf("an unprivileged caller with no lock file must get the positive absence, got %v", err)
	}

	if _, err := os.Lstat(GlobalLockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unprivileged caller must create nothing")
	}

	first, err := Acquire(t.Context(), AcquireOptions{Privileged: true})
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(GlobalLockPath())
	if err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() != 0 && info.Mode().Perm() != 0o600 {
		t.Fatalf("without a recorded account the lock is 0600, got %v", info.Mode())
	}

	// A second acquisition blocks until the first releases, and gives up at its
	// bound rather than hanging.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	if _, err := Acquire(ctx, AcquireOptions{Poll: 10 * time.Millisecond}); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held lock must be waited for and the wait must end at the bound, got %v", err)
	}

	released := make(chan error, 1)

	go func() {
		time.Sleep(100 * time.Millisecond)
		released <- first.Release()
	}()

	second, err := Acquire(t.Context(), AcquireOptions{Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	if err := <-released; err != nil {
		t.Fatal(err)
	}

	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAdmitReadsTheStatusAndNeverCollapsesUncertainty(t *testing.T) {
	useRoot(t)

	hold, err := Acquire(t.Context(), AcquireOptions{Privileged: true})
	if err != nil {
		t.Fatal(err)
	}

	releaseLater(t, hold.Release)

	if err := hold.Admit(); err != nil {
		t.Fatalf("an absent status admits, got %v", err)
	}

	now := time.Now()
	if err := WriteStatus(PhaseIntent, VariantRetainedNode, now); err != nil {
		t.Fatal(err)
	}

	if err := hold.Admit(); err != nil {
		t.Fatalf("intent is not closed, got %v", err)
	}

	if err := WriteStatus(PhaseArchived, VariantRetainedNode, now); err != nil {
		t.Fatal(err)
	}

	var retiring ErrRetiring
	if err := hold.Admit(); !errors.As(err, &retiring) || retiring.Phase != PhaseArchived {
		t.Fatalf("a closed status must refuse naming its phase, got %v", err)
	}

	if err := os.WriteFile(StatusPath(), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	var unknown ErrStatusUnknown
	if err := hold.Admit(); !errors.As(err, &unknown) {
		t.Fatalf("a malformed status is could-not-tell, got %v", err)
	}

	var nilHold *Hold
	if err := nilHold.Admit(); err == nil {
		t.Fatal("admission without a hold must refuse")
	}
}

func TestTheInitLockLivesInTheParentAndExcludes(t *testing.T) {
	parent := t.TempDir()
	identity := filepath.Join(parent, "server")

	if got, want := InitLockPath(identity), filepath.Join(parent, ".billet-identity.lock"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}

	first, err := AcquireInit(t.Context(), identity, nil, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	if _, err := AcquireInit(ctx, identity, nil, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held init lock must be waited for to the bound, got %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	second, err := AcquireInit(t.Context(), identity, nil, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	if err := second.Release(); err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() != 0 {
		unwritable := filepath.Join(t.TempDir(), "locked")
		if err := os.Mkdir(unwritable, 0o500); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if err := os.Chmod(unwritable, 0o700); err != nil {
				t.Error(err)
			}
		})

		if _, err := AcquireInit(t.Context(), filepath.Join(unwritable, "server"), nil, 0); err == nil {
			t.Fatal("a parent this account cannot write must refuse naming the installers")
		}
	}
}
