package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The address a test's destination is told to be secret: its path is the credential.
const secretPath = "/services/T000/B000/s3cr3t-webhook-token"

// assertNoSecret fails if text carries the address or the part of it that matters.
func assertNoSecret(t *testing.T, what, text string, address URL) {
	t.Helper()

	for _, leak := range []string{string(address), secretPath, "s3cr3t-webhook-token"} {
		if strings.Contains(text, leak) {
			t.Errorf("%s carries the destination's address: %q", what, text)
		}
	}
}

func destination(t *testing.T, handler http.HandlerFunc) (*Sender, URL) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	address := URL(srv.URL + secretPath)

	return NewSender("oncall", address), address
}

// A 2XX IS DELIVERED, as the POST the destination was told to expect.
func TestA2xxIsDelivered(t *testing.T) {
	t.Parallel()

	var got struct {
		method, path, contentType string
		body                      string
	}

	s, _ := destination(t, func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(r.Body); err != nil {
			t.Errorf("read the body: %v", err)
		}
		got.method, got.path, got.contentType, got.body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), buf.String()
		w.WriteHeader(http.StatusNoContent)
	})

	if err := s.Send(t.Context(), []byte(`{"text":"hello"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.method != http.MethodPost || got.path != secretPath || got.contentType != "application/json" ||
		got.body != `{"text":"hello"}` {
		t.Errorf("the destination saw %+v", got)
	}
}

// WHAT A DESTINATION ANSWERS DECIDES WHETHER TO TRY AGAIN, and nothing it writes
// reaches an error: Slack's error bodies are short codes, a webhook's are anything.
func TestAnAnswerDecidesARetryAndNeverReachesAnError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		status    int
		permanent bool
	}{
		{http.StatusBadRequest, true},       // Slack's invalid_payload
		{http.StatusForbidden, true},        // Slack's invalid_token
		{http.StatusNotFound, true},         // Slack's no_service
		{http.StatusGone, true},             // Slack's channel_is_archived
		{http.StatusFound, true},            // a redirect, refused rather than followed
		{http.StatusTooManyRequests, false}, // come back later
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			t.Parallel()

			s, address := destination(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://elsewhere.example/notices")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "the destination's own words")
			})

			err := s.Send(t.Context(), []byte(`{}`))
			if err == nil {
				t.Fatal("an unsuccessful answer read as delivered")
			}
			if got := errors.Is(err, ErrPermanent); got != tc.permanent {
				t.Errorf("permanent = %v, want %v: %v", got, tc.permanent, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(tc.status)) {
				t.Errorf("the status is not in the error: %v", err)
			}
			if strings.Contains(err.Error(), "own words") {
				t.Errorf("the destination's words reached the error: %v", err)
			}
			assertNoSecret(t, "the error", err.Error(), address)
		})
	}
}

// A REDIRECT IS NOT FOLLOWED. A notice is not for wherever a destination sends it.
func TestARedirectIsNotFollowed(t *testing.T) {
	t.Parallel()

	var other atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { other.Add(1) }))
	t.Cleanup(elsewhere.Close)

	s, _ := destination(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	})

	if err := s.Send(t.Context(), []byte(`{}`)); !errors.Is(err, ErrPermanent) {
		t.Errorf("a redirect = %v, want a refusal", err)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("the redirect's target received %d requests", got)
	}
}

// A DESTINATION THAT HANGS COSTS ONE BOUNDED TRY, ends with no address in its error,
// and holds up nothing else: another destination is delivered while it hangs.
func TestADestinationThatHangsIsBoundedAndHoldsUpNothingElse(t *testing.T) {
	t.Parallel()

	// RELEASED BEFORE THE SERVER CLOSES, which waits for its handlers: cleanups run
	// last first, so this one is registered after the server's.
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	hung, address := destination(t, func(_ http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	hung.timeout = 100 * time.Millisecond

	fine, _ := destination(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	result := make(chan error, 1)
	begun := time.Now()
	go func() { result <- hung.Send(t.Context(), []byte(`{}`)) }()

	<-entered

	// THE OTHER ONE, WHILE THIS ONE IS DEMONSTRABLY INSIDE ITS REQUEST.
	if err := fine.Send(t.Context(), []byte(`{}`)); err != nil {
		t.Errorf("another destination, while one hung: %v", err)
	}

	select {
	case err := <-result:
		if err == nil || errors.Is(err, ErrPermanent) {
			t.Errorf("a hung try = %v, want one worth trying again", err)
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Errorf("a hung try = %v, want it to say so", err)
		}
		assertNoSecret(t, "the error", err.Error(), address)
	case <-time.After(5 * time.Second):
		t.Fatal("a destination that hangs held its request past its bound")
	}

	// ITS OWN BOUND ENDED IT, not the transport's longer ones behind it.
	if took := time.Since(begun); took < hung.timeout || took > 3*time.Second {
		t.Errorf("the try ended after %s, want its bound of %s", took, hung.timeout)
	}
}

// THE TRY'S OWN BOUND IS THE CALLER'S TOO: a control plane stopping does not wait
// out a destination.
func TestAStopEndsATry(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	hung, _ := destination(t, func(_ http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	result := make(chan error, 1)
	go func() { result <- hung.Send(ctx, []byte(`{}`)) }()

	<-entered
	cancel()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Errorf("a stopped try = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stop did not end the try")
	}
}

// A TRANSPORT FAILURE NAMES ITS CLASS AND NOTHING OF THE ADDRESS, which is exactly
// what the client's own error quotes: it is the one a log line would otherwise carry.
func TestATransportFailureCarriesNoAddress(t *testing.T) {
	t.Parallel()

	// A CONNECTION THAT CLOSES WITHOUT AN ANSWER, held open for the test so no other
	// test's server can take its port.
	hangUp, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hangUp.Close() })
	go func() {
		for {
			conn, err := hangUp.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(plain.Close)
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(untrusted.Close)

	for name, address := range map[string]URL{
		"a connection closed unanswered": URL("http://" + hangUp.Addr().String() + secretPath),
		"https to a plain server":        URL("https://" + strings.TrimPrefix(plain.URL, "http://") + secretPath),
		"a certificate nobody trusts":    URL(untrusted.URL + secretPath),
		"a name that does not resolve":   URL("https://hooks.invalid" + secretPath),
	} {
		err := NewSender("gone", address).Send(t.Context(), []byte(`{}`))
		if err == nil {
			t.Errorf("%s read as delivered", name)

			continue
		}
		assertNoSecret(t, name, err.Error(), address)
		for _, host := range []string{"127.0.0.1", "hooks.invalid"} {
			if strings.Contains(err.Error(), host) {
				t.Errorf("%s: the error names the host: %v", name, err)
			}
		}
		if errors.Is(err, ErrPermanent) {
			t.Errorf("%s is worth trying again: %v", name, err)
		}
	}
}

// AN IDLE CONNECTION IS NEVER KEPT, so nothing a destination writes after its answer
// is read: the transport reports such bytes through the standard logger, quoting
// them, and a destination could write the address back. Not parallel: it watches the
// standard logger.
func TestWhatADestinationWritesAfterItsAnswerReachesNoLog(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	address := URL("http://" + listener.Addr().String() + secretPath)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 4096)
				if _, err := conn.Read(buf); err != nil {
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"); err != nil {
					return
				}
				time.Sleep(200 * time.Millisecond)
				// UNASKED, on what a kept connection would be idle: the address, back.
				if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"+secretPath); err != nil {
					return
				}
				time.Sleep(500 * time.Millisecond)
			}()
		}
	}()

	var logged bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(saved) })

	sender := NewSender("oncall", address)
	if err := sender.Send(t.Context(), []byte(`{}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	time.Sleep(time.Second)

	assertNoSecret(t, "the standard logger", logged.String(), address)
}

// A GATED CONNECTION READS NOTHING UNTIL IT HAS WRITTEN, and a close ends a read that
// waits; the sender dials through one. What the transport would log is decided by a
// race a test cannot hold, so the gate is proved here and its use below.
func TestAConnectionReadsNothingBeforeItsRequest(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	gated := quietUntilWritten(client)

	go func() {
		if _, err := server.Write([]byte("unasked")); err != nil {
			return
		}
	}()

	read := make(chan string, 1)
	go func() {
		buf := make([]byte, 16)
		n, err := gated.Read(buf)
		if err != nil {
			read <- "error: " + err.Error()

			return
		}
		read <- string(buf[:n])
	}()

	select {
	case got := <-read:
		t.Fatalf("read %q before anything was written", got)
	case <-time.After(200 * time.Millisecond):
	}

	go func() {
		if _, err := server.Read(make([]byte, 16)); err != nil {
			return
		}
	}()
	if _, err := gated.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-read:
		if got != "unasked" {
			t.Errorf("read %q after the write", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not follow the write")
	}

	waiting, _ := net.Pipe()
	closing := quietUntilWritten(waiting)
	ended := make(chan error, 1)
	go func() {
		_, err := closing.Read(make([]byte, 1))
		ended <- err
	}()
	if err := closing.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("a waiting read after a close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a close did not end a waiting read")
	}

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		if conn, err := listener.Accept(); err == nil {
			_ = conn.Close()
		}
	}()

	transport, ok := NewSender("oncall", URL("https://hooks.example"+secretPath)).client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the sender's transport is not an *http.Transport")
	}
	conn, err := transport.DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, gatedDial := conn.(*gatedConn); !gatedDial {
		t.Errorf("the sender dials a %T, not a gated connection", conn)
	}
}

