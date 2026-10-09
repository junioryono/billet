package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// The kinds scripts/known-answer-job.sh runs, and the one figure each expects.
const (
	kindBaseline = "baseline"
	kindIdle     = "idle"
	kindCPU      = "cpu"
	kindMemory   = "memory"
	kindNetwork  = "network"
	kindDisk     = "disk"
)

// expectedKey is the key a kind's expectation carries; the baseline expects
// nothing of its own and is only ever subtracted.
var expectedKey = map[string]string{
	kindIdle: "cpu_seconds", kindCPU: "cpu_seconds", kindMemory: "memory_peak_bytes",
	kindNetwork: "net_rx_bytes", kindDisk: "disk_write_bytes",
}

// expectation is what one known-answer job recorded about itself.
type expectation struct {
	Schema         int              `json:"schema"`
	Kind           string           `json:"kind"`
	Lease          string           `json:"lease"`
	Repository     string           `json:"repository"`
	RunID          int64            `json:"run_id"`
	RunAttempt     int64            `json:"run_attempt"`
	GitHubJobID    string           `json:"github_job_id"`
	Seconds        int64            `json:"seconds"`
	LoadStartedAt  string           `json:"load_started_at"`
	LoadFinishedAt string           `json:"load_finished_at"`
	Expected       map[string]int64 `json:"expected"`

	path string
}

// runKey is one attempt of one workflow run: the jobs that are compared with
// each other.
type runKey struct{ ID, Attempt int64 }

func (k runKey) String() string { return fmt.Sprintf("run %d attempt %d", k.ID, k.Attempt) }

func (e expectation) run() runKey { return runKey{e.RunID, e.RunAttempt} }

// leasePattern is what billet's lease ids are made of, and what a file name
// built from one may hold.
var leasePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validate refuses an expectation the checker could misread: the comparison is
// only as good as the record it starts from.
func (e expectation) validate() error {
	if e.Schema != 1 {
		return fmt.Errorf("schema %d is not one this checker reads", e.Schema)
	}
	if e.Kind != kindBaseline && expectedKey[e.Kind] == "" {
		return fmt.Errorf("kind %q is not a known answer", e.Kind)
	}
	if !leasePattern.MatchString(e.Lease) || e.Lease == "." || e.Lease == ".." {
		return fmt.Errorf("lease %q is not a lease id", e.Lease)
	}
	if e.RunID <= 0 || e.RunAttempt <= 0 {
		return fmt.Errorf("run %d attempt %d is not a workflow run", e.RunID, e.RunAttempt)
	}
	// THE TIMED LOADS RAN FOR SOME TIME: the idle tolerance grows with it, and
	// a zero would make a sleep of any length expected to cost nothing extra.
	switch timed := e.Kind == kindIdle || e.Kind == kindCPU || e.Kind == kindMemory; {
	case timed && (e.Seconds <= 0 || e.Seconds > 21600):
		return fmt.Errorf("a %s job ran %d seconds, which no job does", e.Kind, e.Seconds)
	case !timed && e.Seconds != 0:
		return fmt.Errorf("a %s job is not timed and says it ran %d seconds", e.Kind, e.Seconds)
	}
	want := expectedKey[e.Kind]
	for k, v := range e.Expected {
		if k != want {
			return fmt.Errorf("a %s job expects %q, which is not its figure", e.Kind, k)
		}
		// A LOADED JOB EXPECTS SOMETHING: an expected zero would compare a job
		// that did nothing with its idle reference and pass. Only the idle job
		// expects zero, and a JSON null decodes as zero, so it is refused the
		// same way.
		switch {
		case e.Kind == kindIdle && v != 0:
			return fmt.Errorf("an idle job expects %s 0, not %d", k, v)
		case e.Kind != kindIdle && v <= 0:
			return fmt.Errorf("a %s job's expected %s is %d; a load expects more than nothing", e.Kind, k, v)
		}
	}
	if _, ok := e.Expected[want]; want != "" && !ok {
		return fmt.Errorf("a %s job must expect %s", e.Kind, want)
	}

	return nil
}

// loadExpectations reads every expectation.json under dir, which is how
// `gh run download` lays out one directory per artifact.
//
// THROUGH os.Root, so a symlink among the downloaded artifacts cannot lead the
// walk to a file outside the directory it was given.
func loadExpectations(dir string) ([]expectation, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var out []expectation
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "expectation.json" {
			return nil
		}
		path := filepath.Join(dir, rel)
		body, err := root.ReadFile(rel)
		if err != nil {
			return err
		}
		var e expectation
		if err := json.Unmarshal(body, &e); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := e.validate(); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		e.path = path
		out = append(out, e)

		return nil
	})
	if err != nil {
		return nil, err
	}
	// ONE JOB PER KIND PER RUN, AND ONE JOB PER LEASE: two of either would make
	// the subtraction pick one of them silently.
	kinds := map[runKey]map[string]string{}
	leases := map[string]string{}
	for i := range out {
		e := &out[i]
		if prev, ok := leases[e.Lease]; ok {
			return nil, fmt.Errorf("lease %s is claimed by %s and %s", e.Lease, prev, e.path)
		}
		leases[e.Lease] = e.path
		if kinds[e.run()] == nil {
			kinds[e.run()] = map[string]string{}
		}
		if prev, ok := kinds[e.run()][e.Kind]; ok {
			return nil, fmt.Errorf("%s has two %s jobs: %s and %s", e.run(), e.Kind, prev, e.path)
		}
		kinds[e.run()][e.Kind] = e.path
	}
	slices.SortFunc(out, func(a, b expectation) int {
		if c := compareRuns(a.run(), b.run()); c != 0 {
			return c
		}

		return compareKinds(a.Kind, b.Kind)
	})

	return out, nil
}

