package guestagent

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// stuckWriter is a log nobody drains: every write waits until release is closed.
type stuckWriter struct{ release chan struct{} }

func (s stuckWriter) Write(p []byte) (int, error) {
	<-s.release

	return len(p), nil
}

// A log nobody drains holds the agent's loop in a write; SIGTERM is still
// answered within 3 s, because no lock is held across a write and the
// backstop's own line is written from a goroutine of its own.
func TestALogNobodyDrainsDoesNotHoldSIGTERM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		// A hello nobody can tell is logged, and that write never returns.
		node.helloScript = []reply{{status: http.StatusServiceUnavailable}}

		release := make(chan struct{})

		var unused syncBuffer

		cfg := testConfig(t, node, &unused)
		cfg.Log = stuckWriter{release: release}

		r := start(t, cfg)

		time.Sleep(5 * time.Second)
		synctest.Wait()

		took, err := r.stop()
		if err != nil || took > 3*time.Second {
			t.Fatalf("SIGTERM with the log stuck returned %v after %v, want nil within 3s", err, took)
		}

		close(release)
	})
}

// A hello's stop is its status: a body cut short does not turn a 401 or a 404
// into an answer nobody can tell.
func TestAHellosStopDoesNotHangOnItsBody(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		n, err := newNode("http://172.31.0.1:7718", testToken, statusTransport{status: status})
		if err != nil {
			t.Fatal(err)
		}

		if _, v := n.hello(t.Context()); v != stop {
			t.Errorf("a %d hello with a broken body is %v, want a stop", status, v)
		}
	}
}

// utime and stime are each in range, and so must their sum be: a sum past what a
// report carries would make every batch after it one the encoder refuses.
func TestAProcessCPUPastWhatAReportCarriesIsCouldNotTell(t *testing.T) {
	t.Parallel()

	line := func(utime, stime int64) []byte {
		return []byte("1033 (billet) S 1 2 2 0 -1 4194560 10 0 0 0 " + strconv.FormatInt(utime, 10) + " " +
			strconv.FormatInt(stime, 10) + " 0 0 20 0 1 0 4100 1000 10 0")
	}

	if _, err := parsePIDStat(line(guestreport.MaxValue, 1)); !errors.Is(err, errCouldNotTell) {
		t.Fatalf("a CPU of MaxValue+1 parsed (%v), want could-not-tell", err)
	}

	st, err := parsePIDStat(line(guestreport.MaxValue-1, 1))
	if err != nil || st.cpu != guestreport.MaxValue {
		t.Fatalf("a CPU of exactly MaxValue parsed as %d (%v)", st.cpu, err)
	}
}
