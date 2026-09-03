package ec2

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// callsFor returns every request made for one action, under the fake's own lock.
//
// THE LOCK IS NOT DECORATION. These are appended by the server's goroutine, and a
// test reading the slice directly is a data race the detector will find on a run
// where the last response has not been fully unwound.
func (f *fakeEC2) callsFor(action string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()

	var found []url.Values

	for _, c := range f.calls {
		if c.Get("Action") == action {
			found = append(found, c)
		}
	}

	return found
}

// nonceFromUserData reads the marker out of the verifier's own script.
//
// A ROUND TRIP RATHER THAN A FIXTURE. The console the fake serves is built from
// the nonce billet actually minted and wrote into the script it actually sent, so
// these tests fail if the emitter and the parser ever stop agreeing about the
// frame. A hand-written marker in the fake would agree with neither.
func nonceFromUserData(encoded string) string {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}

	m := regexp.MustCompile(reportBegin + ` ([0-9a-f]{32})`).FindSubmatch(raw)
	if m == nil {
		return ""
	}

	return string(m[1])
}

// console is what the verifier is pretending to have printed.
func (b *buildFake) console() string {
	if b.silentConsole || b.nonce == "" {
		return ""
	}

	verdict := b.verdict
	if verdict == "" {
		verdict = "ok"
	}

	body := b.consoleNoise +
		reportBegin + " " + b.nonce + "\n" +
		reportVerdictKey + "=" + verdict + "\n" +
		reportStepKey + "=done\n" +
		"docker_driver=overlay2\n" +
		"docker_root=/var/lib/docker\n" +
		"root_free_kib=20971520\n" +
		reportEnd + " " + b.nonce + "\n"

	return base64.StdEncoding.EncodeToString([]byte(body))
}

// verifyingBuild is a BuildSpec with the verification turned on, which is what
// `billet ami build` passes by default.
func verifyingBuild() BuildSpec {
	return BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
		BaseImage: "ami-base", InstanceType: "c7i.xlarge",
		Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
		Verify: true,
	}
}

// A BUILD BOOTS WHAT IT MADE, and this drives BuildImage rather than VerifyImage.
//
// DRIVE THE CALLER, NOT THE HELPER. A VerifyImage proven in isolation says
// nothing about whether a build ever calls it — which is the exact defect this
// package has shipped before, where stampImage was proven correct while
// CreateImage emitted no tags at all. Deleting the verification block from
// BuildImage has to turn this red.
func TestABuildVerifiesTheImageItProduced(t *testing.T) {
	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	image, err := p.BuildImage(t.Context(), verifyingBuild())
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	if image != "ami-new" {
		t.Errorf("built %q, want ami-new", image)
	}

	launches := f.runInstancesCalls()
	if len(launches) != 2 {
		t.Fatalf("a verified build made %d launches, want 2 (the builder and the verifier); "+
			"one means nothing booted the artifact", len(launches))
	}

	verifier := launches[1]

	if got := verifier.Get("ImageId"); got != "ami-new" {
		t.Errorf("the verifier booted %q, want ami-new: verifying anything other than the "+
			"image the build produced proves nothing about the build", got)
	}

	// THE INSTANCE NAME IS ALSO THE ONLY WAY TO TELL A FINISHED BUILD FROM A FAILED
	// ONE AFTERWARDS, once a successful build's image has been deregistered.
	if got := verifier.Get("TagSpecification.1.Tag.2.Value"); got != "test-image-verify" {
		t.Errorf("the verifier is named %q, want test-image-verify", got)
	}

	// TERMINATE, NOT STOP, WHICH IS THE OPPOSITE OF THE BUILDER. The verifier's own
	// poweroff is what ends an instance billet lost track of; with the builder's
	// value it would sit stopped and billable instead.
	if got := verifier.Get("InstanceInitiatedShutdownBehavior"); got != "terminate" {
		t.Errorf("the verifier shuts down as %q, want terminate: a stopped verifier is a "+
			"machine nobody is watching", got)
	}

	// AND ITS ROOT GOES WITH IT. This client can delete no volumes at all.
	if got := blockDevices(t, verifier); got["/dev/xvda"] != "true" {
		t.Errorf("the verifier's root leaves as %q, want true", got["/dev/xvda"])
	}

	// AND BOTH MACHINES ARE TERMINATED. Deleting either defer costs money by the
	// hour and breaks no other assertion here.
	terminated := map[string]bool{}

	for _, c := range f.callsFor("TerminateInstances") {
		terminated[c.Get("InstanceId.1")] = true
	}

	for _, id := range []string{"i-builder", "i-verify"} {
		if !terminated[id] {
			t.Errorf("%s was never terminated", id)
		}
	}
}

