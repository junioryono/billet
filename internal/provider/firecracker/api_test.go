package firecracker

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A VMM'S OWN FAULT MESSAGE IS THE ACTIONABLE HALF. `Invalid request method and/or
// path` and `Open tap device failed` send an operator to completely different
// places, and a status code alone sends them nowhere.
func TestAVMMsRefusalCarriesItsExplanation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	h.onJailer = func(id string) {
		vmm := h.serveVMM(t, id)
		vmm.refuse["/network-interfaces/eth0"] = "Open tap device failed: Operation not permitted"
	}

	_, err := h.p.Launch(t.Context(), aSpec())
	if err == nil {
		t.Fatal("Launch reported success although the vmm refused a resource")
	}

	if !strings.Contains(err.Error(), "Open tap device failed") {
		t.Errorf("the vmm's explanation was dropped: %v", err)
	}

	// AND IT NAMES WHICH RESOURCE, because a launch places six of them and the
	// message otherwise says only that one was refused.
	if !strings.Contains(err.Error(), "/network-interfaces/eth0") {
		t.Errorf("the error does not say which resource was refused: %v", err)
	}
}

// ANOTHER PROGRAM'S OUTPUT IS BOUNDED BEFORE IT REACHES A TERMINAL. Parsing
// something does not make its contents safe to print, and a fault message is free
// text billet did not write.
func TestAVMMsAnswerIsBoundedBeforeItIsRendered(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	h.onJailer = func(id string) {
		vmm := h.serveVMM(t, id)
		vmm.refuse["/machine-config"] = strings.Repeat("verbose ", 5000)
	}

	_, err := h.p.Launch(t.Context(), aSpec())
	if err == nil {
		t.Fatal("Launch reported success")
	}

	if len(err.Error()) > 2000 {
		t.Errorf("the error is %d characters, so the vmm's answer was not bounded",
			len(err.Error()))
	}
}

// A CONTROL BYTE IN ANOTHER PROGRAM'S OUTPUT MUST NOT BECOME A LIVE TERMINAL
// CONTROL when billet prints it.
func TestARenderedAnswerIsQuoted(t *testing.T) {
	t.Parallel()

	rendered := bounded("first\r\nSECOND\x1b[2J")

	for _, raw := range []string{"\r", "\n", "\x1b"} {
		if strings.Contains(rendered, raw) {
			t.Errorf("bounded left a raw control byte in %q", rendered)
		}
	}
}

// AND TRUNCATION CANNOT CUT INSIDE AN ESCAPE. Slicing the quoted form leaves `\x0`,
// which is not a valid quoted value; assembling it escape by escape cannot.
func TestABoundedValueIsAlwaysValidlyQuoted(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		strings.Repeat("\x00", 500),
		strings.Repeat("é", 500),
		strings.Repeat("x", 500),
		"",
	} {
		got := bounded(input)

		if len(got) > maxDiagnostic {
			t.Errorf("bounded returned %d characters for a %d-byte input", len(got), len(input))
		}

		if !strings.HasPrefix(got, `"`) {
			t.Errorf("bounded did not quote %d bytes: %q", len(input), got)
		}

		// A TRAILING BACKSLASH IS THE TELL that a cut landed inside an escape.
		trimmed := strings.TrimSuffix(strings.TrimSuffix(got, `"`), "…")
		if strings.HasSuffix(trimmed, `\`) {
			t.Errorf("bounded cut inside an escape: %q", got)
		}
	}
}

// A REFUSED CONNECTION PROVES THE VMM IS GONE. Any other failure does not, and the
// difference is what stops billet force-killing a job it merely could not reach.
func TestOnlyAProvenAbsenceCountsAsGone(t *testing.T) {
	t.Parallel()

	dir := shortDir(t)
	socket := filepath.Join(dir, "s")

	api := newVMMAPI(socket)

	// Nothing has ever listened here.
	_, err := api.info(t.Context())
	if !gone(err) {
		t.Errorf("a socket that does not exist was not read as proof the vmm is gone: %v", err)
	}

	// A socket file with nothing behind it: the measured shape after a VMM stops.
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatalf("stage a stale socket file: %v", err)
	}

	_, err = api.info(t.Context())
	if err == nil {
		t.Fatal("a path that is not a socket answered")
	}

	// AND NOTHING ELSE COUNTS. An error billet made up carries no syscall, so it
	// must not be read as absence.
	if gone(errors.New("the database was busy")) {
		t.Error("an unrelated error was read as proof the vmm is gone")
	}

	if gone(nil) {
		t.Error("a nil error was read as proof the vmm is gone")
	}
}

// AN ANSWER THAT IS NOT THIS API'S IS REFUSED RATHER THAN DECODED INTO ZEROES. A
// zero-valued instance description has an empty state, which is not `Running` — so
// it would read as a guest that is not running, and the caller destroys those.
func TestAnAnswerThatIsNotThisAPIIsRefused(t *testing.T) {
	t.Parallel()

	dir := shortDir(t)
	socket := filepath.Join(dir, "s")

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{ReadHeaderTimeout: apiTimeout, Handler: http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			if _, err := w.Write([]byte("<html>not firecracker at all</html>")); err != nil {
				t.Errorf("write: %v", err)
			}
		})}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()

	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close the server: %v", err)
		}
	})

	if _, err := newVMMAPI(socket).info(t.Context()); err == nil {
		t.Error("a non-json answer was decoded rather than refused")
	}
}
