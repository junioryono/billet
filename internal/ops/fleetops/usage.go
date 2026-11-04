package fleetops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// usageRecordSchema names the record format docs/reference/usage-export.md
// describes. A change a consumer would have to read differently is a new number.
const usageRecordSchema = 1

const (
	// usageDefaultLimit is one run's page when --limit is not given, and
	// usageMaxLimit the largest a run reads: an exporter loops until a run prints
	// nothing, and the ledger's reader is shared with the control plane.
	usageDefaultLimit = 1000
	usageMaxLimit     = 10000

	// usageCursorVersion begins every cursor, so a format that changes is refused
	// by name rather than misread.
	usageCursorVersion = "u1"
)

// Usage is `billet usage`: what each finished lease was charged, for something
// outside billet to read. It only reports: nothing here is read back by billet.
func Usage(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return errors.New("usage: billet usage export [--config PATH] [--since CURSOR] [--limit N] [--json]")
	}

	fs := cli.NewFlagSet("billet usage export", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	since := fs.String("since", "", "resume after this record's cursor; empty starts at the first finished lease")
	limit := fs.Int("limit", usageDefaultLimit, "the most records to print; run again from the last one's cursor")
	asJSON := fs.Bool("json", false, "print one JSON object a line")
	if err := cli.Parse(fs, args[1:]); err != nil {
		return err
	}
	if *limit <= 0 || *limit > usageMaxLimit {
		return fmt.Errorf("billet usage export: --limit takes 1 to %d, not %d", usageMaxLimit, *limit)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	_, db, closeDB, err := ControlPlaneStores(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	// PEEKED, NEVER MINTED, and refused when absent. Reporting a bound ledger's
	// history under an identity this command just made would label another
	// deployment's records as a new one's.
	deployment, found, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("billet usage export: %s holds no deployment identity, so there is nothing to "+
			"say these records are whose. A control plane mints one the first time it starts",
			cfg.Server.IdentityDir)
	}

	after, err := resumeUsage(ctx, db, deployment, *since)
	if err != nil {
		return err
	}

	rows, err := db.UsageSince(ctx, after, *limit)
	if err != nil {
		return err
	}

	// SAID BESIDE THE RECORDS, NEVER IN THEM: a reader caught up to the end has not read
	// the whole history while older leases still wait for a number. They follow in later
	// runs, past the cursor it holds, and nothing is skipped.
	waiting, err := db.UsageStillNumbering(ctx)
	if err != nil {
		return err
	}

	if waiting {
		fmt.Fprintln(env.Stderr, "billet usage export: leases that finished before this release are still being "+
			"numbered; they follow in later runs, past this cursor")
	}

	records := make([]usageRecord, 0, len(rows))
	for i := range rows {
		records = append(records, newUsageRecord(cfg, deployment, &rows[i]))
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		for i := range records {
			if err := enc.Encode(&records[i]); err != nil {
				return fmt.Errorf("write a usage record: %w", err)
			}
		}

		return nil
	}

	return printUsage(env, records)
}

// usageCursor is a place in one deployment's order of finished leases.
//
// closed IDENTIFIES THE CLOSE AND NOT ONLY THE LEASE. A ledger restored from a
// backup holding leases A, B and C open can close C at 11 and B at 12 where the
// original closed A and B: a cursor of `12, B` would pass a check of the lease and
// skip C in silence. The close's time is written once, to the nanosecond, so a
// lease closed again has another.
type usageCursor struct {
	deployment string
	sequence   int64
	lease      string
	closed     string
}

func (c usageCursor) String() string {
	return usageCursorVersion + "." + c.deployment + "." + strconv.FormatInt(c.sequence, 10) + "." +
		c.lease + "." + c.closed
}

func parseUsageCursor(s string) (usageCursor, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 5 || parts[0] != usageCursorVersion || parts[4] == "" {
		return usageCursor{}, fmt.Errorf("billet usage export: --since %q is not a cursor this billet wrote", s)
	}

	sequence, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || sequence <= 0 {
		return usageCursor{}, fmt.Errorf("billet usage export: --since %q is not a cursor this billet wrote", s)
	}

	return usageCursor{deployment: parts[1], sequence: sequence, lease: parts[3], closed: parts[4]}, nil
}

// usageCloseMark is what a cursor keeps of a close's time: a digest, because the
// time itself holds the separators a cursor is split on.
func usageCloseMark(finishedAt string) string {
	sum := sha256.Sum256([]byte(finishedAt))

	return hex.EncodeToString(sum[:8])
}

// usageLedger is the ledger's half of a resume.
type usageLedger interface {
	UsageRecordAt(ctx context.Context, sequence int64) (state.UsageClose, error)
}

// resumeUsage is the place to read after, proved against this ledger.
//
// A CURSOR NAMING WHAT THIS LEDGER DOES NOT HOLD IS REFUSED, because the only
// quiet answer is the wrong one. A ledger restored from a backup older than the
// cursor numbers its next leases over the ones the exporter already read: reading
// past the old number would skip them, so the record the cursor names has to still
// be there, closed when it was. Could not read is not "not there", and is reported
// as itself.
func resumeUsage(ctx context.Context, ledger usageLedger, deployment, since string) (int64, error) {
	if since == "" {
		return 0, nil
	}

	cursor, err := parseUsageCursor(since)
	if err != nil {
		return 0, err
	}

	if cursor.deployment != deployment {
		return 0, fmt.Errorf("billet usage export: --since is deployment %s's cursor, and this is deployment %s",
			cursor.deployment, deployment)
	}

	closed, err := ledger.UsageRecordAt(ctx, cursor.sequence)
	switch {
	case errors.Is(err, state.ErrNoUsageRecord):
		return 0, fmt.Errorf("billet usage export: this ledger holds no record %d, which --since names. It "+
			"was restored from a backup older than the cursor, so records past the backup need reconciling; "+
			"nothing was printed", cursor.sequence)
	case err != nil:
		return 0, err
	case closed.LeaseID != cursor.lease || usageCloseMark(closed.FinishedAt) != cursor.closed:
		return 0, fmt.Errorf("billet usage export: record %d on this ledger is not the close --since names "+
			"(lease %s). It was restored from a backup older than the cursor, and has finished leases again "+
			"since; nothing was printed", cursor.sequence, closed.LeaseID)
	}

	return cursor.sequence, nil
}

// usageInterval is a stretch of time on billet's clock. A nil end or length is
// not known, and the record's Unknown says why.
type usageInterval struct {
	From    string       `json:"from"`
	To      string       `json:"to"`
	Seconds *json.Number `json:"seconds"`
}

// usageRecord is one finished lease as docs/reference/usage-export.md states it.
//
// A VALUE THE LEDGER DOES NOT KNOW IS EMPTY OR NULL, AND Unknown SAYS WHY. Zero is
// never written in its place: a billing system adding up a column of zeros has
// silently given minutes away.
type usageRecord struct {
	Schema     int    `json:"schema"`
	Cursor     string `json:"cursor"`
	Sequence   int64  `json:"sequence"`
	LeaseID    string `json:"lease_id"`
	Deployment string `json:"deployment"`
	Target     string `json:"target"`
	Repository string `json:"repository"`
	Tier       string `json:"tier"`
	Site       string `json:"site"`
	Provider   string `json:"provider"`
	Ownership  string `json:"ownership"`

	VCPU        *int64 `json:"vcpu"`
	MemoryBytes *int64 `json:"memory_bytes"`

	// Held is how long the lease held its capacity, and Job how long its job ran.
	Held usageInterval `json:"held"`
	Job  usageInterval `json:"job"`

	BilledMinutes   *int64       `json:"billed_minutes"`
	MinuteWeight    *json.Number `json:"minute_weight"`
	WeightSource    string       `json:"weight_source"`
	WeightedMinutes *json.Number `json:"weighted_minutes"`

	InstanceType       string `json:"instance_type"`
	PriceMicrosPerHour *int64 `json:"price_micros_per_hour"`
	CostMicros         *int64 `json:"cost_micros"`

	Conclusion string `json:"conclusion"`
	Result     string `json:"result"`

	Unknown map[string]string `json:"unknown"`
}

// The tokens a consumer branches on.
const (
	usageOwned  = "owned"
	usageBought = "bought"

	// maxUsageVCPU is a shape no machine has, past which a recorded vCPU is
	// damage rather than a shape to weigh minutes by.
	maxUsageVCPU = 1 << 32

	weightFromTier = "tier"
	weightFromVCPU = "charged_vcpu"
)

func newUsageRecord(cfg *config.Config, deployment string, row *state.UsageRow) usageRecord {
	r := usageRecord{
		Schema:     usageRecordSchema,
		Cursor:     usageCursor{deployment, row.Sequence, row.LeaseID, usageCloseMark(row.FinishedAt)}.String(),
		Sequence:   row.Sequence,
		LeaseID:    row.LeaseID,
		Deployment: deployment,
		Target:     row.Target,
		Repository: row.Repository,
		Tier:       row.Tier,
		Site:       row.Site,
		Provider:   row.Provider,
		Conclusion: row.Conclusion,
		Result:     row.Result,
		Unknown:    map[string]string{},
	}

	if row.Target == "" {
		r.Unknown["target"] = "no runner was registered for this lease, or it finished before billet recorded " +
			"the owner on the history (migration 71)"
	}
	if row.Repository == "" {
		r.Unknown["repository"] = "GitHub never named a job's repository for this lease"
	}

	r.setProvider(row)
	r.setShape(row)
	r.setIntervals(row)
	r.setWeight(cfg, row)
	r.setPrice(row)

	return r
}

func (r *usageRecord) setProvider(row *state.UsageRow) {
	switch kind := config.ProviderKind(row.Provider); {
	case row.Provider == "":
		r.Unknown["provider"] = "no backend was recorded: the lease was never placed, or it finished before " +
			"billet kept that on the history (migration 49)"
		r.Unknown["ownership"] = r.Unknown["provider"]
	case !kind.Valid():
		// A NEWER BINARY'S BACKEND, kept as the ledger has it. Whether a machine was
		// bought is not this billet's to guess.
		r.Unknown["ownership"] = "this billet does not know the backend " + strconv.Quote(row.Provider)
	case kind.RunsOnHost():
		r.Ownership = usageOwned
	default:
		r.Ownership = usageBought
	}
}

func (r *usageRecord) setShape(row *state.UsageRow) {
	const reason = "no shape was recorded: the lease was never charged one, or it finished before billet " +
		"kept that on the history (migration 49)"

	if row.VCPU > 0 {
		r.VCPU = new(row.VCPU)
	} else {
		r.Unknown["vcpu"] = reason
	}

	if row.Memory > 0 {
		r.MemoryBytes = new(row.Memory)
	} else {
		r.Unknown["memory_bytes"] = reason
	}
}

// setIntervals reads the two stretches of time, and from the job's the minutes.
//
// BILLED MINUTES ARE THE JOB'S, rounded up per lease to a whole minute, which is
// how GitHub bills a hosted job. A lease whose job never started has a known zero;
// one GitHub finished without billet seeing it start, or that ended before GitHub
// said how the job did, has no number at all. What the lease held is reported
// beside it either way, so a consumer can decide what it means.
func (r *usageRecord) setIntervals(row *state.UsageRow) {
	r.Held.From, r.Held.To = row.BoughtAt, row.FinishedAt
	r.Job.From, r.Job.To = row.StartedAt, row.ResultAt

	switch {
	case row.BoughtAt == "":
		r.Unknown["held.from"] = "billet did not record when this lease was bought: it finished before billet " +
			"kept that for every lease (migration 71)"
		r.Unknown["held.seconds"] = r.Unknown["held.from"]
	case row.FinishedAt == "":
		r.Unknown["held.to"] = "the history row has a usage number and no finish time, which billet does not write"
		r.Unknown["held.seconds"] = r.Unknown["held.to"]
	default:
		seconds, why := secondsBetween(row.BoughtAt, row.FinishedAt)
		if why != "" {
			r.Unknown["held.seconds"] = why
		} else {
			r.Held.Seconds = jsonSeconds(seconds)
		}
	}

	switch {
	case row.StartedAt == "" && row.ResultAt == "":
		// KNOWN, not unknown: the ledger keeps the start at its first value from the
		// moment the lease is busy, and nothing started here.
		r.BilledMinutes = new(int64(0))
		r.Unknown["job.from"] = "no job started on this lease"
		r.Unknown["job.to"] = r.Unknown["job.from"]
		r.Unknown["job.seconds"] = r.Unknown["job.from"]
		r.Unknown["result"] = r.Unknown["job.from"]
	case row.StartedAt == "":
		r.Unknown["job.from"] = "GitHub reported a result for this lease and billet never saw its job start"
		r.Unknown["job.seconds"] = r.Unknown["job.from"]
		r.Unknown["billed_minutes"] = r.Unknown["job.from"]
	case row.ResultAt == "":
		r.Unknown["job.to"] = "the job started and billet recorded no result from GitHub before exporting this " +
			"record; held says how long the lease lasted"
		r.Unknown["job.seconds"] = r.Unknown["job.to"]
		r.Unknown["billed_minutes"] = r.Unknown["job.to"]
		r.Unknown["result"] = r.Unknown["job.to"]
	default:
		seconds, why := secondsBetween(row.StartedAt, row.ResultAt)
		if why != "" {
			r.Unknown["job.seconds"] = why
			r.Unknown["billed_minutes"] = why

			break
		}
		r.Job.Seconds = jsonSeconds(seconds)
		r.BilledMinutes = new(wholeMinutes(seconds))
	}
}

// setWeight is what one of those minutes counts as, from the tier if it names a
// weight and otherwise from the vCPU the lease was charged. A tier the config no
// longer declares falls to the charged vCPU, and the source says so.
func (r *usageRecord) setWeight(cfg *config.Config, row *state.UsageRow) {
	var thousandths int64

	tier, declared := cfg.TierByLabel(row.Tier)

	switch {
	case declared && tier.MinuteWeight > 0:
		thousandths = tier.MinuteWeight.Thousandths()
		r.WeightSource = weightFromTier
	case row.VCPU > 0 && row.VCPU <= maxUsageVCPU:
		// NORMALISED TO A 2 VCPU RUNNER: a 4 vCPU runner counts double.
		thousandths = row.VCPU * 500
		r.WeightSource = weightFromVCPU
	default:
		r.Unknown["minute_weight"] = "the tier names no weight and no usable shape was recorded for this lease"
		r.Unknown["weight_source"] = r.Unknown["minute_weight"]
		r.Unknown["weighted_minutes"] = r.Unknown["minute_weight"]

		return
	}

	weight := json.Number(config.FormatThousandths(thousandths))
	r.MinuteWeight = &weight

	if r.BilledMinutes == nil {
		r.Unknown["weighted_minutes"] = r.Unknown["billed_minutes"]

		return
	}

	// IN ARBITRARY PRECISION: a number a ledger row can hold must not wrap into a
	// negative charge on its way out.
	product := new(big.Int).Mul(big.NewInt(thousandths), big.NewInt(*r.BilledMinutes))
	if !product.IsInt64() {
		r.Unknown["weighted_minutes"] = "the weight times the minutes is too large to state"

		return
	}

	weighted := json.Number(config.FormatThousandths(product.Int64()))
	r.WeightedMinutes = &weighted
}

// setPrice is what a bought machine cost. It says nothing of an owned one, which
// bought nothing; a price of zero on a machine that was bought is a row written
// before the column existed, and reads as unknown, never as free.
func (r *usageRecord) setPrice(row *state.UsageRow) {
	r.InstanceType = row.InstanceType

	if r.Ownership == usageOwned {
		return
	}

	if row.InstanceType == "" && r.Ownership == usageBought {
		r.Unknown["instance_type"] = "the lease was charged no shape it could buy"
	}

	if row.PriceMicrosPerHour <= 0 {
		const reason = "no price was recorded: the lease was never charged a shape, or it finished before " +
			"billet kept the price on the history (migration 49)"
		r.Unknown["price_micros_per_hour"] = reason
		r.Unknown["cost_micros"] = reason

		return
	}

	r.PriceMicrosPerHour = new(row.PriceMicrosPerHour)

	seconds, why := secondsBetween(row.BoughtAt, row.FinishedAt)
	if why != "" {
		r.Unknown["cost_micros"] = why

		return
	}

	cost, ok := costMicros(row.PriceMicrosPerHour, seconds)
	if !ok {
		r.Unknown["cost_micros"] = "the price across this time is too large to state"

		return
	}

	r.CostMicros = new(cost)
}

// secondsBetween is the length of a stretch on billet's clock, or why there is
// none. A stretch that ends before it starts is a clock that stepped, and is not
// reported as a length.
func secondsBetween(from, to string) (time.Duration, string) {
	start, err := time.Parse(time.RFC3339Nano, from)
	if err != nil {
		return 0, "the start " + strconv.Quote(from) + " is not a time"
	}

	end, err := time.Parse(time.RFC3339Nano, to)
	if err != nil {
		return 0, "the end " + strconv.Quote(to) + " is not a time"
	}

	if end.Before(start) {
		return 0, "the recorded end " + to + " is before the start " + from + ", so the clock moved between them"
	}

	return end.Sub(start), ""
}

// wholeMinutes rounds up: any part of a minute bills the minute.
func wholeMinutes(d time.Duration) int64 {
	minutes := int64(d / time.Minute)
	if d%time.Minute != 0 {
		minutes++
	}

	return minutes
}

func jsonSeconds(d time.Duration) *json.Number {
	n := json.Number(strconv.FormatFloat(d.Seconds(), 'f', 3, 64))

	return &n
}

// costMicros is a price per hour across a length, rounded up to a whole
// millionth of a dollar, or false when the answer does not fit.
func costMicros(pricePerHour int64, d time.Duration) (int64, bool) {
	product := new(big.Int).Mul(big.NewInt(pricePerHour), big.NewInt(int64(d)))
	quotient, remainder := new(big.Int).QuoRem(product, big.NewInt(int64(time.Hour)), new(big.Int))
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}

	return quotient.Int64(), quotient.IsInt64()
}

