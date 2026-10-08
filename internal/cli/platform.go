package cli

import "runtime"

// HostOS is runtime.GOOS behind a seam, so what a command does per platform —
// systemd's shape on Linux, the launch agents' on macOS — and the refusal for
// every other platform are testable from either machine. Only tests set it.
var HostOS = runtime.GOOS
