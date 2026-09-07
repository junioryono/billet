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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	procRoot           = "/proc"
	selfExePath        = "/proc/self/exe"
	systemctlBinary    = "systemctl"
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
	Enabled          maybe  `json:"enabled"`
	ActiveState      maybe  `json:"active_state"`
	SubState         maybe  `json:"sub_state"`
	MainPID          maybe  `json:"main_pid"`
	ExecMainStart    maybe  `json:"exec_main_start"`
	ExecStart        maybe  `json:"exec_start"`
	EnvironmentFiles maybe  `json:"environment_files"`
	Shape            maybe  `json:"shape"`
	ShapeReason      string `json:"shape_reason,omitempty"`

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
	Path      maybe `json:"path"`
	HasServer maybe `json:"has_server"`
	HasNode   maybe `json:"has_node"`
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
	fs := newFlagSet("billet release inspect")
	configPath := fs.String("config", defaultConfigPath(), "path to billet.yaml")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := parse(fs, args); err != nil {
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
	report := inspectReport{Schema: inspectSchema, Config: inspectConfig{Path: configPath}}
	cfg, err := config.Load(configPath)
	if err != nil {
		report.Config.Error = err.Error()
	} else {
		report.Config.Readable = true
	}

	exeSHA, exeInfo := inspectExecutableSection(&report)
	inspectProvenanceSection(&report, exeSHA)
	report.Services = map[string]inspectService{}
	binding := known(true)
	for role, unit := range map[string]string{"server": deploy.ServerUnitName, "node": deploy.NodeUnitName} {
		svc, bound := inspectServiceSection(ctx, role, unit, cfg, configPath, exeSHA, exeInfo)
		report.Services[role] = svc
		binding = weaker(binding, bound)
	}
	report.ConfigBinding = binding
	report.Installed = inspectInstalledSection(cfg, configPath, report.Config)
	report.Host = inspectHostSection(cfg)
	report.Transaction = inspectTransactionSection()
	return report
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
// descriptor, returning the descriptor's own stat.
func hashImage(path string) (string, os.FileInfo, error) {
	f, err := openImage(path)
	if err != nil {
		return "", nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if inspectAfterOpen != nil {
		inspectAfterOpen(path)
	}
	info, err := f.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("stat %s: %w", path, err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", nil, fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), info, nil
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
		"InvocationID", "ExecMainStartTimestamp", "ExecStart", "EnvironmentFiles", "Environment"}
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

// unitFragmentText is `systemctl cat` of a unit: the fragment and every
// drop-in as written, the one place argument boundaries are still visible.
// `systemctl show` renders ExecStart's argv joined by spaces, so a two-argument
// `billet "server --config x"` displays like the supported four-argument form.
func unitFragmentText(ctx context.Context, unit string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, systemctlTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, systemctlBinary, "cat", "--", unit)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("systemctl cat %s: %w: %s", unit, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// unitDirectives counts the directives the supported shape constrains, over
// the fragment and its drop-ins as written.
type unitDirectives struct {
	execStart   []string
	envFiles    int
	environment int
}

func parseUnitDirectives(text string) unitDirectives {
	var d unitDirectives
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ExecStart="):
			d.execStart = append(d.execStart, strings.TrimSpace(strings.TrimPrefix(line, "ExecStart=")))
		case strings.HasPrefix(line, "EnvironmentFile="):
			d.envFiles++
		case strings.HasPrefix(line, "Environment="):
			d.environment++
		}
	}
	return d
}

// environmentFilesOfAll reads every EnvironmentFiles property line systemd
// printed (one per file on systemd 255) into one list.
func environmentFilesOfAll(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, environmentFilesOf(line)...)
	}
	return out
}

