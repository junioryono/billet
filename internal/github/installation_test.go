package github

import (
	"errors"
	"testing"
)

// An operator can remove a permission between creating the app and installing
// it. Without this check, a scale set that cannot register runners fails at job
// time with an error that never mentions permissions.
// Assert the EXACT diagnostics, not their count. A count passes just as happily
// when the messages name the wrong permission or the wrong direction.
func TestPermissionMismatches(t *testing.T) {
	tests := map[string]struct {
		granted map[string]string
		want    []string
	}{
		"exactly what we asked for": {
			granted: map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"},
			want:    nil,
		},
		"write in place of a requested read is a mismatch": {
			// It used to satisfy the requirement ("write implies read"), but
			// write where billet asked for read is MORE access than billet
			// claims to hold — the same isolation falsification as an extra
			// permission, so it is refused the same way.
			granted: map[string]string{"metadata": "write", "organization_self_hosted_runners": "write"},
			want:    []string{"metadata: want read, granted write"},
		},
		"runners downgraded to read": {
			granted: map[string]string{"metadata": "read", "organization_self_hosted_runners": "read"},
			want:    []string{"organization_self_hosted_runners: want write, granted read"},
		},
		"runners removed entirely": {
			granted: map[string]string{"metadata": "read"},
			want:    []string{"organization_self_hosted_runners: want write, not granted"},
		},
		"nothing granted": {
			granted: map[string]string{},
			want: []string{
				"metadata: want read, not granted",
				"organization_self_hosted_runners: want write, not granted",
			},
		},
		// The case that makes the README's claim false. An app edited to add
		// `contents` before installation must fail, not be waved through.
		"an unrequested permission was added": {
			granted: map[string]string{
				"metadata": "read", "organization_self_hosted_runners": "write", "contents": "read",
			},
			want: []string{"contents: granted read, but billet never requested it"},
		},
		"actions was added": {
			granted: map[string]string{
				"metadata": "read", "organization_self_hosted_runners": "write", "actions": "read",
			},
			want: []string{"actions: granted read, but billet never requested it"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inst := &Installation{Permissions: tt.granted}

			got := inst.PermissionMismatches()
			if len(got) != len(tt.want) {
				t.Fatalf("PermissionMismatches() = %v, want %v", got, tt.want)
			}

			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("mismatch[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// Go randomizes map iteration, so an unsorted diagnostic reorders itself between
// runs and cannot be diffed.
func TestPermissionMismatchesAreStable(t *testing.T) {
	inst := &Installation{Permissions: map[string]string{
		"contents": "read", "actions": "read", "issues": "write",
	}}

	first := inst.PermissionMismatches()

	for range 20 {
		next := inst.PermissionMismatches()
		if len(next) != len(first) {
			t.Fatalf("unstable length: %v vs %v", next, first)
		}

		for i := range first {
			if next[i] != first[i] {
				t.Fatalf("unstable order at %d: %q vs %q", i, next[i], first[i])
			}
		}
	}
}

// "Created but not installed" is the ordinary state the CLI polls through, so it
// must be distinguishable from a real failure — otherwise polling would either
// hide a bad key or give up on a normal one.
func TestErrNotInstalledIsDistinguishable(t *testing.T) {
	wrapped := errors.New("something else")

	if errors.Is(wrapped, ErrNotInstalled) {
		t.Error("an unrelated error must not match ErrNotInstalled")
	}

	if !errors.Is(ErrNotInstalled, ErrNotInstalled) {
		t.Error("ErrNotInstalled must match itself")
	}
}
