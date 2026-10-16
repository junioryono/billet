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

// ReadRecord reads the record: absent is a value (false and no error), and any
// other failure to read it, or a record naming no interface, is an error. A
// record that could not be read is never "no record": a clear that read it so
// would leave live shaping behind and say there was none.
func ReadRecord() (Record, bool, error) {
	body, err := os.ReadFile(StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}

	if err != nil {
		return Record{}, false, fmt.Errorf("read the uplink's record %s: %w", StateFile, err)
	}

	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return Record{}, false, fmt.Errorf("the uplink's record %s names no interface", StateFile)
	}

	r := Record{Iface: fields[0], IFB: (&Shaper{Iface: fields[0]}).IFB()}
	if len(fields) > 1 {
		r.Index = fields[1]
	}

	if len(fields) > 2 {
		r.IFB = fields[2]
	}

	return r, true, nil
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
//
// It returns the interface's current name, whether it is gone, and an error
// when the lookup could not tell.
func (r Record) Resolve() (string, bool, error) {
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
// is now, and then the record, and returns the record and whether there was
// one. With no record it does nothing: nothing proves what is on any interface
// is billet's.
func clearRecorded(ctx context.Context,
	run func(ctx context.Context, argv ...string) (string, error),
) (Record, bool, error) {
	r, ok, err := ReadRecord()
	if err != nil || !ok {
		return Record{}, false, err
	}

	// UNDECIDED KEEPS EVERYTHING, the record included, for a later try.
	current, gone, err := r.Resolve()
	if err != nil {
		return r, true, fmt.Errorf("find the interface recorded as %s: %w", r.Iface, err)
	}

	if gone {
		// ONLY THE DEVICE IS LEFT: the interface took its qdiscs, and with them
		// the redirect that pointed at the device, so removing it strands nothing.
		err = (&Shaper{IFBName: r.IFB, Owned: true, Run: run}).clearDevice(ctx)
	} else {
		err = (&Shaper{Iface: current, IFBName: r.IFB, Owned: true, Run: run}).Clear(ctx)
	}

	if err != nil {
		return r, true, fmt.Errorf("remove the shaping recorded on %s: %w", r.Iface, err)
	}

	return r, true, forgetRecord()
}

// ErrUnrecorded is shaping of the kind billet installs found on an interface
// with no record that billet installed it: a clear cannot tell whose it is, so
// it removes nothing and says so, never "nothing to clear".
var ErrUnrecorded = errors.New("shaping of the kind billet installs, and no record of billet's")

// Cleared is what a clear found and did.
type Cleared struct {
	// Record is what was recorded and has been removed, when Recorded.
	Record   Record
	Recorded bool
	// Looked is the interface a clear with no record inspected and found
	// carrying none of the shaping billet installs.
	Looked string
}

// ClearRecorded removes what the record says a run installed, for the unit's
// ExecStopPost and an operator. With no record it removes nothing and inspects
// iface, the default route's interface when empty, so that "nothing to clear"
// is something it saw: CAKE, an ingress qdisc or the shaper's device there is
// ErrUnrecorded, and an interface it could not inspect is an error.
//
// A SHAPER CLEARS ITS OWN SHAPING AS IT STOPS and forgets the record, so the
// unit's ExecStopPost after an ordinary stop finds none; the inspection is what
// tells that from a record lost while the shaping stayed.
func ClearRecorded(ctx context.Context, iface string) (Cleared, error) {
	return clearOrInspect(ctx, iface, nil)
}

func clearOrInspect(ctx context.Context, iface string,
	run func(ctx context.Context, argv ...string) (string, error),
) (Cleared, error) {
	r, recorded, err := clearRecorded(ctx, run)
	if err != nil || recorded {
		return Cleared{Record: r, Recorded: recorded}, err
	}

	if iface == "" {
		found, err := DefaultInterface()
		if err != nil {
			return Cleared{}, fmt.Errorf("no record of shaping on this host, and no interface to inspect "+
				"for shaping without one (name one with --interface): %w", err)
		}

		iface = found
	}

	// AN INTERFACE THAT IS NOT THERE WAS NOT INSPECTED: inspect reads tc's
	// "Cannot find device" as no qdiscs, which is right for a recorded interface
	// that has gone and would be "carries nothing" about a name that is wrong.
	if _, err := os.Stat(filepath.Join(sysNet, iface)); err != nil {
		return Cleared{}, fmt.Errorf("no record of shaping on this host, and %s could not be inspected "+
			"for shaping without one: %w", iface, err)
	}

	s := &Shaper{Iface: iface, Run: run}

	st, err := s.inspect(ctx)
	if err != nil {
		return Cleared{}, fmt.Errorf("no record of shaping on this host, and %s could not be inspected "+
			"for shaping without one: %w", iface, err)
	}

	// THE REMEDY NAMES ONLY WHAT WAS SEEN, in the order a shaper's own clear
	// removes it: the redirect before the device it points at, and a root that
	// is not CAKE never.
	var found, remove []string

	if st.ingress {
		found = append(found, "an ingress qdisc")
		remove = append(remove, "tc qdisc del dev "+iface+" handle ffff: ingress")
	}

	if st.root == "cake" {
		found = append(found, "a CAKE root qdisc")
		remove = append(remove, "tc qdisc del dev "+iface+" root")
	}

	if st.ifb {
		found = append(found, "the device "+s.IFB())
		remove = append(remove, "ip link del "+s.IFB())
	}

	if len(found) > 0 {
		return Cleared{}, fmt.Errorf("%w: %s carries %s and no record says billet installed it, so nothing "+
			"was removed; if you know a shaper of billet's left it, remove it by hand, each only once the "+
			"one before it succeeded: %s",
			ErrUnrecorded, iface, strings.Join(found, ", "), strings.Join(remove, ", then "))
	}

	return Cleared{Looked: iface}, nil
}
