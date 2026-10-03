package scripts_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
		// Each line is one curl invocation: its exit status, the HTTP code its
		// write-out reports and the body, which printf %b expands. Past the last
		// line every call times out.
		answers []string
		within  int
		// gaveUp says an earlier read already spent the agent's budget.
		gaveUp    bool
		wantRead  int
		wantValue string
		wantCalls int
		// maxCalls bounds the attempts within the budget: one a second, so a
		// read that ignored fetch_within would exceed it. Slow scheduling only
		// lowers the count, so the bound cannot flake.
		maxCalls   int
		wantGaveUp bool
		wantLog    string
	}{
		{
			name:    "a read that timed out is asked again until it is answered",
			answers: []string{"28 000", "7 000", "0 200 10"},
			within:  30, wantRead: 0, wantValue: "10", wantCalls: 3,
		},
		{
			name:    "a 200 whose body did not arrive whole is asked again",
			answers: []string{`18 200 {"par`, `0 200 {"whole":1}`},
			within:  30, wantRead: 0, wantValue: `{"whole":1}`, wantCalls: 2,
		},
		{
			name:    "an absent key is an answer and is not asked again",
			answers: []string{"0 404 not found"},
			within:  30, wantRead: 3, wantCalls: 1,
		},
		{
			name:    "an absent key is an answer even when its body was cut off",
			answers: []string{"18 404 not fo"},
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
			within:  2, wantRead: 1, wantGaveUp: true, maxCalls: 4, wantLog: "could not read contract",
		},
		{
			name:    "after one read spent the budget the next is asked once",
			answers: nil, gaveUp: true,
			within: 30, wantRead: 1, wantCalls: 1, wantGaveUp: true,
		},
		{
			name:    "an answer after the budget was spent restores it",
			answers: []string{"0 200 10"}, gaveUp: true,
			within: 30, wantRead: 0, wantValue: "10", wantCalls: 1,
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
			gaveUp := filepath.Join(dir, "unanswered")
			if tc.gaveUp {
				if err := forkSafeWriteFile(gaveUp, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			calls := filepath.Join(dir, "calls")
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			// The fake refuses the arguments the read depends on being absent or
			// wrong: the write-out is where the code is read from, and without the
			// two timeouts a stalled transfer would never reach the budget check.
			// Like curl, it writes the write-out whatever happened.
			curl := `#!/bin/bash
printf '%s\n' "$*" >>"$CALLS"
writeout="" maxtime="" connect="" prev=""
for arg in "$@"; do
	case $prev in
	-w) writeout=$arg ;;
	--max-time) maxtime=$arg ;;
	--connect-timeout) connect=$arg ;;
	esac
	prev=$arg
done
if [ "$writeout" != '\n%{http_code}' ] || [ "$maxtime" != 5 ] || [ "$connect" != 2 ]; then
	echo "fake curl: write-out [$writeout], max-time [$maxtime], connect-timeout [$connect]" >&2
	exit 99
fi
n=$(($(wc -l <"$CALLS")))
line=$(sed -n "${n}p" "$ANSWERS")
if [ -z "$line" ]; then
	line="28 000"
fi
status=${line%% *}
rest=${line#* }
code=${rest%% *}
body=${rest#"$code"}
body=${body# }
printf '%b\n%s' "$body" "$code"
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
				"fetch_gave_up=" + shellQuote(gaveUp),
				block,
				"read=0",
				`value=$(fetch contract) || read=$?`,
				`printf 'read=%s\n' "$read"`,
				`printf 'value=%s<end>\n' "$value"`,
			}, "\n")
			// A read that ignored its budget would otherwise hang the suite.
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", script)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CALLS="+calls, "ANSWERS="+answers)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the read block failed: %v\n%s", err, output)
			}
			out := string(output)

			if strings.Contains(out, "fake curl:") {
				t.Fatalf("the agent called curl with arguments that break the read:\n%s", out)
			}
			if want := "read=" + strconv.Itoa(tc.wantRead) + "\n"; !strings.Contains(out, want) {
				t.Errorf("fetch returned something other than %q:\n%s", want, out)
			}
			if tc.wantRead == 0 && !strings.Contains(out, "value="+tc.wantValue+"<end>") {
				t.Errorf("fetch did not return %q whole:\n%s", tc.wantValue, out)
			}
			if tc.wantLog != "" && !strings.Contains(out, tc.wantLog) {
				t.Errorf("the agent did not say %q:\n%s", tc.wantLog, out)
			}
			_, statErr := os.Stat(gaveUp)
			if gotGaveUp := statErr == nil; gotGaveUp != tc.wantGaveUp {
				t.Errorf("the spent-budget marker exists = %v, want %v (%v)", gotGaveUp, tc.wantGaveUp, statErr)
			}

			recorded, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
			if tc.wantCalls > 0 && len(lines) != tc.wantCalls {
				t.Errorf("curl was called %d times, want %d:\n%s", len(lines), tc.wantCalls, recorded)
			}
			if tc.maxCalls > 0 && len(lines) > tc.maxCalls {
				t.Errorf("curl was called %d times in a %d-second budget, more than %d; the read "+
					"is not honouring fetch_within", len(lines), tc.within, tc.maxCalls)
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
