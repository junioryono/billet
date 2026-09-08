package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostupgrade"
	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
	"github.com/junioryono/billet/internal/wirecert"
)

// `billet release inspect` is the host's own account of what it runs, for a
// converge that must decide whether the fleet is on one release and quiet
// before it selects that release's collection. EVERY SECTION ANSWERS WITH A
// VALUE OR WITH AN EXPLICIT UNKNOWN CARRYING A REASON, never a guess: a check
// answers yes, no or could-not-tell, and could-not-tell never collapses into no.
//
// Nothing here opens the ledger, mints an identity or creates a file, and the
// one lock it touches is the upgrade transaction's, tried non-blocking through
// a read-only descriptor and released at once to answer whether somebody holds
// it. The report costs a stopped controller or a standby nothing it cannot pay.
// What it reads is disk and procfs evidence and it says so: it does not observe
// what a running process loaded or what a listener serves.
//
// ONE CONFIGURATION BINDS THE WHOLE REPORT. Identity, authority and the DSN are
// read from the --config the inspector was given; a unit or a process that
// names another configuration would make those sections describe a different
// deployment from the one running, so config_binding says whether every unit's
// ExecStart and every running process's command line name exactly the
// inspector's path, and a consumer refuses anything but true.

// The seams a test stands in for. procRoot is where a process's exe, stat,
// cmdline and environ are read; selfExePath is this process's own image;
// systemctlBinary answers unit properties; retiredJournalPath is the fixed
// location of a controller's retirement journal, deliberately not under any
// configured directory so a host whose config lost its server: block still
// reports it.
var (
	procRoot        = "/proc"
	selfExePath     = "/proc/self/exe"
	systemctlBinary = "systemctl"
	// busctlBinary answers the LOADED ExecStart as structured records, argument
	// boundaries intact; `systemctl show` joins argv with spaces and `systemctl
	// cat` shows text systemd may not have loaded.
	busctlBinary = "busctl"
	// readPublicFile reads a certificate, a bundle's CA file or the retirement
	// journal; a test records the paths to prove the key file is never asked for.
	readPublicFile     = readRegularFile
	retiredJournalPath = "/var/lib/billet/retired/journal.json"
	// inspectSamples bounds how often a running process's image is re-read when
	// the process changes under the observation.
	inspectSamples = 3
	// openImage opens an executable image by the path given; a test records
	// the paths to prove the link path is opened and never a resolved target.
	openImage = os.Open
	// inspectAfterOpen runs between opening an image and hashing it, so a test
	// can replace the file behind the link and prove the hash is the opened
	// descriptor's.
	inspectAfterOpen func(path string)
	// systemctlTimeout bounds one systemctl show.
	systemctlTimeout = 10 * time.Second
	// inspectAfterConfig runs after the configuration has been read and parsed
	// and before anything is observed, and inspectBeforeClose after every
	// observation and before the closing check, so a test can replace or rewrite
	// the file in either window and prove the report notices.
	inspectAfterConfig func()
	inspectBeforeClose func()
	// inspectBetweenClosingChecks runs between the closing check of the
	// descriptor and the closing stat of the name.
	inspectBetweenClosingChecks func()
	// inspectAfterViewOpen runs after a process's view of a path has been
	// opened, for identity or for reading, so a test can prove an unsupported
	// command line's operand is never opened at all.
	inspectAfterViewOpen func(path string)
)

// inspectSchema is the report's schema version; a field renamed or removed
// under the same number is a broken consumer, so the tests pin the field set.
const inspectSchema = 1

// maybe is a value that may instead be an explicit unknown with a reason. It
// marshals as the value itself, or as {"unknown": "<reason>"}.
type maybe struct {
	known bool
	value any
	why   string
}

func known(v any) maybe        { return maybe{known: true, value: v} }
func unknown(why string) maybe { return maybe{why: why} }

func (m maybe) MarshalJSON() ([]byte, error) {
	if m.known {
		return json.Marshal(m.value)
	}
	return json.Marshal(map[string]string{"unknown": m.why})
}

type inspectReport struct {
	Schema        int                       `json:"schema"`
	Config        inspectConfig             `json:"config"`
	ConfigBinding maybe                     `json:"config_binding"`
	Executable    inspectExecutable         `json:"executable"`
	Provenance    inspectProvenance         `json:"provenance"`
	Services      map[string]inspectService `json:"services"`
	Installed     inspectInstalledConfig    `json:"installed_config"`
	Host          inspectHost               `json:"host"`
	Transaction   inspectTransaction        `json:"transaction"`
}

type inspectConfig struct {
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Error    string `json:"error,omitempty"`
	// Presence is the typed answer behind Readable: `present` (read and
	// parsed), `absent` (positively no file at the path), `malformed` (read but
	// not parsed) or `unreadable` (could not be read, which is not absence).
	Presence string `json:"presence"`
	// SameAsInstalled says whether, at the end of the report, the path still
	// names the file the report read: a replacement by rename after the parse
	// leaves the retained descriptor's metadata unchanged and is caught here.
	SameAsInstalled maybe `json:"same_as_installed"`
}

type inspectExecutable struct {
	Image             string `json:"image"`
	SHA256            maybe  `json:"sha256"`
	ProcessBound      bool   `json:"process_bound"`
	InstalledPath     string `json:"installed_path"`
	InstalledPathSame maybe  `json:"installed_path_same"`
	Version           string `json:"version"`
	IsRelease         bool   `json:"is_release"`
}

