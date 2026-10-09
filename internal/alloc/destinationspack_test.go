package alloc

import (
	"strings"
	"testing"
)

// THE PACKED RECORD IS ONE FIXED-WIDTH ROW PER DESTINATION AND ONE FOR THE
// REST, LAST, at the ordinals the ledger keeps; and a row the record cannot
// hold whole is refused rather than cut, so a destination is never stored
// under part of its address or a count missing its leading digits.
func TestDestinationsArePackedWholeOrNotAtAll(t *testing.T) {
	t.Parallel()

	packed, rows, err := packDestinations(fullDestinations())
	if err != nil {
		t.Fatalf("the most a report can name was refused: %v", err)
	}
	if rows != MaxJobDestinations+1 || len(packed) != rows*packedRowWidth || packedRowWidth != 99 {
		t.Fatalf("packed %d rows into %d characters, want %d rows of 99", rows, len(packed),
			MaxJobDestinations+1)
	}
	if first := packed[:packedRowWidth]; first != "000ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe"+
		strings.Repeat("9223372036854775807", 3) {
		t.Errorf("the first row packs as %q", first)
	}
	zeros := strings.Repeat("0", 17)
	if last := packed[len(packed)-packedRowWidth:]; last != "256"+strings.Repeat(" ", 39)+
		zeros+"07"+zeros+"11"+zeros+"03" {
		t.Errorf("the rest packs as %q", last)
	}

	for name, dest := range map[string]JobDestination{
		"an address too long for the record": {Addr: strings.Repeat("a", packedAddrWidth+1)},
		"an address with a space in it":      {Addr: "10.0.0.1 "},
		"a negative count":                   {Addr: "10.0.0.1", Connections: -1},
	} {
		d := &JobDestinations{Destinations: []JobDestination{dest}}
		if _, _, err := packDestinations(d); err == nil {
			t.Errorf("%s was packed", name)
		}
	}
}