// A FAILED VERIFICATION STAMPS NOTHING, and the error alone does not say that.
//
// An error value is the cheapest thing a function produces. The plausible mutation
// here is warn-and-promote — this codebase's own two-channel pattern is
// warn-and-proceed — and it would leave an AMI carrying the contract it had just
// failed while a test asserting `err != nil` stayed green.
func TestAFailedVerificationStampsNothing(t *testing.T) {
	b := &buildFake{
		stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
		verdict: "fail",
	}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	_, err := p.BuildImage(t.Context(), verifyingBuild())
	if err == nil {
		t.Fatal("a build whose image reported verdict=fail returned success")
	}

	// THE IMAGE ID IS IN THE MESSAGE, because billet speaks no DeregisterImage: the
	// AMI outlives this failure either way, and an operator who is not told its id
	// has an anonymous image and a duplicate-name error on the next build.
	if !strings.Contains(err.Error(), "ami-new") {
		t.Errorf("the failure does not name the image that was left behind: %v", err)
	}

	if n := f.countOf("CreateTags"); n != 0 {
		t.Errorf("a failed verification wrote %d tag(s); an image that failed must not carry "+
			"the contract it failed", n)
	}

	if b.promoted != "" {
		t.Errorf("the image was stamped with contract %q despite failing", b.promoted)
	}
}

// A SILENT CONSOLE IS A FAILURE, not a pass, and that is the direction that
// matters: an image that never booted prints nothing, and so does one whose
// report AWS has not flushed yet. Neither is evidence of anything.
func TestAnImageThatSaysNothingDoesNotPass(t *testing.T) {
	b := &buildFake{
		stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
		silentConsole: true,
	}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	// THE CONTEXT IS WHAT ENDS THIS, not verifyWindow: every wait in the poll goes
	// through the provider's sleep, which returns the context's error rather than
	// ignoring it, so a test does not sit here for fifteen minutes. Everything
	// before the poll answers from a local server in microseconds.
	//
	// SHORT, BECAUSE THE POLL IS FAST HERE. newTestProvider replaces the fifteen-
	// second wait with a millisecond, so this window is hundreds of console reads
	// rather than one — which is the point (the machine really is being asked over
	// and over and really is saying nothing) and is also why it is not seconds.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	if _, err := p.BuildImage(ctx, verifyingBuild()); err == nil {
		t.Fatal("a build whose image printed nothing returned success")
	}

	if n := f.countOf("CreateTags"); n != 0 {
		t.Errorf("an image that reported nothing was stamped %d time(s)", n)
	}
}

// THE STAMP IS THE PROMOTION, and it carries the contract the ARCHITECTURE meets.
//
// Read out of the request rather than compared to the constant: a promoteContract
// that wrote AMIContract unconditionally would pass a test that asserted the
// constant, and would be exactly the bug that made arm64 claim a toolcache it did
// not have.
func TestAPassingVerificationPromotesTheContract(t *testing.T) {
	for _, tc := range []struct {
		aws   string
		arch  string
		shape string
	}{
		{aws: "x86_64", arch: "x64", shape: "c7i.large"},
		{aws: "arm64", arch: "arm64", shape: "c7g.large"},
	} {
		t.Run(tc.aws, func(t *testing.T) {
			b := &buildFake{
				stopReason: "Client.InstanceInitiatedShutdown", imageState: "available",
				arch: tc.aws,
			}

			f := newFakeEC2(t)
			f.respond = b.reply

			p := newTestProvider(t, f, nil)

			spec := verifyingBuild()
			spec.Arch = tc.arch

			if _, err := p.BuildImage(t.Context(), spec); err != nil {
				t.Fatalf("BuildImage: %v", err)
			}

			tagged := f.paramsFor(t, "CreateTags")

			if got := tagged.Get("ResourceId.1"); got != "ami-new" {
				t.Errorf("the contract was written to %q, want ami-new", got)
			}

			if got := tagged.Get("Tag.1.Key"); got != amiContractTag {
				t.Errorf("the promotion writes the tag %q, want %s", got, amiContractTag)
			}

			want := strconv.Itoa(contractFor(tc.arch))
			if got := tagged.Get("Tag.1.Value"); got != want {
				t.Errorf("a %s image was promoted to contract %q, want %q", tc.arch, got, want)
			}

			// AND THE SHAPE MATCHES THE IMAGE, which is not cosmetic: an arm64 AMI
			// cannot boot on an x86 shape at all, so a default that ignored the
			// architecture would fail every arm64 verification at RunInstances.
			if got := f.runInstancesCalls()[1].Get("InstanceType"); got != tc.shape {
				t.Errorf("a %s image is verified on %q, want %q", tc.aws, got, tc.shape)
			}
		})
	}
}

