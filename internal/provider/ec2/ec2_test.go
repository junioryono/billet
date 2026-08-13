package ec2

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// fakeEC2 answers the query API, recording what it was asked.
type fakeEC2 struct {
	*httptest.Server

	mu    sync.Mutex
	calls []url.Values

	// respond decides the reply. Nil means the default for the action.
	respond func(action string, params url.Values) (int, string)
	// redirectTo, when set, is sent as the Location of any non-200 reply.
	redirectTo string
	// auth is the Authorization header of the last request, so a test can read
	// the credential scope billet actually signed with.
	auth string
}

func (f *fakeEC2) lastAuthorization() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.auth
}

func newFakeEC2(t *testing.T) *fakeEC2 {
	t.Helper()

	f := &fakeEC2{}

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)

			return
		}

		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse request body: %v", err)

			return
		}

		// EVERY REQUEST IS SIGNED, including the ones a test does not care about.
		// Asserting it here rather than in one test means a code path that forgets
		// to sign fails wherever it is exercised.
		if r.Header.Get("Authorization") == "" {
			t.Errorf("an unsigned request reached the api: %s", params.Get("Action"))
		}

		f.mu.Lock()
		f.calls = append(f.calls, params)
		f.auth = r.Header.Get("Authorization")
		respond := f.respond
		f.mu.Unlock()

		action := params.Get("Action")

		status, reply := http.StatusOK, defaultReply(action)
		if respond != nil {
			status, reply = respond(action, params)
		}

		if f.redirectTo != "" && status != http.StatusOK {
			w.Header().Set("Location", f.redirectTo)
		}

		w.WriteHeader(status)
		write(t, w, reply)
	}))

	t.Cleanup(f.Close)

	return f
}

// write sends a response body and fails the test if it cannot.
//
// CHECKED RATHER THAN DISCARDED. A short write to the test client does not
// disappear — it reappears as an XML parse failure inside the code under test,
// which reads as a bug in the parser rather than as a fake that stopped talking.
func write(t *testing.T, w io.Writer, body string) {
	t.Helper()

	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write the fake response: %v", err)
	}
}

// paramsFor returns the parameters of the one call to an action, failing when
// there was not exactly one.
func (f *fakeEC2) paramsFor(t *testing.T, action string) url.Values {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	var found []url.Values

	for _, c := range f.calls {
		if c.Get("Action") == action {
			found = append(found, c)
		}
	}

	if len(found) != 1 {
		t.Fatalf("%s was called %d times, want exactly 1", action, len(found))
	}

	return found[0]
}

func (f *fakeEC2) countOf(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, c := range f.calls {
		if c.Get("Action") == action {
			n++
		}
	}

	return n
}