type inspectProvenance struct {
	Version        string `json:"version,omitempty"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
	BinarySHA256   string `json:"binary_sha256,omitempty"`
	Verdict        string `json:"verdict"`
	Reason         string `json:"reason,omitempty"`
}

type inspectService struct {
	UnitPresent      maybe  `json:"unit_present"`
	UnitFileState    maybe  `json:"unit_file_state"`
	Enabled          maybe  `json:"enabled"`
	ActiveState      maybe  `json:"active_state"`
	SubState         maybe  `json:"sub_state"`
	MainPID          maybe  `json:"main_pid"`
	ExecMainStart    maybe  `json:"exec_main_start"`
	ExecStart        maybe  `json:"exec_start"`
	EnvironmentFiles maybe  `json:"environment_files"`
	Shape            maybe  `json:"shape"`
	ShapeReason      string `json:"shape_reason,omitempty"`
	NeedDaemonReload maybe  `json:"need_daemon_reload"`

	// fromObservation marks a service whose config digest and mtime were taken
	// from the configuration observation, so the closing check can withdraw
	// them with it.
	fromObservation bool

	RunningSHA256                  maybe `json:"running_sha256"`
	SameAsExecutable               maybe `json:"same_as_executable"`
	StartedAt                      maybe `json:"started_at"`
	CmdlineConfigPath              maybe `json:"cmdline_config_path"`
	CmdlineMatchesUnit             maybe `json:"cmdline_matches_unit"`
	ConfigSHA256                   maybe `json:"config_sha256"`
	ConfigChangedSinceStart        maybe `json:"config_changed_since_start"`
	EnvironmentFileChangedSinceRun maybe `json:"environment_file_changed_since_start"`
	LoadedConfig                   maybe `json:"loaded_config"`
	DSNEnv                         maybe `json:"dsn_env"`
}

type inspectDSNEnv struct {
	Name        string `json:"name"`
	Present     maybe  `json:"present"`
	MatchesFile maybe  `json:"matches_file"`
}

type inspectInstalledConfig struct {
	Path          maybe `json:"path"`
	SHA256        maybe `json:"sha256"`
	HasServer     maybe `json:"has_server"`
	HasNode       maybe `json:"has_node"`
	LedgerBackend maybe `json:"ledger_backend"`
	Controllers   maybe `json:"controllers"`
}

type inspectHost struct {
	OS           string `json:"os"`
	NodeName     maybe  `json:"node_name"`
	DeploymentID maybe  `json:"deployment_id"`
	Retirement   maybe  `json:"retirement"`
	Authority    maybe  `json:"authority"`
	NodeTrust    maybe  `json:"node_trust"`
}

type inspectCertificate struct {
	PEM       string `json:"pem"`
	DERSHA256 string `json:"der_sha256"`
	Subject   string `json:"subject"`
	NotAfter  string `json:"not_after"`
}

type inspectAuthority struct {
	Current            inspectCertificate  `json:"current"`
	Previous           *inspectCertificate `json:"previous"`
	RotationInProgress bool                `json:"rotation_in_progress"`
	Created            bool                `json:"created"`
}

type inspectNodeTrust struct {
	Leaf inspectCertificate   `json:"leaf"`
	CAs  []inspectCertificate `json:"cas"`
}

type inspectTransaction struct {
	Root          string `json:"root"`
	Active        maybe  `json:"active"`
	LockHeld      maybe  `json:"lock_held"`
	Journal       maybe  `json:"journal"`
	ConvergeGuard maybe  `json:"converge_guard"`
	Preparation   maybe  `json:"preparation"`
}

type inspectJournal struct {
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	Step        string `json:"step"`
	StartedAt   string `json:"started_at"`
	Failure     string `json:"failure,omitempty"`
}

type inspectGuard struct {
	Holder                    string `json:"holder"`
	ClaimedAt                 string `json:"claimed_at"`
	Hostname                  string `json:"hostname"`
	RecoveryPointer           maybe  `json:"recovery_pointer"`
	ReleaseExecutable         string `json:"release_executable"`
	ReleaseExecutableSHA256   string `json:"release_executable_sha256"`
	ReleaseExecutableVerified maybe  `json:"release_executable_verified"`
}

type inspectPreparation struct {
	TransactionLock maybe `json:"transaction_lock"`
}

func cmdReleaseInspect(ctx context.Context, args []string) error {
	flags := newFlagSet("billet release inspect")
	configPath := flags.String("config", defaultConfigPath(), "path to billet.yaml")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	if err := parse(flags, args); err != nil {
		return err
	}
	report := inspectHostRelease(ctx, *configPath)
	if *asJSON {
		body, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("render the report: %w", err)
		}
		fmt.Println(string(body))
		return nil
	}
	printInspectReport(report)
	return nil
}

// inspectHostRelease assembles the whole report. A config that cannot be read
// leaves the sections that need it unknown and fills every other one, because
// what the executable and the upgrade root say is worth having on a host whose
// configuration is the thing that broke.
func inspectHostRelease(ctx context.Context, configPath string) inspectReport {
	// The binding compares paths as strings, so the inspector's own is made
	// absolute first; a relative one would compare equal to a service's
	// relative argument that names another directory's file.
	if abs, err := filepath.Abs(configPath); err == nil {
		configPath = abs
	}
	report := inspectReport{Schema: inspectSchema, Config: inspectConfig{Path: configPath}}
	// ONE OBSERVATION OF THE CONFIGURATION. The bytes parsed, the digest
	// reported and the file identity every process's view is compared with all
	// come from one bracketed read of one descriptor, held open to the end of
	// the report; otherwise a file replaced after the parse would give a report
	// that describes configuration A's roles, backend and identity beside
	// configuration B's digest and binding, and no single hash would notice.
	obs, obsErr := observeConfig(configPath)
	var cfg *config.Config
	var inspectorInfo os.FileInfo
	var digest maybe
	inspectorSHA := ""
	switch {
	case errors.Is(obsErr, fs.ErrNotExist):
		report.Config.Presence = "absent"
		report.Config.Error = obsErr.Error()
		digest = unknown(obsErr.Error())
		report.Config.SameAsInstalled = unknown(obsErr.Error())
	case obsErr != nil:
		report.Config.Presence = "unreadable"
		report.Config.Error = obsErr.Error()
		digest = unknown(obsErr.Error())
		report.Config.SameAsInstalled = unknown(obsErr.Error())
	default:
		defer obs.file.Close()
		inspectorInfo = obs.info
		inspectorSHA = obs.sha
		digest = known(obs.sha)
		parsed, err := config.Parse(configPath, obs.body)
		if err != nil {
			report.Config.Presence = "malformed"
			report.Config.Error = err.Error()
		} else {
			report.Config.Presence = "present"
			cfg = parsed
			report.Config.Readable = true
		}
	}
	if inspectAfterConfig != nil {
		inspectAfterConfig()
	}

	exeSHA, exeInfo := inspectExecutableSection(&report)
	inspectProvenanceSection(&report, exeSHA)
	report.Services = map[string]inspectService{}
	binding := known(true)
	for role, unit := range map[string]string{"server": deploy.ServerUnitName, "node": deploy.NodeUnitName} {
		svc, bound := inspectServiceSection(ctx, role, unit, cfg, configPath, inspectorInfo, inspectorSHA, exeSHA, exeInfo)
		report.Services[role] = svc
		binding = weaker(binding, bound)
	}
	report.ConfigBinding = binding
	report.Installed = inspectInstalledSection(cfg, configPath, report.Config, digest)
	report.Host = inspectHostSection(cfg)
	report.Transaction = inspectTransactionSection()
	if inspectBeforeClose != nil {
		inspectBeforeClose()
	}
	// THE FILE DESCRIBED MUST STILL BE THE FILE ON DISK, BY BYTES AND BY NAME.
	// A rewrite in place during the report keeps the identity the views were
	// compared with and changes the bytes, so a binding that said true no
	// longer says anything, and neither does the digest; a replacement by
	// rename after the parse leaves the retained descriptor's metadata as it
	// was and puts another file at the name, so the name is stat'ed too and a
	// different identity makes the binding false (the host holds a
	// configuration this report does not describe). A false stays false.
	if obsErr == nil {
		changed := "the configuration changed while the report was being made"
		if again, err := obs.file.Stat(); err != nil || !sameMetadata(again, obs.info) {
			withdrawObservation(&report, changed)
		}
		if inspectBetweenClosingChecks != nil {
			inspectBetweenClosingChecks()
		}
		pathInfo, err := os.Stat(configPath)
		switch {
		case err != nil:
			why := fmt.Sprintf("stat %s at the end of the report: %v", configPath, err)
			report.Config.SameAsInstalled = unknown(why)
			if report.ConfigBinding.known && report.ConfigBinding.value == true {
				report.ConfigBinding = unknown(why)
			}
		case !os.SameFile(pathInfo, obs.info):
			report.Config.SameAsInstalled = known(false)
			report.ConfigBinding = known(false)
		case !sameMetadata(pathInfo, obs.info):
			// The name still holds the file, and the stat of the name is
			// evidence of a rewrite the descriptor's own stat came too early
			// to see; it is not discarded.
			report.Config.SameAsInstalled = known(true)
			withdrawObservation(&report, changed)
		default:
			report.Config.SameAsInstalled = known(true)
		}
	}
	return report
}

// sameMetadata is the bracket every hash here uses: equal size and equal
// modification time, which is refusal evidence and not proof of equal bytes.
func sameMetadata(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// withdrawObservation is what the closing check does when the configuration
// observed is no longer the configuration on disk: a binding that said true
// says nothing, the observation's digest says nothing, and so does every
// service field that was copied from the observation; a false binding and the
// fields a service observed on its own (another configuration's digest) stay.
func withdrawObservation(report *inspectReport, why string) {
	if report.ConfigBinding.known && report.ConfigBinding.value == true {
		report.ConfigBinding = unknown(why)
	}
	report.Installed.SHA256 = unknown(why)
	for role := range report.Services {
		svc := report.Services[role]
		if !svc.fromObservation {
			continue
		}
		svc.ConfigSHA256, svc.ConfigChangedSinceStart = unknown(why), unknown(why)
		report.Services[role] = svc
	}
}

// configObservation is one bracketed read of the configuration: its bytes, their
// digest and the descriptor's identity, with the descriptor kept open.
type configObservation struct {
	file *os.File
	info os.FileInfo
	body []byte
	sha  string
}

// observeConfig reads the configuration through one descriptor, bracketed by
// its metadata like every hash here, and hands the descriptor back open.
func observeConfig(path string) (*configObservation, error) {
	f, _, err := openRegular(path, false)
	if err != nil {
		return nil, err
	}
	before, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if inspectAfterOpen != nil {
		inspectAfterOpen(path)
	}
	body, err := io.ReadAll(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	after, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while it was being read", path)
	}
	sum := sha256.Sum256(body)
	return &configObservation{file: f, info: after, body: body, sha: hex.EncodeToString(sum[:])}, nil
}

// inspectExecutableSection hashes THE IMAGE THIS PROCESS EXECUTES. On Linux
// that is /proc/self/exe opened by that link path and read through the
// descriptor, which is the running bytes whatever the managed path holds now;
// os.Executable returns a pathname, and a pathname opened after a replacement
// is the replacement. On darwin there is no process-bound image, so the hash
// is of the managed path and the report says it is path evidence.
// weaker combines two three-valued answers: false beats unknown beats true,
// because one unit that names another configuration is a disagreement whatever
// the other says, and one that could not be read leaves the question open.
func weaker(a, b maybe) maybe {
	switch {
	case a.known && a.value == false:
		return a
	case b.known && b.value == false:
		return b
	case !a.known:
		return a
	case !b.known:
		return b
	}
	return a
}

func inspectExecutableSection(report *inspectReport) (string, os.FileInfo) {
	exe := inspectExecutable{
		InstalledPath: installedBinary,
		Version:       version.Version(),
		IsRelease:     version.IsRelease(version.Version()),
	}
	image := selfExePath
	if hostOS == "darwin" {
		image = installedBinary
	} else {
		exe.ProcessBound = true
	}
	exe.Image = image

	sum, info, err := hashImage(image)
	if err != nil {
		exe.SHA256 = unknown(err.Error())
		exe.InstalledPathSame = unknown("the image could not be hashed")
		report.Executable = exe
		return "", nil
	}
	exe.SHA256 = known(sum)
	installed, err := os.Stat(installedBinary)
	switch {
	case err != nil:
		exe.InstalledPathSame = unknown(fmt.Sprintf("stat %s: %v", installedBinary, err))
	default:
		exe.InstalledPathSame = known(os.SameFile(info, installed))
	}
	report.Executable = exe
	return sum, info
}

// hashImage opens an image by the path given and hashes it through that one
// descriptor, returning the descriptor's own stat. THE READ IS BRACKETED BY THE
// DESCRIPTOR'S METADATA: a stat before and after the hash that disagree in
// size or modification time mean the bytes were written while they were read,
// and a hash paired with either stat would describe a file that never existed
// whole; that is could-not-tell. A write after the second stat is outside this
// observation, and equal size and mtime are not proof the bytes never changed:
// the bracket refuses what it can see and claims nothing more.
func hashImage(path string) (string, os.FileInfo, error) {
	f, err := openImage(path)
	if err != nil {
		return "", nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return hashOpenFile(f, path)
}

// hashOpenFile is the bracketed hash of an already open descriptor: its own
// stat before and after the read must agree in size and modification time.
func hashOpenFile(f *os.File, path string) (string, os.FileInfo, error) {
	before, err := f.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if inspectAfterOpen != nil {
		inspectAfterOpen(path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", nil, fmt.Errorf("read %s: %w", path, err)
	}
	after, err := f.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", nil, fmt.Errorf("%s changed while it was being read", path)
	}
	return hex.EncodeToString(h.Sum(nil)), after, nil
}

// readRegularFile reads a whole file through openRegular (releaserecord.go): EVERY
// FILE THE INSPECTOR READS BY PATHNAME goes through that one open, non-blocking
// and then fstat'ed, because the inspector's inputs are named by its
// configuration or by systemd's answer, and a plain open of a FIFO at any of
// them blocks before any stat, which neither the sample count nor systemctl's
// timeout bounds. A special file is could-not-tell with the reason instead.
func readRegularFile(path string) ([]byte, error) {
	f, _, err := openRegular(path, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// hashRegular is the bracketed hash of a file the inspector names by path and
// that is not a process image: an environment file, a recorded release
// executable. The process images stay with hashImage, opened by their /proc
// link path, which the kernel resolves to the executed inode.
func hashRegular(path string) (string, os.FileInfo, error) {
	f, _, err := openRegular(path, false)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = f.Close() }()
	return hashOpenFile(f, path)
}

// inspectProvenanceSection computes the verdict the way provenance.Installed
// does, from ONE read of the record and the executable hash already taken;
// it never re-reads the record for a second answer.
func inspectProvenanceSection(report *inspectReport, exeSHA string) {
	record, err := provenance.Read()
	switch {
	case errors.Is(err, provenance.ErrNoRecord):
		report.Provenance = inspectProvenance{Verdict: "none", Reason: "no provenance record"}
	case err != nil:
		report.Provenance = inspectProvenance{Verdict: "unreadable", Reason: err.Error()}
	case exeSHA == "":
		report.Provenance = inspectProvenance{
			Version: record.Version, ManifestDigest: record.ManifestDigest,
			BinarySHA256: record.BinarySHA256, Verdict: "unreadable",
			Reason: "the executable could not be hashed, so the record cannot be bound to it",
		}
	case record.BinarySHA256 == exeSHA:
		report.Provenance = inspectProvenance{
			Version: record.Version, ManifestDigest: record.ManifestDigest,
			BinarySHA256: record.BinarySHA256, Verdict: "proved",
		}
	default:
		report.Provenance = inspectProvenance{
			Version: record.Version, ManifestDigest: record.ManifestDigest,
			BinarySHA256: record.BinarySHA256, Verdict: "contradiction",
			Reason: "the record names other bytes than the executable",
		}
	}
}

// unitProperties is one `systemctl show` for the properties the report needs,
// parsed into key -> values (a key can repeat, ExecStart among them).
func unitProperties(ctx context.Context, unit string) (map[string][]string, error) {
	ctx, cancel := context.WithTimeout(ctx, systemctlTimeout)
	defer cancel()
	names := []string{"LoadState", "UnitFileState", "ActiveState", "SubState", "MainPID",
		"InvocationID", "NeedDaemonReload", "ExecMainStartTimestamp", "ExecStart", "EnvironmentFiles",
		"Environment", "RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "MountImages",
		"ExtensionImages", "ExtensionDirectories", "TemporaryFileSystem"}
	args := make([]string, 0, len(names)+3)
	args = append(args, "show")
	for _, n := range names {
		args = append(args, "--property="+n)
	}
	args = append(args, "--", unit)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, systemctlBinary, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("systemctl show %s: %w: %s", unit, err, strings.TrimSpace(stderr.String()))
	}
	props := map[string][]string{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		props[key] = append(props[key], value)
	}
	return props, nil
}

// unitEnablement reads systemd's UnitFileState literally and derives `enabled`
// from it: `enabled` and `enabled-runtime` are both enablement (a runtime
// enablement is one systemd honours until the next boot, so it is not
// "disabled"); false means A RECOGNISED STATE OTHER THAN THOSE TWO, which is
// not "positively disabled" (`static`, `alias`, `indirect` and `generated`
// units can still be started by something else), so a consumer that needs a
// particular state reads unit_file_state against its own accepted set; and an
// answer outside systemd's set is could-not-tell rather than false.
func unitEnablement(answer string) (maybe, maybe) {
	switch answer {
	case "enabled", "enabled-runtime":
		return known(answer), known(true)
	case "disabled", "static", "masked", "masked-runtime", "indirect", "generated", "transient",
		"linked", "linked-runtime", "alias", "bad":
		return known(answer), known(false)
	default:
		why := "systemd answered UnitFileState=" + strconv.Quote(answer)
		return unknown(why), unknown(why)
	}
}

// needDaemonReload reads systemd's NeedDaemonReload as the three-valued fact
// it is. On systemd 255 it is true when this unit's fragment, source or drop-ins
// changed since load OR when the manager's `unit_file_state_outdated` is set,
// which any unit-file operation over the bus that changed something (enable,
// disable, preset, mask, link, revert) does, for every unit, until the next
// daemon-reload; the second cause dominates in practice (measured on the
// reference controller, 2026-09-07: 464 of 464 units after snapd enabled a new
// mount unit, billet's own files untouched since the last reload). So a yes
// does not say that THIS unit's file differs from what was loaded, and the
// loaded records above are what systemd runs either way; the fact is reported
// and left out of the shape.
func needDaemonReload(props map[string][]string) maybe {
	switch v := firstProp(props, "NeedDaemonReload"); v {
	case "yes":
		return known(true)
	case "no":
		return known(false)
	default:
		return unknown("systemd answered NeedDaemonReload=" + strconv.Quote(v))
	}
}

func firstProp(props map[string][]string, name string) string {
	if v := props[name]; len(v) > 0 {
		return strings.TrimSpace(v[0])
	}
	return ""
}

// execStartArgvOf reads the argv out of systemd's rendered ExecStart
// (`{ path=... ; argv[]=... ; ignore_errors=... }`).
func execStartArgvOf(rendered string) string {
	const marker = "argv[]="
	i := strings.Index(rendered, marker)
	if i < 0 {
		return ""
	}
	rest := rendered[i+len(marker):]
	if end := strings.Index(rest, " ;"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// execRecord is one loaded ExecStart entry as systemd holds it: the executable
// path and the argument vector with its boundaries.
type execRecord struct {
	Path string
	Argv []string
}

// unitExecStart reads the LOADED ExecStart of a unit as structured records
// over D-Bus (`busctl --json=short get-property ... Service ExecStart`, whose
// value is a(sasbttttuii)). `systemctl show` renders argv joined by spaces, so
// `billet "server --config x"` displays like the supported four-argument form,
// and `systemctl cat` shows text systemd may not have loaded or may parse
// otherwise (whitespace around `=`, a later assignment resetting the list); the
// bus property is the one place the loaded vector is what it is.
func unitExecStart(ctx context.Context, unit string) ([]execRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, systemctlTimeout)
	defer cancel()
	object := "/org/freedesktop/systemd1/unit/" + busLabel(unit)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, busctlBinary, "--json=short", "get-property",
		"org.freedesktop.systemd1", object, "org.freedesktop.systemd1.Service", "ExecStart")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("busctl get-property ExecStart of %s: %w: %s", unit, err, strings.TrimSpace(stderr.String()))
	}
	var reply struct {
		Type string            `json:"type"`
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		return nil, fmt.Errorf("busctl answered for %s in a form the inspector does not read: %w", unit, err)
	}
	if reply.Type != "a(sasbttttuii)" {
		return nil, fmt.Errorf("busctl answered for %s with type %q, not the ExecStart record type", unit, reply.Type)
	}
	out := make([]execRecord, 0, len(reply.Data))
	for _, raw := range reply.Data {
		var fields []json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || len(fields) < 2 {
			return nil, fmt.Errorf("busctl answered for %s with an ExecStart record the inspector does not read", unit)
		}
		var rec execRecord
		if err := json.Unmarshal(fields[0], &rec.Path); err != nil {
			return nil, fmt.Errorf("busctl answered for %s with an ExecStart path the inspector does not read", unit)
		}
		if err := json.Unmarshal(fields[1], &rec.Argv); err != nil {
			return nil, fmt.Errorf("busctl answered for %s with an ExecStart argv the inspector does not read", unit)
		}
		out = append(out, rec)
	}
	return out, nil
}

// remapped names the first directive that gives the unit's processes a view
// of the filesystem in which an absolute path means another file, or "" when
// none is set: another root, a bind mount, a mounted image, a temporary
// filesystem, or an extension image or directory (a system extension overlays
// /usr, where the binary is, and a configuration extension /etc, where the
// config is, both under a root that still reads "/"). Sandboxing that hides
// paths (ProtectSystem, ProtectHome, InaccessiblePaths) does not substitute the
// configuration and is not listed here.
func remapped(props map[string][]string) string {
	for _, name := range remappingDirectives {
		if strings.TrimSpace(firstProp(props, name)) != "" {
			return name
		}
	}
	return ""
}

// remappingDirectives are the exec settings under which an absolute path names
// another file; the shape refuses any of them set, and the confirming read of
// a sample requires them unchanged.
var remappingDirectives = []string{"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths",
	"MountImages", "ExtensionImages", "ExtensionDirectories", "TemporaryFileSystem"}

// busLabel escapes a unit name the way systemd names its bus objects: letters
// and digits stay (a leading digit is escaped), everything else becomes _XX.
func busLabel(name string) string {
	var b strings.Builder
	for i := range len(name) {
		ch := name[i]
		alnum := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
		leadingDigit := i == 0 && ch >= '0' && ch <= '9'
		if alnum && !leadingDigit {
			b.WriteByte(ch)
			continue
		}
		fmt.Fprintf(&b, "_%02x", ch)
	}
	return b.String()
}

// environmentFilesOfAll reads every EnvironmentFiles property line systemd
// printed (one per file on systemd 255, `<path> (ignore_errors=yes|no)`). The
// suffix is stripped exactly and the WHOLE path is kept, because a path can
// itself contain " (" and a parser that cut at the first one would name a
// sibling file; a line in any other form is refused rather than guessed at.
func environmentFilesOfAll(lines []string) ([]string, error) {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var path string
		switch {
		case strings.HasSuffix(line, " (ignore_errors=yes)"):
			path = strings.TrimSuffix(line, " (ignore_errors=yes)")
		case strings.HasSuffix(line, " (ignore_errors=no)"):
			path = strings.TrimSuffix(line, " (ignore_errors=no)")
		default:
			return nil, fmt.Errorf("an EnvironmentFiles entry in a form the inspector does not read")
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("an EnvironmentFiles entry that is not an absolute path")
		}
		out = append(out, path)
	}
	return out, nil
}

// inspectServiceSection reports one unit and says whether it is bound to the
// inspector's configuration. A unit systemd does not know is present:false; a
// unit that is running is bound to its process by the process's own command
// line, never by the unit file alone, because a unit edited and reloaded
// without a restart names a path the process never read. The second answer is
// the binding: true when every path the unit and its process name is the
// inspector's config, false on a disagreement, unknown when a shape could not
// be read.
func inspectServiceSection(ctx context.Context, role, unit string, cfg *config.Config,
	inspectorConfig string, inspectorInfo os.FileInfo, inspectorSHA string, exeSHA string, exeInfo os.FileInfo,
) (inspectService, maybe) {
	svc := inspectService{LoadedConfig: unknown("nothing on the disk proves what a process read at start")}
	if hostOS == "darwin" {
		return serviceAllUnknown(svc, "launchd has no unit shape this inspector reads"), unknown("launchd has no unit shape this inspector reads")
	}
	props, err := unitProperties(ctx, unit)
	if err != nil {
		return serviceAllUnknown(svc, err.Error()), unknown(err.Error())
	}
	active := firstProp(props, "ActiveState")
	pid, pidErr := strconv.Atoi(firstProp(props, "MainPID"))
	if firstProp(props, "LoadState") == "not-found" {
		// THE FRAGMENT IS GONE; THE RUNTIME FACTS ARE NOT DISCARDED WITH IT: a
		// unit whose file was removed while its service runs still runs.
		svc.UnitPresent = known(false)
		svc.UnitFileState, svc.Enabled = known(nil), known(nil)
		svc.ExecStart, svc.Shape, svc.EnvironmentFiles = known(nil), known(nil), known(nil)
		svc.NeedDaemonReload = needDaemonReload(props)
		svc.ActiveState, svc.SubState = known(active), known(firstProp(props, "SubState"))
		svc.ExecMainStart = known(firstProp(props, "ExecMainStartTimestamp"))
		switch {
		case pidErr != nil:
			svc.MainPID = unknown("systemd reported a MainPID that is not a number")
		case pid == 0:
			// No process is null here as it is for a loaded unit, never 0.
			svc.MainPID = known(nil)
		default:
			svc.MainPID = known(pid)
		}
		if pidErr == nil && pid == 0 && active == "inactive" {
			fillRunningNull(&svc)
			svc.DSNEnv = known(nil)
			return svc, known(true)
		}
		why := "the unit's fragment is gone while the service is not positively stopped"
		fillRunningUnknown(&svc, why)
		svc.DSNEnv = unknown(why)
		return svc, unknown(why)
	}
	svc.UnitPresent = known(true)
	svc.UnitFileState, svc.Enabled = unitEnablement(firstProp(props, "UnitFileState"))
	svc.ActiveState = known(active)
	svc.SubState = known(firstProp(props, "SubState"))
	svc.ExecMainStart = known(firstProp(props, "ExecMainStartTimestamp"))
	rendered := ""
	if len(props["ExecStart"]) > 0 {
		rendered = execStartArgvOf(props["ExecStart"][0])
	}
	svc.ExecStart = known(rendered)
	envFiles, envErr := environmentFilesOfAll(props["EnvironmentFiles"])
	if envErr != nil {
		svc.EnvironmentFiles = unknown(envErr.Error())
	} else {
		svc.EnvironmentFiles = known(envFiles)
	}
	svc.NeedDaemonReload = needDaemonReload(props)

	// THE SHAPE BILLET SHIPS, AND NOTHING ELSE, read where the loaded argument
	// vector is what it is: the bus property holds ExecStart as records with
	// boundaries, so the one record's argv must be exactly `<managed path>
	// <role> --config <absolute path>` and its path the managed path; the
	// rendered `systemctl show` view must agree word for word; at most one
	// EnvironmentFile (the package has none, the role's template adds one), and
	// no Environment= directive. Anything else is could-not-tell for the
	// binding below. The pending-reload flag is deliberately NOT a shape
	// requirement: the loaded records are what systemd runs, and the flag has
	// two causes, this unit's own file and the manager-wide one, which it does
	// not tell apart (needDaemonReload).
	unitConfigPath := ""
	shapeWhy := ""
	records, execErr := unitExecStart(ctx, unit)
	switch {
	case execErr != nil:
		shapeWhy = execErr.Error()
	case remapped(props) != "":
		// A directive that gives the service another view of the filesystem
		// makes its `--config /etc/billet/billet.yaml` name a file the
		// inspector's `/etc/billet/billet.yaml` is not; equal paths would then
		// bind two configurations.
		shapeWhy = "a filesystem remapping directive (" + remapped(props) + ")"
	case len(records) != 1:
		shapeWhy = fmt.Sprintf("%d ExecStart records loaded", len(records))
	case len(records[0].Argv) != 4 || records[0].Argv[0] != installedBinary || records[0].Argv[1] != role ||
		records[0].Argv[2] != "--config":
		shapeWhy = "ExecStart is not `" + installedBinary + " " + role + " --config <path>`: " + strings.Join(records[0].Argv, " ")
	case !filepath.IsAbs(records[0].Argv[3]):
		shapeWhy = "ExecStart's --config path is not absolute"
	case records[0].Path != installedBinary:
		shapeWhy = "ExecStart's executable path is not " + installedBinary
	case rendered != strings.Join(records[0].Argv, " "):
		shapeWhy = "the rendered ExecStart does not match the loaded one"
	case envErr != nil:
		shapeWhy = envErr.Error()
	case len(envFiles) > 1:
		shapeWhy = fmt.Sprintf("%d EnvironmentFile directives", len(envFiles))
	case strings.TrimSpace(firstProp(props, "Environment")) != "":
		shapeWhy = "an Environment= directive"
	default:
		unitConfigPath = records[0].Argv[3]
	}
	binding := unknown("the unit's shape is unsupported, so it names no single config path")
	if shapeWhy != "" {
		svc.Shape = known("unsupported")
		svc.ShapeReason = shapeWhy
	} else {
		svc.Shape = known("supported")
		binding = known(unitConfigPath == inspectorConfig)
	}

	if pidErr != nil {
		why := "systemd reported a MainPID that is not a number"
		svc.MainPID = unknown(why)
		fillRunningUnknown(&svc, why)
		svc.DSNEnv = unknown(why)
		return svc, weaker(binding, unknown(why))
	}
	if pid <= 0 {
		svc.MainPID = known(nil)
		fillRunningNull(&svc)
		svc.DSNEnv = known(nil)
		return svc, binding
	}
	svc.MainPID = known(pid)
	processBinding := inspectRunningProcess(ctx, &svc, role, unit, pid, props, records, cfg, inspectorConfig, inspectorInfo, inspectorSHA, exeSHA, exeInfo, unitConfigPath, envFiles)
	return svc, weaker(binding, processBinding)
}

