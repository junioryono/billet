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
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/provider/ec2"
)

var launched = time.Date(2026, 11, 6, 14, 26, 45, 0, time.UTC)

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
			hostsource.ConsoleLine("pa", "SHA256:key") + "\r\n" + hostsource.ConsoleLine("pb", "SHA256:other") + "\r\n",
	}
	s := newTestSource(t, f)
	s.identity = v

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

	return hostsource.Claim{Order: "pa", HostID: "i-1", Fingerprint: "SHA256:key", Address: "10.0.0.7",
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

// A POSITIVE ANSWER FOR SOMETHING ELSE CONTRADICTS: an ending instance, another
// address, a verified document naming another instance, account, address or launch,
// or a console naming only other keys for the order.
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
		"another address":               {change: func(c *hostsource.Claim) { c.Address = "10.0.0.8" }, reason: "came from 10.0.0.8"},
		"a document of another host":    {doc: map[string]string{"instanceId": "i-2"}, reason: "names instance i-2"},
		"a document of another account": {doc: map[string]string{"accountId": "444455556666"}, reason: "another account"},
		"a document of another address": {doc: map[string]string{"privateIp": "10.0.0.9"}, reason: "address 10.0.0.9"},
		"a document of another launch": {doc: map[string]string{"pendingTime": "2026-11-06T14:00:00Z"},
			reason: "signed for a launch"},
		"a console naming another key": {host: func(f *fakeHosts) {
			f.console = hostsource.ConsoleLine("pa", "SHA256:imposter")
		}, reason: "names SHA256:imposter"},
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

// ANYTHING THE SOURCE CANNOT READ IS COULD-NOT-TELL, and never a contradiction: a
// failed describe or console read, an instance not yet running, a console that has
// not named a key or names two, no document, a document no certificate verifies, a
// launch time EC2 did not give, an account that cannot be read, and a claim that names
// no host.
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
			f.console = hostsource.ConsoleLine("pa", "SHA256:key") + "\n" + hostsource.ConsoleLine("pa", "SHA256:second")
		}},
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
