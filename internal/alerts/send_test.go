package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	case <-time.After(30 * time.Second):
		t.Fatal("a destination that hangs held its request past every bound")
	}

	if took := time.Since(begun); took < hung.timeout {
		t.Errorf("the try ended after %s, before its bound of %s", took, hung.timeout)
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
	case <-time.After(30 * time.Second):
		t.Fatal("a stop did not end the try")
	}
}

// A TRANSPORT FAILURE NAMES ITS CLASS AND NOTHING OF THE ADDRESS, which is exactly
// what the client's own error quotes: it is the one a log line would otherwise carry.
func TestATransportFailureCarriesNoAddress(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := URL(srv.URL + secretPath)
	srv.Close()

	err := NewSender("gone", address).Send(t.Context(), []byte(`{}`))
	if err == nil {
		t.Fatal("a refused connection read as delivered")
	}
	assertNoSecret(t, "the error", err.Error(), address)
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the error names the host: %v", err)
	}
	if errors.Is(err, ErrPermanent) {
		t.Errorf("a refused connection is worth trying again: %v", err)
	}

	tlsAddress := URL("https://" + strings.TrimPrefix(srv.URL, "http://") + secretPath)
	if err := NewSender("gone", tlsAddress).Send(t.Context(), []byte(`{}`)); err == nil {
		t.Error("a refused https connection read as delivered")
	} else {
		assertNoSecret(t, "the error", err.Error(), tlsAddress)
	}
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
			"%v":        fmt.Sprintf("%v", v),
			"%+v":       fmt.Sprintf("%+v", v),
			"%#v":       fmt.Sprintf("%#v", v),
			"%s":        fmt.Sprintf("%s", v),
			"%q":        fmt.Sprintf("%q", v),
			"%x":        fmt.Sprintf("%x", v),
			"json":      string(marshalled),
			"slog json": logged.String(),
			"slog text": text.String(),
		}
	}

	hexed := fmt.Sprintf("%x", string(address))

	for name, v := range map[string]any{
		"a URL": address, "a sender": sender, "a struct holding both": holder{Address: address, Sender: sender},
		"a pointer to one": &holder{Address: address, Sender: sender},
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