// AN UNVERIFIED BUILD IS AN UNSTAMPED ONE, and that is the whole content of
// --verify=false: skipping the check skips the claim.
//
// Writing the tag anyway would give it two meanings — "billet asserted this" and
// "billet did not look" — and `billet check` reads it as the first.
func TestAnUnverifiedBuildIsNotStamped(t *testing.T) {
	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	spec := verifyingBuild()
	spec.Verify = false

	if _, err := p.BuildImage(t.Context(), spec); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	if n := len(f.runInstancesCalls()); n != 1 {
		t.Errorf("--verify=false made %d launches, want 1 (the builder alone)", n)
	}

	if n := f.countOf("CreateTags"); n != 0 {
		t.Errorf("an unverified image was stamped %d time(s); the contract tag means billet "+
			"booted the image and proved it", n)
	}
}

// A PROMOTION THAT DOES NOT LAND IS A FAILURE, however cheerfully CreateTags
// answered.
//
// CreateTags returns nothing but a request id, so "it succeeded" is the API
// agreeing to have been called. The tag is read back for the same reason the
// in-build anchor check reads the trust store back: update-ca-certificates exits
// 0 having added nothing.
func TestAPromotionIsReadBack(t *testing.T) {
	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	// THE TAG IS SWALLOWED, which is what a CreateTags on the wrong resource, or
	// against a policy that answers success and applies nothing, looks like from
	// here.
	f.respond = func(action string, params url.Values) (int, string) {
		status, body := b.reply(action, params)

		if action == "CreateTags" {
			b.promoted = ""
		}

		return status, body
	}

	p := newTestProvider(t, f, nil)

	_, err := p.BuildImage(t.Context(), verifyingBuild())
	if err == nil {
		t.Fatal("a promotion that did not land was reported as a successful build")
	}

	if !strings.Contains(err.Error(), amiContractTag) {
		t.Errorf("the failure does not name the tag that is missing: %v", err)
	}

	// AND IT DOES NOT CLAIM THE IMAGE IS UNSTAMPED, which is the state billet does
	// not know: CreateTags was accepted, so the tag may be there. Telling an
	// operator it is definitely absent sends them looking for something they were
	// told is not there, and the two cases need different actions.
	if !errors.Is(err, ErrPromotionUncertain) {
		t.Errorf("a promotion that could not be confirmed is not marked uncertain: %v", err)
	}

	if strings.Contains(err.Error(), "is NOT stamped") {
		t.Errorf("billet told the operator the image is definitely unstamped when it cannot "+
			"know that: %v", err)
	}

	if !strings.Contains(err.Error(), "PROVED itself") {
		t.Errorf("the failure does not say the image passed verification, which is the half "+
			"that IS known: %v", err)
	}
}

// scriptSchema is the schema the real emitter produced for a real script.
//
// NOT A HAND-WRITTEN SET. A test that declared its own allowlist would assert that
// billet's parser agrees with the TEST, which is the shape of assertion that
// passes while the emitter and the reader disagree — the exact reason the schema
// is built by the writers rather than beside them.
func scriptSchema(t *testing.T) reportSchema {
	t.Helper()

	_, schema, err := verifyScript("x64", AMIContract, verifyProbeNonce)
	if err != nil {
		t.Fatalf("verifyScript: %v", err)
	}

	return schema
}

