package retirement

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/junioryono/billet/internal/regularfile"
)

// Phase is a retirement journal's phase, in order. The status file publishes
// the one the journal holds so a reader without the journal can decide whether
// the authority is closed.
type Phase string

const (
	PhaseIntent          Phase = "intent"
	PhaseStopped         Phase = "stopped"
	PhaseArchived        Phase = "archived"
	PhaseConfigRewritten Phase = "config-rewritten"
	PhaseNodeRestarted   Phase = "node-restarted"
	PhaseDone            Phase = "done"
)

// phaseOrder is the one ordering every comparison uses.
var phaseOrder = map[Phase]int{
	PhaseIntent: 1, PhaseStopped: 2, PhaseArchived: 3, PhaseConfigRewritten: 4,
	PhaseNodeRestarted: 5, PhaseDone: 6,
}

// Known reports whether p is a phase this binary knows.
func (p Phase) Known() bool { _, ok := phaseOrder[p]; return ok }

// Closed reports whether the authority is closed at this phase: from `stopped`
// on, the identity directory is about to move or has moved, and an ordinary
// authority writer must not open it.
func (p Phase) Closed() bool { return phaseOrder[p] >= phaseOrder[PhaseStopped] }

// Variant names what a retirement leaves behind on the host.
type Variant string

const (
	VariantServerOnly   Variant = "server-only"
	VariantRetainedNode Variant = "retained-node"
)

// Status is the published phase.
type Status struct {
	Phase     Phase   `json:"phase"`
	Variant   Variant `json:"variant"`
	UpdatedAt string  `json:"updated_at"`
}

// StatusPresence is the four-valued answer a status read gives: a failed read
// is never absence, and a file that does not parse is neither.
type StatusPresence int

const (
	StatusAbsent StatusPresence = iota
	StatusPresent
	StatusUnreadable
	StatusMalformed
)

// maxStatusBytes bounds a read of the status file.
const maxStatusBytes = 4096

// ReadStatus reads the published status, typed. A LINK IS NOT ABSENCE, even
// when its target is gone; only the file at the publication's own name counts.
func ReadStatus() (Status, StatusPresence, error) {
	raw, err := regularfile.ReadFile(StatusPath(), maxStatusBytes, regularfile.Options{NoFollow: true})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, StatusAbsent, nil
		}

		return Status{}, StatusUnreadable, fmt.Errorf("retirement: read %s: %w", StatusPath(), err)
	}

	var st Status
	if err := strictDecode(raw, &st); err != nil {
		return Status{}, StatusMalformed, fmt.Errorf("retirement: %s: %w", StatusPath(), err)
	}

	if !st.Phase.Known() {
		return Status{}, StatusMalformed, fmt.Errorf("retirement: %s names the phase %q, which this billet does not know", StatusPath(), st.Phase)
	}

	if st.Variant != VariantServerOnly && st.Variant != VariantRetainedNode {
		return Status{}, StatusMalformed, fmt.Errorf("retirement: %s names the variant %q, which this billet does not know", StatusPath(), st.Variant)
	}

	if _, err := time.Parse(time.RFC3339, st.UpdatedAt); err != nil {
		return Status{}, StatusMalformed, fmt.Errorf("retirement: %s carries an unparseable updated_at: %w", StatusPath(), err)
	}

	return st, StatusPresent, nil
}

// WriteStatus publishes a phase. Root only; world-readable by design, since
// the service accounts read it to learn the authority is closed.
func WriteStatus(phase Phase, variant Variant, now time.Time) error {
	if !phase.Known() {
		return fmt.Errorf("retirement: refuse to publish the unknown phase %q", phase)
	}

	if variant != VariantServerOnly && variant != VariantRetainedNode {
		return fmt.Errorf("retirement: refuse to publish the unknown variant %q", variant)
	}

	body, err := json.Marshal(Status{Phase: phase, Variant: variant, UpdatedAt: now.UTC().Format(time.RFC3339)})
	if err != nil {
		return fmt.Errorf("retirement: encode the status: %w", err)
	}

	return publish(StatusPath(), append(body, '\n'), 0o644)
}
