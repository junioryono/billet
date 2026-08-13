package ec2

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
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

// allParamsFor returns the parameters of every call to an action, in order.
//
// For the case where the interesting request is neither the first nor the last —
// checking the MIDDLE launch is what catches a value that was hardcoded to the
// one every other fixture happens to use.
func (f *fakeEC2) allParamsFor(action string) []url.Values {
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

// lastParamsFor returns the parameters of the MOST RECENT call to an action.
//
// paramsFor deliberately refuses when there was more than one call, which is the
// right default — a test that means to inspect "the" request should say so. This
// is for the case where repetition IS the subject: proving that the third launch
// of an image still carries what the first lookup found.
func (f *fakeEC2) lastParamsFor(t *testing.T, action string) url.Values {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Get("Action") == action {
			return f.calls[i]
		}
	}

	t.Fatalf("%s was never called", action)

	return nil
}

// blockDevices reads the numbered BlockDeviceMapping parameters back by device
// name, and refuses to hide the two things a name-keyed map otherwise hides.
//
// A DUPLICATE IS AN ERROR, NOT AN OVERWRITE. Reducing numbered entries by name is
// the natural way to assert them, and it is exactly how a mutant that sends one
// device TWICE stays invisible: the second entry replaces the first, the map still
// has the expected keys, and EC2 would have received two mappings for one device
// disagreeing about its flag.
//
// AND AN EMPTY FLAG IS AN ERROR ANYWHERE, since a device leaving without one is
// the whole defect #53 closed — worth checking wherever the request is read rather
// than only in the test that happens to be about it.
func blockDevices(t *testing.T, got url.Values) map[string]string {
	t.Helper()

	out := map[string]string{}

	// THE HORIZON COMES FROM THE KEYS, not from a scan that gives up after a few
	// holes. Stopping at the first gap would make the contract above a lie, and
	// stopping after N gaps just moves the lie further out — a mapping stranded at
	// index 40 is exactly the kind a mutant would add.
	highest := 0

	for key := range got {
		var n int

		if _, err := fmt.Sscanf(key, "BlockDeviceMapping.%d.", &n); err == nil && n > highest {
			highest = n
		}
	}

	gap := false

	for i := 1; i <= highest; i++ {
		n := strconv.Itoa(i)

		device := got.Get("BlockDeviceMapping." + n + ".DeviceName")
		if device == "" {
			gap = true

			continue
		}

		if gap {
			t.Errorf("%s was sent at index %s, after a gap; these are a list and billet's own "+
				"comment says a dense one is the only shape it can rely on", device, n)
		}

		if _, seen := out[device]; seen {
			t.Errorf("%s was sent more than once, so EC2 would receive two mappings for one "+
				"device", device)
		}

		flag := got.Get("BlockDeviceMapping." + n + ".Ebs.DeleteOnTermination")
		if flag == "" {
			t.Errorf("device %s left with no DeleteOnTermination, which is the whole defect "+
				"#53 exists to close", device)
		}

		out[device] = flag
	}

	return out
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
// launch affordable. An AMI's block devices do not change.
//
// AND WHAT IS CACHED HAS TO BE THE WHOLE ANSWER. Counting the calls proves only
// that billet stopped asking — it says nothing about what it kept, so caching just
// the root device name passed this test while every launch AFTER the first
// silently dropped its non-root overrides and went back to depending on the
// default this entire change exists to stop depending on. A defect visible only
// from the SECOND launch of an AMI onward — nearly every launch in a steady state,
// and none at all in a test that launches once.
func TestAnImageIsLookedUpOncePerImage(t *testing.T) {
	// TWO IMAGES WITH DIFFERENT ROOTS, launched A, B, A. One image cannot tell a
	// per-image cache from a single global slot, and cannot tell a cached root from
	// a hardcoded /dev/xvda — every fixture in this file happens to use that name.
	// Interleaving is what makes the second A launch answer both: it must come from
	// A's entry, which B's lookup must not have replaced.
	const (
		imageA = "ami-0abc"
		imageB = "ami-0def"
	)

	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "DescribeImages" {
			return http.StatusOK, defaultReply(action)
		}

		if params.Get("ImageId.1") == imageB {
			return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
				`<imageId>` + imageB + `</imageId>` +
				`<rootDeviceName>/dev/sda1</rootDeviceName>` +
				`<blockDeviceMapping>` +
				`<item><deviceName>/dev/sda1</deviceName><ebs></ebs></item>` +
				`<item><deviceName>/dev/sdz</deviceName><ebs></ebs></item>` +
				`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>` + imageA + `</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<blockDeviceMapping>` +
			`<item><deviceName>/dev/sdb</deviceName><ebs></ebs></item>` +
			`<item><deviceName>/dev/sdc</deviceName><ebs>` +
			`<deleteOnTermination>false</deleteOnTermination></ebs></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}

	var logged bytes.Buffer

	p := newTestProvider(t, f, nil)
	p.log = slog.New(slog.NewTextHandler(&logged, nil))

	// THE THIRD LAUNCH DIFFERS ON PURPOSE. Launches one and three are both image A,
	// so without this they are byte-identical and lastParamsFor could be returning
	// either with nothing to notice. Asking the LAST one for a disk makes the two
	// distinguishable in the only direction that matters.
	for i, image := range []string{imageA, imageB, imageA} {
		spec := validSpec()
		spec.Image = image

		if i == 2 {
			spec.Disk = 80 * config.GiB
		}

		if _, err := p.Launch(t.Context(), spec); err != nil {
			t.Fatalf("Launch %s: %v", image, err)
		}
	}

	if n := f.countOf("DescribeImages"); n != 2 {
		t.Errorf("DescribeImages was called %d times for two images launched three times, "+
			"want 2", n)
	}

	// THE THIRD LAUNCH — image A again, built entirely from cache, and the only
	// request that can show the cache dropping or confusing what it held.
	got := f.lastParamsFor(t, "RunInstances")

	if v := got.Get("BlockDeviceMapping.1.Ebs.VolumeSize"); v != "80" {
		t.Errorf("the last request carries a volume size of %q, want 80 — only the third "+
			"launch asked for a disk, so lastParamsFor is not reading the last request", v)
	}

	want := map[string]string{
		"/dev/xvda": "true",
		"/dev/sdb":  "true",
		"/dev/sdc":  "false",
	}

	seen := blockDevices(t, got)

	for device, flag := range want {
		if seen[device] != flag {
			t.Errorf("on the third launch %s went out as %q, want %q — the cache did not keep "+
				"what the lookup found", device, seen[device], flag)
		}
	}

	if len(seen) != len(want) {
		t.Errorf("the third launch sent %v, want exactly %v — a device from the other image "+
			"leaked across the cache", seen, want)
	}

	// AND THE OTHER IMAGE GOT ITS OWN ROOT. This is the assertion that catches a
	// root hardcoded to /dev/xvda — the name every other fixture in this file uses,
	// including image A's, so A's own request cannot show it. Image B's root is
	// deliberately /dev/sda1, and it is the MIDDLE request.
	runs := f.allParamsFor("RunInstances")
	if len(runs) != 3 {
		t.Fatalf("RunInstances was called %d times, want 3", len(runs))
	}

	if d := runs[1].Get("BlockDeviceMapping.1.DeviceName"); d != "/dev/sda1" {
		t.Errorf("the second image launched with root %q, want /dev/sda1 — its root came from "+
			"somewhere other than its own lookup", d)
	}

	wantB := map[string]string{"/dev/sda1": "true", "/dev/sdz": "true"}

	if b := blockDevices(t, runs[1]); !maps.Equal(b, wantB) {
		t.Errorf("the second image sent %v, want %v — counting them would let any name or "+
			"flag through", b, wantB)
	}

	// AND THE ORDER OF allParamsFor IS PINNED, which the A/B/A shape alone cannot do:
	// reversing it leaves B in the middle either way. Only the two A launches differ,
	// and only in the disk the third one asked for.
	if v := runs[0].Get("BlockDeviceMapping.1.Ebs.VolumeSize"); v != "" {
		t.Errorf("the first request carries a volume size of %q; allParamsFor is not in "+
			"call order", v)
	}

	if v := runs[2].Get("BlockDeviceMapping.1.Ebs.VolumeSize"); v != "80" {
		t.Errorf("the third request carries a volume size of %q, want 80; allParamsFor is "+
			"not in call order", v)
	}

	// SAID ONCE, not once per launch. The warning lives behind the cache precisely
	// so a busy tier does not repeat it for every job.
	if n := strings.Count(logged.String(), "/dev/sdc"); n != 1 {
		t.Errorf("the kept volume was reported %d times across three launches, want 1", n)
	}

	// AT WARN, not Info. A leak reported at the level the routine "launched instance"
	// line uses is one nobody filters for and nobody sees.
	if !strings.Contains(logged.String(), "level=WARN") {
		t.Errorf("the kept volume was not reported as a warning: %s", logged.String())
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

	// AND IT NAMES THE HOST. The closure formats one deliberately — a bare
	// hostname is safe by the same rule that refuses the query — and the call
	// boundary used to discard the whole message in favour of the bare sentinel,
	// so an operator whose endpoint sits behind a redirecting proxy was told only
	// that a redirect had happened.
	host := strings.TrimPrefix(elsewhere.URL, "http://")
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}

	if !strings.Contains(err.Error(), host) {
		t.Errorf("the refusal does not name the host it refused, so there is nothing to act "+
			"on: %v", err)
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

// THE ERROR CODE IS A CHANNEL TOO, and "AWS's enumeration is fixed" is an
// assumption about somebody else's response rather than a property billet can
// rely on. The endpoint is configurable, so a reply that puts the echoed request
// body in <Code> instead of <Message> would walk through the function whose whole
// job is to stop exactly that.
func TestALaunchErrorCannotCarryTheRegistrationInItsCode(t *testing.T) {
	spec := validSpec()

	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "RunInstances" {
			return http.StatusOK, defaultReply(action)
		}

		var escaped strings.Builder
		if err := xml.EscapeText(&escaped, []byte(params.Get("UserData"))); err != nil {
			t.Errorf("escape: %v", err)

			return http.StatusInternalServerError, ""
		}

		return http.StatusBadRequest, `<Response><Errors><Error><Code>` + escaped.String() +
			`</Code><Message>no</Message></Error></Errors></Response>`
	}

	p := newTestProvider(t, f, nil)
	p.api.sleep = func(context.Context, time.Duration) error { return nil }

	_, err := p.Launch(t.Context(), spec)
	if err == nil {
		t.Fatal("a rejected launch reported success")
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(mustUserData(t, p, spec)))

	for _, secret := range []string{spec.JITConfig, encoded} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the launch error carries the registration in its code: %v", err)
		}
	}
}

// THE OWNER TAG IS THE ONLY THING BETWEEN TWO DEPLOYMENTS SHARING AN AWS ACCOUNT,
// AND NOTHING ASSERTED IT.
//
// Instance names do not distinguish deployments: another billet's instances are
// also called `billet-<leaseID>`, so `provider.LeaseOf` calls them ours, their
// lease ids are absent from THIS ledger, and the sweep destroys them as orphans —
// while they run live jobs. The filter is what makes that unreachable, and
// deleting it from describe() used to pass this entire package's suite and the
// e2e suite as well. Measured, not assumed.
//
// Asserted on EVERY DescribeInstances, and for Find as well as List, because a
// filter present on one path and missing on the other is the same hazard.
func TestEveryDescribeIsScopedToThisDeployment(t *testing.T) {
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

	var describes int

	for i, call := range f.calls {
		if call.Get("Action") != "DescribeInstances" {
			continue
		}

		describes++
		owned := false

		// Walked from 1, the way the api reads them, so a filter stranded beyond a
		// gap counts as absent here exactly as it would there.
		for n := 1; ; n++ {
			name := call.Get(fmt.Sprintf("Filter.%d.Name", n))
			if name == "" {
				break
			}

			if name != "tag:"+ownerTag {
				continue
			}

			if got := call.Get(fmt.Sprintf("Filter.%d.Value.1", n)); got != "dep-1" {
				t.Errorf("call %d scopes to owner %q, want this deployment", i, got)
			}

			owned = true
		}

		if !owned {
			t.Errorf("call %d has no %s filter, so it would return another deployment's "+
				"instances and the sweep would destroy their live jobs: %v", i, ownerTag, call)
		}
	}

	if describes < 2 {
		t.Fatalf("only %d DescribeInstances calls were made; this test is meant to cover both "+
			"List and Find", describes)
	}
}

// EVERY OPTION, not only the one that prompted the rule.
//
// WithHTTPClient(nil) reached a dereference in the constructor and was guarded;
// the other two survived construction and panicked LATER — the logger at the
// first line a launch writes, the credentials at the first signed call — further
// from the cause and on a path that is holding leases. billet bans panic because
// a control plane that panics drops every one of them.
func TestNoOptionCanProduceANilThatPanicsLater(t *testing.T) {
	cfg := validEC2Config("https://ec2.us-west-2.amazonaws.com/")

	for name, opt := range map[string]Option{
		"no http client": WithHTTPClient(nil),
		"no logger":      WithLogger(nil),
		"no credentials": WithCredentials(nil),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New("dep-1", cfg, opt); err == nil {
				t.Fatal("a provider was built with a nil the constructor's own invariant " +
					"says must not reach a later call")
			}
		})
	}
}

// A TYPED NIL IS STILL A NIL, and it is the one an interface hides.
//
// `p.api.creds == nil` is FALSE for (*IMDSCredentials)(nil) — the interface holds
// a type — so it passed the guard and dereferenced at the first signed call,
// which is precisely the later panic that guard exists to prevent.
func TestATypedNilCredentialSourceIsRefused(t *testing.T) {
	cfg := validEC2Config("https://ec2.us-west-2.amazonaws.com/")

	var typed *IMDSCredentials

	if _, err := New("dep-1", cfg, WithCredentials(typed)); err == nil {
		t.Fatal("a typed-nil credential source was accepted; the first signed call would panic")
	}
}

// BILLET SPEAKS WHENEVER IT CONTRADICTS THE IMAGE OR CHARGES FOR OBEYING IT, and
// those are different things, so they say different things.
//
// A NON-ROOT volume the image marks as surviving is honoured — billet passes the
// flag back — and warned about, because it leaks one volume PER JOB, tagged and
// billed and discoverable only by somebody going to look. The ROOT marked the same
// way is OVERRIDDEN to delete, and warned about for the opposite reason: a stated
// intent is being reversed, and finding that out from a missing volume is worse
// than reading it once.
//
// What billet does NOT warn about is filling a gap. A mapping that stated nothing
// gets billet's answer in silence, because there was no intent to contradict.
func TestAnImageWhoseVolumesOutliveItIsReported(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "DescribeImages" {
			return http.StatusOK, defaultReply(action)
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<blockDeviceMapping>` +
			// The root, which billet overrides — and says so. Spelled "0" rather
			// than "false" so the root branch has to go through survivesTermination
			// like every other device; reading only the word here left a mutant alive.
			`<item><deviceName>/dev/xvda</deviceName><ebs>` +
			`<deleteOnTermination>0</deleteOnTermination></ebs></item>` +
			// A second volume the image keeps. This is the one.
			`<item><deviceName>/dev/sdb</deviceName><ebs>` +
			`<deleteOnTermination>false</deleteOnTermination></ebs></item>` +
			// And one that is fine.
			`<item><deviceName>/dev/sdc</deviceName><ebs>` +
			`<deleteOnTermination>true</deleteOnTermination></ebs></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}

	var logged bytes.Buffer

	p := newTestProvider(t, f, nil)
	p.log = slog.New(slog.NewTextHandler(&logged, nil))

	spec := validSpec()
	spec.Disk = 80 * config.GiB

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := logged.String()

	if !strings.Contains(got, "/dev/sdb") {
		t.Errorf("the volume the image asks to keep was not reported: %s", got)
	}

	// THE ROOT IS REPORTED, and differently. billet is reversing what this image
	// asked for rather than honouring it, so the one case it must not do quietly is
	// the one where it wins the disagreement.
	//
	// CHECKED LINE BY LINE RATHER THAN OVER THE WHOLE BUFFER, because a buffer-level
	// Contains proves only that both words appear somewhere — it would pass if the
	// root marker leaked into the non-root message, which is the confusion the two
	// messages exist to prevent.
	root, other := "", ""

	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		switch {
		case strings.Contains(line, "/dev/xvda"):
			root = line
		case strings.Contains(line, "/dev/sdb"):
			other = line
		}
	}

	if root == "" {
		t.Fatalf("billet overrode the image's root flag without saying so: %s", got)
	}

	if !strings.Contains(root, "ROOT") || !strings.Contains(root, "overriding") {
		t.Errorf("the root line does not say billet overrode it: %q", root)
	}

	if strings.Contains(other, "ROOT") || strings.Contains(other, "overriding") {
		t.Errorf("the honoured volume's line reads like an override, so an operator cannot "+
			"tell which way billet went: %q", other)
	}

	if strings.Contains(got, "/dev/sdc") {
		t.Errorf("a volume that is already deleted on termination was reported: %s", got)
	}
}

// XML BOOLEANS HAVE TWO SPELLINGS, and an image using the other one describes the
// same leak.
//
// `xs:boolean` admits "1" and "0" alongside the words. EC2 emits lowercase words
// in practice — which is somebody else's observation, since nothing here has run
// against a real account — so accepting both errs toward saying something.
func TestAVolumeMarkedWithTheOtherBooleanSpellingIsStillReported(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "DescribeImages" {
			return http.StatusOK, defaultReply(action)
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<blockDeviceMapping>` +
			`<item><deviceName>/dev/sdz</deviceName><ebs>` +
			`<deleteOnTermination>0</deleteOnTermination></ebs></item>` +
			`<item><deviceName>/dev/sdy</deviceName><ebs>` +
			`<deleteOnTermination>1</deleteOnTermination></ebs></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}

	var logged bytes.Buffer

	p := newTestProvider(t, f, nil)
	p.log = slog.New(slog.NewTextHandler(&logged, nil))

	spec := validSpec()
	spec.Disk = 80 * config.GiB

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := logged.String()

	if !strings.Contains(got, "/dev/sdz") {
		t.Errorf("a volume marked with \"0\" was not reported: %s", got)
	}

	if strings.Contains(got, "/dev/sdy") {
		t.Errorf("a volume marked with \"1\" — which is deleted — was reported: %s", got)
	}
}

// EVERY EBS DEVICE THE IMAGE DECLARES LEAVES WITH AN EXPLICIT FLAG (#53).
//
// This is the test that makes the argument about EC2's default irrelevant rather
// than resolved. AWS documents that default two ways that careful readers READ
// differently for this case, so billet stops depending on it: whatever it says or
// does not say, the RunInstances request states DeleteOnTermination for every
// device it launches.
//
// The three cases differ in what billet is entitled to decide. A mapping that
// said nothing is billet's to decide, and it decides delete. A NON-ROOT mapping
// that said false is the OPERATOR talking about their own AMI, so it is passed
// back unchanged — quietly deleting a volume somebody deliberately marked to keep
// would be data loss dressed as tidiness. A mapping that said true is restated as
// true, which changes nothing and costs nothing.
//
// The root is not one of the three: it is always sent true whatever it said, and
// TestTheRootIsDeletedEvenWhenTheImageAsksToKeepIt is where that is pinned. This
// fixture's root says NOTHING, so the exception never shows here — which is
// exactly why the sentence above has to name it.
//
// AND THE ROOT OVERRIDE WARNING MUST STAY SILENT, which is the half nothing
// asserted until a reviewer went looking for a surviving mutant: billet speaks
// when it contradicts the image's intent and is silent when it merely fills a gap
// in it. This fixture's root states nothing, so there is no intent to contradict.
//
// THAT IS NOT THE SAME AS SAYING NOTHING. Two of these mappings carry a flag
// billet cannot read, and each gets a line on the OTHER channel — not a statement
// about what the image meant, but about a response outside everything measured.
// Both are asserted below, because a warning nothing checks is a warning that can
// silently stop happening.
func TestEveryImageDeviceLaunchesWithAnExplicitTerminationFlag(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "DescribeImages" {
			return http.StatusOK, defaultReply(action)
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<blockDeviceMapping>` +
			// THE ROOT IS DELIBERATELY NOT FIRST. AWS does not promise an order for
			// response elements, and classifying "the first EBS mapping" as the root
			// passed every test while this fixture led with it — a data volume
			// arriving first would then be swallowed as the root and never restated.
			`<item><deviceName>/dev/sdb</deviceName><ebs></ebs></item>` +
			// AND THE ROOT SAYS NOTHING. billet states true for it without any
			// OVERRIDE warning, having contradicted no stated intent — while still
			// reporting, on the other channel, that it could not read the value.
			`<item><deviceName>/dev/xvda</deviceName><ebs></ebs></item>` +
			`<item><deviceName>/dev/sdc</deviceName><ebs>` +
			`<deleteOnTermination>false</deleteOnTermination></ebs></item>` +
			`<item><deviceName>/dev/sdd</deviceName><ebs>` +
			`<deleteOnTermination>true</deleteOnTermination></ebs></item>` +
			// PADDED, because xs:boolean collapses whitespace and this is the one
			// place the string decode could be strictly worse than the bool it
			// replaced: untrimmed, " false " reads as delete and billet overrides a
			// preservation the image did ask for.
			`<item><deviceName>/dev/sde</deviceName><ebs>` +
			`<deleteOnTermination> false </deleteOnTermination></ebs></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}

	var logged bytes.Buffer

	p := newTestProvider(t, f, nil)
	p.log = slog.New(slog.NewTextHandler(&logged, nil))

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if strings.Contains(logged.String(), "ROOT") {
		t.Errorf("billet announced a root override on an image whose root asked for nothing "+
			"of the kind: %s", logged.String())
	}

	got := f.paramsFor(t, "RunInstances")

	// Order is whatever DescribeImages returned, so this reads by name rather than
	// by position; the root took index 1 regardless.
	want := map[string]string{
		"/dev/xvda": "true",  // root, stated by billet itself, and it said nothing
		"/dev/sdb":  "true",  // said nothing -> billet decides delete
		"/dev/sdc":  "false", // said keep -> honoured
		"/dev/sdd":  "true",  // said delete -> restated
		"/dev/sde":  "false", // said keep, padded -> still honoured
	}

	seen := blockDevices(t, got)

	for device, flag := range want {
		if seen[device] != flag {
			t.Errorf("%s went out with DeleteOnTermination=%q, want %q", device, seen[device], flag)
		}
	}

	if len(seen) != len(want) {
		t.Errorf("sent %d devices, want %d: %v", len(seen), len(want), seen)
	}

	// EVERY KEPT VOLUME IS ANNOUNCED, both spellings of it. Honouring the flag and
	// saying so are separate code paths, so a regression that reads the RAW value
	// for the warning while the request uses the trimmed one would honour the padded
	// device and never mention it — a volume billet knowingly leaves behind, silently.
	for _, device := range []string{"/dev/sdc", "/dev/sde"} {
		if !strings.Contains(logged.String(), device) {
			t.Errorf("%s is being kept and was not reported: %s", device, logged.String())
		}
	}

	// AT WARN. Reported at Info it sits beside the routine launch line and is seen
	// by nobody.
	if !strings.Contains(logged.String(), "level=WARN") {
		t.Errorf("kept volumes were not reported as warnings: %s", logged.String())
	}

	// AND EVERY UNREADABLE FLAG IS REPORTED, root included. The root's line is the
	// one a mutant could drop for free until this existed: it is emitted before the
	// root branch, so moving the branch above it silences the root alone and every
	// other assertion here still passes.
	for _, device := range []string{"/dev/xvda", "/dev/sdb"} {
		if !strings.Contains(logged.String(), `device=`+device) {
			t.Errorf("%s carries a flag billet cannot read and was not reported: %s",
				device, logged.String())
		}
	}

	if n := strings.Count(logged.String(), "cannot read"); n != 2 {
		t.Errorf("%d unreadable flags were reported, want 2 — one per affected device", n)
	}
}

// A MAPPING THAT IS NOT AN EBS VOLUME IS NOT GIVEN AN EBS PARAMETER.
//
// An instance-store or suppressed mapping has no <ebs> child, and sending
// Ebs.DeleteOnTermination for one asks EC2 about a volume that does not exist.
// The response type models this with a POINTER for exactly this reason: a value
// type would decode "no <ebs> element" and "<ebs></ebs>" to the same zero struct,
// and this test would be impossible to write.
func TestANonEBSMappingIsNotRestated(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "DescribeImages" {
			return http.StatusOK, defaultReply(action)
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<blockDeviceMapping>` +
			`<item><deviceName>/dev/sdb</deviceName><virtualName>ephemeral0</virtualName></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}

	p := newTestProvider(t, f, nil)

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	for i := 1; ; i++ {
		n := strconv.Itoa(i)

		device := got.Get("BlockDeviceMapping." + n + ".DeviceName")
		if device == "" {
			break
		}

		if device == "/dev/sdb" {
			t.Errorf("an instance-store mapping was sent as an EBS device: %s=%q",
				"BlockDeviceMapping."+n+".Ebs.DeleteOnTermination",
				got.Get("BlockDeviceMapping."+n+".Ebs.DeleteOnTermination"))
		}
	}
}

// THE INDICES ARE CONTIGUOUS FROM 1, which is what every official SDK emits for a
// query-API list.
//
// What EC2 does with a GAP is not documented anywhere billet can point at, and
// this test does not depend on knowing: a dense list is correct under every
// possible gap semantics. What it pins is that a skipped device (the root, or a
// non-EBS mapping) CLOSES the hole rather than leaving one — numbering by position
// in the image's list would put a gap wherever billet declined to restate, and
// then the answer would depend on that undocumented behaviour.
func TestBlockDeviceIndicesAreContiguous(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "DescribeImages" {
			return http.StatusOK, defaultReply(action)
		}

		// The root and an instance-store device are both skipped, and they sit
		// BEFORE the two real volumes so a positional bug cannot hide.
		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<blockDeviceMapping>` +
			`<item><deviceName>/dev/xvda</deviceName><ebs></ebs></item>` +
			`<item><deviceName>/dev/sda</deviceName><virtualName>ephemeral0</virtualName></item>` +
			`<item><deviceName>/dev/sdb</deviceName><ebs></ebs></item>` +
			`<item><deviceName>/dev/sdc</deviceName><ebs></ebs></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}

	p := newTestProvider(t, f, nil)

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	for _, want := range []struct {
		index  string
		device string
	}{
		{"1", "/dev/xvda"},
		{"2", "/dev/sdb"},
		{"3", "/dev/sdc"},
	} {
		if d := got.Get("BlockDeviceMapping." + want.index + ".DeviceName"); d != want.device {
			t.Errorf("BlockDeviceMapping.%s.DeviceName = %q, want %q", want.index, d, want.device)
		}
	}

	if d := got.Get("BlockDeviceMapping.4.DeviceName"); d != "" {
		t.Errorf("a fourth device was sent (%q); only three should have been", d)
	}
}

// THE ROOT LEAVES AS true EVEN WHEN THE IMAGE ASKED TO KEEP IT.
//
// The one explicit false billet knowingly overrides, pinned so the asymmetry is a
// decision on the record rather than something a later reader has to infer from
// setBlockDevices writing index 1 before the loop runs. Two reviewers found this
// gap by reading the prose against the code; this is what makes the code answer
// for itself.
func TestTheRootIsDeletedEvenWhenTheImageAsksToKeepIt(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "DescribeImages" {
			return http.StatusOK, defaultReply(action)
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<blockDeviceMapping>` +
			`<item><deviceName>/dev/xvda</deviceName><ebs>` +
			`<deleteOnTermination>false</deleteOnTermination></ebs></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}

	var logged bytes.Buffer

	p := newTestProvider(t, f, nil)
	p.log = slog.New(slog.NewTextHandler(&logged, nil))

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	if d := got.Get("BlockDeviceMapping.1.DeviceName"); d != "/dev/xvda" {
		t.Fatalf("BlockDeviceMapping.1.DeviceName = %q, want the root", d)
	}

	if v := got.Get("BlockDeviceMapping.1.Ebs.DeleteOnTermination"); v != "true" {
		t.Errorf("the root went out as %q; a boot disk kept per job leaks on every job billet "+
			"launches from this image", v)
	}

	// AND IT IS NOT ALSO RESTATED AS A SECOND DEVICE, which is what would happen if
	// the root were ever dropped from the skip list — EC2 would receive two entries
	// for one device, disagreeing.
	if d := got.Get("BlockDeviceMapping.2.DeviceName"); d != "" {
		t.Errorf("the root was sent twice: index 2 is %q", d)
	}

	// AND IT SAID SO. The override is only half the promise; the ADR commits to
	// announcing it in the same breath. This fixture spells the flag as the WORD,
	// and the announcement was pinned only for the digit elsewhere — the one cell of
	// the matrix nothing covered.
	if !strings.Contains(logged.String(), "ROOT") {
		t.Errorf("billet overrode a root marked keep without announcing it: %s", logged.String())
	}

	if !strings.Contains(logged.String(), "level=WARN") {
		t.Errorf("the root override was not announced as a warning: %s", logged.String())
	}
}

// A FLAG THAT IS NOT A BOOLEAN AT ALL IS REPORTED, NOT QUIETLY OBEYED.
//
// The companion to the absent case, and it arrived the same way: a reviewer asked
// why a response outside everything measured earns a line when it is MISSING but
// not when it is nonsense. There was no answer, only an accident of how the check
// was written — every unrecognised value fell through to delete in silence, which
// is the destructive direction.
//
// "False" is the sharp one. It is what a hand-rolled client or a proxy that
// re-serialises XML would plausibly emit, it is not in xs:boolean's lexical space,
// and it means the opposite of what billet would have silently done with it.
func TestAFlagBilletCannotReadIsReported(t *testing.T) {
	for _, value := range []string{"False", "TRUE", "yes", "  ", "2", "null"} {
		t.Run(value, func(t *testing.T) {
			f := newFakeEC2(t)
			f.respond = func(action string, params url.Values) (int, string) {
				if action != "DescribeImages" {
					return http.StatusOK, defaultReply(action)
				}

				return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
					`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
					`<blockDeviceMapping>` +
					`<item><deviceName>/dev/xvda</deviceName><ebs>` +
					`<deleteOnTermination>true</deleteOnTermination></ebs></item>` +
					`<item><deviceName>/dev/sdb</deviceName><ebs>` +
					`<deleteOnTermination>` + value + `</deleteOnTermination></ebs></item>` +
					`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
			}

			var logged bytes.Buffer

			p := newTestProvider(t, f, nil)
			p.log = slog.New(slog.NewTextHandler(&logged, nil))

			if _, err := p.Launch(t.Context(), validSpec()); err != nil {
				t.Fatalf("Launch: %v", err)
			}

			got := logged.String()

			if !strings.Contains(got, "cannot read") || !strings.Contains(got, "/dev/sdb") {
				t.Errorf("%q was resolved without a word: %s", value, got)
			}

			// AND IT STILL LAUNCHED, stating delete. Reporting the oddity must not
			// become refusing the job over a field EC2 marks optional.
			if v := f.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.2.Ebs.DeleteOnTermination"); v != "true" {
				t.Errorf("the device went out as %q, want true", v)
			}
		})
	}
}

// THE FOUR TOKENS THAT ARE READABLE ARE READ, and nothing else is.
//
// The positive half, so the anomaly channel cannot creep onto ordinary values —
// a warning that fires on "true" is one an operator stops reading.
func TestTheFourBooleanTokensAreReadWithoutComplaint(t *testing.T) {
	for value, want := range map[string]terminationIntent{
		"true":    intentDelete,
		"1":       intentDelete,
		"false":   intentKeep,
		"0":       intentKeep,
		" true ":  intentDelete,
		" false ": intentKeep,
		"":        intentUnreadable,
		"False":   intentUnreadable,
		"yes":     intentUnreadable,
	} {
		if got := readTermination(value); got != want {
			t.Errorf("readTermination(%q) = %v, want %v", value, got, want)
		}
	}
}

// A ROOT THAT DID NOT ASK TO BE KEPT IS OVERRIDDEN IN SILENCE.
//
// The other half of "speak only when billet contradicts the image", swept across
// every spelling that means keep-nothing. The comprehensive test covers the absent
// case; these cover the two ways an image can say delete, so no narrower reading
// of the root flag can start announcing an override that is not happening.
func TestTheRootIsNotAnnouncedWhenTheImageDidNotAskToKeepIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		ebs  string
	}{
		{name: "said delete in words", ebs: `<deleteOnTermination>true</deleteOnTermination>`},
		{name: "said delete as a digit", ebs: `<deleteOnTermination>1</deleteOnTermination>`},
		{name: "said delete padded", ebs: `<deleteOnTermination> true </deleteOnTermination>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEC2(t)
			f.respond = func(action string, params url.Values) (int, string) {
				if action != "DescribeImages" {
					return http.StatusOK, defaultReply(action)
				}

				return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
					`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
					`<blockDeviceMapping>` +
					`<item><deviceName>/dev/xvda</deviceName><ebs>` + tc.ebs + `</ebs></item>` +
					`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
			}

			var logged bytes.Buffer

			p := newTestProvider(t, f, nil)
			p.log = slog.New(slog.NewTextHandler(&logged, nil))

			if _, err := p.Launch(t.Context(), validSpec()); err != nil {
				t.Fatalf("Launch: %v", err)
			}

			if strings.Contains(logged.String(), "ROOT") {
				t.Errorf("billet announced overriding a root that asked for no such thing: %s",
					logged.String())
			}
		})
	}
}