// WHAT BILLET WILL READ OUT OF A CONSOLE, which is whatever the machine printed
// and therefore data rather than billet's own output.
func TestConsoleReportsAreValidated(t *testing.T) {
	t.Parallel()

	const nonce = "0123456789abcdef0123456789abcdef"

	schema := scriptSchema(t)

	block := func(n, body string) string {
		return reportBegin + " " + n + "\n" + body + reportEnd + " " + n + "\n"
	}

	for _, tc := range []struct {
		name    string
		console string
		wantOK  bool
		want    map[string]string
		absent  []string
	}{
		{
			name:    "nothing at all",
			console: "",
		},
		{
			name:    "a boot log with no report in it",
			console: "[    0.000000] Linux version 6.8.0-1029-aws\n",
		},
		{
			name:    "another run's block",
			console: block("ffffffffffffffffffffffffffffffff", "verdict=ok\n"),
		},
		{
			// TRUNCATED FROM THE FRONT is the ordinary state of a 64 KiB console
			// window, and an opening marker that was cut off must not pair with a
			// closing one that survived.
			name:    "a block whose opening marker was truncated away",
			console: "verdict=ok\n" + reportEnd + " " + nonce + "\n",
		},
		{
			name:    "a block that never closed",
			console: reportBegin + " " + nonce + "\nverdict=ok\n",
		},
		{
			// THE MARKERS ARE NOT A VERDICT. A block carrying no verdict line is a
			// report that was cut in half, and reading it as verdict="" would make
			// a truncated console indistinguishable from a machine that answered.
			name:    "a block with no verdict in it",
			console: block(nonce, "step=docker\n"),
		},
		{
			name:    "the ordinary case",
			console: block(nonce, "verdict=ok\nstep=done\n"),
			wantOK:  true,
			want:    map[string]string{"verdict": "ok", "step": "done"},
		},
		{
			// THE LAST BLOCK WINS, because the verifier reprints and the window
			// truncates: an early block is the one most likely to be half gone.
			//
			// AND IT IS THE LAST BLOCK, NOT A MERGE OF ALL OF THEM. The earlier
			// block carries a field the later one does not, so pairing the FIRST
			// opening marker with the last closing one — which is what a plain
			// Index would do — puts a stale reading into the report billet renders.
			name: "two blocks, the later one authoritative",
			console: block(nonce, "verdict=fail\nstep=docker\ndocker_driver=overlayfs\n") +
				block(nonce, "verdict=ok\nstep=done\n"),
			wantOK: true,
			want:   map[string]string{"verdict": "ok", "step": "done"},
			absent: []string{"docker_driver"},
		},
		{
			// A LINE THE MACHINE CHOSE CANNOT FORGE ONE OF BILLET'S. Escapes,
			// control characters and non-ASCII are dropped rather than rendered.
			name: "a value carrying control characters",
			console: block(nonce, "verdict=ok\nstep=done\n"+
				"docker_driver=over\x1b[2Jlay2\nrunner=2.328.0\n"),
			wantOK: true,
			want:   map[string]string{"verdict": "ok", "runner": "2.328.0"},
			absent: []string{"docker_driver"},
		},
		{
			name: "a key billet never emits",
			console: block(nonce, "verdict=ok\nDROP TABLE=1\n../../etc/passwd=x\n"+
				"UPPER=1\n"),
			wantOK: true,
			want:   map[string]string{"verdict": "ok"},
			absent: []string{"DROP TABLE", "../../etc/passwd", "UPPER"},
		},
		{
			// A KEY SHAPE IS NOT AN ALLOWLIST, and this is the case that says so.
			// `secret` and `note` are as valid a shape as `runner`; what keeps them
			// out is the schema built from what the script actually emits. Without
			// it a machine chooses a line in billet's log.
			name: "a well-formed key billet never emits",
			console: block(nonce, "verdict=ok\nstep=done\nsecret=hunter2\n"+
				"note=anything_at_all\n"),
			wantOK: true,
			want:   map[string]string{"verdict": "ok", "step": "done"},
			absent: []string{"secret", "note"},
		},
		{
			// THE STEP IS CHECKED BY VALUE, because it is one of the two fields
			// billet renders back and the closed set is the whole reason quoting it
			// is safe.
			name:    "a step label billet never wrote",
			console: block(nonce, "verdict=ok\nstep=exfiltrate\n"),
			wantOK:  true,
			want:    map[string]string{"verdict": "ok"},
			absent:  []string{"step"},
		},
		{
			// AND SO IS THE VERDICT, which is the field the whole decision turns
			// on. A machine that answers something billet has no word for has not
			// answered: the block is discarded and the poll goes on, rather than a
			// third state reaching the promotion.
			name:    "a verdict billet has no word for",
			console: block(nonce, "verdict=probably\nstep=done\n"),
		},
		{
			name: "an absurd value",
			console: block(nonce, "verdict=ok\nrunner="+
				strings.Repeat("x", maxReportValue+1)+"\n"),
			wantOK: true,
			want:   map[string]string{"verdict": "ok"},
			absent: []string{"runner"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := parseReport(tc.console, nonce, schema)
			if ok != tc.wantOK {
				t.Fatalf("parseReport ok=%v, want %v (%v)", ok, tc.wantOK, got)
			}

			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s=%q, want %q", k, got[k], v)
				}
			}

			for _, k := range tc.absent {
				if v, present := got[k]; present {
					t.Errorf("%s was rendered as %q; billet renders only the keys it emits, "+
						"with printable values", k, v)
				}
			}
		})
	}
}

