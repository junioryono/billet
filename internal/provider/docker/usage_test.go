package docker

import (
	"strings"
	"testing"
)

func TestAContainersCgroupIsReadUnderEitherDriver(t *testing.T) {
	for _, tc := range []struct {
		name, proc, want, err string
	}{
		{"systemd driver", "0::/system.slice/docker-4f1c.scope\n", "/system.slice/docker-4f1c.scope", ""},
		{"cgroupfs driver", "0::/docker/4f1c\n", "/docker/4f1c", ""},
		{"a hybrid host picks the unified line", "12:memory:/docker/4f1c\n0::/docker/4f1c\n", "/docker/4f1c", ""},
		{"cgroup v1 only", "12:memory:/docker/4f1c\n11:cpu:/docker/4f1c\n", "", "no cgroup-v2 entry"},
		{"the root is not a container's", "0::/\n", "", "not a container's own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unifiedCgroup(tc.proc)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("unifiedCgroup = %q, %v; want an error saying %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("unifiedCgroup = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