func defaultReply(action string) string {
	switch action {
	case "RunInstances":
		return `<RunInstancesResponse><instancesSet><item>` +
			`<instanceId>i-0abc</instanceId><instanceState><name>pending</name></instanceState>` +
			`</item></instancesSet></RunInstancesResponse>`

	case "DescribeImages":
		return `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`</item></imagesSet></DescribeImagesResponse>`

	case "TerminateInstances":
		return `<TerminateInstancesResponse/>`

	default:
		return `<DescribeInstancesResponse><reservationSet/></DescribeInstancesResponse>`
	}
}

// apiFailure is the shape the query API uses for a refusal.
func apiFailure(code string) string {
	return `<Response><Errors><Error><Code>` + code +
		`</Code><Message>nope</Message></Error></Errors></Response>`
}

// describeReply renders instances as one reservation.
func describeReply(nextToken string, items ...string) string {
	var b strings.Builder

	b.WriteString(`<DescribeInstancesResponse><reservationSet><item><instancesSet>`)

	for _, item := range items {
		b.WriteString(item)
	}

	b.WriteString(`</instancesSet></item></reservationSet>`)

	if nextToken != "" {
		b.WriteString(`<nextToken>` + nextToken + `</nextToken>`)
	}

	b.WriteString(`</DescribeInstancesResponse>`)

	return b.String()
}

// instanceXML renders a running instance. Which states count as running is
// decided by runningState and tested directly against every one of them, so the
// state is fixed here rather than being a parameter nothing varies.
func instanceXML(id, name string) string {
	return fmt.Sprintf(`<item><instanceId>%s</instanceId>`+
		`<instanceState><name>running</name></instanceState>`+
		`<tagSet><item><key>Name</key><value>%s</value></item>`+
		`<item><key>sh.billet.owner</key><value>dep-1</value></item></tagSet></item>`,
		id, name)
}

// validEC2Config is a complete backend configuration pointed at a fake.
func validEC2Config(endpoint string) config.EC2Config {
	return config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         endpoint,
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-trusted"},
		InstanceTypes: []config.EC2InstanceType{
			{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB},
			{Type: "c7i.8xlarge", VCPU: 32, Memory: 64 * config.GiB},
		},
	}
}

func newTestProvider(t *testing.T, f *fakeEC2, mutate func(*config.EC2Config)) *Provider {
	t.Helper()

	cfg := validEC2Config(f.URL)
	if mutate != nil {
		mutate(&cfg)
	}

	p, err := New("dep-1", cfg,
		WithHTTPClient(f.Client()),
		WithCredentials(StaticCredentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p
}

// validSpec is a launch that should succeed.
func validSpec() provider.Spec {
	return provider.Spec{
		Name:      provider.InstanceName("lease-1"),
		Image:     "ami-0abc",
		VCPU:      8,
		Memory:    16 * config.GiB,
		Trust:     provider.TrustTrusted,
		JITConfig: "eyJydW5uZXIiOiJqaXQifQ==",
		Command:   []string{"./run.sh"},
	}
}

// A launch has to say every one of these, and each was chosen rather than
// defaulted.
func TestALaunchTellsEC2WhatToStart(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	inst, err := p.Launch(t.Context(), validSpec())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if inst.ID != "i-0abc" || inst.Name != provider.InstanceName("lease-1") {
		t.Errorf("instance = %+v, want the id the api reported and billet's own name", inst)
	}

	got := f.paramsFor(t, "RunInstances")

	for key, want := range map[string]string{
		"ImageId":      "ami-0abc",
		"InstanceType": "c7i.2xlarge",
		"MinCount":     "1",
		"MaxCount":     "1",
		"SubnetId":     "subnet-0abc",

		"SecurityGroupId.1": "sg-trusted",

		"TagSpecification.1.ResourceType": "instance",
		"TagSpecification.1.Tag.1.Key":    "Name",
		"TagSpecification.1.Tag.1.Value":  "billet-lease-1",
		"TagSpecification.1.Tag.2.Key":    "sh.billet.owner",
		"TagSpecification.1.Tag.2.Value":  "dep-1",

		// The volume carries them too, or a root disk that outlives a failed
		// termination is billed and unfindable.
		"TagSpecification.2.ResourceType": "volume",
		"TagSpecification.2.Tag.2.Value":  "dep-1",

		// IMDSv2 with one hop: a container inside the job cannot reach the
		// metadata service, which is where the user data is readable.
		"MetadataOptions.HttpTokens":                  "required",
		"MetadataOptions.HttpPutResponseHopLimit":     "1",
		"InstanceMarketOptions.MarketType":            "",
		"IamInstanceProfile.Name":                     "",
		"NetworkInterface.1.AssociatePublicIpAddress": "",
	} {
		if got.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, got.Get(key), want)
		}
	}
}

// THE NAME IS THE IDEMPOTENCY KEY, and this is the assertion that keeps it one.
//
// A RunInstances that commits and loses its response is the case the Provider
// interface warns about, and it is the one where a retry starts a second machine
// for one job — two runners, one of which nothing will ever tear down. AWS honours
// a client token by returning the SAME instance, and billet's instance name
// encodes a lease id, so it is unique by construction and never reused.
func TestALaunchCannotStartTwoMachinesForOneJob(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	spec := validSpec()

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := f.paramsFor(t, "RunInstances").Get("ClientToken"); got != spec.Name {
		t.Errorf("ClientToken = %q, want the instance name %q; without it a retried launch "+
			"starts a second machine for one job", got, spec.Name)
	}
}

// SPOT IS OFF UNLESS SOMEBODY ASKED, and the reason is what this backend is for.
// It exists so one `runs-on` label survives the bare-metal host going away, and
// GitHub does not requeue a job whose runner vanished mid-execution — so a
// reclaimed instance is a failed build, not a retry. Defaulting to spot makes the
// failover path the unreliable one.
func TestSpotIsOffUnlessAsked(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := f.paramsFor(t, "RunInstances").Get("InstanceMarketOptions.MarketType"); got != "" {
		t.Errorf("MarketType = %q on a default config; spot must be opted into", got)
	}
}

// And when it is asked for, it is a ONE-TIME request that terminates. A
// persistent one would relaunch after a reclaim, and the job it was running is
// already gone — so the replacement boots with nothing to do and holds a lease
// until something reaps it.
func TestSpotIsRequestedOneTimeSoNothingRelaunchesIntoAnEmptyJob(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, func(c *config.EC2Config) { c.Spot = true })

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	for key, want := range map[string]string{
		"InstanceMarketOptions.MarketType":                               "spot",
		"InstanceMarketOptions.SpotOptions.SpotInstanceType":             "one-time",
		"InstanceMarketOptions.SpotOptions.InstanceInterruptionBehavior": "terminate",
	} {
		if got.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, got.Get(key), want)
		}
	}
}

// UNTRUSTED WORK RUNS ON ITS OWN NETWORK OR IT DOES NOT RUN.
//
// An instance isolates the kernel, which is why this backend may run fork
// pull-request code at all. It does not isolate the VPC: the same security group
// as everything else reaches whatever that group reaches.
func TestUntrustedWorkGetsItsOwnSecurityGroups(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, func(c *config.EC2Config) {
		c.UntrustedSecurityGroupIDs = []string{"sg-fork"}
	})

	spec := validSpec()
	spec.Trust = provider.TrustUntrusted

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	if g := got.Get("SecurityGroupId.1"); g != "sg-fork" {
		t.Errorf("a fork's job was placed on %q, want the untrusted group", g)
	}
}

// And without one described, it is refused rather than placed on the trusted
// group — the direction that cannot be undone once a job has run.
func TestUntrustedWorkIsRefusedUntilItsNetworkIsDescribed(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	if err := p.Accepts(provider.TrustUntrusted); err == nil {
		t.Fatal("untrusted work was accepted with no security group of its own")
	}

	spec := validSpec()
	spec.Trust = provider.TrustUntrusted

	if _, err := p.Launch(t.Context(), spec); err == nil {
		t.Fatal("Launch ran untrusted work that Accepts refuses")
	}

	// AND NOTHING WAS STARTED. A refusal that still called RunInstances would be
	// a machine running a fork's code on the trusted network.
	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("a refused launch still called RunInstances %d times", n)
	}
}

// An unclassified job is refused whatever the configuration says, because there
// is no basis for choosing either network. Distinct from untrusted, which is a
// classification billet made.
func TestAnUnclassifiedJobIsRefusedEvenWithAnUntrustedGroup(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, func(c *config.EC2Config) {
		c.UntrustedSecurityGroupIDs = []string{"sg-fork"}
	})

	if err := p.Accepts(provider.TrustUnknown); err == nil {
		t.Fatal("a job billet could not classify was accepted")
	}
}

// THE OPERATOR'S ORDER IS A PREFERENCE, the same way a tier's provider list is.
func TestTheFirstDeclaredShapeThatFitsIsBought(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.VCPU = 16
	spec.Memory = 32 * config.GiB

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := f.paramsFor(t, "RunInstances").Get("InstanceType"); got != "c7i.8xlarge" {
		t.Errorf("InstanceType = %q, want the first declared shape that holds 16 vCPU", got)
	}
}

// A lease no declared shape can hold is a config that promised the allocator
// capacity it cannot supply, and the error has to show both halves — the size
// asked for and what was declared — or an operator is told only the conclusion.
func TestALeaseNoShapeHoldsNamesBothSides(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.VCPU = 96

	_, err := p.Launch(t.Context(), spec)
	if err == nil {
		t.Fatal("a lease larger than every declared shape was launched")
	}

	for _, want := range []string{"96", "c7i.8xlarge", "instance_types"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}

	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("a launch with no usable shape still called RunInstances %d times", n)
	}
}

// SIZING THE ROOT DISK MEANS ASKING WHICH DEVICE IS ROOT.
//
// A block device mapping naming a device that is not the AMI's root does not
// fail: EC2 attaches an ADDITIONAL empty volume, the root stays the size the
// image was built at, and the launch reports success. So a tier asking for 300GiB
// runs out of disk mid-job with an unused 300GiB volume beside it, billed.
func TestSizingTheRootVolumeAsksWhichDeviceIsRoot(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.Disk = 80 * config.GiB

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if f.countOf("DescribeImages") != 1 {
		t.Fatal("the root device name was assumed rather than asked for")
	}

	// And asked about THIS image. Sizing against another AMI's root device is the
	// same silent failure as guessing one.
	if img := f.paramsFor(t, "DescribeImages").Get("ImageId.1"); img != "ami-0abc" {
		t.Errorf("DescribeImages asked about %q, want the image being launched", img)
	}

	got := f.paramsFor(t, "RunInstances")

	if d := got.Get("BlockDeviceMapping.1.DeviceName"); d != "/dev/xvda" {
		t.Errorf("DeviceName = %q, want the root device the api reported", d)
	}

	if v := got.Get("BlockDeviceMapping.1.Ebs.VolumeSize"); v != "80" {
		t.Errorf("VolumeSize = %q, want 80", v)
	}

	if v := got.Get("BlockDeviceMapping.1.Ebs.DeleteOnTermination"); v != "true" {
		t.Errorf("DeleteOnTermination = %q; a root volume that outlives its instance is billed", v)
	}
}

// ROUNDED UP, because EBS sizes in whole GiB and rounding down hands a tier that
// asked for 80GiB a 79GiB disk — the direction that fails a job rather than
// costing a fraction of a cent.
func TestAPartialGibibyteRoundsUp(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.Disk = 80*config.GiB + 1

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if v := f.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.1.Ebs.VolumeSize"); v != "81" {
		t.Errorf("VolumeSize = %q, want 81; a disk smaller than the tier asked for fails a job", v)
	}
}

// A ROOT VOLUME IS DISPOSED OF EXPLICITLY, EVEN WITH NO SIZE TO SET.
//
// Left unstated, whatever the AMI was built with governs — and an AMI built with
// DeleteOnTermination false leaves a root volume behind for every job billet ever
// runs on it, billed indefinitely and discoverable only by hunting tags. This is
// exactly the case that used to skip the block device mapping entirely, on the
// reasoning that a launch with nothing to size should not pay for the lookup.
func TestARootVolumeIsAlwaysDisposedOfEvenWithNoSizeToSet(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.Disk = 0

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	if v := got.Get("BlockDeviceMapping.1.Ebs.DeleteOnTermination"); v != "true" {
		t.Errorf("DeleteOnTermination = %q for a launch that sizes nothing; the AMI's own "+
			"setting would govern, and one built with it false leaks a volume per job", v)
	}

	if v := got.Get("BlockDeviceMapping.1.DeviceName"); v != "/dev/xvda" {
		t.Errorf("DeviceName = %q, want the AMI's root device", v)
	}

	// NO SIZE IS SENT when none was asked for, so the AMI's own size stands.
	// Sending a zero would be asking EC2 for a zero-byte root volume.
	if v := got.Get("BlockDeviceMapping.1.Ebs.VolumeSize"); v != "" {
		t.Errorf("VolumeSize = %q for a tier that asked for no disk", v)
	}
}

// THE LOOKUP IS PAID FOR ONCE PER IMAGE, which is what makes asking on every
// launch affordable. An AMI's root device does not change.
func TestTheRootDeviceIsAskedForOncePerImage(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	for range 3 {
		if _, err := p.Launch(t.Context(), validSpec()); err != nil {
			t.Fatalf("Launch: %v", err)
		}
	}

	if n := f.countOf("DescribeImages"); n != 1 {
		t.Errorf("DescribeImages was called %d times across three launches of one image, want 1", n)
	}
}

// Every one of these is a launch that would otherwise succeed and do nothing, or
// succeed and be unattributable.
func TestALaunchMissingSomethingEssentialIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*provider.Spec){
		"no name":    func(s *provider.Spec) { s.Name = "" },
		"no image":   func(s *provider.Spec) { s.Image = "" },
		"no jit":     func(s *provider.Spec) { s.JITConfig = "" },
		"no command": func(s *provider.Spec) { s.Command = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeEC2(t)
			p := newTestProvider(t, f, nil)

			spec := validSpec()
			mutate(&spec)

			if _, err := p.Launch(t.Context(), spec); err == nil {
				t.Fatal("this launch was accepted and would have started a machine that never " +
					"registers a runner")
			}

			if n := f.countOf("RunInstances"); n != 0 {
				t.Errorf("a refused launch still called RunInstances %d times", n)
			}
		})
	}
}

// THE BOOT SCRIPT IS RUN, NOT PATTERN-MATCHED.
//
// The registration reaches the runner through a single-quoted shell assignment,
// and every part of that — the quoting, the export, the command's own argv — is a
// thing that looks right and can be wrong in a way no substring assertion
// notices. So the script billet generates is executed by a real /bin/sh, with the
// command replaced by one that prints what the runner would have read.
//
// A truncated or expanded credential means a runner that cannot register, which
// surfaces as a job that stays queued while every signal says the launch worked.
func TestTheRegistrationReachesTheGuestByteForByte(t *testing.T) {
	for name, jit := range map[string]string{
		"ordinary base64":   "eyJydW5uZXIiOiJqaXQifQ==",
		"dollars and ticks": `a$b'c"d` + "`e`",
		"backslashes":       `a\b\\c`,
		"shell metachars":   "a;b|c&d>e<f*g?h[i]j{k}l",
		"leading dash":      "-n",
		// Not expected from GitHub, and handled rather than refused: a single
		// quoted word carries a newline literally, so there is no reason to
		// invent a restriction that would fail a launch for a value billet can
		// deliver correctly.
		"newlines": "line one\nline two",
		// The sequence that ends a single-quoted word. If the escaping is wrong
		// this is what finds it.
		"the escape sequence itself": `a'\''b`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeEC2(t)
			p := newTestProvider(t, f, nil)

			spec := validSpec()
			spec.JITConfig = jit
			// Stands in for the runner: prints exactly what it was handed.
			spec.Command = []string{"/bin/sh", "-c", `printf %s "$` + jitEnvVar + `"`}

			script, err := p.userData(spec)
			if err != nil {
				t.Fatalf("userData: %v", err)
			}

			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the generated boot script did not run: %v\n%s\n--- script ---\n%s",
					err, out, script)
			}

			if string(out) != jit {
				t.Errorf("the runner would have read %q, want %q\n--- script ---\n%s",
					out, jit, script)
			}
		})
	}
}