// NOTHING A DESTINATION WRITES BEFORE ITS REQUEST IS READ, so none of it reaches the
// standard logger the transport reports such bytes through. Not parallel: it watches
// the standard logger.
func TestWhatADestinationWritesFirstReachesNoLog(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	address := URL("http://" + listener.Addr().String() + secretPath)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				// FIRST, UNASKED: the address, back, before any request.
				if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"+secretPath); err != nil {
					return
				}
				time.Sleep(500 * time.Millisecond)
			}()
		}
	}()

	var logged bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(saved) })

	// WHAT THE SEND COMES TO IS NOT THE QUESTION; what reaches the log is.
	if err := NewSender("oncall", address).Send(t.Context(), []byte(`{}`)); err != nil {
		assertNoSecret(t, "the error", err.Error(), address)
	}
	time.Sleep(time.Second)

	assertNoSecret(t, "the standard logger", logged.String(), address)
}

// A DESTINATION WHOSE ADDRESS COULD NOT BE HAD SAYS SO ON EVERY NOTICE, and is never
// tried again: a missing secret does not stop a control plane, and is not silent.
func TestAnUnusableDestinationSaysWhyItTookNothing(t *testing.T) {
	t.Parallel()

	err := UnusableSender("oncall", "BILLET_ALERT_ONCALL is empty").Send(t.Context(), []byte(`{}`))
	if !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "BILLET_ALERT_ONCALL is empty") {
		t.Errorf("Send = %v", err)
	}
}

