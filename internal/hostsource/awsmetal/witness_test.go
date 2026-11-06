package awsmetal

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awsidentity"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/joinconsole"
	"github.com/junioryono/billet/internal/provider/ec2"
)

var launched = time.Date(2026, 11, 6, 14, 26, 45, 0, time.UTC)

// fp is a fingerprint of the shape wirecert writes, distinct per seed.
func fp(seed string) string { return "SHA256:" + (seed + strings.Repeat("A", 43))[:43] }

// witnessRig is a source whose host EC2 describes as running at 10.0.0.7, whose
// console names the key, and whose identity documents a key of the test's own signs.
type witnessRig struct {
	hosts *fakeHosts
	src   *Source
	key   *rsa.PrivateKey
}

func newWitnessRig(t *testing.T) *witnessRig {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{Organization: []string{"test"}},
		NotBefore: launched.Add(-time.Hour), NotAfter: launched.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	v, err := awsidentity.FromCertificates([]byte("\nregion: us-west-2\n" +
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHosts{
		record: ec2.HostRecord{ID: "i-1", State: "running", PrivateIP: "10.0.0.7", Order: "pa", LaunchTime: launched},
		console: "\x1b[0m[  12.3] cloud-init: ...\r\n" +
			joinconsole.Line("pa", fp("key")) + "\r\n" + joinconsole.Line("pb", fp("other")) + "\r\n",
	}
	// THROUGH THE CONSTRUCTOR, so a verifier the source is handed and drops is caught.
	s, err := newSource(f, Config{Region: "us-west-2", Location: "us-west-2a", Shapes: []Shape{
		{Type: "z1d.metal", Arch: config.ArchX64, Image: "ami-x64"},
	}, Account: func(context.Context) (string, error) { return "111122223333", nil }, Identity: v})
	if err != nil {
		t.Fatal(err)
	}
	s.code = codeOfStandIn

	return &witnessRig{hosts: f, src: s, key: key}
}

// document is the identity document EC2 would sign for the rig's host, with fields
// changed, and its signature.
func (r *witnessRig) document(t *testing.T, change map[string]string) ([]byte, string) {
	t.Helper()
	d := map[string]string{
		"accountId": "111122223333", "region": "us-west-2", "instanceId": "i-1", "privateIp": "10.0.0.7",
		"pendingTime": "2026-11-06T14:26:45Z", "availabilityZone": "us-west-2a",
	}
	for k, v := range change {
		d[k] = v
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	sig, err := rsa.SignPKCS1v15(rand.Reader, r.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}

	return raw, base64.StdEncoding.EncodeToString(sig)
}

func (r *witnessRig) claim(t *testing.T) hostsource.Claim {
	t.Helper()
	doc, sig := r.document(t, nil)

	return hostsource.Claim{Order: "pa", HostID: "i-1", Fingerprint: fp("key"), Address: "10.0.0.7",
		Document: doc, Signature: sig}
}

// THE SOURCE VOUCHES ONLY WHEN THE CONSOLE, THE DOCUMENT, THE ADDRESS AND EC2 ALL
// AGREE, and with no address seen (a router in front of the listener) the document
// stands for it.
func TestTheSourceVouchesWhenEverythingAgrees(t *testing.T) {
	t.Parallel()
	r := newWitnessRig(t)
	if got := r.src.Witness(t.Context(), r.claim(t)); got.Verdict != hostsource.VerdictVouches {
		t.Fatalf("everything agrees: %+v", got)
	}
	c := r.claim(t)
	c.Address = ""
	if got := r.src.Witness(t.Context(), c); got.Verdict != hostsource.VerdictVouches ||
		!strings.Contains(got.Reason, "address was not seen") {
		t.Fatalf("no address seen: %+v", got)
	}
}

// A POSITIVE ANSWER ABOUT THE HOST ITSELF CONTRADICTS: an ending instance, a verified
// document naming another instance, account or address, or a console naming only other
// keys for the order.
func TestAPositiveAnswerForSomethingElseContradicts(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		host   func(*fakeHosts)
		doc    map[string]string
		change func(*hostsource.Claim)
		reason string
	}{
		"an ending instance":            {host: func(f *fakeHosts) { f.record.State = "shutting-down" }, reason: "shutting-down"},
		"a terminated instance":         {host: func(f *fakeHosts) { f.record.State = "terminated" }, reason: "terminated"},
		"a document of another host":    {doc: map[string]string{"instanceId": "i-2"}, reason: "names instance i-2"},
		"a document of another account": {doc: map[string]string{"accountId": "444455556666"}, reason: "another account"},
		"a document of another address": {doc: map[string]string{"privateIp": "10.0.0.9"}, reason: "address 10.0.0.9"},
		"a console naming another key": {host: func(f *fakeHosts) {
			f.console = "\n" + joinconsole.Line("pa", fp("imposter")) + "\n"
		}, reason: "names " + fp("imposter")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newWitnessRig(t)
			if c.host != nil {
				c.host(r.hosts)
			}
			claim := r.claim(t)
			if c.doc != nil {
				claim.Document, claim.Signature = r.document(t, c.doc)
			}
			if c.change != nil {
				c.change(&claim)
			}
			got := r.src.Witness(t.Context(), claim)
			if got.Verdict != hostsource.VerdictContradicts || !strings.Contains(got.Reason, c.reason) {
				t.Fatalf("%+v, want a contradiction saying %q", got, c.reason)
			}
			if strings.Contains(got.Reason, "111122223333") || strings.Contains(got.Reason, "444455556666") {
				t.Fatalf("a reason names an account: %q", got.Reason)
			}
		})
	}
}

// ANYTHING ELSE IS COULD-NOT-TELL, and never a contradiction: a failed describe or
// console read, an instance not yet running, a console that has not named a key, names
// two or was cut short, no document, a document no certificate verifies, an address or
// a launch time that does not match (a NAT, a proxy, a launch recorded differently), a
// launch time EC2 did not give, an account that cannot be read, and a claim that names
// no host, order or key.
func TestWhatTheSourceCannotReadIsCouldNotTell(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		host   func(*fakeHosts)
		src    func(*Source)
		change func(*hostsource.Claim)
	}{
		"a failed describe":       {host: func(f *fakeHosts) { f.describe = errors.New("throttled") }},
		"a pending instance":      {host: func(f *fakeHosts) { f.record.State = "pending" }},
		"no private address":      {host: func(f *fakeHosts) { f.record.PrivateIP = "" }},
		"no launch time":          {host: func(f *fakeHosts) { f.record.LaunchTime = time.Time{} }},
		"a failed console read":   {host: func(f *fakeHosts) { f.consoleErr = errors.New("throttled") }},
		"a console not named yet": {host: func(f *fakeHosts) { f.console = "[  1.0] booting\n" }},
		"a console naming two keys": {host: func(f *fakeHosts) {
			f.console = "\n" + joinconsole.Line("pa", fp("key")) + "\n" + joinconsole.Line("pa", fp("second")) + "\n"
		}},
		"a console cut short": {host: func(f *fakeHosts) {
			line := joinconsole.Line("pa", fp("key"))
			f.console = line[:len(line)-4]
		}},
		"another address":     {change: func(c *hostsource.Claim) { c.Address = "10.0.0.8" }},
		"another launch time": {host: func(f *fakeHosts) { f.record.LaunchTime = launched.Add(time.Hour) }},
		"an empty account": {src: func(s *Source) {
			s.account = func(context.Context) (string, error) { return "", nil }
		}},
		"no order":    {change: func(c *hostsource.Claim) { c.Order = "" }},
		"no key":      {change: func(c *hostsource.Claim) { c.Fingerprint = "" }},
		"no document": {change: func(c *hostsource.Claim) { c.Document, c.Signature = nil, "" }},
		"a document that is altered": {change: func(c *hostsource.Claim) {
			c.Document = append(c.Document[:len(c.Document)-1:len(c.Document)-1], ' ', '}')
		}},
		"no verifier":             {src: func(s *Source) { s.identity = nil }},
		"another region's source": {src: func(s *Source) { s.region = "us-east-1" }},
		"an unreadable account": {src: func(s *Source) {
			s.account = func(context.Context) (string, error) { return "", errors.New("sts unreachable") }
		}},
		"no host": {change: func(c *hostsource.Claim) { c.HostID = "" }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newWitnessRig(t)
			if c.host != nil {
				c.host(r.hosts)
			}
			if c.src != nil {
				c.src(r.src)
			}
			claim := r.claim(t)
			if c.change != nil {
				c.change(&claim)
			}
			if got := r.src.Witness(t.Context(), claim); got.Verdict != hostsource.VerdictUnknown || got.Reason == "" {
				t.Fatalf("%+v, want could-not-tell with a reason", got)
			}
		})
	}
}

