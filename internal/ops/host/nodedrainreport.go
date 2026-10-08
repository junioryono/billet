package host

import (
	"log/slog"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops/launchd"
	"github.com/junioryono/billet/internal/version"
)

// publishDrainReport is the launchd package's writer behind a seam, so a test
// can see the node call it without writing into the real log directory.
var PublishDrainReport = launchd.PublishDrainReport

// publishNodeDrainReport tells a stop on a Mac that this process handles
// launchd.DrainSignal. Nothing else reads the report, so off macOS nothing is
// written. A report that could not be written is logged and the node runs on:
// a stop then asks it with its one recorded SIGTERM, as it asks a release that
// predates the request.
func PublishNodeDrainReport(goos string) {
	if goos != "darwin" {
		return
	}

	if err := PublishDrainReport(deploy.NodeAgentLabel, version.Version()); err != nil {
		slog.Default().Warn("could not publish this node's drain report, so a stop will ask it "+
			"to drain with a single SIGTERM", "error", err)
	}
}
