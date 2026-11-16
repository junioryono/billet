//go:build linux

package node

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	storecontract "github.com/junioryono/billet/internal/store"
)

// syncFilesystem syncfs's the filesystem mounted at target: store.ErrWritesLost
// when the kernel reports a write-back of it that failed since the filesystem was
// mounted and nobody asked yet, an ordinary error when target cannot be opened.
//
// THE SUPERBLOCK KEEPS THE ERROR, AND ONLY SYNCFS ASKS FOR IT. Since Linux 5.8 a
// failed write-back of any file's data is recorded on its filesystem, and syncfs
// reports it to every descriptor opened before the failure, or to the first that
// asks after it; an unmount writes back without asking, so a filesystem whose
// writes were lost unmounts cleanly and checks clean with the data gone. The node
// writes a content-addressed object and an Actions archive through the page cache,
// and every device billet maps fails an OSD request after osd_request_timeout.
func syncFilesystem(target string) error {
	dir, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("node: open %s to write it back: %w", target, err)
	}
	defer func() { _ = dir.Close() }()

	if err := unix.Syncfs(int(dir.Fd())); err != nil {
		return fmt.Errorf("%w: the filesystem at %s: %w", storecontract.ErrWritesLost, target, err)
	}

	return nil
}
