package initconfig

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// THE GENERATION FINGERPRINT BINDS THE RENDERS TO THE ROOT THAT WROTE THE
// OUTPUTS (#62). The plan render mints it, the generated root declares it as an
// output, and every later render carries it on its command line and compares it
// with what `terraform output -json` says. A name and a region cannot do that:
// two generations sharing both, in different accounts, are otherwise
// indistinguishable.
//
// Sixty-four random bits in four groups, because a person reads it in the
// runbook and retypes or pastes it; the groups make a transposition visible.

// hybridGenerationPattern is the one shape a fingerprint has.
var hybridGenerationPattern = regexp.MustCompile(`^[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}$`)

// NewHybridGeneration mints a fingerprint.
func NewHybridGeneration() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint the generation fingerprint: %w", err)
	}

	h := hex.EncodeToString(b[:])

	return h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16], nil
}

// CheckHybridGeneration refuses anything that is not a fingerprint.
func CheckHybridGeneration(g string) error {
	if !hybridGenerationPattern.MatchString(g) {
		return fmt.Errorf("--generation %q is not a generation fingerprint (four groups of four "+
			"lowercase hex digits, as the runbook prints it)", g)
	}

	return nil
}
