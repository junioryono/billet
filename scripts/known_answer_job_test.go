package scripts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE KNOWN-ANSWER JOB IS EXECUTED, NOT PATTERN-MATCHED. What it writes is the
// expectation the checker holds billet's measurement to, so every rule in it is
// executed here against fakes: the lease is read from the runner's name and
// nothing else, every kind does the same preparation, a download of the wrong
// size fails the job rather than recording the wrong answer, and an input that
// could turn into JSON structure is refused before anything runs.

const knownAnswerScript = "known-answer-job.sh"

// knownAnswerExpectations is where scripts/knownanswer reads what this script
// writes; the files are compared here and rewritten with
// BILLET_UPDATE_FIXTURES=1.
var knownAnswerExpectations = filepath.Join("knownanswer", "testdata", "expectations")

// The fakes record each call as one line, `<name> <args>`, in $FAKE_LOG.
// `sudo` runs what it is given with any leading NAME=value set, the way sudo
// does; `date` answers one fixed time, so the expectation is the same on every
// machine.
var knownAnswerFakes = map[string]string{
	"sudo": `#!/bin/bash
printf 'sudo %s\n' "$*" >>"$FAKE_LOG"
while [ "$#" -gt 0 ]; do
	case "$1" in *=*) export "$1"; shift ;; *) break ;; esac
done
exec "$@"
`,
	"apt-get": `#!/bin/bash
printf 'apt-get DEBIAN_FRONTEND=%s %s\n' "${DEBIAN_FRONTEND:-}" "$*" >>"$FAKE_LOG"
`,
	"stress-ng": `#!/bin/bash
printf 'stress-ng %s\n' "$*" >>"$FAKE_LOG"
exit "${FAKE_STRESS_STATUS:-0}"
`,
	"fio": `#!/bin/bash
printf 'fio %s\n' "$*" >>"$FAKE_LOG"
`,
	"sleep": `#!/bin/bash
printf 'sleep %s\n' "$*" >>"$FAKE_LOG"
`,
	"curl": `#!/bin/bash
printf 'curl %s\n' "$*" >>"$FAKE_LOG"
printf '%s' "$FAKE_CURL_SIZE"
`,
	"nproc": `#!/bin/bash
printf '%s\n' "$FAKE_NPROC"
`,
	"date": `#!/bin/bash
printf '2026-10-09T10:00:00Z\n'
`,
}

type knownAnswerHarness struct{ bin, log, out, temp string }

