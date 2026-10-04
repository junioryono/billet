package ec2

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// `env -i` means a variable not in billet_env does not exist for the job. So the
// question is not whether the file is written — it is whether its contents reach
// the stream the runner is launched with, and only running the block answers that.
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
			// billet-exec-env refuses either, which would refuse the launch.
			name:  "comments and blank lines",
			write: true,
			file:  "# a comment\n\nImageOS=ubuntu24\n   \n",
			want:  []string{"ImageOS=ubuntu24"},
			deny:  []string{"# a comment", "   "},
		},
		{
			// A NAME THAT IS NOT A VARIABLE'S IS SKIPPED for the same reason.
			name:  "a name with a hyphen",
			write: true,
			file:  "NOT-A-NAME=x\nImageOS=ubuntu24\n",
			want:  []string{"ImageOS=ubuntu24"},
			deny:  []string{"NOT-A-NAME"},
		},
		{
			// OPTIND IS THE SHELL'S OWN STATE, which billet-exec-env refuses.
			name:  "OPTIND",
			write: true,
			file:  "OPTIND=x\nImageOS=ubuntu24\n",
			want:  []string{"ImageOS=ubuntu24"},
			deny:  []string{"OPTIND"},
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

			// Print the stream billet-exec-env would read, one assignment per line.
			script := "set -eu\n" + runnable + "\nprintf '%s' \"$billet_env\"\n"

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

// entryPoint is the generated billet-runner script, out of the provisioning
// script that writes it.
func entryPoint(t *testing.T, script string) string {
	t.Helper()

	_, after, ok := strings.Cut(script, "cat > /usr/local/bin/billet-runner <<'BILLETEOF'\n")
	if !ok {
		t.Fatal("no entry point is written")
	}

	entry, _, ok := strings.Cut(after, "BILLETEOF\n")
	if !ok {
		t.Fatal("the entry point heredoc is not closed")
	}

	return entry
}

// imageEnvBlock lifts the entry point's environment build out of the generated
// script: from the billet_env assignment to the `fi` that closes the image-env
// read.
func imageEnvBlock(t *testing.T, script string) string {
	t.Helper()

	lines := strings.Split(entryPoint(t, script), "\n")
	start := -1

	for i, l := range lines {
		if strings.HasPrefix(l, "billet_env='") {
			if start >= 0 {
				t.Fatalf("billet_env is built at line %d and again at %d", start, i)
			}

			start = i
		}
	}

	if start < 0 {
		t.Fatal("the entry point never builds billet_env")
	}

	for i := start; i < len(lines); i++ {
		if lines[i] == "fi" {
			return strings.Join(lines[start:i+1], "\n") + "\n"
		}
	}

	t.Fatalf("the image-env block starting at line %d is never closed", start)

	return ""
}

// launchFixture is the entry point's launch, from the environment build to the
// runner's exit status, rewritten to run here: the image env file, the helper
// and the runner service move, and setpriv is a stub on PATH that records every
// word of its argv, which is env's argv and the helper's too, before it execs
// the rest.
type launchFixture struct {
	script  string
	argvLog string
	path    string
}

func newLaunchFixture(t *testing.T, imageEnv string) launchFixture {
	t.Helper()

	entry := entryPoint(t, mustScript(t))

	start := strings.Index(entry, "billet_env='")
	end := strings.Index(entry, "job_status=$?\nfi\n")
	if start < 0 || end <= start {
		t.Fatal("the entry point's launch is no longer extractable")
	}

	dir := t.TempDir()
	envFile := filepath.Join(dir, "billet-image-env")
	writeFile(t, envFile, imageEnv, 0o600)

	runnable := entry[start : end+len("job_status=$?\nfi\n")]
	runnable = substitute(t, runnable, imageEnvFile, envFile)
	runnable = substitute(t, runnable, execEnvPath, execEnvForTest(t))
	runnable = substitute(t, runnable, "/opt/actions-runner/billet-runner-service", "/usr/bin/env")

	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatalf("make the stub directory: %v", err)
	}

	argvLog := filepath.Join(dir, "setpriv.argv")
	stub := "#!/bin/sh\n" +
		"for word in \"$@\"; do printf '%s\\n' \"$word\"; done >" + argvLog + "\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in --*) shift ;; *) break ;; esac\n" +
		"done\n" +
		"exec \"$@\"\n"
	if err := forkSafeWriteFile(filepath.Join(bin, "setpriv"), []byte(stub)); err != nil {
		t.Fatalf("write the setpriv stub: %v", err)
	}

	return launchFixture{
		script: "set -eu\nrunner_started=222\n" + runnable +
			"echo \"after the launch: $job_status\" >&2\nexit \"$job_status\"\n",
		argvLog: argvLog,
		path:    bin + ":/usr/bin:/bin",
	}
}

