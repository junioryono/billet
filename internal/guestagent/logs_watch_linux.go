//go:build linux

package guestagent

import (
	"context"
	"os"
	"syscall"
	"time"
)

// pagesWatch wakes the shipper as a step log is created. Not an optimisation: a step that
// wrote little can be created, uploaded and deleted within a poll's second, and a file
// not opened before its delete is lost.
type pagesWatch struct {
	dir string
	// file is the inotify descriptor, nil until the directory exists, which it does
	// not before the job starts.
	file *os.File
	buf  []byte
}

func newPagesWatch(dir string) *pagesWatch {
	return &pagesWatch{dir: dir, buf: make([]byte, 4096)}
}

// watchDir begins watching once the directory exists.
func (w *pagesWatch) watchDir() {
	if w.file != nil {
		return
	}

	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		return
	}

	if _, err := syscall.InotifyAddWatch(fd, w.dir, syscall.IN_CREATE|syscall.IN_MOVED_TO); err != nil {
		_ = syscall.Close(fd)

		return
	}

	w.file = os.NewFile(uintptr(fd), "inotify")
}

// wait returns when a file appears, ctx ends or d has passed.
func (w *pagesWatch) wait(ctx context.Context, d time.Duration) {
	w.watchDir()

	if w.file == nil {
		sleepFor(ctx, d)

		return
	}

	if err := w.file.SetReadDeadline(time.Now().Add(d)); err != nil {
		sleepFor(ctx, d)

		return
	}

	// A STOP ENDS THE WAIT AT ONCE: the final budget is counted from it, and the agent's own
	// exit budget does not leave a second to spare.
	stop := context.AfterFunc(ctx, func() { _ = w.file.SetReadDeadline(time.Now()) })
	defer stop()

	_, _ = w.file.Read(w.buf)
}

func (w *pagesWatch) close() {
	if w.file != nil {
		_ = w.file.Close()
	}
}
