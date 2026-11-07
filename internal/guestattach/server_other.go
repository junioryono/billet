//go:build !linux

package guestattach

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
)

// Serve refuses: the server runs inside a Linux guest, and this is not Linux.
func Serve(context.Context, Config, *slog.Logger) error {
	return fmt.Errorf("billet guest attach-serve runs inside a Linux guest, and this is %s", runtime.GOOS)
}
