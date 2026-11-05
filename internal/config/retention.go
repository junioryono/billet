package config

import (
	"fmt"
	"strings"
	"time"
)

// DefaultJobDetailRetention is how long a job's per-sample series, destinations
// and guest report are kept when server.job_detail_retention is absent.
//
// DAYS, NOT WEEKS. The three grew the reference controller's ledger by over
// 100 MB a day from 2026-10-17 to 20, and the startup integrity scan reads the
// whole file cold after every host upgrade, measured at 55 s for 740 MB against
// its 80 s bound (#572). Three days keeps a few days of investigation and the
// ledger far inside that bound.
const DefaultJobDetailRetention = 72 * time.Hour

// minJobDetailRetention is the shortest retention accepted: a job's detail is
// what an operator reads about a job that just ran.
const minJobDetailRetention = time.Hour

// JobDetailRetentionDuration parses server.job_detail_retention, reporting the
// default when unset.
func (s *ServerConfig) JobDetailRetentionDuration() (time.Duration, error) {
	if s == nil || strings.TrimSpace(s.JobDetailRetention) == "" {
		return DefaultJobDetailRetention, nil
	}
	raw := s.JobDetailRetention
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("server.job_detail_retention: %q is not a duration such as \"72h\" or \"168h\": %w",
			raw, err)
	}
	if d < minJobDetailRetention {
		return 0, fmt.Errorf("server.job_detail_retention: %s is shorter than an hour, which would delete a job's "+
			"series and guest report before anyone could read them; give at least \"1h\"", d)
	}

	return d, nil
}

// DefaultTestResultsRetention is how long a job's test suites and cases are kept
// when server.test_results_retention is absent: the two weeks `billet tests stats`
// reads by default, long past job detail because a flaky test shows itself over
// many runs.
const DefaultTestResultsRetention = 14 * 24 * time.Hour

// DefaultTestResultsMaxBytes is how much of them is kept in all when
// server.test_results_max_bytes is absent. A BOUND ON THE TABLE, AND NOT ONLY ON
// TIME, because a busy deployment's rows could otherwise take back the ledger job
// detail's retention freed, and the startup integrity scan reads all of it: 55 s for
// 740 MB against an 80 s bound (#572). 128 MiB costs that scan about 10 s, and at the
// most one job can keep, 87 KiB, it holds about 1,500 jobs.
const DefaultTestResultsMaxBytes = 128 * MiB

// minTestResultsMaxBytes is the smallest bound accepted: it holds a dozen jobs at
// the most one can keep.
const minTestResultsMaxBytes = MiB

// TestResultsRetentionDuration parses server.test_results_retention, reporting the
// default when unset.
func (s *ServerConfig) TestResultsRetentionDuration() (time.Duration, error) {
	if s == nil || strings.TrimSpace(s.TestResultsRetention) == "" {
		return DefaultTestResultsRetention, nil
	}
	raw := s.TestResultsRetention
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("server.test_results_retention: %q is not a duration such as \"336h\": %w", raw, err)
	}
	if d < minJobDetailRetention {
		return 0, fmt.Errorf("server.test_results_retention: %s is shorter than an hour, which would delete a job's "+
			"test results before anyone could read them; give at least \"1h\"", d)
	}

	return d, nil
}

// TestResultsBound is server.test_results_max_bytes, reporting the default when
// unset.
func (s *ServerConfig) TestResultsBound() (ByteSize, error) {
	if s == nil || s.TestResultsMaxBytes == 0 {
		return DefaultTestResultsMaxBytes, nil
	}
	if s.TestResultsMaxBytes < minTestResultsMaxBytes {
		return 0, fmt.Errorf("server.test_results_max_bytes: %s is under %s, which holds too few jobs to rank any "+
			"test by", s.TestResultsMaxBytes, minTestResultsMaxBytes)
	}

	return s.TestResultsMaxBytes, nil
}