// THE TWO WAYS AN IMAGE LOOKUP CAN BE UNUSABLE BOTH REFUSE THE LAUNCH.
//
// Both guards predate this work and both were unkillable: no fixture returned an
// empty imagesSet or omitted rootDeviceName, so deleting either left the suite
// green. The second is the one with teeth — without it a rootless image reaches
// RunInstances with an EMPTY DeviceName, which is the "second disk by mistake"
// hazard the guard's own message warns about.
func TestAnImageBilletCannotReadIsRefusedBeforeLaunching(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		want  string
	}{
		{
			name:  "no such image",
			reply: `<DescribeImagesResponse><imagesSet></imagesSet></DescribeImagesResponse>`,
			want:  "does not exist",
		},
		{
			name: "no root device name",
			reply: `<DescribeImagesResponse><imagesSet><item>` +
				`<imageId>ami-0abc</imageId>` +
				`</item></imagesSet></DescribeImagesResponse>`,
			want: "no root device name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEC2(t)
			f.respond = func(action string, params url.Values) (int, string) {
				if action != "DescribeImages" {
					return http.StatusOK, defaultReply(action)
				}

				return http.StatusOK, tc.reply
			}

			p := newTestProvider(t, f, nil)

			_, err := p.Launch(t.Context(), validSpec())
			if err == nil {
				t.Fatal("Launch succeeded on an image billet cannot read")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the problem: %v", err)
			}

			// AND NOTHING WAS BOUGHT. The refusal has to happen before RunInstances,
			// or billet has paid for an instance it then reports as a failure.
			if n := f.countOf("RunInstances"); n != 0 {
				t.Errorf("%d instances were launched from an image billet could not read", n)
			}
		})
	}
}