// A NUL IS THE ONE BYTE NO SHELL CAN CARRY, so it is refused rather than
// truncating the value at it.
//
// The direction matters: a credential silently cut short produces a runner that
// cannot register, which surfaces as a job sitting queued while the launch, the
// instance and the logs all report success. Refusing turns that into a launch
// failure the caller already knows how to hand capacity back for.
func TestAValueNoShellCanCarryIsRefusedRatherThanTruncated(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	for name, mutate := range map[string]func(*provider.Spec){
		"in the registration": func(s *provider.Spec) { s.JITConfig = "abc\x00def" },
		"in the command":      func(s *provider.Spec) { s.Command = []string{"./run.sh", "a\x00b"} },
	} {
		t.Run(name, func(t *testing.T) {
			spec := validSpec()
			mutate(&spec)

			if _, err := p.userData(spec); err == nil {
				t.Fatal("a value the shell cannot carry was written into a boot script anyway")
			}
		})
	}
}

// The command reaches the guest as the argument vector billet was given, so an
// image needing arguments does not have to smuggle them through a shell.
func TestACommandWithAwkwardArgumentsSurvivesTheBootScript(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.Command = []string{"/bin/sh", "-c", `printf '%s\n' "$@"`, "sh",
		"a b", "it's", `$HOME`, "*"}

	script, err := p.userData(spec)
	if err != nil {
		t.Fatalf("userData: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("the generated boot script did not run: %v\n%s", err, out)
	}

	want := "a b\nit's\n$HOME\n*\n"
	if string(out) != want {
		t.Errorf("the command arrived as %q, want %q", out, want)
	}
}

// And the script is what is actually sent, base64-encoded as EC2 requires.
func TestTheBootScriptIsWhatIsSent(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	spec := validSpec()

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	encoded := f.paramsFor(t, "RunInstances").Get("UserData")

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("UserData is not base64: %v", err)
	}

	if !strings.Contains(string(decoded), spec.JITConfig) {
		t.Error("the registration did not reach the user data")
	}
}

