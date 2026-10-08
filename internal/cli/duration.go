package cli

import (
	"fmt"
	"time"
)

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

// humanBytes renders a size the way an operator reads one.
func HumanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0

	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
