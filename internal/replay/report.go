package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// Record is what the ledger recorded about one job, joined to the trace line
// that produced it.
//
// EVERYTHING BUT Arrival, Repository, WorkflowRef AND Cost COMES FROM THE
// LEDGER. The arrival is the trace's fact and the join is on the request id
// billet recorded; the provider, the charged shape and its price, the site and
// what the cache did are the history row's own columns (migration 49), written
// from the lease rather than from any catalogue. Cost is the one derived figure
// and says where its rate came from.
type Record struct {
	Seq         int64  `json:"seq"`
	LeaseID     string `json:"lease_id"`
	Tier        string `json:"tier"`
	Repository  string `json:"repository"`
	WorkflowRef string `json:"workflow"`
	Node        string `json:"node"`

	// Provider, InstanceType, VCPU, Memory and Site are what the lease was
	// charged for: the backend it ran on, the shape placement bought (empty for a
	// host-backed backend), the charged vCPU and memory, and the host's site.
	Provider     string          `json:"provider"`
	InstanceType string          `json:"instance_type,omitempty"`
	VCPU         int             `json:"vcpu"`
	Memory       config.ByteSize `json:"memory"`
	Site         string          `json:"site,omitempty"`
	// PriceMicrosPerHour is the rate the shape was charged at when the lease was
	// escrowed, in millionths of a dollar. Zero means no price was recorded,
	// which is what a host-backed lease has.
	PriceMicrosPerHour int64 `json:"price_micros_per_hour"`
	// Cost is the run's cost in dollars and CostSource says what rate produced
	// it: "ledger" when the recorded price was positive, "fleet-rate" when the
	// fleet's per-host rate was applied to the charged vCPU-hours, and "" when
	// neither was available, in which case Cost is zero and means unknown.
	Cost       float64 `json:"cost"`
	CostSource string  `json:"cost_source,omitempty"`
	// ImageCache, CacheGeneration and ActionsCache are what the node observed the
	// cache do, in the ledger's closed vocabularies; empty means nothing was
	// observed, which is what a simulated guest reports.
	ImageCache      string `json:"image_cache,omitempty"`
	CacheGeneration string `json:"cache_generation,omitempty"`
	ActionsCache    string `json:"actions_cache,omitempty"`

	Arrival    time.Time `json:"arrival"`
	AssignedAt time.Time `json:"assigned_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// QueueWait is from the trace's arrival to the ledger's recorded start, and
	// RunDuration from that start to the lease's archive.
	QueueWait   Duration `json:"queue_wait"`
	RunDuration Duration `json:"run_duration"`

	Conclusion string `json:"conclusion"`
	Result     string `json:"result"`
	Disruption string `json:"disruption,omitempty"`

	// Locality is DERIVED FROM PLACEMENT, not observed: an earlier job of the
	// same repository was placed on the same node. Beside the observed cache
	// columns because a simulated guest observes nothing, and locality is the
	// question a placement policy can be asked without one.
	Locality bool `json:"locality"`
}

// Report is what one replay recorded.
type Report struct {
	Fleet Fleet
	// Records are the jobs the ledger knows about, in sequence order.
	Records []Record
	// Missing are trace jobs with no ledger row: gaps in the recording, reported
	// as gaps rather than filled in.
	Missing []int64
	// Unstarted are recorded jobs the ledger never saw start.
	Unstarted []int64
	// EscrowRows counts history rows for leases that were never assigned a job:
	// the discovery slots released at shutdown. Reported so a reader can tell
	// them from a missing job.
	EscrowRows int
	// Violations are the overcommits the records prove: a host or the deployment
	// charged more than it has, at some instant.
	Violations []string
}

// historyBound is how many extra rows beyond the trace the read allows for, so
// the discovery escrows do not truncate the jobs.
const historyBound = 1000

// readReport reads job_history back and joins it to the trace.
func readReport(ctx context.Context, db *state.DB, fleet Fleet, trace Trace) (*Report, error) {
	rows, err := state.ReadQueries(db.Reader()).ListJobHistory(ctx, int64(len(trace.Arrivals)+historyBound))
	if err != nil {
		return nil, fmt.Errorf("read job history: %w", err)
	}

	byRequest := make(map[int64]*Arrival, len(trace.Arrivals))
	for i := range trace.Arrivals {
		byRequest[trace.Arrivals[i].Seq] = &trace.Arrivals[i]
	}

	r := &Report{Fleet: fleet}
	seen := make(map[int64]bool, len(rows))

	for i := range rows {
		row := &rows[i]

		if row.RequestID == 0 {
			r.EscrowRows++

			continue
		}

		a, ok := byRequest[row.RequestID]
		if !ok {
			return nil, fmt.Errorf("the ledger recorded request %d, which the trace never offered", row.RequestID)
		}

		if seen[row.RequestID] {
			return nil, fmt.Errorf("the ledger recorded request %d twice", row.RequestID)
		}

		seen[row.RequestID] = true

		rec := Record{
			Seq:         a.Seq,
			LeaseID:     row.LeaseID,
			Tier:        row.Tier,
			Repository:  a.Repository,
			WorkflowRef: a.WorkflowRef,
			Node:        row.Node,
			Arrival:     a.At,
			AssignedAt:  parseStamp(row.QueuedAt),
			StartedAt:   parseStamp(row.StartedAt),
			FinishedAt:  parseStamp(row.FinishedAt),
			Conclusion:  row.Conclusion.String,
			Result:      row.Result,
			Disruption:  row.Disruption,
		}

		// THE CHARGED SHAPE IS THE ROW'S, and the row is read rather than the
		// tier's request assumed: the two agree for a host-backed backend, and
		// reading the ledger is the point of the report.
		placement, err := state.ReadQueries(db.Reader()).ReadJobPlacement(ctx, row.LeaseID)
		if err != nil {
			return nil, fmt.Errorf("read the placement of lease %s: %w", row.LeaseID, err)
		}

		rec.Provider = placement.ChosenProvider
		rec.InstanceType = placement.InstanceType
		rec.VCPU = int(placement.Vcpu)
		rec.Memory = config.ByteSize(placement.Memory)
		rec.Site = placement.Site
		rec.PriceMicrosPerHour = placement.PriceMicrosPerHour
		rec.ImageCache = placement.ImageCache
		rec.CacheGeneration = placement.CacheGeneration
		rec.ActionsCache = placement.ActionsCache

		if rec.StartedAt.IsZero() {
			r.Unstarted = append(r.Unstarted, a.Seq)
		} else {
			rec.QueueWait = Duration(rec.StartedAt.Sub(a.At))

			if !rec.FinishedAt.IsZero() {
				rec.RunDuration = Duration(rec.FinishedAt.Sub(rec.StartedAt))
			}
		}

		rec.Cost, rec.CostSource = fleet.cost(&rec)

		r.Records = append(r.Records, rec)
	}

	for i := range trace.Arrivals {
		if seq := trace.Arrivals[i].Seq; !seen[seq] {
			r.Missing = append(r.Missing, seq)
		}
	}

	slices.SortFunc(r.Records, func(a, b Record) int { return int(a.Seq - b.Seq) })
	r.deriveLocality()
	r.Violations = r.checkCapacity()

	return r, nil
}

// parseStamp reads a ledger timestamp; an empty one is the zero time.
func parseStamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}

	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}

	return t
}

// deriveLocality marks each record whose repository already had a job placed on
// the same node, in start order.
func (r *Report) deriveLocality() {
	order := slices.Clone(r.Records)
	slices.SortStableFunc(order, func(a, b Record) int { return a.StartedAt.Compare(b.StartedAt) })

	type key struct{ node, repo string }

	seen := map[key]bool{}
	warm := map[int64]bool{}

	for i := range order {
		rec := &order[i]
		if rec.Node == "" || rec.StartedAt.IsZero() {
			continue
		}

		k := key{rec.Node, rec.Repository}
		warm[rec.Seq] = seen[k]
		seen[k] = true
	}

	for i := range r.Records {
		r.Records[i].Locality = warm[r.Records[i].Seq]
	}
}

// checkCapacity sweeps every record's charged interval and reports any instant
// at which a host or the deployment was charged more than it has.
//
// THE HARNESS'S OWN MUTATION TEST. A placer that ignores a host's room lands
// jobs past its capacity, and nothing in a per-lease test sees it; this does,
// from the ledger's own timestamps. The interval is assignment to finish, the
// span the ledger says the job held its shape.
func (r *Report) checkCapacity() []string {
	type change struct {
		at         time.Time
		vcpu       int
		memory     config.ByteSize
		node, tier string
	}

	var changes []change

	for i := range r.Records {
		rec := &r.Records[i]
		if rec.AssignedAt.IsZero() || rec.FinishedAt.IsZero() {
			continue
		}

		changes = append(changes,
			change{rec.AssignedAt, rec.VCPU, rec.Memory, rec.Node, rec.Tier},
			change{rec.FinishedAt, -rec.VCPU, -rec.Memory, rec.Node, rec.Tier})
	}

	// Releases before charges at the same instant, so a job finishing as
	// another is assigned is not read as an overlap the ledger never had.
	slices.SortStableFunc(changes, func(a, b change) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}

		return a.vcpu - b.vcpu
	})

	type load struct {
		vcpu   int
		memory config.ByteSize
	}

	perNode := map[string]load{}
	total := load{}
	reported := map[string]bool{}

	var out []string

	for _, c := range changes {
		l := perNode[c.node]
		l.vcpu += c.vcpu
		l.memory += c.memory
		perNode[c.node] = l
		total.vcpu += c.vcpu
		total.memory += c.memory

		if c.vcpu < 0 {
			continue
		}

		if h, ok := r.Fleet.host(c.node); ok && (l.vcpu > h.VCPU || l.memory > h.Memory) && !reported[c.node] {
			reported[c.node] = true
			out = append(out, fmt.Sprintf("host %s carried %d vCPU and %s at %s against %d vCPU and %s",
				c.node, l.vcpu, l.memory, c.at.Format(time.RFC3339), h.VCPU, h.Memory))
		}

		if (total.vcpu > r.Fleet.MaxVCPU || total.memory > r.Fleet.MaxMemory) && !reported[""] {
			reported[""] = true
			out = append(out, fmt.Sprintf("the deployment carried %d vCPU and %s at %s against a ceiling of %d vCPU and %s",
				total.vcpu, total.memory, c.at.Format(time.RFC3339), r.Fleet.MaxVCPU, r.Fleet.MaxMemory))
		}
	}

	return out
}

// Placements is the node each recorded job ran on, by sequence.
func (r *Report) Placements() map[int64]string {
	out := make(map[int64]string, len(r.Records))
	for i := range r.Records {
		out[r.Records[i].Seq] = r.Records[i].Node
	}

	return out
}

// PeakVCPUByNode is the most vCPU each host carried at any instant.
func (r *Report) PeakVCPUByNode() map[string]int {
	type change struct {
		at   time.Time
		vcpu int
		node string
	}

	var changes []change

	for i := range r.Records {
		rec := &r.Records[i]
		if rec.AssignedAt.IsZero() || rec.FinishedAt.IsZero() {
			continue
		}

		changes = append(changes,
			change{rec.AssignedAt, rec.VCPU, rec.Node},
			change{rec.FinishedAt, -rec.VCPU, rec.Node})
	}

	slices.SortStableFunc(changes, func(a, b change) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}

		return a.vcpu - b.vcpu
	})

	load := map[string]int{}
	peak := map[string]int{}

	for _, c := range changes {
		load[c.node] += c.vcpu
		peak[c.node] = max(peak[c.node], load[c.node])
	}

	return peak
}

// HostsUsed is how many distinct hosts carried a job.
func (r *Report) HostsUsed() int {
	used := map[string]bool{}

	for i := range r.Records {
		if node := r.Records[i].Node; node != "" {
			used[node] = true
		}
	}

	return len(used)
}

// Percentiles are the queue-wait figures the summary reports for one group.
type Percentiles struct {
	Count         int
	P50, P95, Max time.Duration
}

// QueueWaitBy groups the started records by a key and reports each group's
// queue-wait percentiles.
func (r *Report) QueueWaitBy(key func(*Record) string) map[string]Percentiles {
	groups := map[string][]time.Duration{}

	for i := range r.Records {
		rec := &r.Records[i]
		if rec.StartedAt.IsZero() {
			continue
		}

		k := key(rec)
		groups[k] = append(groups[k], time.Duration(rec.QueueWait))
	}

	out := make(map[string]Percentiles, len(groups))

	for k, waits := range groups {
		slices.Sort(waits)
		out[k] = Percentiles{
			Count: len(waits),
			P50:   percentile(waits, 0.50),
			P95:   percentile(waits, 0.95),
			Max:   waits[len(waits)-1],
		}
	}

	return out
}

// percentile is the nearest-rank percentile of a sorted, non-empty slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	i := int(float64(len(sorted))*p+0.999999) - 1

	return sorted[min(max(i, 0), len(sorted)-1)]
}

// LocalityRate is the share of started jobs whose repository already had a job
// on the same node.
func (r *Report) LocalityRate() float64 {
	started, warm := 0, 0

	for i := range r.Records {
		rec := &r.Records[i]
		if rec.StartedAt.IsZero() {
			continue
		}

		started++

		if rec.Locality {
			warm++
		}
	}

	if started == 0 {
		return 0
	}

	return float64(warm) / float64(started)
}

// WriteJSONL writes one record per line.
func (r *Report) WriteJSONL(w io.Writer) error {
	enc := json.NewEncoder(w)

	for i := range r.Records {
		if err := enc.Encode(&r.Records[i]); err != nil {
			return fmt.Errorf("replay: write record %d: %w", r.Records[i].Seq, err)
		}
	}

	return nil
}

// Summary renders the report for a person: what was recorded, what was not, how
// long jobs queued, where they went, and what the records prove about capacity.
func (r *Report) Summary() string {
	var b strings.Builder

	fmt.Fprintf(&b, "jobs recorded: %d (missing %d, never started %d, escrow rows %d)\n",
		len(r.Records), len(r.Missing), len(r.Unstarted), r.EscrowRows)

	byTier := r.QueueWaitBy(func(rec *Record) string { return rec.Tier })
	for _, tier := range slices.Sorted(maps.Keys(byTier)) {
		p := byTier[tier]
		fmt.Fprintf(&b, "tier %s: %d jobs, queue wait p50 %s p95 %s max %s\n",
			tier, p.Count, p.P50, p.P95, p.Max)
	}

	byRepo := r.QueueWaitBy(func(rec *Record) string { return rec.Repository })
	for _, repo := range slices.Sorted(maps.Keys(byRepo)) {
		p := byRepo[repo]
		fmt.Fprintf(&b, "repository %s: %d jobs, queue wait p50 %s p95 %s max %s\n",
			repo, p.Count, p.P50, p.P95, p.Max)
	}

	peaks := r.PeakVCPUByNode()
	for _, node := range slices.Sorted(maps.Keys(peaks)) {
		capacity := "unknown capacity"
		if h, ok := r.Fleet.host(node); ok {
			capacity = strconv.Itoa(h.VCPU) + " vCPU"
		}

		fmt.Fprintf(&b, "host %s: peak %d vCPU of %s\n", node, peaks[node], capacity)
	}

	fmt.Fprintf(&b, "locality (derived from placement, not observed): %.0f%% of jobs followed their repository onto a host\n",
		100*r.LocalityRate())

	costs, unpriced := r.CostByTier()
	for _, tier := range slices.Sorted(maps.Keys(costs)) {
		fmt.Fprintf(&b, "cost on %s: $%.2f\n", tier, costs[tier])
	}

	if unpriced > 0 {
		fmt.Fprintf(&b, "cost: %d jobs had no recorded price and no fleet rate, so their cost is unknown\n", unpriced)
	}

	observed := r.CacheOutcomes()
	if len(observed) == 0 {
		fmt.Fprintf(&b, "cache: the ledger recorded no image or Actions cache observation for any job\n")
	} else {
		for _, outcome := range slices.Sorted(maps.Keys(observed)) {
			fmt.Fprintf(&b, "cache %s: %d jobs\n", outcome, observed[outcome])
		}
	}

	if len(r.Violations) == 0 {
		fmt.Fprintf(&b, "capacity: no host or deployment overcommit in the recorded intervals\n")
	} else {
		fmt.Fprintf(&b, "capacity: %d overcommit(s): %s\n", len(r.Violations), strings.Join(r.Violations, "; "))
	}

	return b.String()
}

// CostByTier sums the priced records' cost per tier, and counts the records
// whose cost is unknown.
func (r *Report) CostByTier() (map[string]float64, int) {
	out := map[string]float64{}
	unpriced := 0

	for i := range r.Records {
		rec := &r.Records[i]
		if rec.CostSource == "" {
			unpriced++

			continue
		}

		out[rec.Tier] += rec.Cost
	}

	return out, unpriced
}

// CacheOutcomes counts the cache observations the ledger recorded, keyed as
// "image:<token>" and "actions:<token>". Nothing observed counts nothing.
func (r *Report) CacheOutcomes() map[string]int {
	out := map[string]int{}

	for i := range r.Records {
		rec := &r.Records[i]
		if rec.ImageCache != "" {
			out["image:"+rec.ImageCache]++
		}

		if rec.ActionsCache != "" {
			out["actions:"+rec.ActionsCache]++
		}
	}

	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
