package scripts_test

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// BILLET IS READ IN, IN THE BACKGROUND, AS SOON AS THE GUEST KNOWS A CACHE WILL RUN IT
// (#878): the Go, Bazel and Git caches each run billet as a helper, and a fresh EC2
// guest's first exec of it took 9.0 s against Bazel's 10 s wait. The block is executed
// with a reader that says what it was given and then holds until released, and the
// release is written only after the block has returned and the reader has spoken, so a
// pass proves boot does not wait on the read.
func TestTheAgentReadsBilletInWhenACacheWillRunIt(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_PREREAD")
	billet := filepath.Join(t.TempDir(), "billet")
	if err := forkSafeWriteFile(billet, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		endpoint string
		caches   string
		binary   string
		wantRead bool
	}{
		{name: "go", endpoint: "http://172.31.0.1:7718", caches: "go", binary: billet, wantRead: true},
		{name: "bazel among others", endpoint: "http://172.31.0.1:7718", caches: "docker,bazel,npm",
			binary: billet, wantRead: true},
		{name: "git", endpoint: "http://172.31.0.1:7718", caches: "git", binary: billet, wantRead: true},
		{name: "go test results alone", endpoint: "http://172.31.0.1:7718", caches: "go-test-results",
			binary: billet},
		{name: "no cache runs billet", endpoint: "http://172.31.0.1:7718", caches: "docker,npm", binary: billet},
		{name: "no cache session", caches: "go,bazel", binary: billet},
		{name: "an image without billet", endpoint: "http://172.31.0.1:7718", caches: "go",
			binary: filepath.Join(t.TempDir(), "absent")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			release := filepath.Join(dir, "release")
			reader := filepath.Join(dir, "reader")
			if err := forkSafeWriteFile(reader, []byte("#!/bin/sh\n"+
				"printf '%s\\n' \"$1\" >&3\n"+
				"while [ ! -e "+shellQuote(release)+" ]; do sleep 0.05; done\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			// A READ NOBODY WANTS IS RELEASED BEFORE IT COULD START, so one started wrongly
			// says so and ends.
			if !tc.wantRead {
				if err := forkSafeWriteFile(release, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// RELEASED WHATEVER HAPPENS, and registered after t.TempDir's removal so it runs
			// before it: a reader never outlives the file it waits for.
			t.Cleanup(func() {
				if err := os.WriteFile(release, nil, 0o600); err != nil {
					t.Errorf("release the reader: %v", err)
				}
			})

			// EVERY PROCESS THE BLOCK STARTS INHERITS THE PIPE'S WRITE END as descriptor 3,
			// and the read end comes to its end only once every one of them has exited,
			// reaped or not, so no signal, process group or quiet interval is needed to know
			// the block left nothing running.
			said, err := startSaying(t)
			if err != nil {
				t.Fatal(err)
			}
			output, err := os.Create(filepath.Join(dir, "output"))
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()

			script := strings.Join([]string{
				"set -euo pipefail",
				"cache_endpoint=" + shellQuote(tc.endpoint),
				"guest_caches=" + shellQuote(tc.caches),
				"GUEST_BILLET=" + shellQuote(tc.binary),
				"GUEST_PREREAD=" + shellQuote(reader),
				block,
				"echo done",
			}, "\n")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", script)
			// A FILE, not a pipe, so Wait waits for bash alone and never for a reader that
			// kept its output.
			cmd.Stdout, cmd.Stderr = output, output
			cmd.ExtraFiles = []*os.File{said.writer}
			cmd.WaitDelay = time.Second
			err = cmd.Start()
			said.closeWriter(t)
			if err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			out, readErr := os.ReadFile(output.Name())
			if err != nil || readErr != nil || strings.TrimSpace(string(out)) != "done" {
				t.Fatalf("the block did not return while its read was held: %v %v\n%s", err, readErr, out)
			}

			var read []string
			if tc.wantRead {
				select {
				case line, ok := <-said.lines:
					if !ok {
						t.Fatal("everything the block started ended without reading billet in")
					}
					read = append(read, line)
				case <-time.After(5 * time.Second):
					t.Fatal("billet was not read in within 5 s of the block returning")
				}
				if err := os.WriteFile(release, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ended := time.After(10 * time.Second)
			for open := true; open; {
				select {
				case line, ok := <-said.lines:
					if ok {
						read = append(read, line)
					}
					open = ok
				case <-ended:
					t.Fatalf("what the block started has not ended; it read %q", read)
				}
			}
			var want []string
			if tc.wantRead {
				want = []string{tc.binary}
			}
			if !slices.Equal(read, want) {
				t.Errorf("the block read %q, want %q", read, want)
			}
		})
	}
}

// saying is a pipe whose write end the processes under test inherit, and the lines they
// write to it; lines is closed once every holder of the write end has exited.
type saying struct {
	writer *os.File
	lines  chan string
}

func startSaying(t *testing.T) (*saying, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	s := &saying{writer: w, lines: make(chan string, 8)}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer close(s.lines)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			select {
			case s.lines <- scanner.Text():
			case <-stop:
				return
			}
		}
	}()
	// Closing the read end ends the scan, and the goroutine is joined before the test
	// returns.
	t.Cleanup(func() {
		close(stop)
		if err := r.Close(); err != nil {
			t.Errorf("close the pipe: %v", err)
		}
		<-done
	})

	return s, nil
}

// closeWriter drops the test's own copy of the write end once the child holds its own,
// so the read end's end is the children's.
func (s *saying) closeWriter(t *testing.T) {
	t.Helper()
	if err := s.writer.Close(); err != nil {
		t.Errorf("close the pipe's write end: %v", err)
	}
}