// execStartPathOf reads the executable path out of systemd's rendered
// ExecStart (`{ path=... ; argv[]=... }`).
func execStartPathOf(rendered string) string {
	const marker = "path="
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

// environmentFilesOf reads the paths out of systemd's EnvironmentFiles
// (`/etc/billet/server.env (ignore_errors=yes)`, several separated by spaces).
func environmentFilesOf(rendered string) []string {
	var out []string
	for _, part := range strings.Split(rendered, ")") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.Index(part, " ("); i >= 0 {
			part = part[:i]
		}
		out = append(out, strings.TrimSpace(part))
	}
	return out
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
	inspectorConfig string, exeSHA string, exeInfo os.FileInfo,
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
		svc.Enabled, svc.ExecStart, svc.Shape, svc.EnvironmentFiles = known(nil), known(nil), known(nil), known(nil)
		svc.ActiveState, svc.SubState = known(active), known(firstProp(props, "SubState"))
		svc.ExecMainStart = known(firstProp(props, "ExecMainStartTimestamp"))
		switch {
		case pidErr != nil:
			svc.MainPID = unknown("systemd reported a MainPID that is not a number")
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
	svc.Enabled = known(firstProp(props, "UnitFileState") == "enabled")
	svc.ActiveState = known(active)
	svc.SubState = known(firstProp(props, "SubState"))
	svc.ExecMainStart = known(firstProp(props, "ExecMainStartTimestamp"))
	argv, execPath := "", ""
	if len(props["ExecStart"]) > 0 {
		argv = execStartArgvOf(props["ExecStart"][0])
		execPath = execStartPathOf(props["ExecStart"][0])
	}
	svc.ExecStart = known(argv)
	envFiles := environmentFilesOfAll(props["EnvironmentFiles"])
	svc.EnvironmentFiles = known(envFiles)

	// THE SHAPE BILLET SHIPS, AND NOTHING ELSE, read where its boundaries are
	// visible: `systemctl show` joins argv with spaces, so the fragment text
	// from `systemctl cat` must carry exactly one ExecStart line that is
	// textually `<managed path> <role> --config <path>`, at most one
	// EnvironmentFile (the package has none, the role's template adds one), no
	// Environment= directive; and the rendered properties must agree. Anything
	// else is could-not-tell for the binding below.
	unitConfigPath := ""
	shapeWhy := ""
	text, err := unitFragmentText(ctx, unit)
	directives := parseUnitDirectives(text)
	words := strings.Fields(argv)
	wantExec := installedBinary + " " + role + " --config "
	switch {
	case err != nil:
		shapeWhy = err.Error()
	case len(directives.execStart) != 1 || len(props["ExecStart"]) != 1:
		shapeWhy = fmt.Sprintf("%d ExecStart lines in the unit text and %d rendered", len(directives.execStart), len(props["ExecStart"]))
	case !strings.HasPrefix(directives.execStart[0], wantExec) || len(strings.Fields(directives.execStart[0])) != 4 ||
		strings.ContainsAny(directives.execStart[0], "\"'\\$"):
		shapeWhy = "ExecStart is not `" + wantExec + "<path>`: " + directives.execStart[0]
	case len(words) != 4 || words[0] != installedBinary || words[1] != role || words[2] != "--config" ||
		words[3] != strings.Fields(directives.execStart[0])[3]:
		shapeWhy = "the rendered ExecStart does not match the unit text"
	case execPath != installedBinary:
		shapeWhy = "ExecStart's executable path is not " + installedBinary
	case directives.envFiles > 1 || len(envFiles) > 1:
		shapeWhy = fmt.Sprintf("%d EnvironmentFile directives", max(directives.envFiles, len(envFiles)))
	case directives.environment > 0 || strings.TrimSpace(firstProp(props, "Environment")) != "":
		shapeWhy = "an Environment= directive"
	default:
		unitConfigPath = words[3]
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
	processBinding := inspectRunningProcess(ctx, &svc, role, unit, pid, props, cfg, inspectorConfig, exeSHA, exeInfo, unitConfigPath, envFiles)
	return svc, weaker(binding, processBinding)
}

// serviceAllUnknown is a unit nothing could be asked about.
func serviceAllUnknown(svc inspectService, why string) inspectService {
	svc.UnitPresent = unknown(why)
	svc.Enabled, svc.ActiveState, svc.SubState, svc.MainPID = unknown(why), unknown(why), unknown(why), unknown(why)
	svc.ExecMainStart, svc.ExecStart, svc.Shape, svc.EnvironmentFiles = unknown(why), unknown(why), unknown(why), unknown(why)
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
}

// unitIdentity is what must not move across a sample: which process systemd
// calls the unit's main one, which invocation it is, and the exec-shaping
// properties whose change would make the unit another unit.
func unitIdentity(props map[string][]string) string {
	return strings.Join([]string{
		firstProp(props, "MainPID"), firstProp(props, "InvocationID"),
		strings.Join(props["ExecStart"], "\x00"), strings.Join(props["EnvironmentFiles"], "\x00"),
		strings.Join(props["Environment"], "\x00"),
	}, "\x01")
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
func sampleProcess(ctx context.Context, unit string, pid int, first map[string][]string) (processSample, error) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	identity := unitIdentity(first)
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
		again, err := unitProperties(ctx, unit)
		if err != nil {
			return processSample{}, err
		}
		if unitIdentity(again) != identity {
			continue
		}
		after, err := processStartTicks(dir)
		if err != nil {
			return processSample{}, err
		}
		if before != after {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdlineBody), "\x00"), "\x00")
		return processSample{sha: sum, startTicks: before, cmdline: args, environ: environ}, nil
	}
	return processSample{}, errors.New("the service restarted during the observation")
}

