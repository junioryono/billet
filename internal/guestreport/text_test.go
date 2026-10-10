package guestreport

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// CLEAN MAKES ANY TEXT ONE THE DECODER ADMITS: an agent that cleans what it read
// never has a batch refused for a name.
func TestCleanMakesTextTheDecoderAdmits(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		in    string
		limit int
		want  string
	}{
		{"go", MaxNameBytes, "go"},
		{"kworker/0:1H", MaxNameBytes, "kworker/0:1H"},
		{"bad\xffbyte", MaxNameBytes, "bad?byte"},
		{"esc\x1b[2Jape", MaxNameBytes, "esc?[2Jape"},
		{"c1\u009bcsi", MaxNameBytes, "c1?csi"},
		{"tab\there\nnewline", MaxNameBytes, "tab?here?newline"},
		{"it's 'quoted'", MaxNameBytes, "it's 'quoted'"},
		{strings.Repeat("a", 70), MaxNameBytes, strings.Repeat("a", 64)},
		// A character that would straddle the bound is left out whole.
		{strings.Repeat("a", 63) + "é", MaxNameBytes, strings.Repeat("a", 63)},
		{strings.Repeat("a", 62) + "é", MaxNameBytes, strings.Repeat("a", 62) + "é"},
		{strings.Repeat("ü", 150), MaxFailedNameBytes, strings.Repeat("ü", 100)},
	} {
		got := Clean(c.in, c.limit)
		if got != c.want {
			t.Errorf("Clean(%q, %d) = %q, want %q", c.in, c.limit, got, c.want)
		}

		if !utf8.ValidString(got) || checkText(got, c.limit, "name") != nil {
			t.Errorf("Clean(%q, %d) = %q, which the decoder refuses", c.in, c.limit, got)
		}
	}
}
