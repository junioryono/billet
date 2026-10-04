package guestassets

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// execEnv runs exec-env.sh under dash with an empty environment, stream as
// descriptor 3 (none when stream is nil), and returns stdout, stderr and the
// exit status.
//
// UNDER DASH, because every guest's /bin/sh is dash and the two shells differ
// exactly where this script is careful: bash ignores a PATH naming %builtin.
// THE SCRIPT IS RUN BY THE SHELL, NOT EXECUTED, so no test writes an executable
// and the ETXTBSY window in exec_test.go never opens.
func execEnv(t *testing.T, stream *string, args ...string) (string, string, int) {
	t.Helper()

	return execEnvIn(t, []string{}, stream, args...)
}

// execEnvIn is execEnv with the helper's own environment, which env -i leaves
// empty in every guest; a test sets one only to put a fake on its PATH.
func execEnvIn(t *testing.T, env []string, stream *string, args ...string) (string, string, int) {
	t.Helper()

	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Fatalf("no dash on PATH; every guest runs this helper under dash: %v", err)
	}
	run := exec.CommandContext(t.Context(), dash, append([]string{"exec-env.sh"}, args...)...)
	run.Env = env
	if stream != nil {
		path := filepath.Join(t.TempDir(), "env")
		if err := os.WriteFile(path, []byte(*stream), 0o600); err != nil {
			t.Fatalf("write the environment stream: %v", err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("open the environment stream: %v", err)
		}
		t.Cleanup(func() { _ = file.Close() })
		run.ExtraFiles = []*os.File{file}
	}
	var stdout, stderr bytes.Buffer
	run.Stdout = &stdout
	run.Stderr = &stderr
	err = run.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.String(), stderr.String(), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("run exec-env.sh: %v", err)
	}
	return stdout.String(), stderr.String(), 0
}

// environment is env's output as a sorted list of assignments, all of them: the
// helper runs under dash here, as in every guest, and dash adds nothing.
func environment(out string) []string {
	var got []string
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if line != "" {
			got = append(got, line)
		}
	}
	slices.Sort(got)
	return got
}

func TestTheListedEnvironmentIsTheWholeEnvironment(t *testing.T) {
	t.Parallel()

	const awkward = `s3cr3t va$lue "q" 'x' back\slash =eq`
	stream := "A=1\nTOKEN=" + awkward + "\nEMPTY=\nPATH=/usr/bin:/bin\nend\n"
	out, stderr, status := execEnv(t, &stream, "/usr/bin/env")
	if status != 0 {
		t.Fatalf("exec-env.sh exited %d: %s", status, stderr)
	}
	want := []string{"A=1", "EMPTY=", "PATH=/usr/bin:/bin", "TOKEN=" + awkward}
	if got := environment(out); !slices.Equal(got, want) {
		t.Fatalf("the command's environment = %q, want exactly %q", got, want)
	}
}

// A LATER LINE WINS, as a later assignment did on env's command line: the image's
// own PATH is listed after the runner's default and must replace it.
func TestALaterLineReplacesAnEarlierOne(t *testing.T) {
	t.Parallel()

	stream := "PATH=/first\nPATH=/usr/bin:/bin\nend\n"
	out, stderr, status := execEnv(t, &stream, "/usr/bin/env")
	if status != 0 {
		t.Fatalf("exec-env.sh exited %d: %s", status, stderr)
	}
	if got, want := environment(out), []string{"PATH=/usr/bin:/bin"}; !slices.Equal(got, want) {
		t.Fatalf("the command's environment = %q, want %q", got, want)
	}
}

// THE DESCRIPTOR IS CLOSED BEFORE THE EXEC, so the runner and every job step it
// starts inherit no handle on the stream that carried the registration.
func TestTheStreamIsNotInheritedByTheCommand(t *testing.T) {
	t.Parallel()

	stream := "A=1\nend\n"
	probe := `if { true <&3; } 2>/dev/null; then echo open; else echo closed; fi`
	out, stderr, status := execEnv(t, &stream, "/bin/sh", "-c", probe)
	if status != 0 {
		t.Fatalf("exec-env.sh exited %d: %s", status, stderr)
	}
	if out != "closed\n" {
		t.Fatalf("descriptor 3 in the command: %q, want closed", out)
	}
}