// AN ALREADY-GONE INSTANCE IS SUCCESS, matched on the API's own code rather than
// its prose. Teardown runs on paths that have already failed once, and a caller
// reads a destroy error as "the compute may still exist" and keeps holding the
// capacity.
func TestDestroyingSomethingAlreadyGoneIsSuccess(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusBadRequest, apiFailure("InvalidInstanceID.NotFound")
	}

	p := newTestProvider(t, f, nil)

	if err := p.Destroy(t.Context(), "i-0abc"); err != nil {
		t.Errorf("destroying an instance that is already gone failed: %v", err)
	}
}

// Anything else is a real failure and must not be swallowed: a teardown failure
// that reads as success is an instance running that nothing is tracking, billed.
func TestDestroyDoesNotSwallowARealFailure(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusBadRequest, apiFailure("UnauthorizedOperation")
	}

	p := newTestProvider(t, f, nil)

	if err := p.Destroy(t.Context(), "i-0abc"); err == nil {
		t.Error("a destroy that was refused for a reason billet cannot act on reported success")
	}
}

// LIST FOLLOWS PAGINATION TO THE END.
//
// These results feed reconciliation and teardown, so a truncated list reads as
// "that lease is not running on this node" — which frees capacity for a machine
// still executing a job, and destroys nothing, so nobody finds out until the bill.
func TestListFollowsEveryPage(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if params.Get("NextToken") == "" {
			return http.StatusOK, describeReply("page2",
				instanceXML("i-1", "billet-lease-1"))
		}

		return http.StatusOK, describeReply("",
			instanceXML("i-2", "billet-lease-2"))
	}

	p := newTestProvider(t, f, nil)

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("List returned %d instances, want both pages", len(got))
	}
}

// A PAGINATION TOKEN BILLET HAS ALREADY SEEN ENDS THE WALK.
//
// A node whose sweep never returns stops reporting its inventory, and the
// capacity of anything quarantined on it is held until an operator intervenes.
//
// The CYCLE case is the one that matters, and it is why this does not simply
// compare against the previous token: A, B, A, B repeats nothing consecutively
// and loops forever. The context is bounded so that a regression fails the test
// rather than hanging the suite.
func TestListRefusesToLoopOnATokenItHasSeen(t *testing.T) {
	for name, tokens := range map[string][]string{
		"the same token twice": {"same", "same"},
		"a cycle":              {"a", "b", "a", "b"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeEC2(t)

			var n int

			f.respond = func(string, url.Values) (int, string) {
				tok := tokens[n%len(tokens)]
				n++

				return http.StatusOK, describeReply(tok, instanceXML("i-1", "billet-lease-1"))
			}

			p := newTestProvider(t, f, nil)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			_, err := p.List(ctx)
			if err == nil {
				t.Fatal("List followed a pagination token it had already seen instead of refusing")
			}

			if !strings.Contains(err.Error(), "pagination token") {
				t.Errorf("List failed for some other reason than the loop it should refuse: %v", err)
			}
		})
	}
}

// FILTERS ARE NUMBERED WITHOUT GAPS, and this is not cosmetic.
//
// The query API numbers a list from 1 with no holes. An earlier version hard-coded
// `Filter.2` for the name filter, which left List — which adds none — emitting
// Filter.1 and Filter.3. A DescribeInstances whose state filter is dropped returns
// an hour of terminated instances, and reconciliation reads those as compute to
// account for.
func TestEveryDescribeNumbersItsFiltersWithoutGaps(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	if _, err := p.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}

	if _, _, err := p.Find(t.Context(), "billet-lease-1"); err != nil {
		t.Fatalf("Find: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	for i, call := range f.calls {
		var names []string

		for n := 1; ; n++ {
			v := call.Get(fmt.Sprintf("Filter.%d.Name", n))
			if v == "" {
				break
			}

			names = append(names, v)
		}

		// Every filter the call carries must be reachable by walking from 1.
		var total int

		for key := range call {
			if strings.HasSuffix(key, ".Name") && strings.HasPrefix(key, "Filter.") {
				total++
			}
		}

		if len(names) != total {
			t.Errorf("call %d numbers %d filters but only %d are reachable from Filter.1: %v",
				i, total, len(names), call)
		}

		// And the state filter must be among them, or terminated instances come
		// back.
		if !strings.Contains(strings.Join(names, ","), "instance-state-name") {
			t.Errorf("call %d has no instance-state-name filter: %v", i, names)
		}
	}
}

// Find compares the name exactly after filtering. An EC2 tag filter honours `*`
// as a wildcard, so a name carrying one would match more than itself — and the
// caller's next move on a hit is to terminate.
func TestFindComparesTheNameExactly(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusOK, describeReply("",
			instanceXML("i-2", "billet-lease-10"))
	}

	p := newTestProvider(t, f, nil)

	_, found, err := p.Find(t.Context(), "billet-lease-1")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	if found {
		t.Error("Find returned an instance whose name is not the one asked for")
	}
}

// A stopped instance is the one state that looks alive and is not: billet never
// stops one, so it was stopped by somebody else and will not resume — adopting it
// holds a lease open forever for a job that cannot finish.
func TestWhichStatesCountAsRunning(t *testing.T) {
	for state, want := range map[string]bool{
		"pending":       true,
		"running":       true,
		"shutting-down": true,
		"stopping":      true,
		"stopped":       false,
		"terminated":    false,
		// A state billet has never heard of is not evidence a job is over, and
		// the caller destroys what is not running.
		"hibernating-something-new": true,
	} {
		if got := runningState(state); got != want {
			t.Errorf("runningState(%q) = %v, want %v", state, got, want)
		}
	}
}

