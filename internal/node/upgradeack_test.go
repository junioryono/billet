package node

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
)

// ackPipe gives a test the two ends the updater and the node hold.
func ackPipe(t *testing.T) (reader, writer *os.File) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})

	return r, w
}

// AN UPDATER THAT TOOK THE JOB IS BELIEVED, and nothing after that is waited for.
func TestAnAcceptedUpgradeReturnsAsSoonAsItIsAccepted(t *testing.T) {
	t.Parallel()

	r, w := ackPipe(t)

	if _, err := w.WriteString(AckAccepted + "\n"); err != nil {
		t.Fatalf("write the acknowledgement: %v", err)
	}

	_ = w.Close()

	if err := awaitAck(r, "v0.4.0"); err != nil {
		t.Errorf("an updater that accepted the job was reported as refusing: %v", err)
	}
}

// A REFUSAL REACHES THE CONTROL PLANE, CARRYING ITS REASON.
//
// This is the whole point of the channel. The updater is detached, so every
// refusal it makes before touching anything — a digest that disagrees with the
// fleet's decision, a candidate this build cannot run, a claim another upgrade
// holds — used to be invisible: the control plane recorded the host as draining
// and waited forever, because the host keeps running, stays live, reports the
// same release, and nothing ever contradicts it.
func TestARefusedUpgradeIsReportedWithItsReason(t *testing.T) {
	t.Parallel()

	r, w := ackPipe(t)

	if _, err := w.WriteString(AckRefused + "the release moved underneath the decision\n"); err != nil {
		t.Fatalf("write the refusal: %v", err)
	}

	_ = w.Close()

	err := awaitAck(r, "v0.4.0")
	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("a refused upgrade returned %v, want ErrUpgradeRefused", err)
	}

	if !strings.Contains(err.Error(), "moved underneath the decision") {
		t.Errorf("the refusal did not carry the updater's reason: %v", err)
	}

	// AND NOT THE PROTOCOL WORD, which is billet talking to itself rather than to
	// the operator reading a node's log.
	if strings.Contains(err.Error(), AckRefused) {
		t.Errorf("the refusal leaked its wire prefix: %v", err)
	}
}

// AN UPDATER THAT DIED WITHOUT SAYING ANYTHING IS A REFUSAL TOO.
//
// EOF with nothing written is exactly the case a silent spawn hid: the process
// started, so the old code called it a success, and then it fell over before it
// had done anything at all.
func TestAnUpdaterThatSaysNothingIsNotTakenAsSuccess(t *testing.T) {
	t.Parallel()

	r, w := ackPipe(t)

	_ = w.Close()

	err := awaitAck(r, "v0.4.0")
	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("an updater that said nothing returned %v, want ErrUpgradeRefused", err)
	}

	if !strings.Contains(err.Error(), "without saying why") {
		t.Errorf("the failure does not say what happened: %v", err)
	}
}

// AND WHAT AN UPDATER WRITES IS BOUNDED. The descriptor is inherited by a program
// this node execs rather than by anything a job can reach, but a reader with no
// bound is a reader that can be made to hold the node's whole command slot.
func TestTheAnswerChannelIsBounded(t *testing.T) {
	t.Parallel()

	r, w := ackPipe(t)

	// JOINED, so no goroutine and no descriptor operation outlives the test. The
	// write is larger than a pipe's buffer, so it necessarily blocks until the
	// reader drains it — a test that walked away from that would be leaving a
	// blocked write behind for the next one to trip over.
	written := make(chan struct{})

	go func() {
		defer close(written)

		// THE ERROR IS EXPECTED HERE. This write is larger than a pipe's buffer and
		// the reader stops at the limit, so the tail of it fails or short-writes by
		// design — which is the condition under test.
		_, _ = w.WriteString(AckRefused + strings.Repeat("x", 4*MaxAckBytes) + "\n") //nolint:errcheck // the reader stops early by design; a short write is the case under test
		_ = w.Close()
	}()

	err := awaitAck(r, "v0.4.0")

	<-written
	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("an oversized answer returned %v, want ErrUpgradeRefused", err)
	}

	if len(err.Error()) > 2*MaxAckBytes {
		t.Errorf("the reader took %d bytes from a channel bounded at %d",
			len(err.Error()), MaxAckBytes)
	}
}

