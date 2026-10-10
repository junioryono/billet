package guestreport

// Batch is what the agent sends its node about every BatchInterval. Every time is
// Unix time on the guest's clock, which the guest sets: milliseconds, but a step
// mark's seconds. Every CPU figure is in the guest's clock ticks (TicksPerSecond of
// them a second).
type Batch struct {
	// Seq numbers the agent's batches from 1, one more for each, so the node can
	// order them, drop a retried one and name the ones it never received.
	Seq            uint64 `json:"seq"`
	AgentVersion   string `json:"agent_version"`
	TicksPerSecond int64  `json:"ticks_per_second"`
	// AgentCPUTicks is the CPU the agent itself has used since it started.
	AgentCPUTicks int64           `json:"agent_cpu_ticks"`
	Samples       []Sample        `json:"samples,omitempty"`
	Processes     []ProcessSample `json:"processes,omitempty"`
	Steps         []StepMark      `json:"steps,omitempty"`
	Tests         []SuiteResult   `json:"tests,omitempty"`
}

// Sample is the guest's totals at one instant, every SampleInterval. Every field but
// the three memory levels is a counter since the guest booted, so the difference of
// two samples is what happened between them and dropping the samples between two
// loses resolution but never a total.
type Sample struct {
	AtMillis int64 `json:"at_ms"`
	// CPUBusyTicks and CPUTotalTicks are /proc/stat's cpu line: every column but
	// idle and iowait, and every column.
	CPUBusyTicks  int64 `json:"cpu_busy_ticks"`
	CPUTotalTicks int64 `json:"cpu_total_ticks"`
	// The memory levels are /proc/meminfo's: MemTotal less MemAvailable, then
	// MemAvailable, then Cached.
	MemUsedBytes      int64 `json:"mem_used_bytes"`
	MemAvailableBytes int64 `json:"mem_available_bytes"`
	MemCachedBytes    int64 `json:"mem_cached_bytes"`
	// Disk is vda's line of /proc/diskstats, and net is eth0's.
	DiskReadBytes  int64 `json:"disk_read_bytes"`
	DiskWriteBytes int64 `json:"disk_write_bytes"`
	NetRxBytes     int64 `json:"net_rx_bytes"`
	NetTxBytes     int64 `json:"net_tx_bytes"`
}

// ProcessSample is the guest's processes over the ProcessInterval that ends at
// AtMillis, by name: the busiest KeepProcessesByName names in Rows, every other
// process summed in Other, and the CPU no live process was charged with (a process
// that exited inside the interval took its share with it) in ResidualCPUTicks.
type ProcessSample struct {
	AtMillis         int64        `json:"at_ms"`
	Rows             []ProcessRow `json:"rows,omitempty"`
	Other            ProcessUsage `json:"other"`
	ResidualCPUTicks int64        `json:"residual_cpu_ticks"`
}

// ProcessRow is every process of one name: its comm, never its argv, which can hold
// a secret.
type ProcessRow struct {
	Name string `json:"name"`
	ProcessUsage
}

// ProcessUsage is what some processes did over one interval. Procs and RSSBytes are
// levels at its end (the largest of them, once halved); the rest are what the
// interval added.
type ProcessUsage struct {
	Procs      int64 `json:"procs"`
	CPUTicks   int64 `json:"cpu_ticks"`
	RSSBytes   int64 `json:"rss_bytes"`
	ReadBytes  int64 `json:"read_bytes"`
	WriteBytes int64 `json:"write_bytes"`
}

// StepMark is a step's start as the runner's diagnostic log records it: the step's
// display name as the log gives it, and the log line's time, which is whole seconds
// (Unix seconds on the guest's clock). The log numbers no step; GitHub's own record
// of the job's steps is the authority for numbers and conclusions.
type StepMark struct {
	Name      string `json:"name"`
	AtSeconds int64  `json:"at_s"`
}

// SuiteResult is one test suite's result file. Failed names some of the failed and
// errored tests, at most MaxFailedNames, each cut to MaxFailedNameBytes.
type SuiteResult struct {
	Name     string   `json:"name"`
	Tests    int64    `json:"tests"`
	Failures int64    `json:"failures"`
	Errors   int64    `json:"errors"`
	Skipped  int64    `json:"skipped"`
	Failed   []string `json:"failed,omitempty"`
}

// Report is a job's batches merged in sequence order, as the node keeps it.
type Report struct {
	AgentVersion   string `json:"agent_version"`
	TicksPerSecond int64  `json:"ticks_per_second"`
	// AgentCPUTicks is the agent's own CPU as its last kept batch reported it.
	AgentCPUTicks int64 `json:"agent_cpu_ticks"`

	// FirstSeq and LastSeq are the lowest and highest sequence numbers received.
	// Of the numbers between them, inclusive, Batches were kept, Refused were
	// received and refused, and Missing never arrived; Gaps names the first
	// MaxReportGaps runs of the missing ones. A batch the agent sent after LastSeq
	// is one nobody can count.
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	Batches  int64  `json:"batches"`
	Refused  int64  `json:"refused"`
	Missing  int64  `json:"missing"`
	Gaps     []Gap  `json:"gaps,omitempty"`
	// Duplicates counts a number received again with the same content, and
	// Conflicts one received again with other content; the first received is kept.
	Duplicates int64 `json:"duplicates"`
	Conflicts  int64 `json:"conflicts"`

	// Stride is how many of the agent's samples one kept sample stands for: 1
	// until Downsample halves them.
	Stride int64 `json:"stride"`
	// Dropped counts what Merge left out past the report's bounds.
	Dropped Dropped `json:"dropped"`
	// Saturated counts the sums Downsample held at MaxValue because the guest's
	// figures added past it; a process total is exact only while it is zero.
	Saturated int64 `json:"saturated"`

	Samples   []Sample        `json:"samples,omitempty"`
	Processes []ProcessSample `json:"processes,omitempty"`
	Steps     []StepMark      `json:"steps,omitempty"`
	Tests     []SuiteResult   `json:"tests,omitempty"`
}

// Gap is a run of sequence numbers, inclusive, that never arrived.
type Gap struct {
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
}

// Dropped counts what a Report's bounds left out. FailedNames counts the names cut
// from suites that were kept; a dropped suite's names go with it.
type Dropped struct {
	Steps       int64 `json:"steps"`
	Suites      int64 `json:"suites"`
	FailedNames int64 `json:"failed_names"`
}