// serviceAllUnknown is a unit nothing could be asked about.
func serviceAllUnknown(svc inspectService, why string) inspectService {
	svc.UnitPresent = unknown(why)
	svc.UnitFileState, svc.Enabled = unknown(why), unknown(why)
	svc.ActiveState, svc.SubState, svc.MainPID = unknown(why), unknown(why), unknown(why)
	svc.ExecMainStart, svc.ExecStart, svc.Shape, svc.EnvironmentFiles = unknown(why), unknown(why), unknown(why), unknown(why)
	svc.NeedDaemonReload = unknown(why)
	fillRunningUnknown(&svc, why)
	svc.DSNEnv = unknown(why)
	return svc
}

// fillRunningUnknown is a running process that could not be read.
func fillRunningUnknown(svc *inspectService, why string) {
	svc.RunningSHA256, svc.SameAsExecutable, svc.StartedAt = unknown(why), unknown(why), unknown(why)
	svc.CmdlineConfigPath, svc.CmdlineMatchesUnit = unknown(why), unknown(why)
	svc.ConfigSHA256, svc.ConfigChangedSinceStart = unknown(why), unknown(why)
	svc.EnvironmentFileChangedSinceRun = unknown(why)
}

// fillRunningNull is a unit with no process: the fields do not apply, which is
// an answer and not a failure to answer.
func fillRunningNull(svc *inspectService) {
	svc.RunningSHA256, svc.SameAsExecutable, svc.StartedAt = known(nil), known(nil), known(nil)
	svc.CmdlineConfigPath, svc.CmdlineMatchesUnit = known(nil), known(nil)
	svc.ConfigSHA256, svc.ConfigChangedSinceStart = known(nil), known(nil)
	svc.EnvironmentFileChangedSinceRun = known(nil)
}