// A MAPPING WITH NO DEVICE NAME IS SKIPPED RATHER THAN SENT NAMELESS.
//
// A guard against a malformed response, and it exists as a test because a reviewer
// found that deleting the guard left every other test green: no fixture had a
// nameless mapping, so nothing noticed. An unkillable mutant is a guard nobody is
// maintaining.
func TestAMappingWithNoDeviceNameIsSkipped(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "DescribeImages" {
			return http.StatusOK, defaultReply(action)
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<blockDeviceMapping>` +
			`<item><ebs><deleteOnTermination>false</deleteOnTermination></ebs></item>` +
			`<item><deviceName>/dev/sdb</deviceName><ebs>` +
			`<deleteOnTermination>true</deleteOnTermination></ebs></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}

	var logged bytes.Buffer

	p := newTestProvider(t, f, nil)
	p.log = slog.New(slog.NewTextHandler(&logged, nil))

	if _, err := p.Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// THE NAMELESS MAPPING SAYS "KEEP", so a guard that ran after the warning rather
	// than before it would announce a volume that has no name to announce. Nothing
	// at all should be said here.
	if strings.Contains(logged.String(), "level=WARN") {
		t.Errorf("a nameless mapping produced a warning naming no device: %s", logged.String())
	}

	got := f.paramsFor(t, "RunInstances")

	// The named device takes index 2 — the nameless one consumed no slot at all.
	if d := got.Get("BlockDeviceMapping.2.DeviceName"); d != "/dev/sdb" {
		t.Errorf("BlockDeviceMapping.2.DeviceName = %q, want /dev/sdb; a nameless mapping "+
			"either took a slot or was sent as an empty device", d)
	}

	if d := got.Get("BlockDeviceMapping.3.DeviceName"); d != "" {
		t.Errorf("a third device was sent (%q); the nameless mapping should have been "+
			"dropped entirely", d)
	}
}

