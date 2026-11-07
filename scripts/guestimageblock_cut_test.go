package scripts

import (
	"strings"
	"testing"
)

// A REGION WITH NO END IS REFUSED, not taken to the end of the file: the image would run
// the rest of the agent as though it were this region.
func TestARegionIsCutBetweenItsMarkersOrNotAtAll(t *testing.T) {
	t.Parallel()
	const script = "before\n# R_BEGIN is what it is\nkept\nkept too\n# R_END\nafter\n"
	block, err := cutGuestImageBlock(script, "R")
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if block != "kept\nkept too\n" {
		t.Fatalf("the region = %q", block)
	}
	for name, broken := range map[string]string{
		"no end":          "# R_BEGIN\nkept\n",
		"no begin":        "kept\n# R_END\n",
		"a begin at last": "kept\n# R_BEGIN",
		"an end inline":   "# R_BEGIN\nkept\n# R_END but not alone",
	} {
		if _, err := cutGuestImageBlock(broken, "R"); err == nil || !strings.Contains(err.Error(), "R") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