// fakeUpdater writes a script that answers on fd 3 the way a real updater would.
//
// A SCRIPT RATHER THAN A MOCK, because what is under test is the WIRING: whether
// ExecUpgrader passes the descriptor, waits for the answer, and honours it. A
// fake Upgrader would prove none of that — the defect this covers was three
// fields built and never passed, in exactly the layer a mock replaces.
func fakeUpdater(t *testing.T, answer string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "updater.sh")

	body := "#!/bin/sh\n"
	if answer != "" {
		body += "printf '%s\\n' " + strconv.Quote(answer) + " >&3\n"
	}

	body += "exit 0\n"

	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write the fake updater: %v", err)
	}

	return path
}

// THE DISPATCH WAITS FOR THE UPDATER'S ANSWER AND HONOURS IT.
//
// This is the wiring the acknowledgement exists for: a refusal must come back out
// of StartUpgrade so the control plane hears it. Returning as soon as the process
// started — which is what the code did — left the rollout believing a host was
// draining when the updater had already declined and exited.
func TestTheDispatchReportsAnUpdaterThatRefused(t *testing.T) {
	e := ExecUpgrader{Binary: fakeUpdater(t, AckRefused+"the release moved underneath")}

	err := e.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.4.0"})
	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("a refused dispatch returned %v, want ErrUpgradeRefused", err)
	}

	if !strings.Contains(err.Error(), "moved underneath") {
		t.Errorf("the reason did not survive the dispatch: %v", err)
	}
}

// AND AN UPDATER THAT ACCEPTED IS A SUCCESSFUL DISPATCH.
func TestTheDispatchSucceedsWhenTheUpdaterAccepts(t *testing.T) {
	e := ExecUpgrader{Binary: fakeUpdater(t, AckAccepted)}

	if err := e.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.4.0"}); err != nil {
		t.Errorf("a dispatch the updater accepted was reported as failing: %v", err)
	}
}

// AN UPDATER THAT EXITS WITHOUT ANSWERING IS NOT A SUCCESSFUL DISPATCH.
//
// The process started, which is all the old code checked, and then did nothing at
// all. The control plane would have waited for a convergence that could not come.
func TestTheDispatchReportsAnUpdaterThatSaidNothing(t *testing.T) {
	e := ExecUpgrader{Binary: fakeUpdater(t, "")}

	err := e.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.4.0"})
	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("a silent updater returned %v, want ErrUpgradeRefused", err)
	}
}

// THE WAIT IS BOUNDED, AND THE BOUND ACTUALLY APPLIES.
//
// An updater that has started, holds the answer channel open and says nothing is
// the shape of every hung preflight — a mirror that never responds, a
// verification that never returns. Without a deadline that really fires, the node
// blocks forever holding its single command slot, and every other command to that
// host expires in the queue behind it.
//
// A DEADLINE ON A PIPE IS NOT OBVIOUSLY SUPPORTED, which is the other reason this
// is asserted rather than assumed: it works because os.Pipe registers its ends
// with the runtime poller, and a future change to how the channel is opened could
// quietly take that away.
func TestTheWaitForAnAnswerIsBounded(t *testing.T) {
	r, w := ackPipe(t)

	original := ackWait
	ackWait = 50 * time.Millisecond

	t.Cleanup(func() { ackWait = original })

	// The writer is deliberately left open: nothing will ever close it, so EOF
	// cannot end this wait and only the deadline can.
	_ = w

	start := time.Now()

	err := awaitAck(r, "v0.4.0")
	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("a silent updater returned %v, want ErrUpgradeRefused", err)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the wait took %s against a bound of %s, so the deadline did not apply",
			elapsed, ackWait)
	}

	// AND IT SAYS WHICH SITUATION THIS IS. An operator reading a node's log needs
	// to know the updater never answered, not that it refused — those lead to
	// different machines being looked at.
	if !strings.Contains(err.Error(), "did not say within") {
		t.Errorf("a timed-out wait does not say so: %v", err)
	}
}

