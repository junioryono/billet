package wallmeter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// shellyAnswer is Switch.GetStatus's answer from the Shelly Plug US Gen4 beside
// macos-01 on 2026-10-24, with apower left for the test to set.
const shellyAnswer = `{"id":%d,"source":"init","output":true,"apower":%s,"voltage":117.5,` +
	`"freq":60.0,"current":0.032,"aenergy":{"total":614.519,"by_minute":[0.000,0.000,119.094],` +
	`"minute_ts":1792824600},"temperature":{"tC":47.1,"tF":116.7}}`

// plug serves answer for every Switch.GetStatus of switch id 0, and records the
// query it was asked.
func plug(t *testing.T, status int, answer string) (Shelly, *string) {
	t.Helper()

	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, answer)
	}))
	t.Cleanup(srv.Close)

	return Shelly{URL: srv.URL}, &asked
}

// THE PLUG'S POWER IS ITS apower, asked of the switch billet names.
func TestAShellysPowerIsItsAPower(t *testing.T) {
	t.Parallel()

	s, asked := plug(t, http.StatusOK, fmt.Sprintf(shellyAnswer, 0, "1.8"))
	w, err := s.Watts(t.Context())
	if err != nil || w != 1.8 {
		t.Fatalf("Watts = %v, %v; want 1.8", w, err)
	}
	if *asked != "/rpc/Switch.GetStatus?id=0" {
		t.Errorf("asked %q, want the switch's status", *asked)
	}
}

// AN ANSWER THAT IS NOT THIS SWITCH'S POWER IS REFUSED, by what is wrong with it.
func TestAnAnswerThatIsNotThisSwitchsPowerIsRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		status int
		answer string
		says   string
	}{
		"another switch":    {http.StatusOK, fmt.Sprintf(shellyAnswer, 1, "1.8"), "a switch other than 0"},
		"no apower":         {http.StatusOK, `{"id":0,"output":true}`, "names no apower"},
		"a negative power":  {http.StatusOK, fmt.Sprintf(shellyAnswer, 0, "-3"), "outside 0 to"},
		"an absurd power":   {http.StatusOK, fmt.Sprintf(shellyAnswer, 0, "250000"), "outside 0 to"},
		"not JSON":          {http.StatusOK, "<html>", "is not a switch status"},
		"an error status":   {http.StatusUnauthorized, `{"code":401}`, "answered 401"},
		"an oversized body": {http.StatusOK, strings.Repeat(" ", maxBody+1), "is over"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s, _ := plug(t, tc.status, tc.answer)
			if w, err := s.Watts(t.Context()); err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Watts = %v, %v; want an error saying %q", w, err, tc.says)
			}
		})
	}
}

// A REDIRECT IS A FAILED READING, never followed: the plug's answer cannot send
// the node's request elsewhere on its network, or pass off another plug's power
// as this host's.
func TestARedirectIsNotFollowed(t *testing.T) {
	t.Parallel()

	var followed atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		_, _ = fmt.Fprintf(w, shellyAnswer, 0, "1.8")
	}))
	t.Cleanup(elsewhere.Close)

	s, _ := plug(t, http.StatusOK, "")
	redirecting := httptest.NewServer(http.RedirectHandler(elsewhere.URL+"/rpc/Switch.GetStatus?id=0",
		http.StatusFound))
	t.Cleanup(redirecting.Close)
	s.URL = redirecting.URL

	if w, err := s.Watts(t.Context()); err == nil || !strings.Contains(err.Error(), "answered 302") {
		t.Fatalf("Watts through a redirect = %v, %v; want the redirect refused", w, err)
	}
	if followed.Load() {
		t.Error("the redirect was followed")
	}
}
