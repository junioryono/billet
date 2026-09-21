package actions_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sshOptions reads `-o Name=value` pairs out of an ANSIBLE_SSH_ARGS value.
func sshOptions(t *testing.T, args string) map[string]string {
	t.Helper()

	fields := strings.Fields(args)
	opts := map[string]string{}

	for i := 0; i < len(fields); i++ {
		if fields[i] != "-o" || i+1 == len(fields) {
			continue
		}

		name, value, ok := strings.Cut(fields[i+1], "=")
		if !ok {
			t.Fatalf("ANSIBLE_SSH_ARGS carries -o %q, which is not Name=value", fields[i+1])
		}

		opts[name] = value
		i++
	}

	return opts
}

// requireKeepalive fails unless the SSH arguments detect a dead connection and
// keep the connection sharing Ansible's own default gives.
func requireKeepalive(t *testing.T, step, args string) {
	t.Helper()

	opts := sshOptions(t, args)

	interval, errI := strconv.Atoi(opts["ServerAliveInterval"])
	count, errC := strconv.Atoi(opts["ServerAliveCountMax"])

	// OpenSSH gives up once the unanswered count EXCEEDS ServerAliveCountMax,
	// so a dead flow is noticed after interval × (count + 1) seconds.
	const bound = 90

	if errI != nil || errC != nil || interval <= 0 || count <= 0 || interval*(count+1) > bound {
		t.Errorf("%s runs ssh with ServerAliveInterval=%q ServerAliveCountMax=%q in %q: a connection "+
			"the network dropped must be noticed within %d seconds, or the job waits on it until "+
			"it is cancelled", step, opts["ServerAliveInterval"], opts["ServerAliveCountMax"], args, bound)
	}

	// ANSIBLE_SSH_ARGS REPLACES Ansible's default rather than adding to it, so
	// the multiplexing it carried has to be restated or every task opens a new
	// connection.
	if opts["ControlMaster"] != "auto" || opts["ControlPersist"] == "" || opts["ControlPersist"] == "no" {
		t.Errorf("%s dropped Ansible's connection sharing: %q", step, args)
	}
}

// A DEAD CONNECTION ENDS THE RUN INSTEAD OF HOLDING IT.
//
// MEASURED on a consumer's fleet (2026-09-21), in check mode: the node's sshd
// logged the runner closing the connection 48 seconds into the endpoint dry
// run, and the runner went on waiting for 36 minutes until the job was
// cancelled; the release step then found the multiplexing master broken
// ("read from master failed: Broken pipe"). That is consistent with a flow lost
// in the WARP path, which an ssh with no keepalive has no way to learn about;
// it was not reproduced. The environment is exported once before every pass,
// so the check's single pass and the converge's second pass each show it.
func TestTheConvergeDetectsADeadConnection(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"check", "converge"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			f := newConvergeFixture(t)

			out, err := f.run(t, convergeRun{mode: mode})
			if err != nil {
				t.Fatalf("converge.sh in %s mode: %v\n%s", mode, err, out)
			}

			requireKeepalive(t, "the "+mode, recordedEnv(t, f.pbEnv)["ANSIBLE_SSH_ARGS"])
		})
	}
}

// AND THE RELEASE, which crosses the same path at the end of the same job.
func TestTheReleaseDetectsADeadConnection(t *testing.T) {
	t.Parallel()

	f := newConvergeFixture(t)
	for _, name := range []string{"recap1", "recap2", "list-hosts"} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(cleanRecap("cp-1")), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, err := f.release(t, releaseRun{held: true})
	if err != nil {
		t.Fatalf("release-guard.sh: %v\n%s", err, out)
	}

	requireKeepalive(t, "the release", recordedEnv(t, f.pbEnv)["ANSIBLE_SSH_ARGS"])
}