func compareRuns(a, b runKey) int {
	return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.Attempt, b.Attempt))
}

var kindOrder = []string{kindBaseline, kindIdle, kindCPU, kindMemory, kindNetwork, kindDisk}

func compareKinds(a, b string) int {
	return cmp.Compare(slices.Index(kindOrder, a), slices.Index(kindOrder, b))
}

// record is the part of `billet jobs show --json` the checker reads.
type record struct {
	Lease       string `json:"lease"`
	Node        string `json:"node"`
	Provider    string `json:"provider"`
	VCPU        int64  `json:"vcpu"`
	GitHubJobID string `json:"github_job_id"`
	RunID       int64  `json:"run_id"`
	Result      string `json:"result"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
	Usage       *usage `json:"usage"`
}

// usage is the host's report. Measured is the command's own answer per group;
// a group it does not name is one this checker treats as unmeasured.
type usage struct {
	Measured        map[string]bool `json:"measured"`
	Samples         int64           `json:"samples"`
	IntervalMillis  int64           `json:"interval_ms"`
	WindowMillis    int64           `json:"window_ms"`
	CPUUserMicros   int64           `json:"cpu_user_us"`
	CPUSystemMicros int64           `json:"cpu_system_us"`
	MemoryPeakBytes int64           `json:"memory_peak_bytes"`
	DiskWriteBytes  int64           `json:"disk_write_bytes"`
	NetRxBytes      int64           `json:"net_rx_bytes"`
	EnergyActiveUJ  int64           `json:"energy_active_uj"`
	EnergyIdleUJ    int64           `json:"energy_idle_uj"`
	EnergySource    string          `json:"energy_source"`

	// present names the fields the record carried with a value. A counter that
	// was absent or null decodes as zero, and zero is also a measurement, so a
	// comparison reads only the counters that were there.
	present map[string]bool
}

func (u *usage) UnmarshalJSON(b []byte) error {
	type plain usage
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*u = usage(p)
	u.present = map[string]bool{}
	for k, v := range raw {
		u.present[k] = string(v) != "null"
	}

	return nil
}

// unusable says why the first of fields cannot be compared, or "": a counter
// the record did not carry, a negative one, which no counter can be and which
// subtracted from another would manufacture a figure, and one past any
// measurement, whose sum with another would wrap.
func (u *usage) unusable(fields ...string) string {
	for _, f := range fields {
		if !u.present[f] {
			return "carries no " + f
		}
		if v := u.counter(f); v < 0 || v > maxCounter {
			return fmt.Sprintf("says %s is %d", f, v)
		}
	}

	return ""
}

// maxCounter bounds every counter a comparison reads: 2^53 µs is 285 years of
// CPU and 2^53 bytes 8 PiB, past anything one job measures, and below it a sum
// of a few counters can neither wrap an int64 nor lose a unit in a float64.
const maxCounter = 1 << 53

// counter is the value of the named counter.
func (u *usage) counter(name string) int64 {
	switch name {
	case "samples":
		return u.Samples
	case "interval_ms":
		return u.IntervalMillis
	case "window_ms":
		return u.WindowMillis
	case "cpu_user_us":
		return u.CPUUserMicros
	case "cpu_system_us":
		return u.CPUSystemMicros
	case "memory_peak_bytes":
		return u.MemoryPeakBytes
	case "disk_write_bytes":
		return u.DiskWriteBytes
	case "net_rx_bytes":
		return u.NetRxBytes
	case "energy_active_uj":
		return u.EnergyActiveUJ
	case "energy_idle_uj":
		return u.EnergyIdleUJ
	}
	panic("knownanswer: no counter named " + name)
}

// errNoRecord is a lease with no record file: billet's answer was never
// collected, which is not the same as billet having measured nothing.
var errNoRecord = errors.New("no record was collected")

// loadRecord reads <dir>/<lease>.json.
func loadRecord(dir, lease string) (record, error) {
	if !leasePattern.MatchString(lease) || lease == "." || lease == ".." {
		return record{}, fmt.Errorf("lease %q is not a lease id", lease)
	}
	path := filepath.Join(dir, lease+".json")
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return record{}, fmt.Errorf("%w for lease %s (%s)", errNoRecord, lease, path)
	}
	if err != nil {
		return record{}, err
	}
	var r record
	if err := json.Unmarshal(body, &r); err != nil {
		return record{}, fmt.Errorf("%s: %w", path, err)
	}
	if r.Lease != lease {
		return record{}, fmt.Errorf("%s holds lease %q", path, r.Lease)
	}

	return r, nil
}
