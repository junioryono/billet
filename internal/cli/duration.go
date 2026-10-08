package cli

import "time"

// ShortDuration renders a duration the way an operator typed it, so a report
// names the window it actually looked at.
//
// NEVER ROUNDED TO ZERO. `--since 1ns` reporting "the last 0s" tells an operator
// billet looked at no time at all, when what it did was look at the window they
// asked for and find nothing — two different answers.
func ShortDuration(d time.Duration) string {
	rounded := d.Round(time.Second)
	if d >= time.Hour {
		rounded = d.Round(time.Minute)
	}

	if rounded == 0 {
		return d.String()
	}

	return rounded.String()
}
