package ec2

import (
	"bytes"
	"compress/gzip"
	"fmt"
)

// packUserData compresses a provisioning script if it needs to be compressed.
//
// EC2 CAPS USER DATA AT 16 KiB AND PARITY DOES NOT FIT IN IT. The script that
// installs the runner, Docker and a CA already uses about 11 KB; GitHub's declared
// package set, the toolcache installers and the JDKs add roughly nine more. That
// is not a number to shave down — it is twenty-one kilobytes of provisioning
// against a sixteen kilobyte ceiling.
//
// cloud-init DOCUMENTS THIS EXACT ESCAPE: "Content found to be gzip compressed
// will be uncompressed. The uncompressed data will then be used as if it were not
// compressed. This may be useful when user-data size may be limited based on cloud
// platform." Shell compresses about 3x, so the ceiling holds roughly fifty
// kilobytes of script — comfortable room for parity rather than a squeeze.
//
// WHY NOT FETCH A BUNDLE INSTEAD, which was the other candidate: a bootstrap that
// downloads a digest-pinned tarball introduces a host to trust, a network
// dependency at build time, and an artifact whose lifetime has to match the billet
// that builds from it. Compression introduces none of those. The script still
// travels inside the same signed RunInstances call it always did; only its
// encoding changes.
//
// COMPRESSED ONLY WHEN IT HAS TO BE. A script that fits is sent as plain text,
// because a plain-text script is one an operator can read out of the console or
// out of `describe-instance-attribute` while diagnosing a build. Compression buys
// headroom and costs legibility, so it is spent only where it is needed.
func packUserData(script string) ([]byte, error) {
	plain := []byte(script)

	// EMPTY IS REFUSED AT THE SAME BOUNDARY AS TOO LARGE, and for the same reason.
	// Both are user data that cannot provision an image, and both are cheap to
	// catch here and expensive to discover later: EC2 accepts empty user data
	// happily, so the builder launches, boots, does nothing, and never reaches the
	// `poweroff` that signals success -- billet then waits out the whole build
	// timeout on an instance it is paying for, and reports that the guest never
	// stopped rather than that it was never told to do anything.
	//
	// provisionScript cannot currently return empty. That is exactly why the guard
	// belongs here: this is the last place both a present and a future caller pass
	// through, and the invariant it defends is "nothing reaches RunInstances that
	// cannot finish a build".
	if len(bytes.TrimSpace(plain)) == 0 {
		return nil, fmt.Errorf("ec2: the provisioning script is empty; a builder launched with " +
			"no user data boots, does nothing, and never signals success")
	}

	if len(plain) <= maxUserData {
		return plain, nil
	}

	var buf bytes.Buffer

	// BestCompression, BECAUSE THIS RUNS ONCE PER AMI BUILD. The difference
	// between fast and best is milliseconds here and kilobytes of headroom in a
	// budget that has already been the binding constraint once.
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("ec2: compress the provisioning script: %w", err)
	}

	if _, err := zw.Write(plain); err != nil {
		return nil, fmt.Errorf("ec2: compress the provisioning script: %w", err)
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("ec2: compress the provisioning script: %w", err)
	}

	// STILL CHECKED AFTER COMPRESSING. Compression is headroom, not an exemption:
	// a script that does not fit even compressed has to fail here, before a paid
	// builder exists, rather than at RunInstances with a parameter error that says
	// nothing about which script or why.
	if buf.Len() > maxUserData {
		return nil, fmt.Errorf("ec2: the provisioning script is %d bytes and %d compressed, "+
			"both over EC2's %d limit for user data. Set PayloadBucket so the shared "+
			"installers are staged in S3 and fetched by a bootstrap; embedding them is "+
			"only viable while the declaration is small enough to compress into what "+
			"EC2 will carry",
			len(plain), buf.Len(), maxUserData)
	}

	return buf.Bytes(), nil
}
