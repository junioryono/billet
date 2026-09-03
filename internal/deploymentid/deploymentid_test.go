package deploymentid

import (
	"strings"
	"testing"
)

func TestValidateAcceptsOnlyTheIdentityBilletMints(t *testing.T) {
	t.Parallel()

	if err := Validate("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("Validate a minted identity: %v", err)
	}

	for _, id := range []string{
		strings.Repeat("a", Length-1),
		strings.Repeat("a", Length+1),
		strings.Repeat("A", Length),
		strings.Repeat("g", Length),
		strings.Repeat("a", Length-1) + "\x00",
		strings.Repeat("a", Length-1) + "\xff",
	} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()

			if err := Validate(id); err == nil {
				t.Fatalf("Validate accepted identity %q", id)
			}
		})
	}
}