// A THROTTLE IS "NOT NOW"; A REJECTED PARAMETER IS "NO".
func TestAThrottleIsRetriedAndARefusalIsNot(t *testing.T) {
	for name, tc := range map[string]struct {
		code      string
		wantCalls int
	}{
		"throttled":         {code: "RequestLimitExceeded", wantCalls: maxAttempts},
		"invalid parameter": {code: "InvalidParameterValue", wantCalls: 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeEC2(t)
			// SCOPED TO THE ACTION UNDER TEST. A launch resolves the AMI's root
			// device first, so failing every action would exercise the retry on
			// DescribeImages and never reach RunInstances at all.
			f.respond = func(action string, _ url.Values) (int, string) {
				if action != "RunInstances" {
					return http.StatusOK, defaultReply(action)
				}

				return http.StatusBadRequest, apiFailure(tc.code)
			}

			p := newTestProvider(t, f, nil)
			p.api.sleep = func(context.Context, time.Duration) error { return nil }

			if _, err := p.Launch(t.Context(), validSpec()); err == nil {
				t.Fatal("a refused launch reported success")
			}

			if got := f.countOf("RunInstances"); got != tc.wantCalls {
				t.Errorf("RunInstances was attempted %d times, want %d", got, tc.wantCalls)
			}
		})
	}
}

// A launch that succeeds on the retry is a success, not a failure.
func TestACallThatSucceedsAfterAThrottleSucceeds(t *testing.T) {
	f := newFakeEC2(t)

	var attempts int

	// Only the launch itself stumbles, so what is being measured is the retry of
	// the call under test rather than of the image lookup in front of it.
	f.respond = func(action string, _ url.Values) (int, string) {
		if action != "RunInstances" {
			return http.StatusOK, defaultReply(action)
		}

		attempts++
		if attempts == 1 {
			return http.StatusServiceUnavailable, apiFailure("Unavailable")
		}

		return http.StatusOK, defaultReply(action)
	}

	p := newTestProvider(t, f, nil)
	p.api.sleep = func(context.Context, time.Duration) error { return nil }

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("a launch that succeeded on its second attempt reported failure: %v", err)
	}
}

// AN INCOMPLETE INVENTORY MUST NOT BE REPORTED AS A COMPLETE ONE.
//
// An instance carrying billet's owner tag but no name is billet's compute that
// cannot be matched to a lease. Dropping it from the list is the worst available
// answer, because of what the list is FOR: the control plane frees quarantined
// capacity for any lease absent from a node's inventory, so a silently shortened
// list hands back capacity for a machine that is still running.
//
// This is the docker backend's rule, arrived at for the same reason — a line it
// cannot parse fails List rather than being skipped. An earlier version of this
// function logged a warning and continued, while its own comment claimed it
// reported. Failing closed is what the comment always said.
func TestAnIncompleteInventoryIsRefusedRatherThanShortened(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusOK, `<DescribeInstancesResponse><reservationSet><item><instancesSet>` +
			`<item><instanceId>i-9</instanceId><instanceState><name>running</name></instanceState>` +
			`<tagSet><item><key>sh.billet.owner</key><value>dep-1</value></item></tagSet></item>` +
			`</instancesSet></item></reservationSet></DescribeInstancesResponse>`
	}

	p := newTestProvider(t, f, nil)

	got, err := p.List(t.Context())
	if err == nil {
		t.Fatalf("an inventory billet knows is incomplete was returned as authoritative: %+v", got)
	}

	if !strings.Contains(err.Error(), "i-9") {
		t.Errorf("the error does not name the instance an operator has to go and find: %v", err)
	}
}

// A PRESENT-BUT-USELESS NAME IS THE SAME FAILURE AS AN ABSENT ONE.
//
// The guard used to ask only whether the tag EXISTED, so `<value/>` produced an
// instance named "" and a name billet never assigned produced one it cannot map
// to a lease — both landing in the inventory as though they were accounted for,
// which is the reconciliation hazard the missing-tag case was fixed for, one
// field value away.
func TestANameThatCannotIdentifyALeaseIsRefused(t *testing.T) {
	for name, value := range map[string]string{
		"empty":            "",
		"not billet's":     "someone-elses-instance",
		"the prefix alone": "billet-",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeEC2(t)
			f.respond = func(string, url.Values) (int, string) {
				return http.StatusOK, `<DescribeInstancesResponse><reservationSet><item>` +
					`<instancesSet><item><instanceId>i-9</instanceId>` +
					`<instanceState><name>running</name></instanceState><tagSet>` +
					`<item><key>Name</key><value>` + value + `</value></item>` +
					`<item><key>sh.billet.owner</key><value>dep-1</value></item>` +
					`</tagSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`
			}

			p := newTestProvider(t, f, nil)

			if got, err := p.List(t.Context()); err == nil {
				t.Fatalf("an instance billet cannot match to a lease was reported as "+
					"accounted for: %+v", got)
			}
		})
	}
}

// An instance with no ID is the same failure one field over, and the same answer.
func TestAnInstanceWithNoIDIsRefused(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusOK, `<DescribeInstancesResponse><reservationSet><item><instancesSet>` +
			`<item><instanceId></instanceId><instanceState><name>running</name></instanceState>` +
			`<tagSet><item><key>Name</key><value>billet-lease-1</value></item>` +
			`<item><key>sh.billet.owner</key><value>dep-1</value></item></tagSet></item>` +
			`</instancesSet></item></reservationSet></DescribeInstancesResponse>`
	}

	p := newTestProvider(t, f, nil)

	if got, err := p.List(t.Context()); err == nil {
		t.Fatalf("an instance with no id was reported as something billet could destroy: %+v", got)
	}
}

// A RETRY MUST NOT ACCUMULATE THE PREVIOUS ATTEMPT'S PARTIAL RESPONSE.
//
// encoding/xml APPENDS to slices, and the decode target used to be shared across
// attempts. So a first attempt that failed partway through unmarshalling — a
// truncated body, a connection cut mid-response — left rows in the target, and
// the retry appended a full set to them. DescribeInstances would then report an
// instance twice, and List feeds a loop that destroys.
func TestARetryDoesNotAccumulateThePreviousAttemptsRows(t *testing.T) {
	f := newFakeEC2(t)

	var attempts int

	f.respond = func(action string, _ url.Values) (int, string) {
		if action != "DescribeInstances" {
			return http.StatusOK, defaultReply(action)
		}

		attempts++
		if attempts == 1 {
			// A 200 whose body stops mid-document: the decoder fills what it has
			// read and then fails, which is exactly the state that used to persist.
			return http.StatusOK, `<DescribeInstancesResponse><reservationSet><item><instancesSet>` +
				instanceXML("i-1", "billet-lease-1") +
				instanceXML("i-2", "billet-lease-2")
		}

		return http.StatusOK, describeReply("",
			instanceXML("i-1", "billet-lease-1"), instanceXML("i-2", "billet-lease-2"))
	}

	p := newTestProvider(t, f, nil)
	p.api.sleep = func(context.Context, time.Duration) error { return nil }

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("List reported %d instances after a retry, want 2; the first attempt's partial "+
			"rows were kept: %+v", len(got), got)
	}
}