// A SENDER RENDERS AS ITS NAME ON EVERY PATH, and a URL as a sentence, because fmt,
// encoding/json and slog each ignore the others' methods and reflection reaches an
// unexported field without calling them. Each row is one path; neutering its method
// fails that row alone.
func TestNeitherAURLNorASenderRendersTheAddress(t *testing.T) {
	t.Parallel()

	address := URL("https://hooks.example" + secretPath)
	sender := NewSender("oncall", address)

	type holder struct {
		Address URL
		Sender  *Sender
	}

	render := func(v any) map[string]string {
		var logged bytes.Buffer
		slog.New(slog.NewJSONHandler(&logged, nil)).Info("m", "v", v)
		var text bytes.Buffer
		slog.New(slog.NewTextHandler(&text, nil)).Info("m", "v", v)

		marshalled, err := json.Marshal(v)
		if err != nil {
			t.Errorf("marshal: %v", err)
		}

		return map[string]string{
			"%v":  fmt.Sprintf("%v", v),
			"%+v": fmt.Sprintf("%+v", v),
			"%#v": fmt.Sprintf("%#v", v),
			"%s":  fmt.Sprintf("%s", v),
			"%q":  fmt.Sprintf("%q", v),
			"%x":  fmt.Sprintf("%x", v),
			// A VERB NO OTHER METHOD ANSWERS, which only Format covers: without it fmt
			// prints the value raw beside the complaint.
			"%d":        fmt.Sprintf("%d", v),
			"%t":        fmt.Sprintf("%t", v),
			"json":      string(marshalled),
			"slog json": logged.String(),
			"slog text": text.String(),
		}
	}

	hexed := fmt.Sprintf("%x", string(address))

	type byValue struct {
		Sender Sender
	}

	for name, v := range map[string]any{
		"a URL": address, "a sender": sender, "a sender's value": *sender,
		"a struct holding both":           holder{Address: address, Sender: sender},
		"a pointer to one":                &holder{Address: address, Sender: sender},
		"a struct holding a sender value": byValue{Sender: *sender},
	} {
		for path, text := range render(v) {
			assertNoSecret(t, name+" through "+path, text, address)
			if strings.Contains(text, hexed) {
				t.Errorf("%s through %s carries the address in hex", name, path)
			}
		}
	}

	if got := sender.Name(); got != "oncall" {
		t.Errorf("Name = %q", got)
	}

	// EACH METHOD ON ITS OWN, since fmt answers most verbs from one of the others.
	senderValue := *sender
	for name, text := range map[string]string{
		"URL.GoString": address.GoString(), "URL.LogValue": address.LogValue().String(),
		"Sender.GoString": sender.GoString(), "Sender.LogValue": sender.LogValue().String(),
		"Sender value's String": senderValue.String(),
	} {
		assertNoSecret(t, name, text, address)
	}
}

// THE ADDRESS IS CHECKED WHERE IT IS READ, each refusal quoting nothing of it: what
// fails here may be a credential with a typo, and url.Parse's own error quotes it.
func TestParseURLRefusesWhatWouldSendTheSecretInTheClear(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"https://hooks.example/services/x": "",
		"  https://hooks.example/x\n":      "",
		"http://127.0.0.1:9/x":             "",
		"http://[::1]:9/x":                 "",
		"":                                 "empty",
		"   ":                              "empty",
		"http://hooks.example/x":           "in the clear",
		"http://localhost/x":               "in the clear",
		"ftp://hooks.example/x":            "neither https",
		"https://user:pw@hooks.example/x":  "user name or password",
		"https://hooks.example/x#frag":     "fragment",
		"not a url":                        "names no host",
		"https://":                         "names no host",
		"https://hooks.example/%zz-secret": "not a URL",
	} {
		got, err := ParseURL(raw)

		if want == "" {
			if err != nil {
				t.Errorf("ParseURL(%q) = %v", raw, err)
			}
			if string(got) != strings.TrimSpace(raw) {
				t.Errorf("ParseURL(%q) kept %q", raw, string(got))
			}

			continue
		}

		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseURL(%q) = %v, want a refusal saying %q", raw, err, want)

			continue
		}
		for _, leak := range []string{"secret", "hooks.example", "user:pw"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("ParseURL(%q) quoted the value: %v", raw, err)
			}
		}
	}
}
