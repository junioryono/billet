// Package retirement is the leaf both `cmd/billet` and `internal/wirecert`
// import for the one exclusion a controller's retirement shares with every
// other writer of the deployment identity: the stable authority lock beside the
// state root, the world-readable status a closed authority publishes, the
// service-account record privileged installers write, and the hand-back of what
// a privileged command creates inside an identity directory to the account that
// serves it.
//
// It imports nothing of billet's above the standard library and the bounded
// regular-file reader, because `wirecert` may not import `cmd`, and a path or a
// reader shared by both has to live below both.
package retirement

import (
	"path/filepath"
	"runtime"
)

// Root is the state root every path here hangs off. A variable so a test can
// point the package at a directory it owns; nothing else assigns it.
var Root = "/var/lib/billet"

// Platform is the host's operating system as this package and wirecert judge
// it: ONE seam, so a test that stands a Linux host in pins it once and every
// layer agrees. Production reads the build's own.
var Platform = runtime.GOOS

// Linux is the only platform with the global exclusion. A darwin host keeps the
// identity directory's own `ca.lock` as its whole exclusion, records no service
// account, publishes no status and cannot retire; every entry point below
// answers for that platform by doing nothing, and a caller that needs to know
// asks this.
func Supported(hostOS string) bool { return hostOS == "linux" }

// SupportedHere is Supported for Platform.
func SupportedHere() bool { return Supported(Platform) }

// GlobalLockPath is the stable authority lock: taken first by every privileged
// writer of an identity directory, and by an unprivileged one that finds it,
// so a rename of the identity directory cannot overlap a writer inside it.
func GlobalLockPath() string { return filepath.Join(Root, "authority.lock") }

// StatusPath is the world-readable phase a retirement publishes beside the
// lock, outside the private retirement directory, so a service account can read
// that the authority is closed without traversing root's 0700 directory.
func StatusPath() string { return filepath.Join(Root, "authority-status") }

// ServiceAccountPath is the record of the account the services run as: a
// root-provided ownership assertion the lock owners and the hand-back consult.
func ServiceAccountPath() string { return filepath.Join(Root, "service-account") }

// RetiredDir is the private, root-owned 0700 directory holding a retirement's
// journal, its staged serverless configuration and the archived identity.
func RetiredDir() string { return filepath.Join(Root, "retired") }

// JournalPath is the retirement journal's fixed name.
func JournalPath() string { return filepath.Join(RetiredDir(), "journal.json") }

// StagePath is where the serverless configuration's exact bytes wait between
// `intent` and `config-rewritten`.
func StagePath() string { return filepath.Join(RetiredDir(), "config-serverless.yaml") }

// InitLockPath is the fresh-initialisation lock beside an identity directory:
// in the PARENT, so a caller creating the directory and an installer preparing
// the host contend on something that exists before either has acted.
func InitLockPath(identityDir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(identityDir)), ".billet-identity.lock")
}