// THE CONSOLE IS READ LAST, after every check that costs no throttled call, so a
// claim any of them settles never reads it.
func TestTheConsoleIsReadOnlyOnceEverythingElseAgrees(t *testing.T) {
	t.Parallel()
	r := newWitnessRig(t)
	c := r.claim(t)
	c.Address = "10.0.0.8"
	r.src.Witness(t.Context(), c)
	c = r.claim(t)
	c.Document = nil
	r.src.Witness(t.Context(), c)
	if r.hosts.consoleReads != 0 {
		t.Fatalf("the console was read %d times for claims settled before it", r.hosts.consoleReads)
	}
	r.src.Witness(t.Context(), r.claim(t))
	if r.hosts.consoleReads != 1 {
		t.Fatalf("the console was read %d times for a claim that needed it", r.hosts.consoleReads)
	}
}

// A REASON NEVER QUOTES WHAT AWS ANSWERED: an error's prose can name the caller's
// account and role, so a failure is said by EC2's code, or in billet's words.
func TestAReasonNeverQuotesWhatAWSAnswered(t *testing.T) {
	t.Parallel()
	leaky := errors.New("arn:aws:iam::111122223333:role/billet is not authorized")
	for name, set := range map[string]func(*fakeHosts){
		"a describe":  func(f *fakeHosts) { f.describe = leaky },
		"a console":   func(f *fakeHosts) { f.consoleErr = leaky },
		"a coded one": func(f *fakeHosts) { f.describe = errors.Join(codedError{"UnauthorizedOperation"}, leaky) },
	} {
		r := newWitnessRig(t)
		set(r.hosts)
		got := r.src.Witness(t.Context(), r.claim(t))
		if got.Verdict != hostsource.VerdictUnknown || strings.Contains(got.Reason, "111122223333") ||
			strings.Contains(got.Reason, "arn:") {
			t.Errorf("%s: %+v", name, got)
		}
		if name == "a coded one" && !strings.Contains(got.Reason, "UnauthorizedOperation") {
			t.Errorf("%s: %q does not name the code", name, got.Reason)
		}
	}
}

// A SOURCE IS BUILT WITH AWS'S CERTIFICATES OR NOT AT ALL: without them it could never
// vouch, and a pool on AWS would wait for a person at every join without saying why.
func TestASourceNeedsTheCertificates(t *testing.T) {
	t.Parallel()
	account := func(context.Context) (string, error) { return "111122223333", nil }
	_, err := New(nil, Config{Region: "us-west-2", Location: "us-west-2a",
		Shapes: []Shape{{Type: "z1d.metal", Arch: config.ArchX64, Image: "ami-x64"}}, Account: account})
	if err == nil || !strings.Contains(err.Error(), "certificates") {
		t.Fatalf("a source without the certificates answered %v", err)
	}
}
