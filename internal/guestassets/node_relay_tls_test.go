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
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
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
	var config net.ListenConfig
	probe, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	command := exec.CommandContext(ctx, python, script, "--mode", "node-relay",
		"--listen", address, "--upstream", upstream)
	command.Env = append(os.Environ(), "SSL_CERT_FILE="+roots)
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		// KILLED, so this is what it says every time.
		if err := command.Wait(); err != nil {
			t.Logf("the relay ended: %v", err)
		}
	})
	dialer := net.Dialer{Timeout: time.Second}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if conn, err := dialer.DialContext(t.Context(), "tcp", address); err == nil {
			_ = conn.Close()

			return address
		}
	}
	t.Fatal("the relay never listened")

	return ""
}

// serveOne runs handle on the listener's first connection, and the test does not end
// before it has, so what it reports reaches the test.
func serveOne(t *testing.T, listener net.Listener, handle func(net.Conn) error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err

			return
		}
		defer conn.Close()
		done <- handle(conn)
	}()
	t.Cleanup(func() {
		guard := time.NewTimer(30 * time.Second)
		defer guard.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the node: %v", err)
			}
		case <-guard.C:
			t.Error("the node's side never finished")
		}
	})
}

// dialRelay is a client of the relay with a deadline that fails the test rather than
// hang it, far inside the relay's own idle bound.
func dialRelay(t *testing.T, relay string) *net.TCPConn {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", relay)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("a dial answered %T", conn)
	}

	return client
}

// AN EC2 GUEST'S RELAY DOES THE TLS ITSELF (ADR-024), so everything in the guest keeps
// speaking plain bytes to its bridge. What goes in comes out, both ways, and more of
// it than one TLS record holds.
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
	// OPEN UNTIL THE CLIENT HAS READ IT ALL: a node that closed after writing would wake
	// the relay's wait on its own.
	finished := make(chan struct{})
	serveOne(t, listener, func(conn net.Conn) error {
		got := make([]byte, len(request))
		if _, err := io.ReadFull(conn, got); err != nil {
			received <- nil

			return err
		}
		received <- got
		if _, err := conn.Write(answer); err != nil {
			return err
		}
		<-finished

		return nil
	})

	relay := startRelay(t, "https://"+listener.Addr().String(), ca)
	client := dialRelay(t, relay)
	defer close(finished)
	if _, err := client.Write(request); err != nil {
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

// A TLS SOCKET CAN BE READABLE WITH NOTHING FOR THE APPLICATION: a Go listener with tickets
// on, as this one is, sends one in its first flight. A relay that blocks reading it
// stops serving the client, and a request past one read then stalls against a node
// waiting for the rest of it until the idle bound.
func TestALargeRequestIsNotHeldUpByARecordWithNoData(t *testing.T) {
	ca, leaf := relayAuthority(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	request := bytes.Repeat([]byte("a large body "), 1<<20/13)
	finished := make(chan struct{})
	defer close(finished)
	serveOne(t, listener, func(conn net.Conn) error {
		// EVERY BYTE BEFORE ANY ANSWER, as a node reading a body does.
		if _, err := io.ReadFull(conn, make([]byte, len(request))); err != nil {
			return err
		}
		if _, err := conn.Write([]byte("stored")); err != nil {
			return err
		}
		<-finished

		return nil
	})

	client := dialRelay(t, startRelay(t, "https://"+listener.Addr().String(), ca))
	if _, err := client.Write(request); err != nil {
		t.Fatalf("send the request through the relay: %v", err)
	}
	got := make([]byte, len("stored"))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read the answer through the relay: %v", err)
	}
	if string(got) != "stored" {
		t.Fatalf("the answer = %q", got)
	}
}

// A CLIENT THAT HALF-CLOSES STILL GETS ITS ANSWER: shutting down a TLS socket ends the
// session both ways, so the relay leaves that side to the idle bound.
func TestAClientsHalfCloseDoesNotEndTheNodesAnswer(t *testing.T) {
	ca, leaf := relayAuthority(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	request, answer := []byte("request"), []byte("an answer that comes after the close")
	halfClosed := make(chan struct{})
	serveOne(t, listener, func(conn net.Conn) error {
		if _, err := io.ReadFull(conn, make([]byte, len(request))); err != nil {
			return err
		}
		// AFTER THE CLOSE, and long enough for the relay to have acted on it.
		<-halfClosed
		time.Sleep(200 * time.Millisecond)
		_, err := conn.Write(answer)

		return err
	})

	client := dialRelay(t, startRelay(t, "https://"+listener.Addr().String(), ca))
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		close(halfClosed)
		t.Fatal(err)
	}
	close(halfClosed)
	got := make([]byte, len(answer))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read the answer after a half-close: %v", err)
	}
	if !bytes.Equal(got, answer) {
		t.Fatalf("the answer = %q", got)
	}
}

