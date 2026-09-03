// Package deploy carries the service definitions billet ships — systemd units
// for Linux and launchd agents for macOS — embedded so a running binary can
// compare what a host actually has against what this build expects.
//
// THE PACKAGE LIVES BESIDE THE FILES, and it has to. go:embed cannot reach a
// parent directory, so an embed kept in internal/ would need ../../deploy and
// simply fails to build. The units are the same bytes GoReleaser installs (see
// .goreleaser.yaml's nfpms contents), which is the point: one source for the
// file on disk and the reference billet reports divergence from.
package deploy

import _ "embed"

// Unit file names, as systemd knows them. Exported so a caller naming a unit
// and the file it compares against cannot disagree by a typo.
const (
	ServerUnitName = "billet-server.service"
	NodeUnitName   = "billet-node.service"
	// BackupUnitName and BackupTimerName are the oneshot that captures a
	// deployment and the timer that runs it. They are NOT part of what `billet
	// local up`/`down`/`status` manage — those own the two services that hold
	// compute and a ledger, and their whole safety content is the order they act
	// in. Embedded so a test can pin what the package installs.
	BackupUnitName  = "billet-backup.service"
	BackupTimerName = "billet-backup.timer"
)

// ServerUnit is the control-plane unit this build ships.
//
//go:embed billet-server.service
var ServerUnit string

// NodeUnit is the compute-host unit this build ships.
//
//go:embed billet-node.service
var NodeUnit string

// BackupUnit and BackupTimer are the scheduled backup this build ships.
//
//go:embed billet-backup.service
var BackupUnit string

//go:embed billet-backup.timer
var BackupTimer string

// Launch agent labels, as launchd knows them. A macOS service is addressed by
// its LABEL rather than its filename, so these are the strings `launchctl`
// takes — and the filenames happen to match, which is convention rather than a
// requirement launchd enforces.
const (
	ServerAgentLabel = "sh.billet.server"
	NodeAgentLabel   = "sh.billet.node"
	ServerAgentName  = ServerAgentLabel + ".plist"
	NodeAgentName    = NodeAgentLabel + ".plist"
)

// ServerAgent and NodeAgent are the macOS launch agents this build ships.
//
// AGENTS RATHER THAN DAEMONS, which is not a packaging preference: since macOS
// 15 Virtualization.framework needs an unlocked login.keychain to run a VM, and
// tart's image store is per-user — so a root daemon has neither the keychain
// nor the images. The plists carry the full reasoning.
//
//go:embed sh.billet.server.plist
var ServerAgent string

//go:embed sh.billet.node.plist
var NodeAgent string
