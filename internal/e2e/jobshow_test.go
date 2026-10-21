package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/ops/fleetops"
	"github.com/junioryono/billet/internal/provider"
)

// operatorConfig is the billet.yaml an operator on the stack's host reads its
// ledger through: the stack's state directory, one docker tier that is not the
// job's, and a target on an owner that holds no repository the harness's jobs
// run in. A report's read of a job's steps resolves an undeclared tier to the
// target holding the job's repository, so this one finds none and says so
// before any request is made, rather than asking the real GitHub from a test.
func operatorConfig(t *testing.T, stateDir string) string {
	t.Helper()

	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	path := filepath.Join(dir, "billet.yaml")
	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: billet-operator-view-holds-nothing
  app_id: 1
  installation_id: 2
  private_key_path: ` + keyPath + `
tiers:
  - label: billet-operator-view
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
    command: ["/home/runner/run.sh"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return path
}

// `billet jobs show --run` PRINTS THE TIMELINE OF A JOB THIS STACK RAN, found
// by the run id GitHub showed, read through the operator's own command over the
// ledger the real control plane wrote (#28): the lease, the node, every part of
// its life in the order it lived them, the runner GitHub registered, the node
// process that launched and held it, and both verdicts.
func TestJobsShowPrintsTheTimelineOfAJobTheStackRan(t *testing.T) {
	s, _, _ := simulatedStack(t, overTheWire)

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 5001, "push", simulatedTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)
	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 5001, "push", simulatedTier))

	names := s.awaitOneRunning(t)
	leaseID, ours := provider.LeaseOf(names[0])
	if !ours {
		t.Fatalf("instance %q does not carry a lease", names[0])
	}

	// GITHUB STARTS THE JOB ON THE RUNNER IT WAS GIVEN (the fake mints id 99),
	// and then completes it.
	started := fakeactions.JobFields{RunnerID: 99, RunnerName: names[0]}
	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		started.Apply(fakeactions.JobJSON("JobStarted", 5001, "push", simulatedTier)))

	deadline = time.Now().Add(30 * time.Second)
	for {
		rec, err := s.alloc.Job(t.Context(), leaseID)
		if err == nil && rec.StartedAt != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job's start was never recorded: %+v, err %v", rec, err)
		}

		time.Sleep(50 * time.Millisecond)
	}

	completed := fakeactions.JobFields{RunnerID: 99, RunnerName: names[0], Result: "succeeded"}
	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		completed.Apply(fakeactions.JobJSON("JobCompleted", 5001, "push", simulatedTier)))

	s.awaitGone(t)

	deadline = time.Now().Add(30 * time.Second)
	for {
		rec, err := s.alloc.Job(t.Context(), leaseID)
		if err == nil && rec.Conclusion != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job's lease never ended: %+v, err %v", rec, err)
		}

		time.Sleep(100 * time.Millisecond)
	}

	var out, errs bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}
	if err := fleetops.Jobs(t.Context(), env, []string{"show", "--config", operatorConfig(t, s.dir),
		"--run", "5001"}); err != nil {
		t.Fatalf("billet jobs show --run 5001: %v\n%s", err, errs.String())
	}

	report := out.String()
	for _, want := range []string{
		"== run 5001, job 1 of 1: lease " + leaseID + " ==",
		"tier       " + simulatedTier + " on " + s.node + " (simulated, 2 vCPU, 2.0 GiB)",
		`result     github "succeeded"; billet done`,
		"runner     GitHub runner 99, instance " + names[0],
		"held by    launched and held to the end by process ",
		// AND NOTHING WAS ASKED OF GITHUB: a run's jobs are shown without steps.
		"steps      not read for a run's jobs",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not say %q:\n%s", want, report)
		}
	}

	// EVERY PART OF ITS LIFE THE LEDGER RECORDED, each on a line of its own
	// with the time the ledger holds for it: the real stack launched the lease,
	// minted its runner, saw the job start and settled it.
	rec, err := s.alloc.Job(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("read the job's history: %v", err)
	}
	for phase, at := range map[string]string{
		"bought": rec.BoughtAt, "assigned": rec.AssignedAt, "launched": rec.LaunchedAt,
		"registered": rec.RegisteredAt, "started": rec.StartedAt, "finished": rec.FinishedAt,
	} {
		if at == "" {
			t.Errorf("the ledger recorded no time the job was %s", phase)

			continue
		}
		if line := phase + strings.Repeat(" ", 12-len(phase)) + at; !strings.Contains(report, line) {
			t.Errorf("the timeline does not show the job %s at %s:\n%s", phase, at, report)
		}
	}
}
