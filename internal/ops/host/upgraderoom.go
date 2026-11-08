package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/config"
)

// upgradeRoomMargin is what a transaction writes under the upgrade root besides
// the ledger's snapshot: the candidate's archive and binary, the preserved
// binary, units and configuration, and the journal. A release's archive is
// about 17 MiB and its binary about 47 MiB (v0.34.0, linux/arm64), so this holds
// several times what one transaction stages.
const upgradeRoomMargin = 512 << 20

// freeBytes answers how many bytes an unprivileged writer may still use on the
// filesystem holding path; a variable so a test can stand a short disk in.
var freeBytes = func(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}

	// IN FLOATING POINT, as the node's own readings are: the two fields differ in
	// type between Linux and darwin, and a product of free blocks and block size
	// is far inside a float64's exact range for any disk.
	return int64(float64(st.Bavail) * float64(st.Bsize)), nil
}

// refuseShortOfRoom is the upgrade's preflight of the disk it is about to fill:
// a refusal before the claim, answered to the node as any other preflight
// refusal is, so the rollout backs off and asks again while nothing on the host
// has changed.
//
// THE SNAPSHOT IS WHAT FILLED IT. On 2026-11-08 a controller's 15 GiB root held
// seven local backups and three committed recovery directories, and the v0.34.0
// upgrade stopped both services, hid the binary and fenced the ledger before its
// snapshot ran out of room at 671 MiB of 988 MiB; the rollback could not write its
// own journal, so the host was cordoned with the control plane down (#802). The
// snapshot is written into the recovery directory under upgradeRoot, so that
// filesystem must hold the ledger's whole size, as it stands, and the margin.
//
// A READING THAT FAILS REFUSES: could not tell is not room.
func refuseShortOfRoom(cfg *config.Config) error {
	need := int64(upgradeRoomMargin)

	ledger, err := sqliteLedgerBytes(cfg)
	if err != nil {
		return fmt.Errorf("the upgrade is refused before anything stops: could not tell how large the "+
			"ledger is, so not whether %s can hold its snapshot: %w", upgradeRoot, err)
	}

	need += ledger

	free, err := freeBytes(upgradeRoot)
	if err != nil {
		return fmt.Errorf("the upgrade is refused before anything stops: could not read how much room "+
			"%s has: %w", upgradeRoot, err)
	}

	if free < need {
		return fmt.Errorf("the upgrade is refused before anything stops: %s has %s free, and the transaction "+
			"needs %s there (the ledger's snapshot, %s, and %s for the binaries and the journal); free room "+
			"on that filesystem, for example local backups the bucket holds, and the rollout asks again",
			upgradeRoot, mib(free), mib(need), mib(ledger), mib(upgradeRoomMargin))
	}

	return nil
}

// sqliteLedgerBytes is how large the SQLite ledger the snapshot copies is: the
// database and its write-ahead log. A host with no server, or one whose ledger
// is PostgreSQL, snapshots nothing here.
func sqliteLedgerBytes(cfg *config.Config) (int64, error) {
	if cfg.Server == nil || ledgerKindFor(cfg) != "" {
		return 0, nil
	}

	db := filepath.Join(cfg.Server.IdentityDir, "billet.db")

	info, err := os.Stat(db)
	if err != nil {
		return 0, err
	}

	size := info.Size()

	wal, err := os.Stat(db + "-wal")
	switch {
	case err == nil:
		size += wal.Size()
	case !errors.Is(err, os.ErrNotExist):
		return 0, err
	}

	return size, nil
}

// mib renders a byte count for an operator.
func mib(n int64) string {
	return fmt.Sprintf("%d MiB", n>>20)
}