// A public address can only be requested through a network interface block, and
// a request carrying both a top-level SubnetId and an interface is refused — so
// the two spellings must not be mixed.
func TestAPublicAddressUsesTheInterfaceSpellingOnly(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, func(c *config.EC2Config) { c.AssignPublicIP = true })

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	if got.Get("SubnetId") != "" || got.Get("SecurityGroupId.1") != "" {
		t.Error("both spellings were sent; EC2 refuses a request carrying an interface and a " +
			"top-level subnet")
	}

	for key, want := range map[string]string{
		"NetworkInterface.1.DeviceIndex":              "0",
		"NetworkInterface.1.SubnetId":                 "subnet-0abc",
		"NetworkInterface.1.AssociatePublicIpAddress": "true",
		"NetworkInterface.1.SecurityGroupId.1":        "sg-trusted",
	} {
		if got.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, got.Get(key), want)
		}
	}
}

// A provider with no deployment identity cannot tell its own compute from another
// billet's, and List feeds a loop that terminates.
func TestAProviderWithoutADeploymentIdentityIsRefused(t *testing.T) {
	if _, err := New("  ", config.EC2Config{Region: "us-west-2"}); err == nil {
		t.Fatal("a provider with no owner was built; its List would match another billet's " +
			"instances")
	}
}

// THE ENDPOINT RULE IS RE-APPLIED BY THE CONSTRUCTOR, not left to config
// validation alone.
//
// New is exported, so it cannot assume its configuration came through
// config.Load — and the rule is that a signed request carrying a session token
// never goes out in plaintext. Loopback is the exception, which is both billet's
// existing trust-boundary rule and what lets these tests point at an httptest
// server.
func TestAProviderRefusesAPlaintextEndpoint(t *testing.T) {
	cfg := validEC2Config("http://ec2.us-west-2.amazonaws.com/")

	if _, err := New("dep-1", cfg); err == nil {
		t.Fatal("a provider was built against a plaintext endpoint, which would send a session " +
			"token in the clear")
	}

	// And loopback still works, or every test in this file would be impossible.
	if _, err := New("dep-1", validEC2Config("http://127.0.0.1:9/")); err != nil {
		t.Errorf("a loopback endpoint was refused: %v", err)
	}
}

// A REDIRECT MUST NOT CARRY A SIGNED REQUEST SOMEWHERE ELSE.
//
// The endpoint is validated as https, and then Go's client follows redirects by
// default — so a 307 preserves the method and body, and the hop can be plaintext
// or another host entirely. Everything the endpoint check exists to prevent
// happens one response later, to a URL nobody validated.
//
// AWS does not redirect. That is exactly why this must be refused rather than
// followed: a redirect from this endpoint is not the API answering.
func TestASignedRequestIsNotFollowedToARedirect(t *testing.T) {
	var reached int

	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached++
	}))

	t.Cleanup(elsewhere.Close)

	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusTemporaryRedirect, ""
	}

	p := newTestProvider(t, f, nil)

	// The fake sets Location via respond's status only, so add it here.
	f.redirectTo = elsewhere.URL

	if _, err := p.List(t.Context()); err == nil {
		t.Fatal("a signed request followed a redirect")
	}

	if reached != 0 {
		t.Errorf("a signed request reached the redirect target %d time(s)", reached)
	}
}

// THE CONSTRUCTOR RE-APPLIES THE SECURITY-GROUP RULE, not only config
// validation. New is exported, so `New(..., EC2Config{UntrustedSecurityGroupIDs:
// []string{""}})` would otherwise admit fork pull-request work on the strength of
// a list holding one empty string — Accepts gates on length.
func TestAProviderRefusesABlankSecurityGroup(t *testing.T) {
	for name, mutate := range map[string]func(*config.EC2Config){
		"trusted":   func(c *config.EC2Config) { c.SecurityGroupIDs = []string{"sg-a", ""} },
		"untrusted": func(c *config.EC2Config) { c.UntrustedSecurityGroupIDs = []string{"  "} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validEC2Config("https://ec2.us-west-2.amazonaws.com/")
			mutate(&cfg)

			if _, err := New("dep-1", cfg); err == nil {
				t.Fatal("a provider was built with a blank security group")
			}
		})
	}
}

// THE REGION CHOOSES THE HOST WHEN NO ENDPOINT IS SET, so an unvalidated one is a
// way to send a signed request and its session token somewhere else entirely.
//
// Measured: `x@attacker.example/?` interpolated into the default endpoint yields
// a url whose host is attacker.example. The constructor re-applies the region
// rule for that reason and not merely for tidiness.
func TestAProviderRefusesARegionThatWouldChooseTheHost(t *testing.T) {
	for name, region := range map[string]string{
		"userinfo and query": "x@attacker.example/?",
		"a whole url":        "x.amazonaws.com/../..",
		"empty":              "",
		"a typo":             "uswest2",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validEC2Config("")
			cfg.Region = region

			if _, err := New("dep-1", cfg); err == nil {
				t.Fatal("a provider was built with a region billet cannot trust to name a host")
			}
		})
	}
}

// A config with NO trusted group at all reaches RunInstances without one, and EC2
// then picks the VPC's default — which in a VPC somebody already had usually
// permits a good deal more than they are picturing.
func TestAProviderRefusesAConfigWithNoTrustedGroup(t *testing.T) {
	cfg := validEC2Config("https://ec2.us-west-2.amazonaws.com/")
	cfg.SecurityGroupIDs = nil

	if _, err := New("dep-1", cfg); err == nil {
		t.Fatal("a provider was built with no security group, so EC2 would choose the VPC default")
	}
}

// AN OPTION MUST NOT PRODUCE A PANIC. billet bans panic outright: a control plane
// that panics drops every in-flight lease.
func TestAProviderRefusesANilHTTPClient(t *testing.T) {
	cfg := validEC2Config("https://ec2.us-west-2.amazonaws.com/")

	if _, err := New("dep-1", cfg, WithHTTPClient(nil)); err == nil {
		t.Fatal("a provider was built with no http client")
	}
}

