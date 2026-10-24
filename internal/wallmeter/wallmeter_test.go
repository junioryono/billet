package wallmeter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// fakePlug answers each Poll with the next of watts, an error for a negative.
type fakePlug struct {
	watts []float64
	at    int
}

var errUnplugged = errors.New("unplugged")

func (p *fakePlug) read(context.Context) (float64, error) {
	w := p.watts[p.at]
	p.at++
	if w < 0 {
		return 0, errUnplugged
	}

	return w, nil
}

// meterAt is a meter reading p on a clock the test sets, each read taking no
// time.
func meterAt(p *fakePlug, now *time.Time) *Meter {
	m := New(p.read, time.Second, nil)
	m.now = func() time.Time { return *now }

	return m
}

// ENERGY IS THE TRAPEZOID BETWEEN READINGS, one missed reading is bridged, and
// a longer gap begins a new epoch with nothing added for it.
func TestAMeterIntegratesItsReadingsAndBreaksOnAGap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	// A negative reading is a failed one.
	p := &fakePlug{watts: []float64{10, 20, -1, 30, -1, -1, -1, 40, 50}}
	m := meterAt(p, &now)

	if _, _, err := m.Energy(); !errors.Is(err, ErrNoReading) {
		t.Fatalf("Energy before any reading = %v, want ErrNoReading", err)
	}

	poll := func() { m.Poll(t.Context()); now = now.Add(time.Second) }

	poll() // 10 W, the first reading: a new epoch, nothing to integrate.
	_, first, err := m.Energy()
	if err != nil {
		t.Fatal(err)
	}
	poll() // 20 W a second later: 15 J.
	poll() // failed.
	poll() // 30 W two seconds after 20 W, bridged: 50 J.
	uj, epoch, err := m.Energy()
	if err != nil || uj != 65_000_000 || epoch != first {
		t.Fatalf("after a bridged gap: %d µJ epoch %d (%v), want 65000000 in epoch %d", uj, epoch, err, first)
	}

	poll()
	poll()
	poll() // three failed readings: the last reading is now four seconds old.
	if _, _, err := m.Energy(); !errors.Is(err, ErrNoReading) {
		t.Fatalf("Energy with its last reading four intervals old = %v, want ErrNoReading", err)
	}
	poll() // 40 W four seconds after 30 W: not bridged, a new epoch.
	uj, epoch, err = m.Energy()
	if err != nil || uj != 65_000_000 || epoch == first {
		t.Fatalf("after a long gap: %d µJ epoch %d (%v), want 65000000 in a new epoch", uj, epoch, err)
	}
	poll() // 50 W: 45 J more in the new epoch.
	if uj, again, err := m.Energy(); err != nil || uj != 110_000_000 || again != epoch {
		t.Fatalf("after the gap: %d µJ epoch %d (%v), want 110000000 in epoch %d", uj, again, err, epoch)
	}
}
