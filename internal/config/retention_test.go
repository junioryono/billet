package config

import (
	"strings"
	"testing"
	"time"
)

// A JOB'S DETAIL IS KEPT 72 HOURS UNLESS THE FILE SAYS OTHERWISE, the file may
// say anything from an hour up, and a value it cannot read or shorter than an
// hour is refused when the file is loaded, not when the retention first runs.
func TestJobDetailRetentionDefaultsAndIsCheckedOnLoad(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, err := cfg.Server.JobDetailRetentionDuration(); err != nil || got != 72*time.Hour {
		t.Errorf("absent, the retention is %s (%v), want 72h", got, err)
	}

	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"1h", time.Hour},
		{"168h", 168 * time.Hour},
	} {
		cfg, err := Load(writeConfig(t, withServerKey("job_detail_retention: "+tc.value)))
		if err != nil {
			t.Fatalf("Load with %s: %v", tc.value, err)
		}
		if got, err := cfg.Server.JobDetailRetentionDuration(); err != nil || got != tc.want {
			t.Errorf("%s reads as %s (%v), want %s", tc.value, got, err, tc.want)
		}
	}

	for _, tc := range []struct {
		value, says string
	}{
		{"59m", "shorter than an hour"},
		{"0s", "shorter than an hour"},
		{"3d", "not a duration"},
		{"-72h", "shorter than an hour"},
	} {
		_, err := Load(writeConfig(t, withServerKey("job_detail_retention: "+tc.value)))
		if err == nil || !strings.Contains(err.Error(), "server.job_detail_retention") ||
			!strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s loaded with %v, want refused as %q", tc.value, err, tc.says)
		}
	}
}
