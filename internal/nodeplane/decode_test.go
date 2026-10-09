package nodeplane

import (
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
)

// A BODY IS ONE JSON VALUE AND NOTHING AFTER IT. The decoder stops at the end
// of the first value, so without the check a second value, or anything else
// after the first, was accepted with the rest unread. Whitespace after the
// value, a newline from an encoder included, is not anything.
func TestABodyIsOneValueAndNothingAfterIt(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		body string
		ok   bool
	}{
		{`{}`, true},
		{"{}\n", true},
		{"{} \t\r\n", true},
		{`{}{}`, false},
		{`{} {"Node":"x"}`, false},
		{`{} x`, false},
		{`{}]`, false},
		{`{} 0`, false},
	} {
		if got := strictly(t.Context(), []byte(c.body), new(nodeapi.HeartbeatRequest), maxBody); got != c.ok {
			t.Errorf("the plane accepted %q: %v, want %v", c.body, got, c.ok)
		}
	}
}
