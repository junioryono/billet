package firecracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE EXPERIMENT'S PINNING CHECK REFUSES A VMM OUTSIDE THE CGROUP IT NAMES,
// even when that cgroup's cpuset matches the claim, and a cpuset that does not.
func TestThePinningCheckProvesMembershipAndTheCPUSet(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T, member, effective string) (proc, root, dir string) {
		t.Helper()

		proc, root = t.TempDir(), t.TempDir()
		dir = filepath.Join(root, "firecracker-v1.16.1", "guest")
		for _, d := range []string{dir, filepath.Join(proc, "42", "task", "42"), filepath.Join(proc, "42", "task", "43")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write := func(path, body string) {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(dir, "cpuset.cpus.effective"), effective+"\n")
		write(filepath.Join(proc, "42", "task", "42", "cgroup"), "0::/firecracker-v1.16.1/guest\n")
		write(filepath.Join(proc, "42", "task", "43", "cgroup"), "0::"+member+"\n")

		return proc, root, dir
	}

	for name, tc := range map[string]struct {
		member, effective, want string
	}{
		"in its cgroup, confined":         {"/firecracker-v1.16.1/guest", "0-3", ""},
		"a thread in the parent":          {"/firecracker-v1.16.1", "0-3", "not "},
		"a thread in a same-named cgroup": {"/guest", "0-3", "not "},
		"a cpuset wider than the claim":   {"/firecracker-v1.16.1/guest", "0-7", "not the 0-3"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			proc, root, dir := setup(t, tc.member, tc.effective)
			err := cgroupConfines(proc, root, 42, dir, "0-3")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused a confined guest: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("answered %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}