// processSample is everything read about one process under one bracket.
type processSample struct {
	sha        string
	startTicks int64
	cmdline    []string
	environ    []byte
	// view is the file the process's own root resolves its --config path to,
	// opened through /proc/<pid>/root inside the sample, viewSHA its bracketed
	// digest, and viewErr says why neither could be had.
	view    os.FileInfo
	viewSHA string
	viewErr string
}

// unitIdentity is what must not move across a sample: which process systemd
// calls the unit's main one, which invocation it is, and the exec-shaping
// properties whose change would make the unit another unit.
func unitIdentity(props map[string][]string, records []execRecord) string {
	parts := make([]string, 0, 6+len(remappingDirectives))
	parts = append(parts,
		firstProp(props, "MainPID"), firstProp(props, "InvocationID"),
		strings.Join(props["ExecStart"], "\x00"), strings.Join(props["EnvironmentFiles"], "\x00"),
		strings.Join(props["Environment"], "\x00"), recordsKey(records),
	)
	// The remapping directives and the structured records are part of the
	// shape, so a reload that adds a bind mount or moves an argument boundary
	// under the sample, leaving the rendered strings alone, must discard it.
	for _, name := range remappingDirectives {
		parts = append(parts, firstProp(props, name))
	}
	return strings.Join(parts, "\x01")
}

