package firecracker

import (
	"fmt"
	"path/filepath"
	"strings"
)

// kernelForGeneration decides which kernel file a launch must boot.
//
// THE GENERATION'S OWN KERNEL WINS OVER THE NODE'S CONFIGURATION, which is what
// makes the pairing an invariant rather than bookkeeping. Recording which kernel a
// generation was verified against means nothing if the launch then uses a
// different one -- and a guest booted with the wrong kernel does not fail to
// start, it fails in the middle of somebody's job, which is the whole reason the
// two are published together.
//
// A GENERATION THAT RECORDS NOTHING FALLS BACK. build-guest-image.sh installs no
// kernel and genuinely does not know which one will be used, so its generations
// arrive here unpaired -- and refusing to launch them would break every deployment
// that builds by hand.
func kernelForGeneration(recorded, kernelDir, configured string) (string, error) {
	recorded = strings.TrimSpace(recorded)

	if recorded == "" {
		if strings.TrimSpace(configured) == "" {
			return "", fmt.Errorf("firecracker: this generation records no kernel and this " +
				"node configures none, so there is nothing to boot it with. Set " +
				"node.firecracker.kernel_image, or pull an image that records its kernel")
		}

		return configured, nil
	}

	// REFUSED RATHER THAN JOINED. The value comes from cluster metadata, which any
	// client with write access to the pool can set, and it is about to become a
	// path this process opens and hands to a VMM. A name that climbs out of the
	// managed directory would have billet boot whatever it pointed at.
	if recorded != filepath.Base(recorded) || recorded == "." || recorded == ".." {
		return "", fmt.Errorf("firecracker: this generation records %q as its kernel, which "+
			"is not a plain file name; refusing to boot a path that could name anything",
			recorded)
	}

	return filepath.Join(kernelDir, recorded), nil
}
