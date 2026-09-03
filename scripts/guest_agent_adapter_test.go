package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE AGENT'S ADAPTER STARTUP IS RUN HERE, not grepped for.
//
// What decides whether a workflow can use billet's cache from a container-driver
// BuildKit is one publication: BILLET_ACTIONS_CACHE_URL reaches the job only if
// the listener is serving. Published for a listener that never started, it points
// the builder at a refused connection; withheld from one that did, the feature is
// silently off. Neither is visible in the shell, and the image gate's greps are
// satisfied by the shutdown line and the hook alone — delete the whole startup
// block and every static check still passes.
//
// So the block between the markers is extracted VERBATIM and executed against
// fake service-manager commands, in every direction it can go.
func TestTheAgentPublishesTheAdapterURLOnlyWhenItIsServing(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_ADAPTER_START")
	// THE PUBLICATION IS THE SECOND HALF OF THE FEATURE. A listener that serves
	// and a job that is never told its address are the same thing to a workflow,
	// and the two live in different parts of the agent.
	publish := agentBlock(t, "BILLET_ADAPTER_ENV")
	// THE FAKES ARE WRITTEN ONCE, before any subtest runs. A freshly created
	// executable costs seconds on its first exec on this development host, and a
	// copy per case paid that four times; what varies per case is the record path
	// and the two exit statuses, which are environment.
	fakes := writeFakeServiceManagers(t)

	for _, tc := range []struct {
		name string
		// active is the interception gate the block sits behind.
		active string
		// runFails and startFails are the two service-manager steps.
		runFails, startFails bool
		wantURL              string
		wantStopped          bool
		wantLaunched         bool
	}{
		{
			name:         "the adapter starts and its URL is published",
			active:       "1",
			wantURL:      "http://127.0.0.1:41321/",
			wantLaunched: true,
		},
		{
			// THE UNIT COULD NOT BE CREATED. Nothing is serving, so nothing may be
			// published: a builder pointed at this port would meet a refusal.
			name:         "the unit could not be created",
			active:       "1",
			runFails:     true,
			wantStopped:  true,
			wantLaunched: true,
		},
		{
			// THE SOCKET EXISTS AND THE SERVICE NEVER REACHED READY. Type=notify is
			// what makes `systemctl start` mean "serving"; a bare socket would
			// accept a connection and answer nothing.
			name:         "the service never became ready",
			active:       "1",
			startFails:   true,
			wantStopped:  true,
			wantLaunched: true,
		},
		{
			// NO INTERCEPTION, NO LISTENER. The cleartext port exists only for a VM
			// that was issued an interception session.
			name:   "interception was never activated",
			active: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			record := filepath.Join(t.TempDir(), "record")
			script := strings.Join([]string{
				"set -euo pipefail",
				`PATH="` + fakes + `:$PATH"`,
				"export BILLET_FAKE_RECORD=" + shellQuote(record),
				"export BILLET_FAKE_RUN_STATUS=" + exitStatus(tc.runFails),
				"export BILLET_FAKE_START_STATUS=" + exitStatus(tc.startFails),
				`log() { printf 'billet-agent: %s\n' "$*" >&2; }`,
				"actions_cache_active=" + shellQuote(tc.active),
				"python_runtime=/opt/hostedtoolcache/Python/3.13.0/x64/bin/python",
				"actions_proxy=http://billet:sesh@10.9.8.7:9000",
				"results_fallback=140.82.114.21,140.82.114.22",
				"actions_ca_path=/home/runner/runner/_work/_billet/actions-cache-ca.pem",
				"actions_cache_port=41321",
				`actions_cache_url=""`,
				"runner_env=()",
				block,
				publish,
				`printf 'url=%s\n' "$actions_cache_url"`,
				`for entry in ${runner_env[@]+"${runner_env[@]}"}; do printf 'env=%s\n' "$entry"; done`,
			}, "\n")

			run := exec.CommandContext(t.Context(), "bash", "-c", script)
			output, err := run.CombinedOutput()
			if err != nil {
				t.Fatalf("the adapter startup block failed: %v\n%s", err, output)
			}
			if got := adapterURL(string(output)); got != tc.wantURL {
				t.Errorf("published URL %q, want %q\n%s", got, tc.wantURL, output)
			}
			// AND IT REACHES THE JOB. The runner's environment is passed through
			// `env -i`, so a variable absent from this array does not exist for
			// the job whatever else the agent knows.
			entry := "env=BILLET_ACTIONS_CACHE_URL=" + tc.wantURL
			if published := strings.Contains(string(output), entry); published != (tc.wantURL != "") {
				t.Errorf("the job's environment carries the adapter URL=%v, want %v\n%s",
					published, tc.wantURL != "", output)
			}

			launched := readArgv(t, record+".systemd-run")
			if (len(launched) > 0) != tc.wantLaunched {
				t.Fatalf("systemd-run invoked=%v, want %v", len(launched) > 0, tc.wantLaunched)
			}
			serviced := readArgv(t, record+".systemctl")
			if stopped := containsArgv(serviced, []string{"stop"}); stopped != tc.wantStopped {
				t.Errorf("the units were stopped=%v, want %v\n%s", stopped, tc.wantStopped,
					strings.Join(serviced, " | "))
			}
			if !tc.wantLaunched {
				return
			}
			// The unit itself: the mode that serves the loopback endpoint, the
			// loopback-only bind on the port the published URL names, the trust
			// bundle and the fail-open addresses the adapter refuses to run
			// without, and the readiness protocol that makes `start` mean serving.
			for _, required := range [][]string{
				// The unit and the program, because a fake service manager succeeds
				// for whatever it is handed: without these the block could start a
				// differently named unit running something else and nothing here
				// would notice.
				{"--unit=billet-actions-cache-adapter"},
				{"/opt/hostedtoolcache/Python/3.13.0/x64/bin/python",
					"/usr/local/bin/billet-actions-proxy"},
				{"--mode", "cache-adapter"},
				{"--systemd-socket"},
				{"--socket-property=ListenStream=127.0.0.1:41321"},
				{"--property=Type=notify"},
				{"--ca-file", "/home/runner/runner/_work/_billet/actions-cache-ca.pem"},
				{"--fallback-addr", "140.82.114.21,140.82.114.22"},
				{"--upstream", "http://billet:sesh@10.9.8.7:9000"},
				{"--uid=runner"},
			} {
				if !containsArgv(launched, required) {
					t.Errorf("the adapter unit does not carry %q as its own arguments:\n%s",
						required, strings.Join(launched, " | "))
				}
			}
			if tc.wantURL != "" &&
				!containsArgv(serviced, []string{"start", "billet-actions-cache-adapter.service"}) {
				t.Error("the service was never started, so the socket alone would have been " +
					"published as ready")
			}
		})
	}
}