// recordsKey is the loaded ExecStart records as one comparable string.
func recordsKey(records []execRecord) string {
	parts := make([]string, 0, len(records))
	for _, r := range records {
		parts = append(parts, r.Path+"\x00"+strings.Join(r.Argv, "\x00"))
	}
	return strings.Join(parts, "\x02")
}

// sampleProcess reads a unit's main process as ONE SAMPLE bound to one
// incarnation: the start time before; the image through /proc/<pid>/exe opened
// by that path (a "(deleted)" target reopened by name would hash the
// replacement), the command line and the environment; then systemd's identity
// of the unit again (MainPID, InvocationID and the exec-shaping properties
// must equal the first read) and, AFTER that systemd read, the start time
// again. A pid reused between the two systemd reads has a different start
// time at the end, and a unit that changed under the sample has a different
// identity, so evidence about the image is never attributed to a process the
// other reads saw. Three samples at most, then could-not-tell.
func sampleProcess(ctx context.Context, unit, role string, pid int, first map[string][]string, records []execRecord, inspectorConfig string) (processSample, error) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	identity := unitIdentity(first, records)
	for range inspectSamples {
		before, err := processStartTicks(dir)
		if err != nil {
			return processSample{}, err
		}
		sum, _, err := hashImage(filepath.Join(dir, "exe"))
		if err != nil {
			return processSample{}, err
		}
		cmdlineBody, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			return processSample{}, fmt.Errorf("read the process command line: %w", err)
		}
		environ, err := os.ReadFile(filepath.Join(dir, "environ"))
		if err != nil {
			return processSample{}, fmt.Errorf("read the process environment: %w", err)
		}
		// EVERY ARGUMENT IS KEPT, EMPTY ONES INCLUDED: the kernel writes each
		// argument NUL-terminated, so exactly one terminator comes off and the
		// rest splits; trimming every trailing NUL would erase a trailing empty
		// argument and let a five-argument command line pass as the supported
		// four.
		body := string(cmdlineBody)
		if body == "" || !strings.HasSuffix(body, "\x00") {
			return processSample{}, errors.New("the process command line is empty or not NUL-terminated")
		}
		args := strings.Split(body[:len(body)-1], "\x00")
		// THE PROCESS'S OWN VIEW OF ITS CONFIG PATH. An absolute path in its
		// command line is resolved under its root and inside its mount
		// namespace, where a RootDirectory, a bind mount or an extension
		// overlay makes it another file than the inspector's; and a directive
		// removed and reloaded without a restart no longer shows in the loaded
		// unit while the running process keeps the namespace it started in. So
		// the file is opened through /proc/<pid>/root, inside the sample, and
		// compared by identity rather than by the root's name.
		// ONLY THE SUPPORTED COMMAND LINE IS FOLLOWED INTO THE FILESYSTEM: an
		// unsupported process's fourth argument is not a configuration and is
		// never opened, let alone read (it could name a key). The inspector's
		// own path is opened for identity alone, since the observation's bytes
		// are the ones a bound service publishes; another path is hashed
		// through the process's root, because the inspector's file at that
		// name may be another file.
		var view os.FileInfo
		viewSHA, viewErr := "", ""
		switch {
		case !supportedCmdline(args, role):
		case args[3] == inspectorConfig:
			view, viewErr = statThroughRoot(dir, args[3])
		default:
			view, viewSHA, viewErr = hashThroughRoot(dir, args[3])
		}
		again, err := unitProperties(ctx, unit)
		if err != nil {
			return processSample{}, err
		}
		// A failed re-read of the records is a nil key; equal to the first
		// read only when that failed too, so a shape that appeared or vanished
		// under the sample discards it.
		againRecords, againErr := unitExecStart(ctx, unit)
		if againErr != nil {
			againRecords = nil
		}
		if unitIdentity(again, againRecords) != identity {
			continue
		}
		after, err := processStartTicks(dir)
		if err != nil {
			return processSample{}, err
		}
		if before != after {
			continue
		}
		return processSample{sha: sum, startTicks: before, cmdline: args, environ: environ, view: view, viewSHA: viewSHA, viewErr: viewErr}, nil
	}
	return processSample{}, errors.New("the service restarted during the observation, or its unit changed under it")
}

// supportedCmdlineWords is the four-word shape billet ships, `<managed path>
// <role> --config <path>`, without regard to the path's form.
func supportedCmdlineWords(args []string, role string) bool {
	return len(args) == 4 && args[0] == installedBinary && args[1] == role && args[2] == "--config"
}

// supportedCmdline is the shape with an absolute path, the one predicate the
// sample and the report share, so nothing is opened for a command line the
// report would not read.
func supportedCmdline(args []string, role string) bool {
	return supportedCmdlineWords(args, role) && filepath.IsAbs(args[3])
}

// statThroughRoot opens an absolute path as the process sees it and returns the
// opened file's identity, reading no bytes.
func statThroughRoot(procDir, path string) (os.FileInfo, string) {
	f, err := openInRoot(filepath.Join(procDir, "root"), path)
	if err != nil {
		return nil, fmt.Sprintf("the process's view of %s (through %s) could not be opened: %v", path, filepath.Join(procDir, "root"), err)
	}
	defer func() { _ = f.Close() }()
	if inspectAfterViewOpen != nil {
		inspectAfterViewOpen(path)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Sprintf("the process's view of %s could not be read: %v", path, err)
	}
	return info, ""
}

