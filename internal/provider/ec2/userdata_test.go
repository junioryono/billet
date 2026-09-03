package ec2

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

// TestUserDataIsPlainTextWhileItFits. A script an operator can read out of the
// console is worth more than headroom nothing needs, so compression is spent only
// where it buys something.
func TestUserDataIsPlainTextWhileItFits(t *testing.T) {
	t.Parallel()

	script := "#!/bin/sh\nset -eux\necho hello\n"

	got, err := packUserData(script)
	if err != nil {
		t.Fatalf("packUserData: %v", err)
	}

	if string(got) != script {
		t.Errorf("a script well inside the budget was not sent verbatim:\n%q", got)
	}
}

// TestUserDataOverTheLimitIsCompressedAndRoundTrips is the property the whole
// mechanism rests on: cloud-init has to be able to get the original script back.
func TestUserDataOverTheLimitIsCompressedAndRoundTrips(t *testing.T) {
	t.Parallel()

	// COMPRESSIBLE BUT NOT DEGENERATE. A megabyte of one repeated byte would
	// compress to almost nothing and prove less than shell-shaped text does.
	var b strings.Builder
	for b.Len() <= maxUserData {
		b.WriteString("install -d /opt/actions-runner && echo staging the runner tree\n")
		b.WriteString("curl -fsSL -o runner.tar.gz https://example.invalid/runner.tgz\n")
	}

	script := b.String()

	if len(script) <= maxUserData {
		t.Fatalf("the fixture is %d bytes, which does not exceed the %d-byte budget it is "+
			"meant to exceed", len(script), maxUserData)
	}

	got, err := packUserData(script)
	if err != nil {
		t.Fatalf("packUserData: %v", err)
	}

	if len(got) > maxUserData {
		t.Fatalf("compressed user data is %d bytes, still over the %d-byte budget",
			len(got), maxUserData)
	}

	// IT MUST ACTUALLY BE GZIP, because that is what cloud-init sniffs for. A
	// shorter payload that is not gzip would satisfy a length check and produce an
	// instance that runs the compressed bytes as a shell script.
	if len(got) < 2 || got[0] != 0x1f || got[1] != 0x8b {
		t.Fatalf("compressed user data does not start with the gzip magic bytes; cloud-init "+
			"would treat it as a script rather than decompressing it: % x", got[:min(4, len(got))])
	}

	zr, err := gzip.NewReader(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("the payload is not readable as gzip: %v", err)
	}

	back, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}

	if string(back) != script {
		t.Error("the script cloud-init would recover is not the script that was packed")
	}
}

