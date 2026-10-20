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