// hashThroughRoot opens an absolute path as the process sees it and returns the
// opened file's identity and bracketed digest. The open goes through the
// /proc/<pid>/root magic link WITH ROOT-SCOPED RESOLUTION (openInRoot): a plain
// open of the joined path would resolve an absolute symlink met on the way
// against the inspector's root, so a config that is a symlink into a directory
// the service has mounted differently would be opened in the inspector's
// namespace and compare equal to a file the service never reads. The identity
// descriptor is reopened for reading on the same inode, and only when it is a
// regular file: a FIFO or a device at that name is could-not-tell, never a wait
// or an open with a side effect.
func hashThroughRoot(procDir, path string) (os.FileInfo, string, string) {
	f, err := openInRoot(filepath.Join(procDir, "root"), path)
	if err != nil {
		return nil, "", fmt.Sprintf("the process's view of %s (through %s) could not be opened: %v", path, filepath.Join(procDir, "root"), err)
	}
	defer func() { _ = f.Close() }()
	if inspectAfterViewOpen != nil {
		inspectAfterViewOpen(path)
	}
	r, err := reopenForReading(f)
	if err != nil {
		return nil, "", fmt.Sprintf("the process's view of %s is not a regular file this inspector reads: %v", path, err)
	}
	defer func() { _ = r.Close() }()
	sum, info, err := hashOpenFile(r, path)
	if err != nil {
		return nil, "", fmt.Sprintf("the process's view of %s could not be read: %v", path, err)
	}
	return info, sum, ""
}

// viewEvidence is a service's config digest and mtime verdict taken from its
// own view of the path it names, or could-not-tell when that view could not be
// read; the inspector's namespace is never consulted for a path it does not
// itself hold.
func viewEvidence(sample processSample, startedAt time.Time, startKnown bool) (maybe, maybe) {
	if sample.viewErr != "" {
		return unknown(sample.viewErr), unknown(sample.viewErr)
	}
	if !startKnown {
		return known(sample.viewSHA), unknown("the process start time is unknown")
	}
	return known(sample.viewSHA), known(sample.view.ModTime().After(startedAt))
}

// inspectRunningProcess binds a running process to the inspector's
// configuration through its own command line and reports its image against
// the executable's; the second answer is that binding.
func inspectRunningProcess(ctx context.Context, svc *inspectService, role, unit string, pid int,
	props map[string][]string, records []execRecord, cfg *config.Config, inspectorConfig string,
	inspectorInfo os.FileInfo, inspectorSHA string, exeSHA string, exeInfo os.FileInfo, unitConfigPath string, envFiles []string,
) maybe {
	sample, err := sampleProcess(ctx, unit, role, pid, props, records, inspectorConfig)
	if err != nil {
		fillRunningUnknown(svc, err.Error())
		svc.DSNEnv = unknown(err.Error())
		return unknown(err.Error())
	}
	svc.RunningSHA256 = known(sample.sha)
	if exeSHA == "" || exeInfo == nil {
		svc.SameAsExecutable = unknown("the executable could not be hashed")
	} else {
		svc.SameAsExecutable = known(sample.sha == exeSHA)
	}
	startedAt, startErr := processStartWallClock(sample.startTicks)
	if startErr != nil {
		svc.StartedAt = unknown(startErr.Error())
	} else {
		svc.StartedAt = known(startedAt.UTC().Format(time.RFC3339))
	}

	// THE COMMAND LINE MUST BE THE SUPPORTED SHAPE TOO: exactly
	// `<managed path> <role> --config <path>`, so a process started some other
	// way is could-not-tell rather than read for the first --config it carries.
	why := "the process command line is not `" + installedBinary + " " + role + " --config <absolute path>`"
	binding := unknown(why)
	switch {
	case !supportedCmdlineWords(sample.cmdline, role):
		svc.CmdlineConfigPath, svc.CmdlineMatchesUnit = unknown(why), unknown(why)
		svc.ConfigSHA256, svc.ConfigChangedSinceStart = unknown(why), unknown(why)
	case !filepath.IsAbs(sample.cmdline[3]):
		// A relative path is relative to the process's working directory,
		// which this inspector does not share; read here it would name a file
		// the service never opened.
		why = "the process's --config path is relative, so it cannot be compared with the inspector's"
		binding = unknown(why)
		svc.CmdlineConfigPath, svc.CmdlineMatchesUnit = unknown(why), unknown(why)
		svc.ConfigSHA256, svc.ConfigChangedSinceStart = unknown(why), unknown(why)
	default:
		cmdlineConfig := sample.cmdline[3]
		svc.CmdlineConfigPath = known(cmdlineConfig)
		if unitConfigPath == "" {
			svc.CmdlineMatchesUnit = unknown("the unit's shape is unsupported, so it names no single config path")
		} else {
			svc.CmdlineMatchesUnit = known(cmdlineConfig == unitConfigPath)
		}
		// THE PATH AND THE FILE: the process must name the inspector's path,
		// and the file its own root resolves that path to must be the file the
		// inspector read (the one observation's identity, not a fresh stat of
		// the path), or equal paths bind two configurations. Under another root
		// or in another mount namespace a view that could not be opened cannot
		// be compared and is could-not-tell.
		switch {
		case cmdlineConfig != inspectorConfig:
			// ANOTHER CONFIGURATION, READ IN THE PROCESS'S OWN NAMESPACE: the
			// inspector's file at that name may be another file entirely, so
			// the digest and mtime come from the sampled view or are
			// could-not-tell.
			binding = known(false)
			svc.ConfigSHA256, svc.ConfigChangedSinceStart = viewEvidence(sample, startedAt, startErr == nil)
		case sample.viewErr != "":
			why = sample.viewErr
			binding = unknown(why)
			svc.ConfigSHA256, svc.ConfigChangedSinceStart = unknown(why), unknown(why)
		case inspectorInfo == nil:
			why = "the inspector's configuration could not be read, so the process's view cannot be compared with it"
			binding = unknown(why)
			svc.ConfigSHA256, svc.ConfigChangedSinceStart = unknown(why), unknown(why)
		case !os.SameFile(sample.view, inspectorInfo):
			why = "the process's view of " + cmdlineConfig + " is not the inspector's file"
			binding = known(false)
			svc.ConfigSHA256, svc.ConfigChangedSinceStart = unknown(why), unknown(why)
		default:
			// THE OBSERVATION'S DIGEST, not a second open of the pathname: the
			// process's view is the observation's file by identity, so the
			// bytes this report parsed are the bytes to publish for it, and a
			// pathname opened again could already name a replacement.
			binding = known(true)
			svc.fromObservation = true
			svc.ConfigSHA256 = known(inspectorSHA)
			if startErr == nil {
				svc.ConfigChangedSinceStart = known(inspectorInfo.ModTime().After(startedAt))
			} else {
				svc.ConfigChangedSinceStart = unknown("the process start time is unknown")
			}
		}
	}
	switch len(envFiles) {
	case 0:
		svc.EnvironmentFileChangedSinceRun = known(nil)
	case 1:
		_, svc.EnvironmentFileChangedSinceRun = fileHashAndChanged(envFiles[0], startedAt, startErr == nil)
	default:
		svc.EnvironmentFileChangedSinceRun = unknown("more than one environment file")
	}
	svc.DSNEnv = inspectDSN(sample.environ, role, cfg, envFiles)
	return binding
}

// fileHashAndChanged hashes a file and says whether its mtime is later than the
// process start, both from ONE descriptor, so a rename between two operations
// cannot pair the replacement's hash with the old file's mtime. The mtime is
// REFUSAL EVIDENCE ONLY: a later mtime says the file moved; an earlier one
// proves nothing about what the process loaded.
func fileHashAndChanged(path string, startedAt time.Time, startKnown bool) (maybe, maybe) {
	sum, info, err := hashRegular(path)
	if err != nil {
		return unknown(err.Error()), unknown(err.Error())
	}
	if !startKnown {
		return known(sum), unknown("the process start time is unknown")
	}
	return known(sum), known(info.ModTime().After(startedAt))
}

// processStartTicks is field 22 of /proc/<pid>/stat, the start time in clock
// ticks since boot; the comm field can hold spaces and parentheses, so the
// fields are counted from the last ')'.
func processStartTicks(dir string) (int64, error) {
	body, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return 0, fmt.Errorf("read the process stat: %w", err)
	}
	text := string(body)
	i := strings.LastIndex(text, ")")
	if i < 0 {
		return 0, errors.New("the process stat has no comm field")
	}
	fields := strings.Fields(text[i+1:])
	// Field 3 (state) is fields[0], so field 22 is fields[19].
	if len(fields) < 20 {
		return 0, errors.New("the process stat is truncated")
	}
	// A zombie or a dead process keeps its start time, so equal ticks before
	// and after would not prove the process lived through the sample.
	if fields[0] == "Z" || fields[0] == "X" || fields[0] == "x" {
		return 0, errors.New("the process is a zombie or dead (state " + fields[0] + ")")
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the process start time is not a number: %w", err)
	}
	return ticks, nil
}