func printUsage(env cli.Env, records []usageRecord) error {
	if len(records) == 0 {
		fmt.Fprintln(env.Stdout, "no finished lease past this cursor")

		return nil
	}

	table := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "SEQ\tLEASE\tTARGET\tTIER\tPROVIDER\tOWNED\tVCPU\tMEMORY\tHELD_S\tJOB_S\tMIN\tWEIGHT\tWEIGHTED\tCOST_USD")

	for i := range records {
		r := &records[i]
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Sequence, r.LeaseID, orUnknown(r.Target), r.Tier, orUnknown(r.Provider), orUnknown(r.Ownership),
			numberOrUnknown(r.VCPU), numberOrUnknown(r.MemoryBytes), jsonOrUnknown(r.Held.Seconds),
			jsonOrUnknown(r.Job.Seconds), numberOrUnknown(r.BilledMinutes), jsonOrUnknown(r.MinuteWeight),
			jsonOrUnknown(r.WeightedMinutes), microsOrUnknown(r.CostMicros))
	}

	if err := table.Flush(); err != nil {
		return fmt.Errorf("write the usage table: %w", err)
	}

	fmt.Fprintf(env.Stdout, "cursor %s\n", records[len(records)-1].Cursor)

	return nil
}

func numberOrUnknown(n *int64) string {
	if n == nil {
		return "-"
	}

	return strconv.FormatInt(*n, 10)
}

func jsonOrUnknown(n *json.Number) string {
	if n == nil {
		return "-"
	}

	return n.String()
}

func microsOrUnknown(n *int64) string {
	if n == nil {
		return "-"
	}

	return strconv.FormatFloat(float64(*n)/1e6, 'f', 6, 64)
}
