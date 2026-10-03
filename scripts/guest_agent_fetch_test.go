package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// THE AGENT'S METADATA READ IS RUN HERE against a curl that answers from a script.
// One read of the contract that timed out on a loaded host was reported as "older
// than this image" and the runner never connected; what the read returns for no
// answer, for an absent key and for a refusal is the whole fix, and only executing
// the block shows it.
func TestTheAgentAsksAgainOnlyWhenTheMetadataServiceDidNotAnswer(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_AGENT_FETCH")

	for _, tc := range []struct {
		name string
		// Each line is one curl invocation: its exit status, the HTTP code it
		// reports and the body, which printf %b expands. Past the last line every
		// call times out.
		answers   []string
		within    int
		wantRead  int
		wantValue string
		wantCalls int
		wantLog   string
	}{
		{
			name:    "a read that timed out is asked again until it is answered",
			answers: []string{"28 000", "7 000", "0 200 10"},
			within:  30, wantRead: 0, wantValue: "10", wantCalls: 3,
		},
		{
			name:    "an absent key is an answer and is not asked again",
			answers: []string{"0 404 not found"},
			within:  30, wantRead: 3, wantCalls: 1,
		},
		{
			name:    "a refusal is an answer and names its code",
			answers: []string{"0 401 unauthorized"},
			within:  30, wantRead: 1, wantCalls: 1, wantLog: "answered 401 for contract",
		},
		{
			name:    "a service that never answers is reported as unreadable, not absent",
			answers: nil,
			within:  1, wantRead: 1, wantLog: "could not read contract",
		},
		{
			name:    "a value is returned whole",
			answers: []string{`0 200 {"a":1}\nsecond line`},
			within:  30, wantRead: 0, wantValue: "{\"a\":1}\nsecond line", wantCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			answers := filepath.Join(dir, "answers")
			if err := forkSafeWriteFile(answers, []byte(strings.Join(tc.answers, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "calls")
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			curl := `#!/bin/sh
printf '%s\n' "$*" >>"$CALLS"
n=$(($(wc -l <"$CALLS")))
line=$(sed -n "${n}p" "$ANSWERS")
if [ -z "$line" ]; then
	exit 28
fi
status=${line%% *}
rest=${line#* }
code=${rest%% *}
body=${rest#"$code"}
body=${body# }
if [ "$code" != 000 ]; then
	printf '%b\n%s' "$body" "$code"
fi
exit "$status"
`
			if err := forkSafeWriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
				t.Fatal(err)
			}

			script := strings.Join([]string{
				"set -euo pipefail",
				`log() { printf 'billet-agent: %s\n' "$*" >&2; }`,
				"MMDS=169.254.169.254",
				"token=session-token",
				"fetch_within=" + strconv.Itoa(tc.within),
				block,
				"read=0",
				`value=$(fetch contract) || read=$?`,
				`printf 'read=%s\n' "$read"`,
				`printf 'value=%s<end>\n' "$value"`,
			}, "\n")
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CALLS="+calls, "ANSWERS="+answers)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the read block failed: %v\n%s", err, output)
			}
			out := string(output)

			if want := "read=" + strconv.Itoa(tc.wantRead) + "\n"; !strings.Contains(out, want) {
				t.Errorf("fetch returned something other than %q:\n%s", want, out)
			}
			if tc.wantRead == 0 && !strings.Contains(out, "value="+tc.wantValue+"<end>") {
				t.Errorf("fetch did not return %q whole:\n%s", tc.wantValue, out)
			}
			if tc.wantLog != "" && !strings.Contains(out, tc.wantLog) {
				t.Errorf("the agent did not say %q:\n%s", tc.wantLog, out)
			}

			recorded, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
			if tc.wantCalls > 0 && len(lines) != tc.wantCalls {
				t.Errorf("curl was called %d times, want %d:\n%s", len(lines), tc.wantCalls, recorded)
			}
			if tc.wantCalls == 0 && len(lines) < 2 {
				t.Errorf("a service that never answered was asked %d times; it must be asked again "+
					"until the bound", len(lines))
			}
			for _, line := range lines {
				if !strings.Contains(line, "X-metadata-token: session-token") ||
					!strings.HasSuffix(line, "http://169.254.169.254/latest/meta-data/billet/contract") {
					t.Errorf("a read did not carry the session token to the contract's path: %q", line)
				}
			}
		})
	}
}