// THE CONFIGURATION IS THE PROVIDER'S ONCE IT IS BUILT.
//
// A caller keeps the slices it passed, so without a copy it can widen a security
// group after construction — moving a fork's job onto a privileged network after
// the validation meant to prevent exactly that. NodePolicy.Clone exists for the
// same reason one layer up.
func TestAProviderKeepsItsOwnCopyOfTheNetwork(t *testing.T) {
	f := newFakeEC2(t)

	cfg := validEC2Config(f.URL)
	cfg.UntrustedSecurityGroupIDs = []string{"sg-fork"}

	p, err := New("dep-1", cfg,
		WithHTTPClient(f.Client()),
		WithCredentials(StaticCredentials{AccessKeyID: "AKID", SecretAccessKey: "s"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// EVERY MUTABLE FIELD, not just the one that first suggested the rule —
	// removing any single clone would otherwise leave this green.
	cfg.UntrustedSecurityGroupIDs[0] = "sg-privileged"
	cfg.SecurityGroupIDs[0] = "sg-privileged"
	cfg.InstanceTypes[0].Type = "m7i.metal-48xl"

	spec := validSpec()
	spec.Trust = provider.TrustUntrusted

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	if g := got.Get("SecurityGroupId.1"); g != "sg-fork" {
		t.Errorf("a fork's job was placed on %q, which the caller substituted after the "+
			"provider had validated the network", g)
	}

	if g := got.Get("InstanceType"); g != "c7i.2xlarge" {
		t.Errorf("instance type = %q, which the caller substituted after construction", g)
	}

	// And the trusted list too, through a launch that uses it.
	trusted := validSpec()

	if _, err := p.Launch(t.Context(), trusted); err != nil {
		t.Fatalf("Launch (trusted): %v", err)
	}

	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()

	if g := last.Get("SecurityGroupId.1"); g != "sg-trusted" {
		t.Errorf("a trusted job was placed on %q, which the caller substituted", g)
	}
}

// A PADDED REGION MUST NOT BE VALIDATED AS ONE THING AND SIGNED AS ANOTHER.
//
// config.Load normalizes; a direct caller does not, and this constructor exists
// precisely for the caller who never went through Load. Trimming a local copy to
// validate it and then signing with the original produced a request that dialled
// the right host with spaces in its credential scope — which AWS answers with a
// 403 naming nothing.
func TestAPaddedRegionIsSignedAsTheTrimmedOne(t *testing.T) {
	f := newFakeEC2(t)

	cfg := validEC2Config(f.URL)
	cfg.Region = "  us-west-2  "

	p, err := New("dep-1", cfg,
		WithHTTPClient(f.Client()),
		WithCredentials(StaticCredentials{AccessKeyID: "AKID", SecretAccessKey: "s"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := p.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}

	auth := f.lastAuthorization()
	if !strings.Contains(auth, "/us-west-2/ec2/aws4_request") {
		t.Errorf("the credential scope is %q, want the trimmed region", auth)
	}
}

// A REFUSED REDIRECT MUST NOT CARRY THE TARGET'S QUERY STRING INTO AN ERROR.
//
// The callback names only the host, and that is not enough on its own:
// net/http wraps whatever it returns in a *url.Error, and THAT renders the whole
// target url. So the refusal is a sentinel the call boundary recognises and
// replaces, rather than one it wraps again.
func TestARefusedRedirectDoesNotRenderTheTarget(t *testing.T) {
	const marker = "hunter2hunter2"

	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	t.Cleanup(elsewhere.Close)

	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusTemporaryRedirect, ""
	}
	f.redirectTo = elsewhere.URL + "/?token=" + marker

	p := newTestProvider(t, f, nil)

	_, err := p.List(t.Context())
	if err == nil {
		t.Fatal("a signed request followed a redirect")
	}

	if strings.Contains(err.Error(), marker) {
		t.Errorf("the refusal carried the redirect target's query string: %v", err)
	}

	// AND THE IDENTITY SURVIVES THE WRAPPING, which is what lets retryable tell a
	// redirect from a transport failure rather than repeating a verdict.
	if !errors.Is(err, errRedirected) {
		t.Errorf("the refusal lost its identity on the way out: %v", err)
	}
}

// A REFUSED REDIRECT IS NOT "NOT NOW", so it is not retried.
//
// retryable treats anything that is not an apiError as a transport failure worth
// repeating, which is right for a connection that dropped and wrong for this: an
// endpoint that answers with a redirect will answer with one again, so the retries
// are three round trips that cannot change the outcome — and each is a signed
// request handed to whatever is answering.
func TestARefusedRedirectIsNotRetried(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	t.Cleanup(elsewhere.Close)

	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusTemporaryRedirect, ""
	}
	f.redirectTo = elsewhere.URL

	p := newTestProvider(t, f, nil)
	p.api.sleep = func(context.Context, time.Duration) error { return nil }

	if _, err := p.List(t.Context()); err == nil {
		t.Fatal("a signed request followed a redirect")
	}

	if got := f.countOf("DescribeInstances"); got != 1 {
		t.Errorf("a redirecting endpoint was asked %d times; a redirect will not become a "+
			"non-redirect, so each retry is another signed request for nothing", got)
	}
}

// THE SUFFIX IS NOT THE SAME IN EVERY PARTITION, and the region rule deliberately
// admits partitions billet has never run in — so deriving the commercial suffix
// for all of them produces a host that does not exist, and a config that loads
// cleanly and fails at the first API call.
func TestTheDerivedEndpointFollowsThePartition(t *testing.T) {
	for region, want := range map[string]string{
		"us-west-2":      "https://ec2.us-west-2.amazonaws.com/",
		"us-gov-west-1":  "https://ec2.us-gov-west-1.amazonaws.com/",
		"cn-north-1":     "https://ec2.cn-north-1.amazonaws.com.cn/",
		"cn-northwest-1": "https://ec2.cn-northwest-1.amazonaws.com.cn/",
	} {
		t.Run(region, func(t *testing.T) {
			cfg := validEC2Config("")
			cfg.Region = region

			p, err := New("dep-1", cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			if got := p.api.endpoint; got != want {
				t.Errorf("endpoint = %q, want %q", got, want)
			}
		})
	}
}

// AN INSTANCE PROFILE IS A CREDENTIAL, AND UNTRUSTED WORK MUST NOT BE HANDED ONE.
//
// This backend may run fork pull-request code because an instance is a real
// isolation boundary. That boundary does not extend to IMDS: a workflow step runs
// directly ON the instance, not in a container, so the one-hop metadata limit —
// which stops a container reaching it — stops nothing here. A fork's job with an
// instance profile attached can read the role's temporary credentials straight
// out of the metadata service and take them away.
func TestUntrustedWorkIsNotHandedAnInstanceProfile(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, func(c *config.EC2Config) {
		c.InstanceProfile = "billet-runner"
		c.UntrustedSecurityGroupIDs = []string{"sg-fork"}
	})

	spec := validSpec()
	spec.Trust = provider.TrustUntrusted

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := f.paramsFor(t, "RunInstances").Get("IamInstanceProfile.Name"); got != "" {
		t.Errorf("a fork's job was given the %q instance profile; its steps can read the "+
			"role's credentials out of IMDS", got)
	}
}

// And trusted work still gets it, or the option would mean nothing.
func TestTrustedWorkStillGetsTheInstanceProfile(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, func(c *config.EC2Config) { c.InstanceProfile = "billet-runner" })

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := f.paramsFor(t, "RunInstances").Get("IamInstanceProfile.Name"); got != "billet-runner" {
		t.Errorf("IamInstanceProfile.Name = %q, want the configured profile", got)
	}
}

// THE SERVICE'S OWN MESSAGE IS NOT BILLET'S TO REPEAT WHEN IT MIGHT CARRY THE
// REGISTRATION.
//
// RunInstances sends the JIT config as UserData, and an API error renders the
// service's <Message> verbatim into an error that travels back through the node's
// command result and into logs. AWS is unlikely to echo a rejected parameter that
// large — but the endpoint is configurable, a proxy sits in some deployments, and
// the JIT config is a live runner registration until it is consumed. Whether the
// echo is likely is not the question; whether the credential can travel is.
func TestALaunchErrorCannotCarryTheRegistration(t *testing.T) {
	f := newFakeEC2(t)
	spec := validSpec()

	f.respond = func(action string, params url.Values) (int, string) {
		if action != "RunInstances" {
			return http.StatusOK, defaultReply(action)
		}

		// The service echoes the parameter it rejected, user data included.
		return http.StatusBadRequest, `<Response><Errors><Error>` +
			`<Code>InvalidParameterValue</Code><Message>Invalid value '` +
			params.Get("UserData") + `' for parameter UserData</Message>` +
			`</Error></Errors></Response>`
	}

	p := newTestProvider(t, f, nil)
	p.api.sleep = func(context.Context, time.Duration) error { return nil }

	_, err := p.Launch(t.Context(), spec)
	if err == nil {
		t.Fatal("a rejected launch reported success")
	}

	// BOTH FORMS. The first version looked only for the raw registration and
	// passed while the error carried the BASE64 user data containing it — which is
	// a disclosure with one decode step in front of it, and exactly the kind of
	// pass-for-the-wrong-reason this project keeps finding.
	script, err2 := p.userData(spec)
	if err2 != nil {
		t.Fatalf("userData: %v", err2)
	}

	for name, secret := range map[string]string{
		"the registration": spec.JITConfig,
		"the boot script":  base64.StdEncoding.EncodeToString([]byte(script)),
	} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the launch error carries %s: %v", name, err)
		}
	}

	// The diagnosis still has to survive, or this trade is a bad one.
	if !strings.Contains(err.Error(), "InvalidParameterValue") {
		t.Errorf("the error lost the api's own code, which is what an operator acts on: %v", err)
	}
}