// THE ANSWER IS ONE LINE, AND THE NODE STOPS READING AT IT.
//
// Reading to EOF makes the node wait for the updater to CLOSE the descriptor
// rather than to ANSWER on it. An updater that accepted and then took its time
// closing — or that inherited the descriptor into something it exec'd — would be
// reported as refusing, ninety seconds after it had already said yes, while it
// went on to install. The deadline stops that being forever; it does not stop it
// being wrong.
func TestTheAnswerIsReadAsOneLineRatherThanUntilTheChannelCloses(t *testing.T) {
	r, w := ackPipe(t)

	original := ackWait
	ackWait = 30 * time.Second

	t.Cleanup(func() { ackWait = original })

	// Written and NOT closed, which is what an updater that answers and carries on
	// looks like from here.
	if _, err := w.WriteString(AckAccepted + "\n"); err != nil {
		t.Fatalf("write the acknowledgement: %v", err)
	}

	start := time.Now()

	if err := awaitAck(r, "v0.4.0"); err != nil {
		t.Fatalf("an updater that answered and held the channel open was reported as "+
			"refusing: %v", err)
	}

	// AND IT RETURNED ON THE ANSWER, not on the deadline.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the read took %s, so it waited for the channel to close rather than "+
			"for the answer", elapsed)
	}
}

// AN ANSWER THAT NEVER ENDED IS NOT AN ACCEPTANCE, WHATEVER IT SAYS SO FAR.
//
// ReadString returns what it has TOGETHER WITH the error, so bytes that happen to
// spell an acceptance arrive identically whether the updater finished writing or
// died halfway through the word. Taking them at face value records the host as
// draining on the strength of a sentence nobody finished, and the rollout then
// waits forever for a machine that never took the job.
func TestAnUnterminatedAcceptanceIsNotAccepted(t *testing.T) {
	t.Parallel()

	r, w := ackPipe(t)

	// The token, with no newline, and then the updater dies.
	if _, err := w.WriteString(AckAccepted); err != nil {
		t.Fatalf("write a partial acknowledgement: %v", err)
	}

	_ = w.Close()

	err := awaitAck(r, "v0.4.0")
	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("an unterminated acceptance returned %v, want ErrUpgradeRefused", err)
	}

	if !strings.Contains(err.Error(), "partway") {
		t.Errorf("the failure does not say the answer was incomplete: %v", err)
	}
}

// AND ONE CUT OFF BY THE DEADLINE IS NOT EITHER. Same bytes, different reason,
// and the message has to distinguish them: one machine died, the other is
// possibly still working.
func TestAnAcceptanceCutOffByTheDeadlineIsNotAccepted(t *testing.T) {
	r, w := ackPipe(t)

	original := ackWait
	ackWait = 50 * time.Millisecond

	t.Cleanup(func() { ackWait = original })

	// Written without a newline and the channel deliberately left open.
	if _, err := w.WriteString(AckAccepted); err != nil {
		t.Fatalf("write a partial acknowledgement: %v", err)
	}

	err := awaitAck(r, "v0.4.0")
	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("an acceptance cut off by the deadline returned %v, want "+
			"ErrUpgradeRefused", err)
	}

	if !strings.Contains(err.Error(), "did not say within") {
		t.Errorf("a timed-out partial answer is not reported as a timeout: %v", err)
	}
}

// A MAXIMAL ANSWER STILL ARRIVES TERMINATED.
//
// The writer bounds its payload at MaxAckBytes and then appends a newline, so a
// reader capped at MaxAckBytes would cut the terminator off the largest legal
// answer — and by the rule above, refuse it. The two limits have to agree about
// who owns the newline.
func TestAMaximalAnswerIsStillTerminated(t *testing.T) {
	r, w := ackPipe(t)

	payload := AckRefused + strings.Repeat("x", MaxAckBytes-len(AckRefused))

	if len(payload) != MaxAckBytes {
		t.Fatalf("the test built a %d byte payload, want %d", len(payload), MaxAckBytes)
	}

	written := make(chan struct{})

	go func() {
		defer close(written)

		if _, err := w.WriteString(payload + "\n"); err != nil {
			t.Errorf("write a maximal answer: %v", err)
		}

		_ = w.Close()
	}()

	err := awaitAck(r, "v0.4.0")

	<-written

	if !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("a maximal answer returned %v, want ErrUpgradeRefused", err)
	}

	// AND IT IS THE REFUSAL IT SENT, not "it stopped partway through its answer".
	if strings.Contains(err.Error(), "partway") {
		t.Errorf("a maximal but complete answer was read as truncated: %v", err)
	}
}