// inspectRunningProcess binds a running process to the inspector's
// configuration through its own command line and reports its image against
// the executable's; the second answer is that binding.
func inspectRunningProcess(ctx context.Context, svc *inspectService, role, unit string, pid int,
	props map[string][]string, cfg *config.Config, inspectorConfig string, exeSHA string,
	exeInfo os.FileInfo, unitConfigPath string, envFiles []string,
) maybe {
	sample, err := sampleProcess(ctx, unit, pid, props)
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
	why := "the process command line is not `" + installedBinary + " " + role + " --config <path>`"
	binding := unknown(why)
	if len(sample.cmdline) != 4 || sample.cmdline[0] != installedBinary || sample.cmdline[1] != role || sample.cmdline[2] != "--config" {
		svc.CmdlineConfigPath, svc.CmdlineMatchesUnit = unknown(why), unknown(why)
		svc.ConfigSHA256, svc.ConfigChangedSinceStart = unknown(why), unknown(why)
	} else {
		cmdlineConfig := sample.cmdline[3]
		svc.CmdlineConfigPath = known(cmdlineConfig)
		if unitConfigPath == "" {
			svc.CmdlineMatchesUnit = unknown("the unit's shape is unsupported, so it names no single config path")
		} else {
			svc.CmdlineMatchesUnit = known(cmdlineConfig == unitConfigPath)
		}
		binding = known(cmdlineConfig == inspectorConfig)
		svc.ConfigSHA256, svc.ConfigChangedSinceStart = fileHashAndChanged(cmdlineConfig, startedAt, startErr == nil)
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
// process start. The mtime is REFUSAL EVIDENCE ONLY: a later mtime says the
// file moved; an earlier one proves nothing about what the process loaded.
func fileHashAndChanged(path string, startedAt time.Time, startKnown bool) (maybe, maybe) {
	info, err := os.Stat(path)
	if err != nil {
		why := fmt.Sprintf("stat %s: %v", path, err)
		return unknown(why), unknown(why)
	}
	sum, _, err := hashImage(path)
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
// first: every line is blank, a comment, or `NAME=value`; a value is unquoted
// with no quote, backslash or hash in it, or wholly quoted in one kind of quote
// with none of that kind inside and no backslash; a name assigned twice is
// refused, because systemd's last assignment wins and a reader that stopped at
// the first would compare the wrong value. Anything else is unsupported
// syntax, refused rather than read the way systemd might read it.
func environmentFileValue(path, name string) (string, bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	seen := map[string]bool{}
	value, found := "", false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
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

// environmentValue is the restricted value grammar: unquoted with no quote,
// backslash or hash, or wholly quoted in one kind with none of that kind
// inside and no backslash.
func environmentValue(v string) (string, bool) {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		inner := v[1 : len(v)-1]
		if strings.ContainsRune(inner, rune(v[0])) || strings.Contains(inner, "\\") {
			return "", false
		}
		return inner, true
	}
	if strings.ContainsAny(v, "\"'\\#") {
		return "", false
	}
	return v, true
}

// inspectInstalledSection says which roles the inspector's configuration
// declares; config_binding is what makes that the units' configuration too, so
// a node-only host with a dormant packaged server unit is not mistaken for a
// controller.
func inspectInstalledSection(cfg *config.Config, configPath string, loaded inspectConfig) inspectInstalledConfig {
	if cfg == nil {
		why := fmt.Sprintf("load %s: %s", configPath, loaded.Error)
		return inspectInstalledConfig{Path: known(configPath), HasServer: unknown(why), HasNode: unknown(why)}
	}
	return inspectInstalledConfig{Path: known(configPath), HasServer: known(cfg.Server != nil), HasNode: known(cfg.Node != nil)}
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
	body, err := os.ReadFile(retiredJournalPath)
	if err != nil {
		if os.IsNotExist(err) {
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
	leafPEM, err := os.ReadFile(certPath)
	if err != nil {
		return unknown(fmt.Sprintf("read %s: %v", certPath, err))
	}
	leaves, err := wirecert.ParseCertificates(leafPEM)
	if err != nil || len(leaves) != 1 {
		return unknown(fmt.Sprintf("%s does not hold exactly one certificate", certPath))
	}
	caPEM, err := os.ReadFile(caPath)
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
	case err != nil && os.IsNotExist(err):
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
	case err != nil && os.IsNotExist(err):
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
	case os.IsNotExist(err):
		return known(false)
	default:
		return unknown(fmt.Sprintf("lstat %s: %v", path, err))
	}
}

// inspectLockHeld tries the transaction lock non-blocking through a read-only
// descriptor and releases it at once; an absent lock file is held by nobody.
func inspectLockHeld() maybe {
	f, err := os.OpenFile(filepath.Join(upgradeRoot, txLockName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
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
	body, err := os.ReadFile(filepath.Join(active, "guard.json"))
	switch {
	case err != nil && os.IsNotExist(err):
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
		sum, _, err := hashImage(guard.ReleaseExecutable)
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