// AN ARCHITECTURE BILLET DOES NOT KNOW IS A REFUSAL, NOT A DEFAULT.
//
// Defaulting to x64 would assert x64 toolcache paths against an image that has
// none — every entry structurally absent, and a correct image reported broken.
// The empty case is its own answer: an image that reports no architecture has not
// said x86_64.
func TestAnUnknownArchitectureIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		aws  string
		want string
		ok   bool
	}{
		{aws: "x86_64", want: "x64", ok: true},
		{aws: "arm64", want: "arm64", ok: true},
		{aws: "i386"},
		{aws: "x64"},
		{aws: ""},
	} {
		t.Run("aws="+tc.aws, func(t *testing.T) {
			t.Parallel()

			got, err := billetArch("ami-1", tc.aws)

			if !tc.ok {
				if err == nil {
					t.Fatalf("architecture %q was accepted as %q", tc.aws, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("architecture %q was refused: %v", tc.aws, err)
			}

			if got != tc.want {
				t.Errorf("architecture %q became %q, want %q", tc.aws, got, tc.want)
			}
		})
	}
}

// AN OVERSIZED CONSOLE IS TRUNCATED FROM THE FRONT, AND WHAT SURVIVES IS STILL
// LEGIBLE.
//
// Two properties in one, because getting either wrong produces the same symptom —
// a working image reported as silent — and neither is visible from the other.
// Keeping the HEAD would drop the report, which is the last thing the machine
// printed. Cutting anywhere but a four-character boundary would leave bytes that
// are no longer base64, so a console billet could have read becomes one it cannot.
func TestAnOversizedConsoleKeepsItsTailAndStaysReadable(t *testing.T) {
	t.Parallel()

	const nonce = verifyProbeNonce

	report := reportBegin + " " + nonce + "\nverdict=ok\nstep=done\n" +
		reportEnd + " " + nonce + "\n"

	// AN ODD PREFIX LENGTH ON PURPOSE. A boot log whose size happens to be a
	// multiple of three encodes without any of the misalignment this is about, so a
	// fixture that used a round number would pass against a cut that ignores the
	// quantum entirely.
	console := strings.Repeat("boot log line\n", 60_000) + "x" + report

	f := newFakeEC2(t)
	f.respond = func(action string, _ url.Values) (int, string) {
		if action != "GetConsoleOutput" {
			return http.StatusOK, defaultReply(action)
		}

		return http.StatusOK, `<GetConsoleOutputResponse><instanceId>i-verify</instanceId>` +
			`<output>` + base64.StdEncoding.EncodeToString([]byte(console)) + `</output>` +
			`</GetConsoleOutputResponse>`
	}

	p := newTestProvider(t, f, nil)

	got, err := p.consoleOutput(t.Context(), "i-verify", true)
	if err != nil {
		t.Fatalf("consoleOutput: %v", err)
	}

	if len(got) >= len(console) {
		t.Errorf("the console came back at %d bytes against %d sent; nothing was bounded",
			len(got), len(console))
	}

	// THE BOUND IS WHAT MAKES THE CUT ALIGNED, so it is asserted rather than assumed
	// by the code that relies on it: base64 is always a multiple of four characters,
	// and a bound that is not one would slice mid-quantum and leave a console
	// billet could have read unreadable.
	if maxEncodedConsole%4 != 0 {
		t.Errorf("maxEncodedConsole is %d, which is not a whole number of base64 quanta",
			maxEncodedConsole)
	}

	if _, ok := parseReport(got, nonce, scriptSchema(t)); !ok {
		t.Errorf("the report did not survive truncation, so a working image reads as one "+
			"that printed nothing; kept %d bytes ending %q",
			len(got), got[max(0, len(got)-80):])
	}
}

// A TERMINATE ISSUED SECONDS AFTER A LAUNCH CAN BE ANSWERED "NO SUCH INSTANCE",
// AND THAT IS NOT "IT IS GONE".
//
// Destroy deliberately reads InvalidInstanceID.NotFound as success, because this
// API is eventually consistent and a node's own List is what settles it later. A
// one-shot verifier has no List, so the same tolerance would let a cancelled
// verification report a clean teardown for a machine that appears a moment later
// — and an image with broken cloud-init never reaches the poweroff that would
// otherwise end it.
func TestTheVerifierIsTerminatedOnceItIsVisible(t *testing.T) {
	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)

	// THE VERIFIER IS INVISIBLE FOR THE FIRST TWO ASKS, which is exactly the window
	// AWS documents. Counting them is also what stops this passing because the
	// instance happened to be visible immediately.
	var invisible int

	f.respond = func(action string, params url.Values) (int, string) {
		if action == "DescribeInstances" && params.Get("InstanceId.1") == "i-verify" &&
			invisible < 2 {
			invisible++

			return http.StatusBadRequest, `<Response><Errors><Error>` +
				`<Code>InvalidInstanceID.NotFound</Code><Message>nope</Message>` +
				`</Error></Errors></Response>`
		}

		return b.reply(action, params)
	}

	p := newTestProvider(t, f, nil)

	if _, err := p.BuildImage(t.Context(), verifyingBuild()); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	if invisible != 2 {
		t.Fatalf("the verifier was asked about %d times while invisible, want 2; this test "+
			"did not stage the window it is about", invisible)
	}

	terminated := map[string]bool{}
	for _, c := range f.callsFor("TerminateInstances") {
		terminated[c.Get("InstanceId.1")] = true
	}

	if !terminated["i-verify"] {
		t.Error("the verifier was never terminated: billet gave up while the api was still " +
			"catching up with a launch it had already accepted, and the instance would run " +
			"until it powered itself off — which an image that cannot boot never does")
	}
}

