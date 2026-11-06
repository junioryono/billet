//go:build linux

package guestagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A PAGE WAKES THE WAIT, on real inotify and real time: a step that wrote little can be gone
// within a poll's second. The wait is far longer than the test may take, so only the wake can
// end it in time.
func TestAPageWakesTheWatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	watch := newPagesWatch(dir)
	defer watch.close()

	// The first wait begins the watch, and has nothing to wake it.
	watch.wait(t.Context(), time.Millisecond)

	done := make(chan struct{})

	go func() {
		defer close(done)

		watch.wait(t.Context(), time.Minute)
	}()

	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(dir, "page.log"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a page appearing did not wake the wait")
	}
}