// A RESPONSE THAT OMITS DeleteOnTermination IS REPORTED AS AN ANOMALY.
//
// THIS ASSERTION IS INVERTED FROM WHAT IT WAS, and the measurement is why. It used
// to pin silence, on the reasoning that billet could not tell what an omission
// meant and a warning fired on an uninterpretable state is noise.
//
// A live account then showed a registered image reading back with the value
// present, and no image in a 26,044-image corpus omitting it. So an omission is no
// longer an ordinary state to be guessed at — it is a response that does not look
// like the ones that were observed, which is worth exactly one line per image.
//
// The launch is NOT refused: billet states delete and carries on, because turning
// a missing optional field into a failed CI job is worse than deleting a volume
// created fresh for that job. What is not acceptable is doing that quietly, which
// is how a policy applied to an unmeasured state stops being visible.
func TestAResponseOmittingTerminationIsReportedAsAnomalous(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, params url.Values) (int, string) {
		if action != "DescribeImages" {
			return http.StatusOK, defaultReply(action)
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<blockDeviceMapping>` +
			`<item><deviceName>/dev/sdq</deviceName><ebs></ebs></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}

	var logged bytes.Buffer

	p := newTestProvider(t, f, nil)
	p.log = slog.New(slog.NewTextHandler(&logged, nil))

	spec := validSpec()
	spec.Disk = 80 * config.GiB

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := logged.String()

	if !strings.Contains(got, "/dev/sdq") {
		t.Errorf("a response omitting the termination flag was not reported: %s", got)
	}

	if !strings.Contains(got, "level=WARN") {
		t.Errorf("the anomaly was not reported as a warning: %s", got)
	}

	// AND THE LAUNCH STILL HAPPENED, stating delete. Reporting the oddity must not
	// become refusing the job.
	if v := f.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.2.Ebs.DeleteOnTermination"); v != "true" {
		t.Errorf("the device went out as %q, want true — billet should state delete rather "+
			"than refuse or guess", v)
	}
}