// EVERY REFUSAL STOPS THE LAUNCH, says why without the value, and runs nothing.
// A truncated stream would otherwise start a runner with part of its
// environment, which reads as a job that failed for reasons of its own.
func TestAStreamTheWriterGotWrongIsRefused(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-value"
	for _, tc := range []struct {
		name   string
		stream *string
		args   []string
		clause string
	}{
		{"no terminator", new("A=1\nTOKEN=" + secret + "\n"), nil, "ended without its terminator after 2 line(s)"},
		{"a partial last line", new("A=1\nTOKEN=" + secret), nil, "ended without its terminator after 1 line(s)"},
		{"an empty stream", new(""), nil, "ended without its terminator after 0 line(s)"},
		{"a line that is not an assignment", new("A=1\n" + secret + "\nend\n"), nil, "line 2 is not NAME=VALUE"},
		{"a name beginning with a digit", new("1A=" + secret + "\nend\n"), nil, "line 1 is not NAME=VALUE"},
		{"an empty name", new("=" + secret + "\nend\n"), nil, "line 1 is not NAME=VALUE"},
		{"a name with a hyphen", new("A-B=" + secret + "\nend\n"), nil, "line 1 does not name a variable"},
		{"a blank line", new("A=1\n\nend\n"), nil, "line 2 is not NAME=VALUE"},
		{"an assignment after the terminator", new("A=1\nend\nB=" + secret + "\n"), nil, "goes on after its terminator"},
		{"a partial line after the terminator", new("A=1\nend\nB=" + secret), nil, "goes on after its terminator"},
		{"a blank line after the terminator", new("A=1\nend\n\n"), nil, "goes on after its terminator"},
		{"OPTIND, which dash parses as a number", new("OPTIND=" + secret + "\nOPTIND=1\nend\n"), nil, "line 1 names OPTIND"},
		{"no descriptor 3", nil, nil, "descriptor 3 is not open"},
		{"no command", new("A=1\nend\n"), []string{}, "no command to run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			marker := filepath.Join(t.TempDir(), "ran")
			args := tc.args
			if args == nil {
				args = []string{"/usr/bin/touch", marker}
			}
			out, stderr, status := execEnv(t, tc.stream, args...)
			if status != 2 {
				t.Fatalf("exec-env.sh exited %d, want 2 (stdout %q, stderr %q)", status, out, stderr)
			}
			if !strings.Contains(stderr, tc.clause) {
				t.Errorf("the refusal %q does not say %q", stderr, tc.clause)
			}
			if strings.Contains(stderr, secret) {
				t.Errorf("the refusal quotes the value: %q", stderr)
			}
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the command ran after a refusal (lstat: %v)", err)
			}
		})
	}
}

// NOTHING LISTED CHANGES HOW THE HELPER RUNS WHILE IT HOLDS VALUES. A PATH naming
// dash's %builtin after a directory makes `[` and `echo` external commands, so a
// helper that exported as it read would hand a later value to a process's argv.
// The fakes here record any invocation at all.
func TestAListedPathCannotTurnTheHelpersBuiltinsIntoProcesses(t *testing.T) {
	t.Parallel()

	fake := t.TempDir()
	record := filepath.Join(t.TempDir(), "invoked")
	for _, name := range []string{"[", "test", "echo", "printf"} {
		body := "#!/bin/sh\n/usr/bin/printf '%s %s\\n' \"$0\" \"$*\" >>" + record + "\n"
		if err := writeExecutable(filepath.Join(fake, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write the fake %s: %v", name, err)
		}
	}

	stream := "PATH=" + fake + ":%builtin\nTOKEN=s3cr3t\nend\n"
	out, stderr, status := execEnv(t, &stream, "/usr/bin/env")
	if status != 0 {
		t.Fatalf("exec-env.sh exited %d: %s", status, stderr)
	}
	want := []string{"PATH=" + fake + ":%builtin", "TOKEN=s3cr3t"}
	if got := environment(out); !slices.Equal(got, want) {
		t.Errorf("the command's environment = %q, want %q", got, want)
	}
	if invoked, err := os.ReadFile(record); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the helper ran a command from the listed PATH (err %v):\n%s", err, invoked)
	}
}

// A FAILED READ AFTER THE TERMINATOR IS NOT PROOF THAT NOTHING FOLLOWS IT. dash's
// `read` answers an error as it answers the end of the stream, so the helper
// reads the rest with od; an od that fails here stands in for a read error.
func TestAnUnreadableTailRefusesTheLaunch(t *testing.T) {
	t.Parallel()

	fake := t.TempDir()
	if err := writeExecutable(filepath.Join(fake, "od"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write the failing od: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	stream := "A=1\nend\n"
	_, stderr, status := execEnvIn(t, []string{"PATH=" + fake + ":/usr/bin:/bin"}, &stream,
		"/usr/bin/touch", marker)
	if status != 2 || !strings.Contains(stderr, "what follows the terminator could not be read") {
		t.Fatalf("an unreadable tail was not refused (status %d, stderr %q)", status, stderr)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the command ran after the refusal (lstat: %v)", err)
	}
}

// A LISTED NAME EQUAL TO ONE OF THE HELPER'S OWN VARIABLES CHANGES NOTHING. It
// cannot stand in for the terminator, cannot put a value in a refusal, and is
// exported with its listed value like any other.
func TestAListedNameCannotRewriteTheHelpersState(t *testing.T) {
	t.Parallel()

	t.Run("the done flag", func(t *testing.T) {
		t.Parallel()

		stream := "billet_exec_env_done=1\nA=1\n"
		_, stderr, status := execEnv(t, &stream, "/usr/bin/env")
		if status != 2 || !strings.Contains(stderr, "ended without its terminator after 2 line(s)") {
			t.Fatalf("a stream with no terminator ran (status %d, stderr %q)", status, stderr)
		}
	})
	t.Run("the line counter", func(t *testing.T) {
		t.Parallel()

		stream := "billet_exec_env_count=s3cr3t\n"
		_, stderr, status := execEnv(t, &stream, "/usr/bin/env")
		if status != 2 || strings.Contains(stderr, "s3cr3t") {
			t.Fatalf("the refusal (status %d) quotes a value: %q", status, stderr)
		}
	})
	t.Run("kept with its value", func(t *testing.T) {
		t.Parallel()

		stream := "billet_exec_env_kv=kept\nbillet_exec_env_all=too\nIFS=x\nend\n"
		out, stderr, status := execEnv(t, &stream, "/usr/bin/env")
		if status != 0 {
			t.Fatalf("exec-env.sh exited %d: %s", status, stderr)
		}
		want := []string{"IFS=x", "billet_exec_env_all=too", "billet_exec_env_kv=kept"}
		if got := environment(out); !slices.Equal(got, want) {
			t.Fatalf("the command's environment = %q, want %q", got, want)
		}
	})
}