// processStartWallClock turns start ticks into wall-clock time through the
// boot time in /proc/stat. USER_HZ, which /proc reports in, is 100 on the
// Linux architectures billet supports, whatever the kernel's own tick rate is.
func processStartWallClock(ticks int64) (time.Time, error) {
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}, fmt.Errorf("read the boot time: %w", err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "btime" {
			btime, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("the boot time is not a number: %w", err)
			}
			return time.Unix(btime+ticks/100, (ticks%100)*10_000_000), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}

// inspectDSN reports, for a PostgreSQL controller, whether the DSN variable is
// present in the process's environment and whether its value equals the one
// the unit's environment file holds NOW. The values are compared here and
// never leave this process.
func inspectDSN(env []byte, role string, cfg *config.Config, envFiles []string) maybe {
	if role != "server" {
		return known(nil)
	}
	if cfg == nil {
		return unknown("the configuration could not be read, so whether a DSN applies is unknown")
	}
	if cfg.Server == nil || cfg.Server.LedgerBackend() != config.StatePostgres {
		return known(nil)
	}
	name := cfg.Server.LedgerDSNEnv()
	out := inspectDSNEnv{Name: name}
	value, present := "", false
	for _, entry := range strings.Split(string(env), "\x00") {
		if k, v, ok := strings.Cut(entry, "="); ok && k == name {
			value, present = v, true
			break
		}
	}
	out.Present = known(present)
	switch {
	case !present:
		out.MatchesFile = known("absent")
	case len(envFiles) != 1:
		out.MatchesFile = unknown("the unit names no single environment file")
	default:
		fileValue, found, err := environmentFileValue(envFiles[0], name)
		switch {
		case err != nil:
			out.MatchesFile = unknown(fmt.Sprintf("read %s: %v", envFiles[0], err))
		case !found:
			out.MatchesFile = known("not_in_file")
		case fileValue == value:
			out.MatchesFile = known("equal")
		default:
			out.MatchesFile = known("differs")
		}
	}
	return known(out)
}

// environmentFileValue reads one variable out of a systemd environment file
// written the way the role's template writes it, and VALIDATES THE WHOLE FILE
// first. The grammar is exactly: a line is empty or whitespace, or a comment
// beginning with # or ; in column 0, or `NAME=value` with NAME in column 0; a
// value is unquoted with no leading or trailing whitespace and no quote,
// backslash or hash in it, or wholly quoted in one kind of quote with none of
// that kind and no backslash inside. systemd trims and unescapes more than
// that, so anything outside the grammar is unsupported syntax, refused rather
// than read the way systemd might read it, and a name assigned twice is refused
// because systemd's last assignment wins.
func environmentFileValue(path, name string) (string, bool, error) {
	body, err := readRegularFile(path)
	if err != nil {
		return "", false, err
	}
	// THE DELIMITERS FIRST: systemd treats a carriage return as a newline, so a
	// CR inside a line this reader would skip as a comment hides an assignment
	// systemd applies. Only LF-terminated, valid UTF-8 text with no control
	// character (C0, DEL or C1) but LF and TAB is inside the grammar.
	if !utf8.Valid(body) {
		return "", false, errors.New("unsupported environment file syntax: not UTF-8")
	}
	for _, r := range string(body) {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", false, errors.New("unsupported environment file syntax: a control character")
		}
		if isNoncharacter(r) {
			return "", false, errors.New("unsupported environment file syntax: a Unicode noncharacter")
		}
	}
	seen := map[string]bool{}
	value, found := "", false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !environmentNamePattern.MatchString(k) {
			return "", false, errors.New("unsupported environment file syntax")
		}
		if seen[k] {
			return "", false, errors.New("unsupported environment file syntax: a variable assigned twice")
		}
		seen[k] = true
		plain, ok := environmentValue(v)
		if !ok {
			return "", false, errors.New("unsupported environment file syntax")
		}
		if k == name {
			value, found = plain, true
		}
	}
	return value, found, nil
}

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isNoncharacter is systemd's stricter UTF-8 rule: its environment loader
// refuses U+FDD0 to U+FDEF and the last two code points of every plane, which
// Go's utf8.Valid accepts, so a file Go reads is one systemd fails to load.
func isNoncharacter(r rune) bool {
	return (r >= 0xFDD0 && r <= 0xFDEF) || r&0xFFFE == 0xFFFE
}

// environmentValue is the restricted value grammar: wholly quoted in one kind
// with none of that kind and no backslash inside, or unquoted with no leading
// or trailing whitespace and no quote, backslash or hash.
func environmentValue(v string) (string, bool) {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		inner := v[1 : len(v)-1]
		if strings.ContainsRune(inner, rune(v[0])) || strings.Contains(inner, "\\") {
			return "", false
		}
		return inner, true
	}
	if v != strings.TrimSpace(v) || strings.ContainsAny(v, "\"'\\#") {
		return "", false
	}
	return v, true
}

// inspectInstalledSection says which roles the inspector's configuration
// declares, and for a controller which ledger backend and controller mode;
// config_binding is what makes that the units' configuration too, so a
// node-only host with a dormant packaged server unit is not mistaken for a
// controller.
func inspectInstalledSection(cfg *config.Config, configPath string, loaded inspectConfig, digest maybe) inspectInstalledConfig {
	// The digest is the one observation's, the bytes that were parsed, and is
	// independent of any running process: a stopped host still answers what
	// configuration it holds.
	if cfg == nil {
		why := fmt.Sprintf("load %s: %s", configPath, loaded.Error)
		return inspectInstalledConfig{Path: known(configPath), SHA256: digest, HasServer: unknown(why), HasNode: unknown(why),
			LedgerBackend: unknown(why), Controllers: unknown(why)}
	}
	out := inspectInstalledConfig{Path: known(configPath), SHA256: digest, HasServer: known(cfg.Server != nil), HasNode: known(cfg.Node != nil),
		LedgerBackend: known(nil), Controllers: known(nil)}
	if cfg.Server != nil {
		out.LedgerBackend = known(string(cfg.Server.LedgerBackend()))
		out.Controllers = known(string(cfg.Server.Controllers))
	}
	return out
}

// inspectHostSection reports identity and trust evidence from the disk, and
// says so: the deployment id is peeked and NEVER minted, the authority is a
// confirming-read snapshot with no lock, and a node's bundle is read without
// its key.
func inspectHostSection(cfg *config.Config) inspectHost {
	host := inspectHost{OS: hostOS}
	host.Retirement = inspectRetirement()
	if cfg == nil {
		why := "the configuration could not be read"
		host.NodeName, host.DeploymentID = unknown(why), unknown(why)
		host.Authority, host.NodeTrust = unknown(why), unknown(why)
		return host
	}
	if cfg.Node != nil && cfg.Node.Name != "" {
		host.NodeName = known(cfg.Node.Name)
	} else {
		host.NodeName = known(nil)
	}
	if cfg.Server == nil {
		host.DeploymentID = known(nil)
		host.Authority = known(nil)
	} else {
		id, found, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
		switch {
		case err != nil:
			host.DeploymentID = unknown(err.Error())
		case !found:
			host.DeploymentID = unknown("not minted")
		default:
			host.DeploymentID = known(id)
		}
		host.Authority = inspectAuthoritySection(cfg.Server.IdentityDir)
	}
	if cfg.Node == nil || cfg.Node.TLS == nil {
		host.NodeTrust = known(nil)
	} else {
		host.NodeTrust = inspectNodeTrustSection(cfg.Node.TLS.CertPath, cfg.Node.TLS.CAPath)
	}
	return host
}

// inspectRetirement reads the retirement journal from its fixed path: the
// phases a retiring controller has durably reached, which the loader follows
// to decide where that host's identity now lives.
func inspectRetirement() maybe {
	if hostOS == "darwin" {
		return unknown("retirement is not tracked on darwin: a launch agent's account owns no /var/lib/billet")
	}
	body, err := readPublicFile(retiredJournalPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return known(nil)
		}
		return unknown(fmt.Sprintf("read %s: %v", retiredJournalPath, err))
	}
	var journal map[string]any
	if err := json.Unmarshal(body, &journal); err != nil {
		return unknown(fmt.Sprintf("%s is not a JSON object: %v", retiredJournalPath, err))
	}
	if phase, ok := journal["phase"].(string); !ok || phase == "" {
		return unknown(fmt.Sprintf("%s names no phase", retiredJournalPath))
	}
	return known(journal)
}

func inspectAuthoritySection(stateDir string) maybe {
	snap, err := wirecert.SnapshotAuthority(stateDir)
	switch {
	case errors.Is(err, wirecert.ErrAuthorityLost):
		return known(nil)
	case err != nil:
		return unknown(err.Error())
	}
	// The PEM published is re-encoded from the validated certificate's DER,
	// never the file's bytes, so nothing beside the certificate can ride along.
	out := inspectAuthority{
		Current:            describeCert(pemOf(snap.Current), snap.Current),
		RotationInProgress: snap.Rotating(),
		Created:            snap.Created,
	}
	if snap.Previous != nil {
		prev := describeCert(pemOf(snap.Previous), snap.Previous)
		out.Previous = &prev
	}
	return known(out)
}

