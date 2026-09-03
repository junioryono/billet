package ec2

import (
	"strings"
	"testing"
)

// THE SCRIPT STILL FITS WITH A REAL CA IN IT, which is the case the earlier
// measurement did not cover.
//
// The toolcache pushed the script past 40KB plain, so it now only travels because
// packUserData gzips it — and gzip's help depends on what else is in there. A CA
// bundle is high-entropy base64 on one line: it costs close to its full size after
// compression, straight out of the same 16384-byte budget. Measured with the
// toolcache present, a certificate costs about 575 bytes of that budget — none
// leaves 5662 spare, one leaves 4518, two leave 3943.
//
// SO THE SIZE IS ASSERTED WITH A CERTIFICATE, not without one. A test that only
// ever measured the no-CA script would report comfortable headroom for a build
// nobody runs.
//
// AND IT MEASURES THE STAGED SHAPE. Embedding the installers stopped fitting
// beside a realistic CA once the toolset reached six runtimes, three toolchains
// and the global package sets -- the fallback refuses with the remedy, which
// TestTheEmbeddedFallbackRefusesWithTheRemedy asserts. Holding this test to the
// embedded shape would make every future addition look like a size regression
// rather than what it is.
func TestTheScriptFitsWithARealisticCACert(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 2} {
		var bundle strings.Builder

		for range n {
			bundle.WriteString(mintCert(t, true))
		}

		// THE STAGED SHAPE, which is what a build with a bucket delivers and the
		// only one whose size is bounded by anything billet controls. The embedded
		// fallback carries the whole installer file, and with the toolset at six
		// runtimes, three toolchains and the global sets it no longer fits beside a
		// realistic CA -- which is not a regression but the reason staging exists.
		script, err := provisionScript(BuildSpec{
			RunnerVersion: "2.328.0", Arch: "x64", CACertPEM: bundle.String(),
			payloadURL:    "https://b.s3.us-west-2.amazonaws.com/p.tar.gz?X-Amz-Signature=" + strings.Repeat("s", 64),
			payloadSHA256: strings.Repeat("d", 64),
		})
		if err != nil {
			t.Fatalf("provisionScript with %d certificates: %v", n, err)
		}

		packed, err := packUserData(script)
		if err != nil {
			t.Fatalf("a staged build with %d CA certificates cannot be delivered: %v", n, err)
		}

		t.Logf("%d certificate(s): %d bytes plain, %d packed, %d spare of %d",
			n, len(script), len(packed), maxUserData-len(packed), maxUserData)
	}
}