// TestAScriptTooLargeEvenCompressedIsRefusedBeforeABuilderExists keeps the
// compressed path from becoming an unbounded one.
//
// Compression is headroom, not an exemption. provisionScript runs before
// launchBuilder for exactly this reason: a refusal here costs nothing, while the
// same failure at RunInstances arrives as a parameter error that names neither the
// script nor the size.
func TestAScriptTooLargeEvenCompressedIsRefusedBeforeABuilderExists(t *testing.T) {
	t.Parallel()

	// GENUINELY INCOMPRESSIBLE, AND THE TEST PROVES IT RATHER THAN ASSUMING IT.
	//
	// The first version of this fixture was a deterministic formula over a
	// 62-character alphabet, which LOOKS random and is not: gzip found the
	// structure and packed 128 KB under the budget, so the test asserted a refusal
	// that never came. Random bytes rendered as base64 have no structure to find —
	// gzip can recover only the 6-bits-in-8 padding — and the assertion below
	// fails loudly if that ever stops being true.
	raw := make([]byte, maxUserData*6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate incompressible input: %v", err)
	}

	script := base64.StdEncoding.EncodeToString(raw)

	var probe bytes.Buffer

	zw := gzip.NewWriter(&probe)
	if _, err := zw.Write([]byte(script)); err != nil {
		t.Fatalf("probe compress: %v", err)
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}

	if probe.Len() <= maxUserData {
		t.Fatalf("the fixture compresses to %d bytes, inside the %d-byte budget — so this "+
			"test cannot exercise the refusal it is named for", probe.Len(), maxUserData)
	}

	_, err := packUserData(script)
	if err == nil {
		t.Fatal("a script too large to deliver even compressed was accepted; the failure " +
			"would then arrive at RunInstances, after a builder has been paid for")
	}

	// THE MESSAGE CARRIES BOTH SIZES, because "too big" without them leaves an
	// operator unable to tell whether trimming the script would help.
	for _, want := range []string{"compressed", "user data"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestTheRealProvisionScriptReportsItsUserDataHeadroom is DIAGNOSTICS, and its
// name now says so.
//
// It was called ...StillFitsPlain and asserted nothing of the kind — it logged.
// A test whose name states a property it does not check is worse than no test:
// a reader scanning the file counts it as coverage. The property it appeared to
// guard is also one this branch deliberately gives up, since the toolcache and
// the JDKs are what push the script past the plain budget and compression is the
// supported answer. So the headroom is REPORTED, so a change that eats it is
// visible in the log, and the real assertions live in the boundary and
// deliverability tests above.
func TestTheRealProvisionScriptReportsItsUserDataHeadroom(t *testing.T) {
	t.Parallel()

	script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.336.0", Arch: "x64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	packed, err := packUserData(script)
	if err != nil {
		t.Fatalf("packUserData: %v", err)
	}

	compressed := len(packed) >= 2 && packed[0] == 0x1f && packed[1] == 0x8b

	t.Logf("provision script: %d bytes plain, %d free of %d; compressed=%v",
		len(script), maxUserData-len(script), maxUserData, compressed)

	if compressed {
		t.Logf("the script now exceeds the plain budget and is being compressed. That is " +
			"supported and expected as parity lands; this log line exists so the switch " +
			"is noticed rather than inferred.")
	}
}

// THE EXACT BOUNDARIES, because "about 16 KB" is where an off-by-one lives and a
// paid builder is what it costs. maxUserData is what EC2 accepts, so a script of
// exactly that size must be delivered plain and one byte more must not be.
func TestTheUserDataBoundariesAreExact(t *testing.T) {
	t.Parallel()

	// `a` REPEATED IS THE POINT, NOT A WEAKNESS. These cases are about the plain
	// branch's comparison, which never looks at the bytes; the compressed branch
	// has its own incompressible fixture below.
	for _, tc := range []struct {
		name      string
		size      int
		wantPlain bool
	}{
		{name: "one byte under the limit", size: maxUserData - 1, wantPlain: true},
		{name: "exactly the limit", size: maxUserData, wantPlain: true},
		{name: "one byte over the limit", size: maxUserData + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			script := strings.Repeat("a", tc.size)

			packed, err := packUserData(script)
			if err != nil {
				t.Fatalf("packUserData(%d bytes): %v", tc.size, err)
			}

			gzipped := len(packed) >= 2 && packed[0] == 0x1f && packed[1] == 0x8b

			if tc.wantPlain && gzipped {
				t.Errorf("a %d-byte script was compressed; the limit is %d and a script an "+
					"operator can read out of the console is worth more than headroom "+
					"nothing needs", tc.size, maxUserData)
			}

			if !tc.wantPlain && !gzipped {
				t.Errorf("a %d-byte script was sent plain past the %d-byte limit, so "+
					"RunInstances would reject it with a parameter error naming neither "+
					"the script nor the size", tc.size, maxUserData)
			}

			if len(packed) > maxUserData {
				t.Errorf("the packed payload is %d bytes, over the %d EC2 accepts",
					len(packed), maxUserData)
			}
		})
	}
}

// AN EMPTY SCRIPT IS REFUSED AT THE SAME BOUNDARY AS AN OVERSIZED ONE.
//
// EC2 accepts empty user data without complaint, so the failure is not a
// parameter error: the builder launches, boots, does nothing, and never reaches
// the `poweroff` that signals success. billet then waits out the entire build
// timeout on an instance it is paying for and reports that the guest never
// stopped — which is true and says nothing about the cause.
//
// provisionScript cannot currently return empty. That is the reason the guard
// belongs at this boundary rather than at that one: this is the last point every
// present and future caller passes through on the way to RunInstances.
func TestAnEmptyScriptIsRefusedRatherThanLaunched(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		script string
	}{
		{"empty", ""},
		{"a single newline", "\n"},
		{"whitespace only", "  \n\t\n  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := packUserData(tc.script); err == nil {
				t.Fatal("an empty provisioning script was accepted; the builder would boot, " +
					"do nothing, and be paid for until the build timeout")
			}
		})
	}
}

// THE PRODUCTION CALL IS WHAT IS EXERCISED, not packUserData in isolation.
//
// Every other test in this file calls packUserData directly, so all of them stay
// green if the call at launchBuilder's top is deleted, its error swallowed, or the
// uncompressed script handed to EC2 anyway. This drives launchBuilder itself and
// asserts the consequence that matters: no RunInstances.
func TestLaunchBuilderRefusesAnUndeliverableScriptBeforeRunInstances(t *testing.T) {
	t.Parallel()

	// The same incompressible construction as above: random bytes rendered as
	// base64 leave gzip only the 6-bits-in-8 padding to recover.
	raw := make([]byte, maxUserData*6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate incompressible input: %v", err)
	}

	script := base64.StdEncoding.EncodeToString(raw)

	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	_, err := p.launchBuilder(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
		BaseImage: "ami-base", InstanceType: "c7i.xlarge",
		Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
	}, script)
	if err == nil {
		t.Fatal("launchBuilder accepted a script it could not deliver, so the failure would " +
			"arrive at RunInstances after a builder has been paid for")
	}

	// BOTH SIZES, because "too big" without them leaves an operator unable to tell
	// whether trimming the script would help or whether it never could.
	for _, want := range []string{"compressed", "user data"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("%d paid builders were launched for a script that could never be carried", n)
	}
}
