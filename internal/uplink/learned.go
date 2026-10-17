package uplink

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// learnedHalfLife is how fast a resumed rate returns toward its ceiling with
// the time the shaper was down: a restart of seconds keeps what was learned,
// one of an hour keeps half of the cut, and one of a day keeps nothing.
//
// A RATE WAS LEARNED FROM THE LINE AS IT WAS. A shaper that runs does not let
// it rise without the line proving it can carry more, so while it runs it never
// decays; the longer one was down, the less its last word says about the line
// now, and the ceiling is what a run with nothing learned starts at.
const learnedHalfLife = time.Hour

// learned is the rates a run of the shaper last set, kept under the unit's
// StateDirectory: a restart of the unit, which every upgrade causes, finds it,
// and so does the next boot, the decay counting the time the host was down.
// Before it, every restart started each direction at the ceiling again, and a
// gateway that drops rather than queues went unprotected until the line was
// cut back down (#493). Boot is the kernel's boot ID when they were set, which
// says whether the interface's index is still comparable.
type learned struct {
	Iface string      `json:"interface"`
	Index string      `json:"index,omitempty"`
	Boot  string      `json:"boot,omitempty"`
	Up    learnedRate `json:"up"`
	Down  learnedRate `json:"down"`
	At    time.Time   `json:"at"`
}

// learnedRate is one direction's rate and the ceiling it was learned under.
type learnedRate struct {
	Max  float64 `json:"max_mbit"`
	Rate float64 `json:"rate_mbit"`
}

// LearnedFile is where the rates are kept: under the unit's StateDirectory,
// not beside the interface record in its RuntimeDirectory, which a reboot
// empties along with the shaping the record describes. A clear never removes
// it; it is knowledge about the line, not shaping.
var LearnedFile = "/var/lib/billet-uplink/learned"

func learnedFile() string { return LearnedFile }

// bootIDFile is the kernel's identity for this boot, which changes at every
// boot and at no other time.
var bootIDFile = "/proc/sys/kernel/random/boot_id"

// bootID is this boot's identity, or empty when it cannot be read, which
// leaves the index compared as within one boot.
func bootID() string {
	body, err := os.ReadFile(bootIDFile)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(body))
}

// writeLearned publishes the rates by rename, so a reader never finds half of
// them.
func writeLearned(l learned) error {
	body, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encode the learned rates: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(learnedFile()), 0o755); err != nil {
		return fmt.Errorf("record the learned rates: %w", err)
	}

	next := learnedFile() + ".next"

	//nolint:gosec // G306: two rates and an interface name for root's own shaper, which any user may read
	if err := os.WriteFile(next, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("record the learned rates: %w", err)
	}

	if err := os.Rename(next, learnedFile()); err != nil {
		return fmt.Errorf("record the learned rates: %w", err)
	}

	return nil
}

// readLearned reads the rates: absent is false and no error, and anything it
// cannot read or decode is an error, which the caller answers by starting at
// the ceiling, as a run with nothing learned does.
func readLearned() (learned, bool, error) {
	body, err := os.ReadFile(learnedFile())
	if errors.Is(err, os.ErrNotExist) {
		return learned{}, false, nil
	}

	if err != nil {
		return learned{}, false, fmt.Errorf("read the learned rates %s: %w", learnedFile(), err)
	}

	var l learned
	if err := json.Unmarshal(body, &l); err != nil {
		return learned{}, false, fmt.Errorf("the learned rates %s are not what a shaper writes: %w",
			learnedFile(), err)
	}

	return l, true, nil
}

// resumes reports whether the rates were learned on this interface, and when
// not, why: a record of another interface describes another line, and one from
// a clock ahead of this one cannot say how long the shaper was down.
//
// THE INDEX IS COMPARED ONLY WITHIN ONE BOOT. Within it, an index that changed
// under the same name is another device; but a boot renumbers interfaces, so a
// record from an earlier boot is matched by name, and a boot ID that cannot be
// read on either side leaves the index compared.
func (l learned) resumes(iface, index, boot string, now time.Time) (bool, string) {
	rebooted := l.Boot != "" && boot != "" && l.Boot != boot

	switch {
	case rebooted && l.Iface != iface:
		return false, fmt.Sprintf("they were learned on %s, not %s", l.Iface, iface)
	case !rebooted && l.Index != "" && index != "" && l.Index != index:
		return false, fmt.Sprintf("they were learned on the interface with index %s, not %s's %s", l.Index, iface, index)
	case (l.Index == "" || index == "") && l.Iface != iface:
		return false, fmt.Sprintf("they were learned on %s, not %s", l.Iface, iface)
	case l.At.IsZero() || l.At.After(now):
		return false, "they carry no time this clock has reached"
	}

	return true, ""
}

// resume is the rate a direction starts at after the shaper was down for down,
// under ceiling: what was learned, returning toward the ceiling with
// learnedHalfLife, and never above the ceiling or below the floor. A rate at
// the ceiling it was learned under is nothing learned, so a raised ceiling
// starts at itself.
func (r learnedRate) resume(ceiling float64, down time.Duration) float64 {
	if !(r.Rate > 0) || !(r.Max > 0) || math.IsInf(r.Rate, 0) || r.Rate >= r.Max {
		return ceiling
	}

	return clamp(ceiling-(ceiling-r.Rate)*math.Exp2(-down.Hours()/learnedHalfLife.Hours()), ceiling)
}

// resumeLearned starts ctl's directions at what an earlier run learned on this
// interface, and says so; a record it cannot use leaves them at their ceilings,
// and says why.
func resumeLearned(log *slog.Logger, ctl *Controller, iface, index, boot string, now time.Time) {
	l, found, err := readLearned()
	if err != nil {
		log.Warn("starting each direction at its ceiling: the rates an earlier run learned could not be read",
			"error", err)

		return
	}

	if !found {
		return
	}

	if ok, why := l.resumes(iface, index, boot, now); !ok {
		log.Info("starting each direction at its ceiling rather than at the rates an earlier run learned: "+why,
			"interface", iface)

		return
	}

	down := now.Sub(l.At)
	ctl.Up.Rate = l.Up.resume(ctl.Up.Max, down)
	ctl.Down.Rate = l.Down.resume(ctl.Down.Max, down)

	log.Info("resuming the rates an earlier run learned, returned toward the ceiling for the time it was down",
		"up_mbit", round(ctl.Up.Rate), "down_mbit", round(ctl.Down.Rate),
		"learned_up_mbit", round(l.Up.Rate), "learned_down_mbit", round(l.Down.Rate),
		"down_for", down.Round(time.Second))
}
