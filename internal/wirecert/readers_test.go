package wirecert

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// THE DESCRIPTOR THAT IS READ IS THE ONE THAT IS JUDGED. The readers check the
// name first, for their diagnostics, and a host can replace the file between
// that check and the read; what must hold for the bytes loaded (a regular file,
// a key nobody else can read) is decided on the descriptor's own fstat.

func TestReadPublicRefusesAFIFORenamedOverTheCertificateAfterTheCheck(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(cert, []byte("-----BEGIN CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("this platform will not make a fifo: %v", err)
	}
	beforeContentRead = func(path string) {
		if path == cert {
			if err := os.Rename(fifo, cert); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { beforeContentRead = nil })
	blocked := unblockAfter(t, cert, 5*time.Second)
	_, err := readPublic(cert)
	if blocked() {
		t.Fatal("readPublic blocked on the FIFO renamed over the certificate until the test wrote to it")
	}
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a FIFO renamed over the certificate after the check was read or refused for another reason: %v", err)
	}
}

func TestReadSecretJudgesTheModeOfTheKeyItReads(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(key, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose.key")
	if err := os.WriteFile(loose, []byte("loose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeContentRead = func(path string) {
		if path == key {
			if err := os.Rename(loose, key); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { beforeContentRead = nil })
	body, err := readSecret(key)
	if err == nil {
		t.Fatalf("a 0644 key renamed over a 0600 one after the check was read: %q", body)
	}
	if !strings.Contains(err.Error(), "must not be readable by anyone else") {
		t.Errorf("the refusal is not the mode's: %v", err)
	}
}

// unblockAfter opens the FIFO for writing once the deadline passes, so an open
// that blocked returns and the test fails naming it; joined before it answers.
func unblockAfter(t *testing.T, fifo string, after time.Duration) func() bool {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	var acted atomic.Bool
	go func() {
		defer close(done)
		timer := time.NewTimer(after)
		defer timer.Stop()
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				acted.Store(true)
				time.Sleep(50 * time.Millisecond)
				_ = syscall.Close(fd)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	var once atomic.Bool
	stopAndJoin := func() bool {
		if once.CompareAndSwap(false, true) {
			close(stop)
		}
		<-done
		return acted.Load()
	}
	t.Cleanup(func() { stopAndJoin() })
	return stopAndJoin
}