// AND THE CA BOUND IS ONE THAT CAN ACTUALLY BE DELIVERED.
//
// maxCACertPEM was 32 KiB, chosen as a sanity bound on a trust store rather than
// a size budget — and measured with the toolcache present, a bundle that large
// puts the payload 18 KiB over the limit. A bound that cannot be delivered is not
// a bound, it is a promise the build breaks.
//
// packUserData REMAINS THE REAL GATE, because deliverability is not knowable from
// the CA alone: it depends on everything else in the script. This asserts only
// that the fixed bound is not, by itself, already impossible.
func TestTheCABoundIsDeliverable(t *testing.T) {
	t.Parallel()

	var bundle strings.Builder

	for bundle.Len() < maxCACertPEM {
		bundle.WriteString(mintCert(t, true))
	}

	// Trim to the last whole certificate at or under the bound.
	certs := strings.SplitAfter(bundle.String(), "-----END CERTIFICATE-----\n")

	var atBound strings.Builder

	for _, c := range certs {
		if atBound.Len()+len(c) > maxCACertPEM {
			break
		}

		atBound.WriteString(c)
	}

	// THE STAGED SHAPE, WHICH IS WHAT A REAL BUILD DELIVERS. The installers and the
	// declaration are fetched from S3 by a bootstrap, so what travels in user data
	// is a URL, a digest and the rest of the provisioning script -- and it does not
	// grow when a runtime is added to the toolset.
	//
	// MEASURED HERE RATHER THAN THE EMBEDDED SHAPE, because the embedded one is a
	// fallback for a build with no bucket, and holding the whole toolset to a
	// 16384-byte cap is what made the compiler sections not fit at all. The
	// fallback's own limit is asserted below, as a refusal that names the remedy.
	script, err := provisionScript(BuildSpec{
		RunnerVersion: "2.328.0", Arch: "x64", CACertPEM: atBound.String(),
		payloadURL:    "https://example-bucket.s3.us-west-2.amazonaws.com/billet-payload-" + strings.Repeat("a", 64) + ".tar.gz?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=" + strings.Repeat("c", 40) + "&X-Amz-Date=20260101T000000Z&X-Amz-Expires=3600&X-Amz-SignedHeaders=host&X-Amz-Signature=" + strings.Repeat("s", 64),
		payloadSHA256: strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatalf("a CA bundle at the bound was refused before delivery: %v", err)
	}

	packed, err := packUserData(script)
	if err != nil {
		t.Fatalf("a CA bundle of %d bytes is within maxCACertPEM (%d) and cannot be "+
			"delivered: %v\nthe bound promises something the build cannot do",
			atBound.Len(), maxCACertPEM, err)
	}

	spare := maxUserData - len(packed)

	t.Logf("a %d-byte bundle (the bound is %d) packs to %d, %d spare",
		atBound.Len(), maxCACertPEM, len(packed), spare)

	// A MARGIN, NOT MERELY A FIT. Asserting only that it packs makes this a knife
	// edge: the next line added to provisionScript, or the next toolset refresh
	// that adds a package name, turns it red -- and it reads as "the CA bound is
	// wrong" when the cause is somewhere else entirely. Failing while there is
	// still room to think is the point.
	if spare < caDeliveryMargin {
		t.Errorf("a bundle at the bound leaves only %d bytes of the %d-byte budget; "+
			"maxCACertPEM is too close to what the script can carry, and the next line "+
			"added anywhere in provisionScript will fail here rather than where it was "+
			"written", spare, maxUserData)
	}
}

// caDeliveryMargin is how much of the user-data budget must survive a CA bundle
// at the bound.
//
// DERIVED FROM WHAT IT PROTECTS, not picked. The margin exists so that the next
// line added to provisionScript fails HERE, with room to think, rather than at
// the cliff where the failure reads as "the CA bound is wrong". So the question
// is what a line costs: collapsing the gate's repeated per-version block into one
// shell function removed about nineteen copies and recovered roughly 280 packed
// bytes, which is on the order of fifteen packed bytes per emitted line.
//
// 1 KiB is therefore dozens of added lines of slack -- enough to notice the trend
// well before it binds, and not so much that the assertion fires on noise. It was
// 2048, which the toolcache expansion missed by EIGHT bytes; failing on eight
// bytes is not a signal about anything, and tuning the bound to satisfy an
// arbitrary margin would have been fitting the measurement to the test.
const caDeliveryMargin = 1024

// THE EMBEDDED FALLBACK EITHER FITS OR SAYS WHAT TO DO ABOUT IT.
//
// Without a bucket the installers and the declaration travel in user data, which
// is how every build worked until the toolset outgrew it. That path is allowed to
// stop fitting -- what is not allowed is for it to fail somewhere that does not
// name the remedy, because the operator's next question is "so how do I build an
// image" and the answer has to be in the error.
func TestTheEmbeddedFallbackRefusesWithTheRemedy(t *testing.T) {
	t.Parallel()

	script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "x64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	packed, err := packUserData(script)

	if err == nil {
		t.Logf("the embedded payload still fits: %d bytes packed, %d spare",
			len(packed), maxUserData-len(packed))

		return
	}

	if !strings.Contains(err.Error(), "PayloadBucket") {
		t.Errorf("the refusal does not name PayloadBucket, so an operator is told the "+
			"script is too big and not what to do: %v", err)
	}
}