func inspectNodeTrustSection(certPath, caPath string) maybe {
	leafPEM, err := readPublicFile(certPath)
	if err != nil {
		return unknown(fmt.Sprintf("read %s: %v", certPath, err))
	}
	leaves, err := wirecert.ParseCertificates(leafPEM)
	if err != nil || len(leaves) != 1 {
		return unknown(fmt.Sprintf("%s does not hold exactly one certificate", certPath))
	}
	caPEM, err := readPublicFile(caPath)
	if err != nil {
		return unknown(fmt.Sprintf("read %s: %v", caPath, err))
	}
	cas, err := wirecert.ParseCertificates(caPEM)
	if err != nil {
		return unknown(fmt.Sprintf("%s: %v", caPath, err))
	}
	out := inspectNodeTrust{Leaf: describeCert(pemOf(leaves[0]), leaves[0])}
	for _, ca := range cas {
		out.CAs = append(out.CAs, describeCert(pemOf(ca), ca))
	}
	return known(out)
}

func pemOf(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// describeCert is the public description of one certificate: its PEM, the
// SHA-256 of its DER (the fingerprint the loader compares), subject and expiry.
func describeCert(pemBody []byte, cert *x509.Certificate) inspectCertificate {
	sum := sha256.Sum256(cert.Raw)
	return inspectCertificate{
		PEM:       string(pemBody),
		DERSHA256: hex.EncodeToString(sum[:]),
		Subject:   cert.Subject.String(),
		NotAfter:  cert.NotAfter.UTC().Format(time.RFC3339),
	}
}

// inspectTransactionSection classifies the claim by its shape with Lstat and
// never by following anything: a dangling symlink is still a Go claim.
func inspectTransactionSection() inspectTransaction {
	tx := inspectTransaction{}
	rootInfo, err := os.Lstat(upgradeRoot)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		tx.Root = "absent"
		tx.Active, tx.LockHeld = known("none"), known(false)
		tx.Journal, tx.ConvergeGuard = known(nil), known(nil)
		tx.Preparation = known(inspectPreparation{TransactionLock: known(false)})
		return tx
	case err != nil || !rootInfo.IsDir():
		tx.Root = "unreadable"
		why := "the upgrade root could not be read"
		if err != nil {
			why = err.Error()
		}
		tx.Active, tx.LockHeld, tx.Journal = unknown(why), unknown(why), unknown(why)
		tx.ConvergeGuard, tx.Preparation = unknown(why), unknown(why)
		return tx
	}
	tx.Root = "present"
	tx.LockHeld = inspectLockHeld()
	tx.Preparation = known(inspectPreparation{TransactionLock: exists(filepath.Join(upgradeRoot, txLockName))})

	active := activePath()
	info, err := os.Lstat(active)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		tx.Active, tx.Journal, tx.ConvergeGuard = known("none"), known(nil), known(nil)
	case err != nil:
		tx.Active, tx.Journal, tx.ConvergeGuard = unknown(err.Error()), unknown(err.Error()), unknown(err.Error())
	case info.Mode()&os.ModeSymlink != 0:
		tx.Active = known("host-upgrade")
		tx.ConvergeGuard = known(nil)
		tx.Journal = inspectJournalOf(active)
	case info.Mode().IsRegular():
		tx.Active, tx.Journal, tx.ConvergeGuard = known("legacy-role"), known(nil), known(nil)
	case info.IsDir():
		tx.Journal = known(nil)
		tx.Active, tx.ConvergeGuard = inspectGuardOf(active)
	default:
		why := "the claim is neither a symlink, a file nor a directory"
		tx.Active, tx.Journal, tx.ConvergeGuard = unknown(why), unknown(why), unknown(why)
	}
	return tx
}

// exists is three-valued: a read that failed for any reason but absence is
// not an absence.
func exists(path string) maybe {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return known(true)
	case errors.Is(err, fs.ErrNotExist):
		return known(false)
	default:
		return unknown(fmt.Sprintf("lstat %s: %v", path, err))
	}
}

// inspectLockHeld tries the transaction lock non-blocking through a read-only
// descriptor and releases it at once; an absent lock file is held by nobody.
func inspectLockHeld() maybe {
	f, _, err := openRegular(filepath.Join(upgradeRoot, txLockName), true)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return known(false)
		}
		return unknown(fmt.Sprintf("open the transaction lock: %v", err))
	}
	defer func() { _ = f.Close() }()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // the Close releases it whatever this returns
		return known(false)
	case errors.Is(err, syscall.EWOULDBLOCK):
		return known(true)
	default:
		return unknown(fmt.Sprintf("try the transaction lock: %v", err))
	}
}

func inspectJournalOf(active string) maybe {
	dir, err := os.Readlink(active)
	if err != nil {
		return unknown(fmt.Sprintf("read the claim: %v", err))
	}
	journal, err := hostupgrade.ReadJournal(dir)
	if err != nil {
		return unknown(fmt.Sprintf("read the journal in %s: %v", dir, err))
	}
	return known(inspectJournal{
		FromVersion: journal.FromVersion, ToVersion: journal.ToVersion,
		Step: string(journal.Step), StartedAt: journal.StartedAt, Failure: journal.Failure,
	})
}

// inspectGuardOf classifies a directory claim: guard.json makes it a converge
// guard, its absence an unpublished one that a hold never returned from.
func inspectGuardOf(active string) (maybe, maybe) {
	body, err := readRegularFile(filepath.Join(active, "guard.json"))
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return known("unpublished-guard"), known(nil)
	case err != nil:
		return unknown(fmt.Sprintf("read the guard: %v", err)), unknown(err.Error())
	}
	var guard struct {
		Holder                  string `json:"holder"`
		ClaimedAt               string `json:"claimed_at"`
		Hostname                string `json:"hostname"`
		ReleaseExecutable       string `json:"release_executable"`
		ReleaseExecutableSHA256 string `json:"release_executable_sha256"`
	}
	if err := json.Unmarshal(body, &guard); err != nil || guard.Holder == "" {
		return known("converge-guard"), unknown("guard.json does not name a holder")
	}
	out := inspectGuard{
		Holder: guard.Holder, ClaimedAt: guard.ClaimedAt, Hostname: guard.Hostname,
		RecoveryPointer:         exists(filepath.Join(active, "recovery")),
		ReleaseExecutable:       guard.ReleaseExecutable,
		ReleaseExecutableSHA256: guard.ReleaseExecutableSHA256,
	}
	// THE RECORDED EXECUTABLE IS VERIFIED, NEVER RUN, HERE: its digest now
	// against the digest the hold recorded.
	switch {
	case guard.ReleaseExecutable == "" || guard.ReleaseExecutableSHA256 == "":
		out.ReleaseExecutableVerified = unknown("the guard records no release executable")
	default:
		sum, _, err := hashRegular(guard.ReleaseExecutable)
		if err != nil {
			out.ReleaseExecutableVerified = unknown(err.Error())
		} else {
			out.ReleaseExecutableVerified = known(sum == guard.ReleaseExecutableSHA256)
		}
	}
	return known("converge-guard"), known(out)
}

func printInspectReport(r inspectReport) {
	fmt.Printf("config        %s", r.Config.Path)
	if !r.Config.Readable {
		fmt.Printf(" (unreadable: %s)", r.Config.Error)
	}
	fmt.Println()
	fmt.Printf("executable    %s %s\n", r.Executable.Version, describeMaybe(r.Executable.SHA256))
	fmt.Printf("provenance    %s", r.Provenance.Verdict)
	if r.Provenance.Reason != "" {
		fmt.Printf(" (%s)", r.Provenance.Reason)
	}
	fmt.Println()
	for _, role := range []string{"server", "node"} {
		svc := r.Services[role]
		if svc.UnitPresent.known && svc.UnitPresent.value == false {
			fmt.Printf("%-13s no unit\n", role)
			continue
		}
		fmt.Printf("%-13s %s/%s shape=%s image=%s\n", role, describeMaybe(svc.ActiveState),
			describeMaybe(svc.SubState), describeMaybe(svc.Shape), describeMaybe(svc.SameAsExecutable))
	}
	fmt.Printf("binding       %s\n", describeMaybe(r.ConfigBinding))
	fmt.Printf("deployment    %s\n", describeMaybe(r.Host.DeploymentID))
	fmt.Printf("transaction   root=%s active=%s lock_held=%s\n", r.Transaction.Root,
		describeMaybe(r.Transaction.Active), describeMaybe(r.Transaction.LockHeld))
}

func describeMaybe(m maybe) string {
	if !m.known {
		return "unknown (" + m.why + ")"
	}
	if m.value == nil {
		return "-"
	}
	return fmt.Sprint(m.value)
}