func newKnownAnswerHarness(t *testing.T) knownAnswerHarness {
	t.Helper()
	root := t.TempDir()
	h := knownAnswerHarness{bin: filepath.Join(root, "bin"), log: filepath.Join(root, "calls.log"),
		out: filepath.Join(root, "out"), temp: filepath.Join(root, "runner-temp")}
	if err := os.MkdirAll(h.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// ONLY THESE ARE ON PATH, so a fake that failed to land can never fall
	// through to a real stress-ng, fio or curl on the machine.
	for _, tool := range []string{"bash", "mkdir", "mv", "rm"} {
		resolved, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("this test needs %s on PATH: %v", tool, err)
		}
		if err := os.Symlink(resolved, filepath.Join(h.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range knownAnswerFakes {
		if err := forkSafeWriteFile(filepath.Join(h.bin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	return h
}

// run executes the script for one kind with the runner's environment plus env.
func (h knownAnswerHarness) run(t *testing.T, kind string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(h.bin, "bash"), knownAnswerScript, kind, h.out)
	cmd.Env = append([]string{
		"PATH=" + h.bin, "HOME=" + t.TempDir(), "FAKE_LOG=" + h.log, "FAKE_NPROC=8",
		"FAKE_CURL_SIZE=104857600", "RUNNER_NAME=billet-lease-" + kind, "RUNNER_TEMP=" + h.temp,
		"GITHUB_RUN_ID=18000000001", "GITHUB_RUN_ATTEMPT=2", "GITHUB_REPOSITORY=acme/bench",
		"KA_GITHUB_JOB_ID=52000000077", "KA_SECONDS=60", "KA_CPU_WORKERS=4", "KA_MEMORY_BYTES=2147483648",
		"KA_NETWORK_URL=https://objects.example/known?bytes=104857600", "KA_NETWORK_BYTES=104857600",
		"KA_DISK_BYTES=2147483648",
	}, env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()

	return out.String(), err
}

func (h knownAnswerHarness) calls(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(h.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// EVERY KIND PREPARES THE SAME WAY AND THEN RUNS EXACTLY ITS LOAD, and what it
// writes is the committed fixture the checker's tests read.
func TestTheKnownAnswerJobWritesWhatItRan(t *testing.T) {
	t.Parallel()

	prepare := []string{
		"sudo apt-get update -qq",
		"apt-get DEBIAN_FRONTEND= update -qq",
		"sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq stress-ng fio curl",
		"apt-get DEBIAN_FRONTEND=noninteractive install -y -qq stress-ng fio curl",
	}
	for _, tc := range []struct {
		kind  string
		load  []string
		print string
	}{
		{kindBaseline, nil, "expected {}"},
		{kindIdle, []string{"sleep 60"}, `expected {"cpu_seconds":0}`},
		{kindCPU, []string{"stress-ng --cpu 4 --cpu-method matrixprod --timeout 60s --metrics-brief"},
			`expected {"cpu_seconds":240}`},
		{kindMemory, []string{"stress-ng --vm 1 --vm-bytes 2147483648 --vm-keep --timeout 60s --metrics-brief"},
			`expected {"memory_peak_bytes":2147483648}`},
		{kindNetwork, []string{"curl --fail --silent --show-error --location --output /dev/null " +
			"--write-out %{size_download} https://objects.example/known?bytes=104857600"},
			`expected {"net_rx_bytes":104857600}`},
		{kindDisk, []string{"fio --name=known-answer --filename=RUNNER_TEMP/known-answer-work/fio.dat " +
			"--rw=write --bs=1M --size=2147483648 --direct=1 --ioengine=libaio --iodepth=16 --end_fsync=1"},
			`expected {"disk_write_bytes":2147483648}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			h := newKnownAnswerHarness(t)
			out, err := h.run(t, tc.kind)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			want := append(append([]string{}, prepare...), tc.load...)
			got := h.calls(t)
			for i := range got {
				got[i] = strings.ReplaceAll(got[i], h.temp, "RUNNER_TEMP")
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			for _, line := range []string{"known-answer: kind=" + tc.kind + " lease=lease-" + tc.kind +
				" run=18000000001 attempt=2 github_job_id=52000000077", "known-answer: " + tc.print} {
				if !strings.Contains(out, line) {
					t.Errorf("the job log does not say %q:\n%s", line, out)
				}
			}
			written, err := os.ReadFile(filepath.Join(h.out, "expectation.json"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(knownAnswerExpectations, tc.kind, "expectation.json")
			if os.Getenv("BILLET_UPDATE_FIXTURES") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, written, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			committed, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with BILLET_UPDATE_FIXTURES=1 to write the fixtures)", err)
			}
			if !bytes.Equal(committed, written) {
				t.Errorf("%s is not what the job writes today; run with BILLET_UPDATE_FIXTURES=1:\n%s", path, written)
			}
		})
	}
}

const (
	kindBaseline = "baseline"
	kindIdle     = "idle"
	kindCPU      = "cpu"
	kindMemory   = "memory"
	kindNetwork  = "network"
	kindDisk     = "disk"
)

// A JOB THAT CANNOT KNOW ITS ANSWER REFUSES BEFORE IT PREPARES ANYTHING, and
// one whose load fails writes no expectation, so no record of a wrong answer
// is ever uploaded.
func TestTheKnownAnswerJobRefusesWhatItCannotKnow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		kind     string
		env      []string
		says     string
		prepared bool
	}{
		{"a runner billet did not name", kindCPU, []string{"RUNNER_NAME=ubuntu-latest-7"}, "is not one billet launched", false},
		{"a runner named only billet-", kindCPU, []string{"RUNNER_NAME=billet-"}, "is not one billet launched", false},
		{"a lease that would break the JSON", kindCPU, []string{`RUNNER_NAME=billet-a"b`}, "names no lease", false},
		{"a repository that would break the JSON", kindCPU, []string{`GITHUB_REPOSITORY=acme/"x`}, "is not owner/name", false},
		{"a job id that is not a check run id", kindCPU, []string{`KA_GITHUB_JOB_ID=1"2`}, "is not a check run id", false},
		{"seconds in another notation", kindCPU, []string{"KA_SECONDS=1e3"}, "KA_SECONDS must be a positive integer", false},
		{"seconds with a leading zero", kindIdle, []string{"KA_SECONDS=060"}, "KA_SECONDS must be a positive integer", false},
		{"seconds past a job's life", kindIdle, []string{"KA_SECONDS=21601"}, "longer than a job can run", false},
		{"more workers than vCPUs", kindCPU, []string{"KA_CPU_WORKERS=9"}, "cannot each run on a guest with 8 vCPUs", false},
		{"no memory size", kindMemory, []string{"KA_MEMORY_BYTES="}, "KA_MEMORY_BYTES must be a positive integer", false},
		{"a URL that is not http", kindNetwork, []string{"KA_NETWORK_URL=file:///etc/passwd"}, "must be an http or https URL", false},
		{"a body of another size", kindNetwork, []string{"FAKE_CURL_SIZE=104857599"}, "the known answer is not known", true},
		{"an empty body", kindNetwork, []string{"FAKE_CURL_SIZE=0"}, "the downloaded size must be a positive integer", true},
		{"a load that failed", kindCPU, []string{"FAKE_STRESS_STATUS=3"}, "", true},
		{"an unknown kind", "gpu", nil, "unknown kind 'gpu'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newKnownAnswerHarness(t)
			out, err := h.run(t, tc.kind, tc.env...)
			if err == nil {
				t.Fatalf("the job succeeded:\n%s", out)
			}
			if !strings.Contains(out, tc.says) {
				t.Errorf("the job did not say %q:\n%s", tc.says, out)
			}
			if prepared := len(h.calls(t)) > 0; prepared != tc.prepared {
				t.Errorf("prepared = %v, want %v: %v", prepared, tc.prepared, h.calls(t))
			}
			if _, err := os.Stat(filepath.Join(h.out, "expectation.json")); !os.IsNotExist(err) {
				t.Errorf("an expectation was written (stat: %v)", err)
			}
		})
	}
}

// THE OPTIONAL SIZE IS OPTIONAL: with none given, the expectation is the size
// that arrived.
func TestTheKnownAnswerNetworkJobExpectsWhatArrivedWhenNoSizeIsGiven(t *testing.T) {
	h := newKnownAnswerHarness(t)
	out, err := h.run(t, kindNetwork, "KA_NETWORK_BYTES=", "FAKE_CURL_SIZE=5000")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	written, err := os.ReadFile(filepath.Join(h.out, "expectation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `"expected":{"net_rx_bytes":5000}`) {
		t.Errorf("expectation = %s", written)
	}
}