// dropFirstListener closes its first connection the moment it is accepted, as a node on
// its way down does mid-handshake.
type dropFirstListener struct {
	net.Listener

	dropped atomic.Bool
}

func (l *dropFirstListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.dropped.Swap(true) {
			return conn, nil
		}
		// THE HELLO READ FIRST, so the close is an end of stream: with bytes unread the
		// kernel sends a reset instead, which is not the case this is about.
		if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return nil, err
		}
		if _, err := conn.Read(make([]byte, 16<<10)); err != nil {
			return nil, err
		}
		if err := conn.Close(); err != nil {
			return nil, err
		}
	}
}

// THE REAL OPENSSL'S WORD FOR A NODE GOING AWAY MID-HANDSHAKE, because the relay retries
// only what it recognises, and anything else it takes for final and hangs up on the client.
func TestAHandshakeTheNodeDropsIsTriedAgain(t *testing.T) {
	ca, leaf := relayAuthority(t)
	var config net.ListenConfig
	base, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(&dropFirstListener{Listener: base}, &tls.Config{
		Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13,
	})
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Logf("close the listener: %v", err)
		}
	})
	request := []byte("the client's first bytes")
	serveOne(t, listener, func(conn net.Conn) error {
		if _, err := io.ReadFull(conn, make([]byte, len(request))); err != nil {
			return err
		}
		_, err := conn.Write([]byte("reached"))

		return err
	})

	client := dialRelay(t, startRelay(t, "https://"+listener.Addr().String(), ca))
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("reached"))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("the relay gave up on a node that dropped one handshake: %v", err)
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
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Logf("close the listener: %v", err)
		}
	})
	// WHAT THE NODE DECRYPTED, which is what an unverified peer would hold of a bearer.
	var heard bytes.Buffer
	dialled := make(chan struct{})
	over := make(chan struct{})
	go func() {
		defer close(over)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(dialled)
		// UNTIL THE RELAY HANGS UP, so everything it was going to send has been read. A
		// failed handshake is what this ends with.
		if _, err := io.Copy(&heard, conn); err != nil {
			t.Logf("the node's side ended: %v", err)
		}
	}()

	relay := startRelay(t, "https://"+listener.Addr().String(), otherCA)
	client := dialRelay(t, relay)
	const bearer = "Authorization: Bearer secret"
	if _, err := client.Write([]byte(bearer)); err != nil {
		t.Fatal(err)
	}
	// A CLOSE OR A RESET, whichever the unread bytes make it; what matters is nothing
	// came back, and that it was not the deadline that ended the wait.
	got, err := io.ReadAll(client)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the relay held the client rather than closing on it")
	}
	if len(got) != 0 {
		t.Fatalf("the client read %q; want the relay to close on it", got)
	}
	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	select {
	case <-dialled:
	case <-guard.C:
		t.Fatal("the relay never dialled the node, so this proves nothing")
	}
	select {
	case <-over:
	case <-guard.C:
		t.Fatal("the relay never hung up on the node")
	}
	if heard.Len() != 0 {
		t.Fatalf("an unverified node was sent %q", heard.Bytes())
	}
}