// AN AMBIGUOUS CreateTags IS NOT A REFUSAL, AND THE READ-BACK IS WHAT SETTLES IT.
//
// Only the API answering with a code is AWS's own verdict that nothing was
// written. A dropped connection or a body that will not parse may have committed,
// and this package's own client retries exactly those — so treating any error as
// conclusive puts the build back in the collapsed state ErrPromotionUncertain was
// added to remove, and this time with the WRONG answer: the tag is there.
func TestAnAmbiguousPromotionIsSettledByTheReadBack(t *testing.T) {
	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)

	// THE TAG IS APPLIED AND A GATEWAY ANSWERS 502 WITH PROSE, which is the shape
	// that matters: an apiError carrying a STATUS and no code, which is not AWS
	// saying no — the request may well have reached it, and here it did.
	//
	// NOT A 200 WITH A BAD BODY, which was the first version of this fixture and
	// staged nothing: CreateTags is called with a nil target, so the client returns
	// success without parsing anything and the ambiguous branch is never reached.
	f.respond = func(action string, params url.Values) (int, string) {
		status, body := b.reply(action, params)

		if action == "CreateTags" {
			return http.StatusBadGateway, "<html>gateway timeout</html>"
		}

		return status, body
	}

	p := newTestProvider(t, f, nil)

	if _, err := p.BuildImage(t.Context(), verifyingBuild()); err != nil {
		t.Fatalf("a promotion whose response was unreadable, but which landed, failed the "+
			"build: %v", err)
	}

	if b.promoted != strconv.Itoa(AMIContract) {
		t.Errorf("the fake recorded %q as the promoted contract; this test did not stage the "+
			"case it is about", b.promoted)
	}
}

// A TAG THAT IS NOT VISIBLE ON THE FIRST ASK IS NOT A TAG THAT IS NOT THERE.
//
// DescribeImages is eventually consistent like everything else here, and this
// package has already made that mistake twice against instances and once against
// images. A single read-back turns a correct promotion into a failed build and an
// error accusing something else of writing the image's tags.
func TestAPromotionToleratesADelayedTag(t *testing.T) {
	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)

	// THE TAG IS WITHHELD FROM THE FIRST READ-BACK ONLY, and the count is asserted
	// so this cannot pass by never staging the delay.
	var withheld int

	f.respond = func(action string, params url.Values) (int, string) {
		if action == "DescribeImages" && b.promoted != "" && withheld == 0 {
			withheld++
			held := b.promoted
			b.promoted = ""

			status, body := b.reply(action, params)
			b.promoted = held

			return status, body
		}

		return b.reply(action, params)
	}

	p := newTestProvider(t, f, nil)

	if _, err := p.BuildImage(t.Context(), verifyingBuild()); err != nil {
		t.Fatalf("a tag that took one extra describe to appear failed the build: %v", err)
	}

	if withheld != 1 {
		t.Errorf("the read-back was never made to miss, so this test did not stage the delay " +
			"it is about")
	}
}
