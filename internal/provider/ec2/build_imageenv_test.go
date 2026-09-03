package ec2

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE IMAGE DECLARES WHAT IT IS, in the file and the format the guest uses.
//
// One contract across both backends: /etc/billet-image-env, KEY=VALUE per line,
// read into the job's environment. A different spelling here would make a
// workflow's toolcache lookup a per-backend question, which is the asymmetry this
// work exists to remove.
func TestTheImageDeclaresItsToolcacheAndIdentity(t *testing.T) {
	t.Parallel()

	script := mustScript(t)

	for _, want := range []struct {
		line string
		why  string
	}{
		{"RUNNER_TOOL_CACHE=" + toolcacheDir, "setup-* actions read only this one"},
		{"AGENT_TOOLSDIRECTORY=" + toolcacheDir, "and the runner itself honours this one"},
		{"ImageOS=ubuntu24", "third-party actions branch on it"},
		{"ImageVersion=billet", "and on this"},
	} {
		if !strings.Contains(script, want.line+"\n") {
			t.Errorf("the image env file is missing %q — %s", want.line, want.why)
		}
	}

	// THE DIRECTORY IS CREATED, AND WRITABLE. Exporting RUNNER_TOOL_CACHE without
	// it is WORSE than exporting neither: it points every setup action at a path
	// under root-owned /opt that the unprivileged runner cannot create, so an
	// action that would have fallen back to its own location fails outright.
	if !strings.Contains(script, "install -d -m 0777 "+toolcacheDir+"\n") {
		t.Errorf("the toolcache directory is not created writable, so the runner cannot add " +
			"to it and the variables above point at nothing")
	}

	// AND IT IS CREATED BEFORE IT IS DECLARED, since a job that starts between the
	// two would be told about a directory that is not there.
	lines := strings.Split(script, "\n")
	mk := lineOf(t, lines, "install -d -m 0777 "+toolcacheDir)
	declare := lineOf(t, lines, "RUNNER_TOOL_CACHE="+toolcacheDir)

	if mk >= declare {
		t.Errorf("the toolcache is declared at line %d and created at %d", declare, mk)
	}
}

// THE ENTRY POINT ACTUALLY READS THE FILE, and this runs the emitted shell rather
// than looking for it.
//
// `env -i` means a variable not named on that command line does not exist for the
// job. So the question is not whether the file is written — it is whether its
// contents reach the invocation, and only running the block answers that.
func TestTheEntryPointCarriesTheImageEnvIntoTheJob(t *testing.T) {
	t.Parallel()

	block := imageEnvBlock(t, mustScript(t))

	for _, tc := range []struct {
		name  string
		file  string
		write bool
		want  []string
		deny  []string
	}{
		{
			name:  "the ordinary file",
			write: true,
			file: "ImageOS=ubuntu24\nRUNNER_TOOL_CACHE=/opt/hostedtoolcache\n" +
				"AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache\n",
			want: []string{"ImageOS=ubuntu24", "RUNNER_TOOL_CACHE=/opt/hostedtoolcache"},
		},
		{
			// WHAT THE TOOLCACHE INSTALL WILL APPEND. JAVA_HOME and its per-version
			// siblings are only known after the JDKs exist, which is why the entry
			// point reads a file rather than carrying baked-in values.
			name:  "a JAVA_HOME the install appended",
			write: true,
			file:  "RUNNER_TOOL_CACHE=/opt/hostedtoolcache\nJAVA_HOME_17_X64=/usr/lib/jvm/x\n",
			want:  []string{"JAVA_HOME_17_X64=/usr/lib/jvm/x"},
		},
		{
			// A VALUE WITH A SPACE. Expanding the file unquoted into the command
			// line would split this into two arguments, and env would reject the
			// second as a malformed assignment — taking the job with it.
			name:  "a value containing a space",
			write: true,
			file:  "BILLET_NOTE=two words\n",
			want:  []string{"BILLET_NOTE=two words"},
		},
		{
			// COMMENTS AND BLANKS ARE SKIPPED, the same filter the guest applies.
			// Handed to env, either is a malformed assignment.
			name:  "comments and blank lines",
			write: true,
			file:  "# a comment\n\nImageOS=ubuntu24\n   \n",
			want:  []string{"ImageOS=ubuntu24"},
			deny:  []string{"# a comment"},
		},
		{
			// NO FILE IS NOT A FAILURE. The entry point runs under `set -eu`, and
			// an unreadable file must leave the job with an empty addition rather
			// than kill the runner before it registers.
			name:  "no file at all",
			write: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "billet-image-env")

			if tc.write {
				if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
					t.Fatalf("write the image env: %v", err)
				}
			}

			// Only the path moves; the commands are the generated script's bytes.
			runnable := strings.ReplaceAll(block, imageEnvFile, path)

			// Print what would reach `env -i`, one argument per line, so a value
			// containing a space is visibly one argument rather than two.
			script := "set -eu\n" + runnable + "\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"

			out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", script).Output()
			if err != nil {
				t.Fatalf("the image-env block failed: %v\n--- block ---\n%s", err, runnable)
			}

			got := string(out)

			for _, w := range tc.want {
				if !strings.Contains(got, w+"\n") {
					t.Errorf("%q never reached the job's environment; got:\n%s", w, got)
				}
			}

			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("%q reached env as an assignment; got:\n%s", d, got)
				}
			}
		})
	}
}

