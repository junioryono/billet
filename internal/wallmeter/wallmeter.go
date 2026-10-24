// Package wallmeter reads the power a host draws at the wall from the smart plug
// it is plugged into (node.monitoring.meter), for the node's sampler to share
// among jobs.
//
// A leaf: the standard library only.
package wallmeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
)

// maxBody bounds a plug's answer; Switch.GetStatus answers in under 1 KB.
const maxBody = 64 << 10

// MaxWatts bounds a reading: far above what a plug can switch (a Shelly Plug US
// is rated 15 A at 120 V, 1.8 kW), to refuse a unit mistake rather than model a
// plug.
const MaxWatts = 10_000

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
	// Client is the HTTP client; nil uses one that follows no redirect and has
	// no timeout of its own, so the caller's context bounds every read.
	Client *http.Client
}

// noRedirects is the client a Shelly with none of its own reads through.
//
// A REDIRECT IS A FAILED READING, never followed: it would let the device on
// the other end send the node's request somewhere else on its network, and
// take another plug's answer as this host's power.
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

// shellyStatus is the part of Switch.GetStatus's answer billet reads.
type shellyStatus struct {
	ID     *int     `json:"id"`
	APower *float64 `json:"apower"`
}

// Watts reads the plug's instantaneous active power.
func (s Shelly) Watts(ctx context.Context) (float64, error) {
	u, err := url.Parse(s.URL)
	if err != nil {
		return 0, errors.New("wallmeter: the plug's address does not parse")
	}
	u = u.JoinPath("rpc", "Switch.GetStatus")
	u.RawQuery = url.Values{"id": {strconv.Itoa(s.Switch)}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("wallmeter: %w", err)
	}

	client := s.Client
	if client == nil {
		client = noRedirects
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("wallmeter: read the plug: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("wallmeter: the plug answered %s", resp.Status)
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
