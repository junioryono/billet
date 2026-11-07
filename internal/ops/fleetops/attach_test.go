package fleetops

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/relay"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// sessionPipe runs a session against a guest the test plays, and answers the guest's end and how
// the session ended.
func sessionPipe(t *testing.T, in io.Reader, resizes <-chan [2]uint16) (net.Conn, *bytes.Buffer, <-chan sessionEnd) {
	t.Helper()

	operator, guest := net.Pipe()
	t.Cleanup(func() {
		operator.Close()
		guest.Close()
	})

	if err := guest.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer

	done := make(chan sessionEnd, 1)

	go func() {
		status, err := runSession(operator, in, &out, resizes)
		done <- sessionEnd{status, err}
	}()

	return guest, &out, done
}

type sessionEnd struct {
	status int
	err    error
}

func awaitSession(t *testing.T, done <-chan sessionEnd) sessionEnd {
	t.Helper()

	select {
	case end := <-done:
		return end
	case <-time.After(30 * time.Second):
		t.Fatal("the session did not end")

		return sessionEnd{}
	}
}

// A SESSION CARRIES BOTH WAYS AND ANSWERS THE SHELL'S STATUS: what is typed goes out as data, a
// window as a resize, what comes back to the terminal, and the exit frame ends it.
func TestASessionCarriesBothWaysAndEndsWithTheShell(t *testing.T) {
	t.Parallel()

	inner, typing := io.Pipe()
	defer typing.Close()

	resizes := make(chan [2]uint16, 1)
	resizes <- [2]uint16{24, 80}

	guest, out, done := sessionPipe(t, inner, resizes)

	frame, err := relay.ReadFrame(guest)
	if err != nil || frame.Kind != relay.KindResize {
		t.Fatalf("the first frame was %+v, %v; want the window", frame, err)
	}

	rows, cols, err := relay.ParseResize(frame.Payload)
	if err != nil || rows != 24 || cols != 80 {
		t.Errorf("the window read %d by %d, %v", rows, cols, err)
	}

	go func() { _, _ = typing.Write([]byte("ls\n")) }() //nolint:errcheck // the read at the other end is what is checked

	frame, err = relay.ReadFrame(guest)
	if err != nil || frame.Kind != relay.KindData || string(frame.Payload) != "ls\n" {
		t.Fatalf("what was typed arrived as %+v, %v", frame, err)
	}

	if err := relay.WriteFrame(guest, relay.KindData, []byte("a file\n")); err != nil {
		t.Fatal(err)
	}
	if err := relay.WriteFrame(guest, relay.KindExit, relay.ExitPayload(3)); err != nil {
		t.Fatal(err)
	}

	end := awaitSession(t, done)
	if end.err != nil || end.status != 3 {
		t.Errorf("the session ended %+v, want the shell's 3", end)
	}
	if out.String() != "a file\n" {
		t.Errorf("the terminal shows %q", out.String())
	}
}

// A STREAM THAT ENDS WITHOUT A STATUS IS AN ERROR, never zero: a VM destroyed under a session must
// not read as a shell that exited cleanly.
func TestAStreamEndingWithoutAStatusIsAnError(t *testing.T) {
	t.Parallel()

	inner, typing := io.Pipe()
	defer typing.Close()

	guest, _, done := sessionPipe(t, inner, nil)

	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}

	if end := awaitSession(t, done); end.err == nil {
		t.Errorf("a stream that ended read as status %d", end.status)
	}
}

// A FRAME NO END WRITES ENDS THE SESSION, and quotes nothing of it.
func TestAFrameNoEndWritesEndsTheSession(t *testing.T) {
	t.Parallel()

	inner, typing := io.Pipe()
	defer typing.Close()

	guest, _, done := sessionPipe(t, inner, nil)

	go func() { _, _ = guest.Write([]byte("\x09HOSTILE")) }() //nolint:errcheck // the read at the other end is what is checked

	end := awaitSession(t, done)
	if end.err == nil {
		t.Fatal("a frame of no kind was taken")
	}
	if strings.Contains(end.err.Error(), "HOSTILE") {
		t.Errorf("the error quotes the guest: %v", end.err)
	}
}

// A COMMAND LINE THAT NAMES NO ONE JOB IS REFUSED BEFORE ANYTHING IS OPENED.
func TestAttachNamesOneJob(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"nothing":          nil,
		"two leases":       {"a", "b"},
		"a run and no job": {"--run", "5"},
		"a job and no run": {"--job", "test", "a"},
		"a run and lease":  {"--run", "5", "--job", "test", "a"},
		"a run of none":    {"--run", "0", "--job", "test"},
	} {
		var out bytes.Buffer

		err := JobsAttach(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: func(string) string { return "" }}, args)
		if err == nil {
			t.Errorf("%s: no refusal", name)
		}
	}
}

// WHAT IS HELD IS SAID, and a hold whose VM went some other way is not: its row says nothing until its
// bound, and a line offering to open a VM that has gone would send someone after it.
func TestWhatIsHeldIsSaid(t *testing.T) {
	t.Parallel()

	a := waitAllocator(t, ledgertest.Dir(t), nil)
	lease := startWaitedJob(t, a, 1, alloc.HistoryJob{JobID: "job-1"})

	until := time.Now().Add(time.Hour)

	for _, id := range []string{lease, "a-lease-that-has-gone"} {
		if _, err := a.OpenLeaseHold(t.Context(), id, waitTier.Label, until); err != nil {
			t.Fatalf("OpenLeaseHold: %v", err)
		}
	}

	var status, waiting bytes.Buffer

	PrintHolds(t.Context(), cli.Env{Stdout: &status}, a)

	if !strings.Contains(status.String(), lease) {
		t.Errorf("status says %q, without the held lease", status.String())
	}
	if strings.Contains(status.String(), "a-lease-that-has-gone") {
		t.Errorf("status says %q, offering a VM that has gone", status.String())
	}

	sayHolds(t.Context(), cli.Env{Stdout: &waiting}, a)

	if !strings.Contains(waiting.String(), "1 failed jobs' VMs are held") {
		t.Errorf("a wait says %q", waiting.String())
	}
}