// agentBlock returns one marked block of the agent, dedented.
func agentBlock(t *testing.T, marker string) string {
	t.Helper()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatalf("read build-guest-image.sh: %v", err)
	}

	_, rest, found := strings.Cut(string(raw), marker+"_BEGIN")
	if !found {
		t.Fatalf("the guest agent no longer marks %s", marker)
	}
	_, rest, found = strings.Cut(rest, "\n")
	if !found {
		t.Fatalf("%s_BEGIN ends the file", marker)
	}
	block, _, found := strings.Cut(rest, marker+"_END")
	if !found {
		t.Fatalf("%s has no end marker", marker)
	}

	var lines []string
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimPrefix(line, "\t")
		if strings.HasPrefix(strings.TrimSpace(trimmed), "#") || strings.TrimSpace(trimmed) == "" {
			continue
		}
		lines = append(lines, trimmed)
	}
	if len(lines) < 3 {
		t.Fatalf("%s is %d lines; the extraction is not finding it", marker, len(lines))
	}

	return strings.Join(lines, "\n")
}

// writeFakeServiceManagers writes stand-ins for the two service-manager commands
// the block invokes. They record the argv they were given and exit as the
// environment tells them, so the block's decisions are observable without systemd
// and one pair of executables serves every case.
func writeFakeServiceManagers(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	// ONE LINE PER ARGUMENT, NOT "$*". Joined with spaces, `--exec a b` and
	// `--exec "a b"` record identically -- so an assertion about the interpreter
	// and the program it runs would pass for a single argument systemd could not
	// execute. The boundaries are the thing being checked.
	for name, body := range map[string]string{
		"systemd-run": "#!/bin/sh\n" +
			`printf '%s\n' "$@" >>"$BILLET_FAKE_RECORD.systemd-run"` + "\n" +
			`exit "${BILLET_FAKE_RUN_STATUS:-0}"` + "\n",
		// Only `start` can fail: it is the readiness signal, and a stop that
		// refused would hide the cleanup half of what this proves.
		"systemctl": "#!/bin/sh\n" +
			`printf '%s\n' "$@" >>"$BILLET_FAKE_RECORD.systemctl"` + "\n" +
			`if [ "$1" = start ]; then exit "${BILLET_FAKE_START_STATUS:-0}"; fi` + "\n" +
			"exit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			t.Fatalf("write the fake %s: %v", name, err)
		}
	}

	return dir
}

func exitStatus(fails bool) string {
	if fails {
		return "1"
	}

	return "0"
}

// readArgv returns the arguments the fakes recorded, one per line.
func readArgv(t *testing.T, path string) []string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}

	return strings.FieldsFunc(string(body), func(r rune) bool { return r == '\n' })
}

// containsArgv reports whether the recorded arguments contain the wanted ones as
// a consecutive run, so that `--exec a b` and `--exec "a b"` are not one record.
func containsArgv(recorded, want []string) bool {
	for index := 0; index+len(want) <= len(recorded); index++ {
		if slices.Equal(recorded[index:index+len(want)], want) {
			return true
		}
	}

	return false
}

func adapterURL(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if value, found := strings.CutPrefix(line, "url="); found {
			return strings.TrimSpace(value)
		}
	}

	return ""
}
