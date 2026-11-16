package ceph

import (
	"context"
	"fmt"
	"os"

	storecontract "github.com/junioryono/billet/internal/store"
	"github.com/junioryono/billet/internal/store/blockfs"
)

// verifyWrittenBack is the filesystem verifier a client uses unless a test gives
// it another: the device's write-back is asked for first, then its filesystem is
// checked.
//
// A FILESYSTEM CHECK CANNOT SEE WRITES THAT NEVER LANDED. Every device billet maps
// fails an OSD request after mapRequestTimeout, and a failed write-back leaves its
// pages clean, so what the guest or the node wrote there is gone while the
// filesystem can still check clean. The kernel reports it to the first fsync that
// asks after it, and nothing in a guest's VMM or a host unmount asks; so a candidate
// is published only after this fsync answers nothing.
func verifyWrittenBack(ctx context.Context, device string) (storecontract.Filesystem, error) {
	if err := syncDevice(device); err != nil {
		return storecontract.Filesystem{}, err
	}

	return blockfs.VerifyExt4(ctx, device)
}

// deviceSyncer is the descriptor syncDevice asks.
type deviceSyncer interface {
	Sync() error
	Close() error
}

func syncDevice(device string) error {
	return syncDeviceWith(device, func(name string) (deviceSyncer, error) {
		return os.OpenFile(name, os.O_RDONLY, 0)
	})
}

// syncDeviceWith fsyncs device through open: storecontract.ErrWritesLost when the
// fsync fails, and an ordinary error, which proves nothing either way, when the
// open does.
func syncDeviceWith(device string, open func(string) (deviceSyncer, error)) error {
	disk, err := open(device)
	if err != nil {
		return fmt.Errorf("ceph: open %s to ask for its write-back: %w", device, err)
	}
	defer func() { _ = disk.Close() }()

	if err := disk.Sync(); err != nil {
		return fmt.Errorf("%w: %s: %w", storecontract.ErrWritesLost, device, err)
	}

	return nil
}
