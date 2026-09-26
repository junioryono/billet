package firecracker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A LAUNCHED MICROVM IS MEASURED WHERE THE JAILER PUT IT: the cgroup named
// after the resolved binary and the jail id, the VMM pid the jail recorded,
// and the tap its resources claimed.
func TestAMicroVMsCountersAreWhereTheJailerPutThem(t *testing.T) {
	root := referenceCgroupRoot(t)
	h := newHarness(t)
	// The harness points the mount table at a scratch hierarchy after New;
	// this points it at the fixture instead.
	withMounts(t, root)(h.p)
	inst, _ := h.launch(t)

	j := h.p.jailFor(inst.ID)
	raw, err := os.ReadFile(j.pidFile())
	if err != nil {
		t.Fatalf("the harness wrote no pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := resourcesOf(j)
	if err != nil || res.Tap == "" {
		t.Fatalf("the launch claimed no tap: %+v %v", res, err)
	}

	h.p.pidOwner = func(got int, id string) (bool, error) { return got == pid && id == inst.ID, nil }
	target, err := h.p.UsageTarget(t.Context(), inst.ID)
	if err != nil {
		t.Fatalf("UsageTarget: %v", err)
	}
	if want := filepath.Join(root, "firecracker-v1.16.1", inst.ID); target.CgroupDir != want {
		t.Errorf("cgroup = %s, want %s", target.CgroupDir, want)
	}
	if target.PID != pid || target.NetDevice != res.Tap || !target.NetHostView ||
		target.VCPUThreadPrefix != "fc_vcpu" {
		t.Errorf("target = %+v, want pid %d on tap %s seen from the host", target, pid, res.Tap)
	}

	// A PID THE KERNEL HAS GIVEN TO SOMETHING ELSE IS NOT THE GUEST'S, and
	// reading its threads would charge another process's CPU to this job.
	h.p.pidOwner = func(int, string) (bool, error) { return false, nil }
	if _, err := h.p.UsageTarget(t.Context(), inst.ID); err == nil {
		t.Error("a reused pid was measured as the microVM's")
	}
}
