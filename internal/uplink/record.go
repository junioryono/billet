package uplink

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StateFile records what a shaper installed: the interface by name and by the
// kernel's index, and the IFB device it made. It lives under the unit's
// RuntimeDirectory, which a reboot empties along with everything the shaper
// installed. It is the only proof that what is on an interface is billet's.
var StateFile = "/run/billet-uplink/interface"

// Record is what one run of the shaper installed.
type Record struct {
	Iface, Index, IFB string
}

// ReadRecord reads the record, if there is one.
func ReadRecord() (Record, bool) {
	body, err := os.ReadFile(StateFile)
	if err != nil {
		return Record{}, false
	}

	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return Record{}, false
	}

	r := Record{Iface: fields[0], IFB: (&Shaper{Iface: fields[0]}).IFB()}
	if len(fields) > 1 {
		r.Index = fields[1]
	}

	if len(fields) > 2 {
		r.IFB = fields[2]
	}

	return r, true
}

// writeRecord publishes a record by rename, so the only proof of ownership is
// never truncated and then lost to a failure before it is written again.
func writeRecord(r Record) error {
	if err := os.MkdirAll(filepath.Dir(StateFile), 0o755); err != nil {
		return fmt.Errorf("record the shaped interface: %w", err)
	}

	next := StateFile + ".next"
	line := strings.Join([]string{r.Iface, r.Index, r.IFB}, " ")

	//nolint:gosec // G306: interface names for root's own cleanup, which any user may read
	if err := os.WriteFile(next, []byte(line+"\n"), 0o644); err != nil {
		return fmt.Errorf("record the shaped interface: %w", err)
	}

	if err := os.Rename(next, StateFile); err != nil {
		return fmt.Errorf("record the shaped interface: %w", err)
	}

	return nil
}

func forgetRecord() error {
	if err := os.Remove(StateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("forget the shaped interface: %w", err)
	}

	return nil
}

// Resolve finds the recorded interface as it is now, BY ITS INDEX: a device
// that has taken the recorded name since is somebody else's, and one renamed
// since is still this one. gone means no interface has the index any more, and
// its qdiscs went with it. A record with no index (written before indexes were)
// is taken by name. AN ERROR IS A LOOKUP THAT COULD NOT TELL, never gone:
// gone permits removing the device a redirect on the interface may still use.
func (r Record) Resolve() (current string, gone bool, err error) {
	if r.Index == "" {
		_, err := os.Stat(filepath.Join(sysNet, r.Iface))

		switch {
		case err == nil:
			return r.Iface, false, nil
		case errors.Is(err, os.ErrNotExist):
			return "", true, nil
		}

		return "", false, fmt.Errorf("%w: %w", errUndecided, err)
	}

	name, found, err := nameForIndex(r.Index)
	if err != nil {
		return "", false, err
	}

	return name, !found, nil
}

// clearRecorded removes everything the record says a run installed, wherever it
// is now, and then the record. With no record it does nothing: nothing proves
// what is on any interface is billet's.
func clearRecorded(ctx context.Context, run func(ctx context.Context, argv ...string) (string, error)) error {
	r, ok := ReadRecord()
	if !ok {
		return nil
	}

	// UNDECIDED KEEPS EVERYTHING, the record included, for a later try.
	current, gone, err := r.Resolve()
	if err != nil {
		return fmt.Errorf("find the interface recorded as %s: %w", r.Iface, err)
	}

	if gone {
		// ONLY THE DEVICE IS LEFT: the interface took its qdiscs, and with them
		// the redirect that pointed at the device, so removing it strands nothing.
		err = (&Shaper{IFBName: r.IFB, Owned: true, Run: run}).clearDevice(ctx)
	} else {
		err = (&Shaper{Iface: current, IFBName: r.IFB, Owned: true, Run: run}).Clear(ctx)
	}

	if err != nil {
		return fmt.Errorf("remove the shaping recorded on %s: %w", r.Iface, err)
	}

	return forgetRecord()
}

// ClearRecorded removes what the record says a run installed, for the unit's
// ExecStopPost and an operator.
func ClearRecorded(ctx context.Context) (Record, error) {
	r, _ := ReadRecord()

	return r, clearRecorded(ctx, nil)
}
