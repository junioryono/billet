package guestassets

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// relayAuthority is a CA and a leaf for 127.0.0.1, as the image bakes the listener's
// issuer and the node serves its leaf.
func relayAuthority(t *testing.T) (string, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	return ca, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startRelay runs the real entry point against upstream, with roots as the system's
// trust, and returns the address it listens on.
func startRelay(t *testing.T, upstream, roots string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on this development host")
	}
	script := filepath.Join(t.TempDir(), "actions_proxy.py")
	if err := os.WriteFile(script, []byte(ActionsProxyScript), 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, python, script, "--mode", "node-relay",
		"--listen", address, "--upstream", upstream)
	command.Env = append(os.Environ(), "SSL_CERT_FILE="+roots)
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = command.Wait()
	})
	for range 200 {
		if conn, err := net.Dial("tcp", address); err == nil {
			_ = conn.Close()

			return address
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the relay never listened")

	return ""
}

// AN EC2 GUEST'S RELAY DOES THE TLS ITSELF (ADR-023), so everything in the guest keeps
// speaking plain bytes to its bridge. What goes in comes out, both ways, and more of
// it than one TLS record holds, which is what a read of the descriptor alone would
// sit on: decrypted bytes the socket no longer shows.
//
// NOT PARALLEL, nor is the test below, because startRelay names a free port and the
// process binds it a moment later, and this package's other probes do the same: beside
// each other, one test's client reached another's listener.
func TestTheRelayCarriesBytesOverTLSToAnHTTPSNode(t *testing.T) {
	ca, leaf := relayAuthority(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	request := []byte("the client's first bytes")
	answer := bytes.Repeat([]byte("the node's answer "), 256<<10/18)
	received := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		got := make([]byte, len(request))
		if _, err := io.ReadFull(conn, got); err != nil {
			received <- nil

			return
		}
		received <- got
		_, _ = conn.Write(answer)
	}()

	relay := startRelay(t, "https://"+listener.Addr().String(), ca)
	client, err := net.Dial("tcp", relay)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(answer))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read the answer through the relay: %v", err)
	}
	if !bytes.Equal(got, answer) {
		t.Fatal("the answer changed on its way through the relay")
	}
	if first := <-received; !bytes.Equal(first, request) {
		t.Fatalf("the node saw %q", first)
	}
}

// A NODE THE IMAGE'S ROOTS DO NOT VOUCH FOR GETS NOTHING: the bearer is the first thing
// a client sends.
func TestTheRelaySendsNothingToANodeItCannotVerify(t *testing.T) {
	_, leaf := relayAuthority(t)
	otherCA, _ := relayAuthority(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	heard := make(chan int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 64)
		count, _ := conn.Read(buffer)
		heard <- count
	}()

	relay := startRelay(t, "https://"+listener.Addr().String(), otherCA)
	client, err := net.Dial("tcp", relay)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("Authorization: Bearer secret")); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// A CLOSE OR A RESET, whichever the unread bytes make it; what matters is nothing
	// came back.
	if got, _ := io.ReadAll(client); len(got) != 0 {
		t.Fatalf("the client read %q; want the relay to close on it", got)
	}
	select {
	case count := <-heard:
		if count != 0 {
			t.Fatalf("the unverified node read %d bytes", count)
		}
	case <-time.After(5 * time.Second):
		// The handshake failed on the client's side before the node had anything to
		// read, which is the same answer.
	}
}
