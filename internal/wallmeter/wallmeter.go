// Package wallmeter reads the power a host draws at the wall from the smart plug
// it is plugged into, and integrates it into the energy the node's sampler
// shares among jobs (node.monitoring.meter).
//
// A leaf: the standard library only.
package wallmeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// maxBody bounds a plug's answer; Switch.GetStatus answers in under 1 KB.
const maxBody = 64 << 10

// MaxWatts bounds a reading: far above what a plug can switch (a Shelly Plug US
// is rated 15 A at 120 V, 1.8 kW), to refuse a unit mistake rather than model a
// plug.
const MaxWatts = 10_000

// staleAfter is how many intervals may pass between two readings before the
// energy between them is not known: one missed reading is bridged, as the
// sampler bridges one missed tick, and a longer gap begins a new epoch.
const staleAfter = 3

// Shelly is a Shelly Gen2 or later plug (Plus, Pro, Gen3 and Gen4), read through
// its local RPC API, which needs no account and no cloud.
//
// INSTANTANEOUS POWER, NOT THE ENERGY COUNTER. Measured on a Shelly Plug US Gen4
// beside macos-01 on 2026-10-24: Switch.GetStatus's apower changed every second
// in 0.1 W steps, while aenergy.total moved about once a minute in steps of
// about 0.12 Wh (430 J), too coarse for a job that runs a few minutes.
type Shelly struct {
	// URL is the plug's base address, such as http://192.168.1.246.
	URL string
	// Switch is the plug's switch (output) id, 0 on a single-outlet plug.
	Switch int
	// Client is the HTTP client; nil uses one with no timeout of its own, so the
	// caller's context bounds every read.
	Client *http.Client
}

// shellyStatus is the part of Switch.GetStatus's answer billet reads.
type shellyStatus struct {
	ID     *int     `json:"id"`
	APower *float64 `json:"apower"`
}

// Watts reads the plug's instantaneous active power.
func (s Shelly) Watts(ctx context.Context) (float64, error) {
	u, err := url.Parse(s.URL)
	if err != nil {
		return 0, fmt.Errorf("wallmeter: the plug's address %q: %w", s.URL, err)
	}
	u = u.JoinPath("rpc", "Switch.GetStatus")
	u.RawQuery = url.Values{"id": {strconv.Itoa(s.Switch)}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("wallmeter: %w", err)
	}

	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("wallmeter: read the plug at %s: %w", s.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("wallmeter: the plug at %s answered %s", s.URL, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, fmt.Errorf("wallmeter: read the plug's answer: %w", err)
	}
	if len(body) > maxBody {
		return 0, fmt.Errorf("wallmeter: the plug's answer is over %d bytes", maxBody)
	}

	var st shellyStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return 0, fmt.Errorf("wallmeter: the plug's answer is not a switch status: %w", err)
	}

	switch {
	case st.ID == nil || *st.ID != s.Switch:
		return 0, fmt.Errorf("wallmeter: the plug answered for a switch other than %d", s.Switch)
	case st.APower == nil:
		return 0, errors.New("wallmeter: the plug's answer names no apower, so it measures no power")
	case math.IsNaN(*st.APower) || math.IsInf(*st.APower, 0) || *st.APower < 0 || *st.APower > MaxWatts:
		return 0, fmt.Errorf("wallmeter: the plug read %v W, outside 0 to %d", *st.APower, MaxWatts)
	}

	return *st.APower, nil
}

// ErrNoReading is Energy's answer before the meter has read the plug, and when
// its last reading is too old to say what the host drew since.
var ErrNoReading = errors.New("wallmeter: no recent reading of the plug")

// Meter reads a plug once an interval and integrates its power into energy.
//
// READ ON ITS OWN GOROUTINE, never the sampler's: a plug is a device on the
// network, and a read that hangs must not stall the one goroutine every job is
// sampled on. Energy answers at once from what has been read.
type Meter struct {
	read     func(context.Context) (float64, error)
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger

	mu sync.Mutex
	// total is the energy integrated so far, in µJ, and frac the part of a µJ
	// not yet added: an integer total stays exact for as long as a host runs,
	// where a float's would round once it passed 2^53 µJ (about a year at 300 W).
	total   int64
	frac    float64
	epoch   uint64
	last    reading
	failing bool
}

type reading struct {
	watts float64
	at    time.Time
	ok    bool
}

// New is a meter reading through read every interval. A nil log discards.
func New(read func(context.Context) (float64, error), interval time.Duration, log *slog.Logger) *Meter {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return &Meter{read: read, interval: interval, now: time.Now, log: log}
}

// Run reads the plug every interval until ctx ends.
func (m *Meter) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()

	for {
		m.Poll(ctx)

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll takes one reading, bounded to most of an interval so a slow plug cannot
// pile readings up, and adds the energy since the last one.
func (m *Meter) Poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, m.interval*4/5)
	defer cancel()

	sent := m.now()
	watts, err := m.read(ctx)
	got := m.now()
	// THE READING IS PLACED AT THE MIDDLE OF ITS REQUEST, where the plug most
	// likely took it.
	at := sent.Add(got.Sub(sent) / 2)

	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		if !m.failing {
			m.log.Warn("could not read the power meter; a job's energy over the gap is not measured",
				"error", err)
		}
		m.failing = true

		return
	}

	if m.failing {
		m.log.Info("reading the power meter again")
	}
	m.failing = false

	gap := at.Sub(m.last.at)
	switch {
	case m.last.ok && gap > 0 && gap <= staleAfter*m.interval:
		whole, frac := math.Modf(m.frac + (m.last.watts+watts)/2*gap.Seconds()*1e6)
		m.total, m.frac = m.total+int64(whole), frac
	default:
		// THE ENERGY SINCE THE LAST READING IS NOT KNOWN: a new epoch, which the
		// sampler reads as could-not-tell, never as nothing drawn.
		m.epoch++
	}
	m.last = reading{watts: watts, at: at, ok: true}
}

// Energy is the energy integrated so far, in µJ, and its epoch: two answers of
// one epoch have between them exactly what the plug was read to draw. It
// answers ErrNoReading when the plug has not been read within staleAfter
// intervals, since a total that stopped moving would read as the host drawing
// nothing.
func (m *Meter) Energy() (int64, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.last.ok || m.now().Sub(m.last.at) > staleAfter*m.interval {
		return 0, 0, ErrNoReading
	}

	return m.total, m.epoch, nil
}