// imageEnvBlock lifts the entry point's image-env read out of the generated
// script: from `set --` to the `fi` that closes it.
func imageEnvBlock(t *testing.T, script string) string {
	t.Helper()

	// THE EXACT LINE, not a substring: `set -- "$@" "$billet_line"` inside the loop
	// contains `set --` too, and lineOf refuses an ambiguous marker rather than
	// picking one — which is how this was caught instead of silently lifting the
	// wrong region.
	lines := strings.Split(script, "\n")
	start := -1

	for i, l := range lines {
		if l == "set --" {
			if start >= 0 {
				t.Fatalf("`set --` opens a block at line %d and again at %d", start, i)
			}

			start = i
		}
	}

	if start < 0 {
		t.Fatal("the entry point never opens the image-env block")
	}

	for i := start; i < len(lines); i++ {
		if lines[i] == "fi" {
			return strings.Join(lines[start:i+1], "\n") + "\n"
		}
	}

	t.Fatalf("the image-env block starting at line %d is never closed", start)

	return ""
}

// THE VARIABLES REACH THE CHILD'S ENVIRONMENT, which is the property, and the
// first version of this file did not test it.
//
// TestTheEntryPointCarriesTheImageEnvIntoTheJob lifts the `set --` block and runs
// it, so it proves the positional parameters get populated. It says nothing about
// whether they are PASSED — and a mutant deleting `"$@"` from the env invocation
// survived it. That is the toolcache on disk and invisible, which is the entire
// failure this phase exists to prevent.
//
// So this runs the invocation itself. `setpriv` is Linux-only and this suite runs
// on macOS too, so the privilege drop is replaced with nothing and the runner
// with a printenv — everything between them, including `"$@"` and the explicit
// assignments, is the generated script's own bytes.
func TestTheImageEnvReachesTheJobsEnvironment(t *testing.T) {
	t.Parallel()

	script := mustScript(t)
	lines := strings.Split(script, "\n")

	start := -1

	for i, l := range lines {
		if l == "set --" {
			start = i

			break
		}
	}

	if start < 0 {
		t.Fatal("the entry point never opens the image-env block")
	}

	end := -1

	for i := start; i < len(lines); i++ {
		if strings.HasSuffix(lines[i], "/opt/actions-runner/billet-runner-service") {
			end = i

			break
		}
	}

	if end <= start {
		t.Fatalf("the invocation opens at line %d and never reaches the runner", start)
	}

	dir := t.TempDir()
	envFile := filepath.Join(dir, "billet-image-env")

	if err := os.WriteFile(envFile, []byte(
		"RUNNER_TOOL_CACHE=/opt/hostedtoolcache\n"+
			"AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache\n"+
			"ImageOS=ubuntu24\n"+
			"JAVA_HOME_17_X64=/usr/lib/jvm/temurin-17\n"), 0o600); err != nil {
		t.Fatalf("write the image env: %v", err)
	}

	runnable := strings.Join(lines[start:end+1], "\n") + "\n"
	runnable = strings.ReplaceAll(runnable, imageEnvFile, envFile)

	// The privilege drop is Linux-only; the assignments after it are the subject.
	runnable = strings.ReplaceAll(runnable,
		"setpriv --reuid=runner --regid=runner --init-groups \\\n", "")
	runnable = strings.ReplaceAll(runnable,
		"/opt/actions-runner/billet-runner-service", "sh -c printenv")

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c",
		"set -eu\nrunner_started=0\n"+jitEnvVar+"=jit\n"+runnable)

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the invocation failed: %v\n--- block ---\n%s", err, runnable)
	}

	got := string(out)

	for _, want := range []string{
		"RUNNER_TOOL_CACHE=/opt/hostedtoolcache",
		"AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache",
		"ImageOS=ubuntu24",
		"JAVA_HOME_17_X64=/usr/lib/jvm/temurin-17",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("%q never reached the job's environment, so the toolcache is on disk "+
				"and invisible; env was:\n%s", want, got)
		}
	}

	// AND THE EXPLICIT ASSIGNMENTS SURVIVE ALONGSIDE THEM. Putting "$@" in the
	// wrong place — after the command, say — would carry the image env and drop
	// these, which no assertion above would notice.
	if !strings.Contains(got, "ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED=true\n") {
		t.Errorf("the explicit assignments no longer reach the job; env was:\n%s", got)
	}
}