// EC2 CAPS USER DATA AT 16 KiB, and a tier's command has no bound of its own.
//
// Refused BEFORE the launch, because by the time RunInstances rejects it a JIT
// registration has already been minted against GitHub — and a launch that fails
// ambiguously holds the lease in custody, since absence from one Find is not proof
// nothing started. A local size check turns all of that into an ordinary refusal.
func TestABootScriptTooLargeForEC2IsRefusedLocally(t *testing.T) {
	f := newFakeEC2(t)
	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.Command = []string{"/bin/sh", "-c", strings.Repeat("x", 17<<10)}

	if _, err := p.Launch(t.Context(), spec); err == nil {
		t.Fatal("a boot script larger than EC2 accepts was sent anyway")
	}

	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("a launch that cannot succeed still called RunInstances %d times", n)
	}
}

// THE THREAT IS AN ECHO OF THE RAW BODY, WHICH IS NOT THE SECRET IN ANY FORM
// BILLET HOLDS.
//
// The registration travels as base64 inside a form-encoded body, so an
// intermediary that repeats what it received contains neither the raw
// registration nor the raw base64 — it contains the URL-escaped version of the
// second. Substituting known forms out of the message was tried first and could
// not cover that: any list of encodings is a guess at what somebody else did.
//
// This fake echoes the body EXACTLY as it arrived, which is the case the earlier
// fake could not produce because it parsed the form before echoing.
func TestALaunchErrorCannotCarryTheRegistrationInAnyEncoding(t *testing.T) {
	spec := validSpec()

	var raw string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)

			return
		}

		if strings.Contains(string(body), "RunInstances") {
			raw = string(body)

			// ESCAPED, so the document PARSES and the code is available. An
			// unescaped body breaks the XML, billet falls back to reporting the
			// status alone, and the test would pass without the message ever having
			// been rendered — proving nothing about the thing under test.
			var escaped strings.Builder
			if err := xml.EscapeText(&escaped, body); err != nil {
				t.Errorf("escape: %v", err)

				return
			}

			w.WriteHeader(http.StatusBadRequest)
			write(t, w, `<Response><Errors><Error><Code>InvalidParameterValue</Code>`+
				`<Message>rejected: `+escaped.String()+`</Message></Error></Errors></Response>`)

			return
		}

		write(t, w, defaultReply("DescribeImages"))
	}))

	t.Cleanup(srv.Close)

	p := newTestProvider(t, newFakeEC2(t), func(c *config.EC2Config) { c.Endpoint = srv.URL })
	p.api.sleep = func(context.Context, time.Duration) error { return nil }

	_, err := p.Launch(t.Context(), spec)
	if err == nil {
		t.Fatal("a rejected launch reported success")
	}

	if raw == "" {
		t.Fatal("the fake never saw a RunInstances body, so this proves nothing")
	}

	// The body really did contain the registration, or the test would be vacuous.
	if !strings.Contains(raw, url.QueryEscape(base64.StdEncoding.EncodeToString(
		[]byte(mustUserData(t, p, spec))))) {
		t.Fatal("the request body did not carry the encoded boot script, so this test is not " +
			"exercising the case it names")
	}

	for _, secret := range []string{
		spec.JITConfig,
		base64.StdEncoding.EncodeToString([]byte(mustUserData(t, p, spec))),
		url.QueryEscape(base64.StdEncoding.EncodeToString([]byte(mustUserData(t, p, spec)))),
	} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the launch error carries the registration: %v", err)
		}
	}

	if !strings.Contains(err.Error(), "InvalidParameterValue") {
		t.Errorf("the error lost the api's own code, which is what an operator acts on: %v", err)
	}
}

func mustUserData(t *testing.T, p *Provider, spec provider.Spec) string {
	t.Helper()

	script, err := p.userData(spec)
	if err != nil {
		t.Fatalf("userData: %v", err)
	}

	return script
}
