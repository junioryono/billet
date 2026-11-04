package cli

import (
	"testing"
	"time"
)

// A SPAN IS DAYS OR A GO DURATION AND IS POSITIVE; ANYTHING ELSE IS REFUSED, not read as
// nothing, which would report every job or none.
func TestASpanIsDaysOrADuration(t *testing.T) {
	t.Parallel()

	for text, want := range map[string]time.Duration{
		"14d": 14 * 24 * time.Hour, "1d": 24 * time.Hour, "36h": 36 * time.Hour, "90m": 90 * time.Minute,
	} {
		if got, err := ParseSpan(text); err != nil || got != want {
			t.Errorf("%q = %s, %v; want %s", text, got, err, want)
		}
	}
	for _, text := range []string{"", "soon", "d", "1.5d", "-1d", "0d", "0s", "-3h", "99999999999d"} {
		if got, err := ParseSpan(text); err == nil {
			t.Errorf("%q = %s, want refused", text, got)
		}
	}
}
