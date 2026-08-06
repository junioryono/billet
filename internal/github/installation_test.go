package github

import (
	"errors"
	"testing"
)

// An operator can remove a permission between creating the app and installing
// it. Without this check, a scale set that cannot register runners fails at job
// time with an error that never mentions permissions.
func TestMissingPermissions(t *testing.T) {
	tests := map[string]struct {
		granted map[string]string
		want    int
	}{
		"exactly what we asked for": {
			granted: map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"},
			want:    0,
		},
		"more than we asked for is fine": {
			granted: map[string]string{
				"metadata": "write", "organization_self_hosted_runners": "write", "issues": "write",
			},
			want: 0,
		},
		"runners downgraded to read": {
			granted: map[string]string{"metadata": "read", "organization_self_hosted_runners": "read"},
			want:    1,
		},
		"runners removed entirely": {
			granted: map[string]string{"metadata": "read"},
			want:    1,
		},
		"nothing granted": {
			granted: map[string]string{},
			want:    2,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inst := &Installation{Permissions: tt.granted}

			if got := inst.MissingPermissions(); len(got) != tt.want {
				t.Errorf("MissingPermissions() = %v (%d), want %d", got, len(got), tt.want)
			}
		})
	}
}

// write implies read, so a stronger grant must not be reported as missing.
func TestMissingPermissionsTreatsWriteAsSufficientForRead(t *testing.T) {
	inst := &Installation{Permissions: map[string]string{
		"metadata":                         "write",
		"organization_self_hosted_runners": "write",
	}}

	if got := inst.MissingPermissions(); len(got) != 0 {
		t.Errorf("write should satisfy a read requirement, got %v", got)
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