// run executes the launch with the given exported variables and the inherited
// ones a root shell would carry, poisoned so a value that leaked past `env -i`
// is visible.
func (f launchFixture) run(t *testing.T, exported map[string]string) (string, string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", f.script)
	cmd.Env = []string{"PATH=" + f.path, "HOME=/root", "USER=root", "POISON=inherited"}
	for name, value := range exported {
		cmd.Env = append(cmd.Env, name+"="+value)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return stdout.String(), stderr.String(), err
}

// THE RUNNER'S ENVIRONMENT IS EXACTLY THE LISTED SET, AND NO VALUE IS IN AN
// ARGUMENT LIST (#352). This runs the entry point's own launch, so it proves the
// image's variables, the fixed assignments and the registration all reach the
// runner, that nothing root's shell carried does, and that the registration and
// the cache bearer appear in no word of setpriv's argv, which holds env's and
// billet-exec-env's in turn.
func TestTheEntryPointLaunchesTheRunnerWithExactlyItsEnvironment(t *testing.T) {
	t.Parallel()

	const (
		jit   = `jit s3cr3t $HOME "q" 'x' back\slash =eq`
		token = "cache-s3cr3t"
	)

	f := newLaunchFixture(t,
		"RUNNER_TOOL_CACHE=/opt/hostedtoolcache\n"+
			"AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache\n"+
			"ImageOS=ubuntu24\n"+
			"JAVA_HOME_17_X64=/usr/lib/jvm/temurin-17\n"+
			"PATH=/image/bin:/usr/bin:/bin\n")

	out, stderr, err := f.run(t, map[string]string{
		jitEnvVar:                                            jit,
		"BILLET_LAUNCH_EPOCH_NS":                             "111",
		"BILLET_CACHE_ENDPOINT":                              "http://node:7000",
		"BILLET_CACHE_TOKEN":                                 token,
		"BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES":            "5",
		"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE": "1",
	})
	if err != nil {
		t.Fatalf("the launch failed: %v\n%s", err, stderr)
	}

	// ALL OF IT: the helper runs under dash, as in the image, and dash adds nothing.
	got := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	slices.Sort(got)

	want := []string{
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=" + jobTimingHookPath,
		jitEnvVar + "=" + jit,
		"ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED=true",
		"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1",
		"AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache",
		"BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES=5",
		"BILLET_CACHE_ENDPOINT=http://node:7000",
		"BILLET_CACHE_TOKEN=" + token,
		"BILLET_LAUNCH_EPOCH_NS=111",
		"BILLET_RUNNER_START_EPOCH_NS=222",
		"HOME=/home/runner",
		"ImageOS=ubuntu24",
		"JAVA_HOME_17_X64=/usr/lib/jvm/temurin-17",
		"LANG=C.UTF-8",
		"LOGNAME=runner",
		// THE IMAGE'S PATH REPLACES THE BASE ONE, as a later assignment did on
		// env's command line.
		"PATH=/image/bin:/usr/bin:/bin",
		"RUNNER_TOOL_CACHE=/opt/hostedtoolcache",
		"USER=runner",
	}
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("the runner's environment is\n%s\nwant exactly\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	argv, err := os.ReadFile(f.argvLog)
	if err != nil {
		t.Fatalf("read setpriv's argv: %v", err)
	}
	for _, secret := range []string{"s3cr3t", "http://node:7000"} {
		if strings.Contains(string(argv), secret) {
			t.Errorf("%q is in an argument list on the way to the runner:\n%s", secret, argv)
		}
	}
	if !strings.Contains(string(argv), "\n-i\n") {
		t.Errorf("the launch does not empty the environment with env -i:\n%s", argv)
	}
}

// A VALUE HOLDING A NEWLINE REFUSES THE LAUNCH, naming the variable and not the
// value. One line per variable cannot carry it, and splitting it would hand the
// runner a second assignment the value chose.
func TestTheEntryPointRefusesAValueHoldingANewline(t *testing.T) {
	t.Parallel()

	f := newLaunchFixture(t, "ImageOS=ubuntu24\n")

	_, stderr, err := f.run(t, map[string]string{
		jitEnvVar: "first-s3cr3t\nBILLET_CACHE_TOKEN=forged",
	})
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 1 {
		t.Fatalf("the launch ended with %v, want exit status 1\n%s", err, stderr)
	}
	// THE REFUSAL IS A FAILED JOB, not an exit, so the image-store steps after the
	// launch still run; the fixture's last line stands in for them.
	if !strings.Contains(stderr, "after the launch: 1\n") {
		t.Errorf("the refusal skipped the steps after the launch: %q", stderr)
	}
	if !strings.Contains(stderr, jitEnvVar+" holds a newline") {
		t.Errorf("the refusal %q does not name the variable", stderr)
	}
	if strings.Contains(stderr, "s3cr3t") {
		t.Errorf("the refusal quotes the value: %q", stderr)
	}
	if _, err := os.Lstat(f.argvLog); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("setpriv ran after the refusal (lstat: %v)", err)
	}
}
