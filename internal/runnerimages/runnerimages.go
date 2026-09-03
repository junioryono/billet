// Package runnerimages is what billet knows about GitHub's own runner image.
//
// WHY THIS EXISTS, AND WHY IT IS A VENDORED FILE RATHER THAN A FETCH.
//
// GitHub does not publish the image its hosted runners boot. `actions/runner-images`
// is SOURCE: Packer templates whose only builder is `azure-arm`, producing a managed
// image in the builder's own Azure subscription. Every image release there carries
// exactly one asset, a ~50KB `internal.<image>.json`; there is no VHD, no qcow2 and
// no rootfs to download anywhere. Anyone matching that image — Blacksmith included —
// is rebuilding it, not booting it.
//
// What IS published is enough to rebuild: `toolset-<version>.json` declares every
// apt package, toolcache entry, JDK, NDK and pinned tool version, and eighty plain
// bash installers sit beside it. That file is what this package vendors, and it is
// the single answer to "what should be in a billet runner image" for BOTH backends.
//
// ONE MANIFEST, TWO BACKENDS. The firecracker guest image and the ec2 AMI used to
// carry two hand-maintained package lists in two languages, which is the same shape
// as the runner-version bug `runnerrelease` exists to prevent: two pins is one pin
// that is wrong. The shell build reads this same file through `jq`, so a package
// added for one backend cannot silently miss the other.
//
// PINNED, NOT TRACKED. An image is a thing you reproduce. Reading upstream `main` at
// build time would make two runs of the same command produce different images, and
// the difference would surface as a job failing on one generation and not another —
// which is the argument `billet ami` already makes about the runner version.
package runnerimages

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// pinned records which upstream commit this copy came from and what it hashes to.
//
// TWO FACTS ON ONE LINE, FOR THE REASON runnerrelease GIVES ABOUT THE RUNNER. The
// commit is provenance — which upstream revision this content is — and the digest is
// integrity of the vendored copy. Held apart, a refresh updates one of them: either
// the digest names a file nobody has, or the commit claims a revision the bytes did
// not come from. Together, a refresh is one line and cannot be half done.
//
//go:embed pinned.txt
var pinned string

// toolset is GitHub's declaration of the ubuntu-24.04 image contents, verbatim.
//
//go:embed toolset-2404.json
var toolset []byte

// aptAliases maps a declared package name to one apt can actually install.
//
// BILLET'S OWN FILE, DELIBERATELY NOT THE VENDORED ONE. The toolset is upstream's
// and is pinned by digest so it cannot be edited; this is the narrow reviewed
// mapping that makes a name in it installable, and keeping the two apart means
// every deviation from what GitHub declared is one line in a file that exists to
// list deviations.
//
// APPLIED HERE AS WELL AS IN THE SHELL BUILD, because this is what will drive the
// ec2 AMI. A mapping applied to one backend and not the other is the two-pins
// problem this package was created to remove — the ec2 build would hit the exact
// failure that took down the first end-to-end guest build.
//
//go:embed apt-aliases.json
var aptAliases []byte

// PinnedCommit is the actions/runner-images revision toolset-2404.json came from.
func PinnedCommit() string {
	commit, _, _ := strings.Cut(strings.TrimSpace(pinned), " ")

	return commit
}

// PinnedSHA256 is the digest of the vendored toolset as it should be on disk.
func PinnedSHA256() string {
	_, sum, _ := strings.Cut(strings.TrimSpace(pinned), " ")

	return strings.TrimSpace(sum)
}

// ToolsetBytes is the vendored file exactly as it is on disk.
//
// RETURNED AS A COPY. The embedded slice is package state, and a caller that sliced
// into it could change what every later digest check hashes — which would make the
// integrity check agree with the tampering rather than catch it.
func ToolsetBytes() []byte {
	out := make([]byte, len(toolset))
	copy(out, toolset)

	return out
}

// SourceURL names where the vendored copy came from, at the pinned commit.
func SourceURL() string {
	return "https://raw.githubusercontent.com/actions/runner-images/" + PinnedCommit() +
		"/images/ubuntu/toolsets/toolset-2404.json"
}

// Toolset is the half of GitHub's declaration billet acts on.
//
// DELIBERATELY NOT THE WHOLE DOCUMENT. The upstream file also describes PowerShell
// modules, Azure modules and Homebrew formulae, which are either Windows-only or
// things billet installs by another route. Decoding only what is used means an
// upstream addition billet ignores is visibly absent here rather than silently
// carried as a field nothing reads.
type Toolset struct {
	Toolcache []ToolcacheEntry `json:"toolcache"`
	Java      Java             `json:"java"`
	Android   Android          `json:"android"`
	// TWO LISTS, ONE MECHANISM. Upstream maintains these apart because different
	// teams own them; both are Install-Module from PSGallery, and a single field
	// would misrepresent the declaration while two installers would duplicate it.
	PowerShellModules []PSModule    `json:"powershellModules"`
	AzureModules      []PSModule    `json:"azureModules"`
	Apt               Apt           `json:"apt"`
	Docker            Docker        `json:"docker"`
	DotNet            DotNet        `json:"dotnet"`
	Clang             Clang         `json:"clang"`
	GCC               Versions      `json:"gcc"`
	GFortran          Versions      `json:"gfortran"`
	PHP               Versions      `json:"php"`
	Node              NodeDefault   `json:"node"`
	PowerShell        Version       `json:"pwsh"`
	PostgreSQL        Version       `json:"postgresql"`
	CMake             Version       `json:"cmake"`
	Pipx              []PipxPackage `json:"pipx"`
	NodeModules       []NodeModule  `json:"node_modules"`
}

// ToolcacheEntry is one tool billet bakes into /opt/hostedtoolcache.
//
// Versions are upstream's glob forms ("3.13.*", "22.*"), not resolved versions: the
// toolcache is a cache rather than a contract, so the build resolves the newest
// matching release at build time and checksum-verifies what it downloads.
type ToolcacheEntry struct {
	Name            string   `json:"name"`
	Arch            string   `json:"arch"`
	Platform        string   `json:"platform"`
	PlatformVersion string   `json:"platform_version"`
	Versions        []string `json:"versions"`
	Default         string   `json:"default"`
}

// Java is the JDK set, and the default JAVA_HOME points at Default.
type Java struct {
	Default  string   `json:"default"`
	Versions []string `json:"versions"`
	Maven    string   `json:"maven"`
}

// PSModule is one module installed from PSGallery.
//
// Versions IS A LIST AND USUALLY EMPTY. Upstream pins two of the four it declares
// and leaves the rest floating, so the absence of a pin is the declaration saying
// "newest", not a field billet failed to read.
type PSModule struct {
	Name     string   `json:"name"`
	Versions []string `json:"versions"`
}

// Android is the SDK, its NDKs, and the extras a build expects to find offline.
type Android struct {
	CmdlineTools         string   `json:"cmdline-tools"`
	PlatformMinVersion   string   `json:"platform_min_version"`
	BuildToolsMinVersion string   `json:"build_tools_min_version"`
	ExtraList            []string `json:"extra_list"`
	AddonList            []string `json:"addon_list"`
	AdditionalTools      []string `json:"additional_tools"`
	NDK                  NDK      `json:"ndk"`
}

// NDK is the native development kit set.
type NDK struct {
	Default  string   `json:"default"`
	Versions []string `json:"versions"`
}

// Apt is the three package lists, which upstream installs in this order.
type Apt struct {
	VitalPackages  []string `json:"vital_packages"`
	CommonPackages []string `json:"common_packages"`
	CmdPackages    []string `json:"cmd_packages"`
}

// Docker is the engine components and CLI plugins, pinned by upstream.
type Docker struct {
	Components []DockerComponent `json:"components"`
	Plugins    []DockerPlugin    `json:"plugins"`
}

// DockerComponent is one apt package of the engine.
type DockerComponent struct {
	Package string `json:"package"`
	Version string `json:"version"`
}

// DockerPlugin is one CLI plugin and the asset naming it ships under.
type DockerPlugin struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	Asset   string `json:"asset"`
}

// DotNet is the SDK feature bands and the global tools installed beside them.
type DotNet struct {
	Versions []string     `json:"versions"`
	Tools    []DotNetTool `json:"tools"`
}

// DotNetTool is one `dotnet tool install` entry.
type DotNetTool struct {
	Name string `json:"name"`
}

// Clang carries a default because the unsuffixed `clang` must resolve to one.
type Clang struct {
	Versions       []string `json:"versions"`
	DefaultVersion string   `json:"default_version"`
}

// Versions is a bare version list, used where upstream needs nothing else.
type Versions struct {
	Versions []string `json:"versions"`
}

// Version is a single pinned version.
type Version struct {
	Version string `json:"version"`
}

// NodeDefault is the system-wide node, distinct from the toolcache entries.
type NodeDefault struct {
	Default string `json:"default"`
}

// PipxPackage is one isolated python application.
type PipxPackage struct {
	Package string `json:"package"`
	Cmd     string `json:"cmd"`
}

// NodeModule is one globally installed npm package and the command it provides.
type NodeModule struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// VerifyToolset reports whether these bytes are the ones the pin names.
//
// TAKES ITS INPUTS RATHER THAN READING PACKAGE STATE, which is what makes it
// testable. The first version of this check ran only over the embedded file, and
// the test that claimed to cover it did its OWN comparison of the same bytes
// against the same pin before calling Load — so deleting the production check
// left that test green, and corrupting the pin proved only that the test's
// duplicate comparison worked. A guard nothing can drive with wrong input is a
// guard nothing has checked.
func VerifyToolset(data []byte, wantDigest string) error {
	sum := sha256.Sum256(data)

	if got := hex.EncodeToString(sum[:]); got != wantDigest {
		return fmt.Errorf("runnerimages: the vendored toolset hashes to %s and pinned.txt "+
			"names %s; refresh both together from %s, or restore the file",
			got, wantDigest, SourceURL())
	}

	return nil
}

// Load parses the vendored toolset, after proving it is the file that was pinned.
//
// THE DIGEST IS CHECKED BEFORE THE CONTENT IS USED, not as a separate audit somebody
// remembers to run. This file decides what goes into an image that runs other
// people's CI on the operator's own hardware, and an edit to it is an edit to every
// image built afterwards — so "is this still the reviewed file" has to be answered
// on the path that reads it rather than beside it.
//
// PARSED AFRESH ON EVERY CALL, AND NOT CACHED. A cached Toolset returned by value
// still hands out the package's own slices: a caller that changed one apt package
// would change it for every later caller, and the digest check — which ran once,
// over the bytes rather than the parsed value — would never notice. Reparsing a
// few kilobytes is cheaper than any scheme for making shared mutable state safe.
func Load() (Toolset, error) {
	if err := VerifyToolset(toolset, PinnedSHA256()); err != nil {
		return Toolset{}, err
	}

	// UNKNOWN FIELDS ARE ALLOWED, DELIBERATELY, and this is the one place the
	// usual billet rule is inverted. Upstream adds keys on its own schedule —
	// a new language, a new module list — and refusing them would turn a
	// routine upstream refresh into a build that stops before it starts, for a
	// key billet does not read. The drift gate is what reports an addition
	// worth acting on; a decoder is the wrong place to enforce it.
	var out Toolset

	if err := json.Unmarshal(toolset, &out); err != nil {
		return Toolset{}, fmt.Errorf("runnerimages: parse the vendored toolset: %w", err)
	}

	// THE ALIAS MAP IS VALIDATED HERE, because AptPackages applies it and a caller
	// that got a Toolset has no reason to suspect the mapping separately. An
	// unusable entry is a build that installs one fewer package than it was told
	// to, with no error anywhere.
	if err := ValidateAptAliases(); err != nil {
		return Toolset{}, err
	}

	return out, nil
}

// AptPackages is every package the image installs, in upstream's own order.
//
// ORDER IS PRESERVED AND DUPLICATES ARE REMOVED. Upstream installs vital, then
// common, then cmd, and a package named in two lists is one install; emitting it
// twice makes an apt command longer for no reason and makes a diff of this list
// read as though something changed.
// The names are the ones apt can install, which is not always the name upstream
// declared: see AptAliases.
func (t Toolset) AptPackages() []string {
	alias := AptAliases()

	seen := make(map[string]struct{})
	out := make([]string, 0,
		len(t.Apt.VitalPackages)+len(t.Apt.CommonPackages)+len(t.Apt.CmdPackages))

	// THE TOOLCHAIN SECTIONS ARE PART OF THE SAME LIST, because every consumer of
	// this one either installs it or checks it, and a compiler that arrived through
	// a second path would be installed by one backend and unknown to the other's
	// gate. Appended last so the three apt groups keep upstream's own order.
	for _, group := range [][]string{
		t.Apt.VitalPackages, t.Apt.CommonPackages, t.Apt.CmdPackages,
		t.ToolchainPackages(),
	} {
		for _, pkg := range group {
			if pkg == "" {
				continue
			}

			// MAPPED BEFORE DEDUPLICATION. Two declared names could map onto one
			// installable package, and emitting it twice would ask apt to install
			// the same thing twice.
			if mapped, ok := alias[pkg]; ok {
				pkg = mapped
			}

			if _, dup := seen[pkg]; dup {
				continue
			}

			seen[pkg] = struct{}{}

			out = append(out, pkg)
		}
	}

	return out
}

// ToolchainPackages are the compiler and language sections, as apt package names.
//
// FIVE SECTIONS, THREE SHAPES, AND THE DECLARATION IS NOT CONSISTENT ABOUT THEM.
// `gcc` and `gfortran` list COMPLETE package names ("g++-13", "gfortran-13");
// `clang` lists bare majors ("18") that a name has to be built from; `php` and
// `postgresql` carry a single version each rather than a list. Reading them as one
// shape produces names apt has never heard of, and the build fails minutes in with
// a message about a package rather than about a declaration.
//
// MEASURED AGAINST ubuntu-24.04's OWN main+universe, on BOTH architectures and
// with identical answers: clang-16/17/18, g++-12/13/14, gfortran-12/13/14,
// php8.3-cli and postgresql-client-16 all resolve. No third-party apt repository
// is needed for any of them -- an earlier probe said otherwise and was reading a
// package list left empty by an `apt-get update` whose failure it had sent to
// /dev/null.
//
// THE CLIENT FOR POSTGRES AND THE CLI FOR PHP, not the server and not the whole
// stack. What a job uses is `psql` and `php`; `postgresql-16` would install and
// enable a database in every image for the benefit of nobody, and upstream's own
// image reaches these through a service container instead.
func (t Toolset) ToolchainPackages() []string {
	out := make([]string, 0, 12)

	// BARE MAJORS, SO THE NAME IS BUILT. The default_version beside them is an
	// update-alternatives concern rather than another package.
	for _, v := range t.Clang.Versions {
		if v != "" {
			out = append(out, "clang-"+v)
		}
	}

	// ALREADY COMPLETE NAMES. Prefixing these would produce `g++-g++-13`.
	for _, group := range [][]string{t.GCC.Versions, t.GFortran.Versions} {
		for _, v := range group {
			if v != "" {
				out = append(out, v)
			}
		}
	}

	for _, v := range t.PHP.Versions {
		if v != "" {
			out = append(out, "php"+v+"-cli")
		}
	}

	if t.PostgreSQL.Version != "" {
		out = append(out, "postgresql-client-"+t.PostgreSQL.Version)
	}

	// pipx COMES FROM APT, NOT FROM pip. Ubuntu 24.04 marks its system python
	// externally-managed, so `pip install pipx` is refused outright -- and the
	// declaration lists pipx PACKAGES without saying how pipx itself arrives.
	if len(t.Pipx) > 0 {
		out = append(out, "pipx")
	}

	return out
}

// AptAliases is the declared-name to installable-name mapping.
//
// A MISSING OR MALFORMED FILE YIELDS AN EMPTY MAP RATHER THAN AN ERROR, because
// the mapping is an exception list: having none is the ordinary case. What it
// must never do is silently DROP an entry it was given, so the file is embedded
// rather than read from disk and a parse failure is impossible at runtime for a
// binary that compiled.
func AptAliases() map[string]string {
	// DECODED PER MEMBER, NOT AS ONE MAP OF ENTRIES. The file documents itself in
	// a `_comment` member whose value is an ARRAY of lines, so unmarshalling the
	// whole document into map[string]entry fails on that one member and discards
	// EVERY mapping — silently, because the failure returns an empty map and an
	// empty map is the legitimate "no exceptions" answer. That is exactly what
	// happened: the shell mapped netcat and Go did not, and the two-sided test is
	// what caught it.
	var raw map[string]json.RawMessage

	if err := json.Unmarshal(aptAliases, &raw); err != nil {
		return map[string]string{}
	}

	out := make(map[string]string, len(raw))

	for name, body := range raw {
		// A COMMENT KEY IS SKIPPED BY ITS NAME, NOT BY FAILING TO PARSE.
		//
		// The previous rule was "skip anything that does not decode into an entry
		// with a non-empty install", which silently swallowed a REAL entry whose
		// install was empty or misspelt — and the three readers then disagreed:
		// Go kept the declared name while both shell readers emitted a blank that
		// dropped the package from the install list AND from the gate's expected
		// set. Naming the convention makes a malformed entry loud instead.
		if strings.HasPrefix(name, "_") {
			continue
		}

		var entry struct {
			Install string `json:"install"`
		}

		if err := json.Unmarshal(body, &entry); err != nil || entry.Install == "" {
			// RECORDED AS UNUSABLE RATHER THAN DROPPED. The caller turns this
			// into an error; returning a partial map would be the same silent
			// hole under a different name.
			out[name] = ""

			continue
		}

		out[name] = entry.Install
	}

	return out
}

// ValidateAptAliases reports every alias entry that names no installable package.
//
// SEPARATE FROM AptAliases SO Load CAN FAIL ON IT. An unusable entry must not be
// a map the callers quietly route around: the shell readers turn one into a blank
// package name, which removes the package from what is installed and from what
// the gate requires in the same step, and nothing then reports it missing.
func ValidateAptAliases() error {
	var bad []string

	for name, install := range AptAliases() {
		if install == "" {
			bad = append(bad, name)
		}
	}

	if len(bad) == 0 {
		return nil
	}

	sort.Strings(bad)

	return fmt.Errorf("runnerimages: apt-aliases.json maps %s to nothing installable; an "+
		"entry needs a non-empty \"install\", and a comment key must start with \"_\"",
		strings.Join(bad, ", "))
}

// JavaHomeVars is the JAVA_HOME_<version>_X64 environment every hosted image sets.
//
// THESE ARE NOT DECORATION. setup-java reads JAVA_HOME_<version>_X64 to find a JDK
// already on the machine, and a workflow that pins a toolchain by environment
// variable rather than by action reads them directly. An image carrying the JDKs
// without the variables has them installed and unfindable, which is the toolcache
// failure one directory over: nothing errors, the job just does the slow thing.
func (t Toolset) JavaHomeVars(root string) map[string]string {
	out := make(map[string]string, len(t.Java.Versions)+1)

	for _, v := range t.Java.Versions {
		if v == "" {
			continue
		}

		out["JAVA_HOME_"+v+"_X64"] = root + "/temurin-" + v + "-jdk-amd64"
	}

	if t.Java.Default != "" {
		out["JAVA_HOME"] = root + "/temurin-" + t.Java.Default + "-jdk-amd64"
	}

	return out
}

// InstallToolcacheScript is the toolcache installers both backends run.
//
// EMBEDDED AND ALSO SOURCED FROM DISK, deliberately: scripts/build-guest-image.sh
// dots this same file in by path, so there is one implementation rather than a Go
// copy and a shell copy that drift. The EC2 backend has no path to it on the
// builder, so it carries these bytes in the provisioning script.
//
//go:embed install-toolcache.sh
var InstallToolcacheScript string

// installerKeys are the top-level sections install-toolcache.sh reads.
//
// EVERY ONE IS A jq EXPRESSION IN THAT FILE, and a section missing here reaches
// the builder as an empty answer -- which the installers treat as "the toolset
// declares no cmake version" and refuse. That is not hypothetical: shipping only
// toolcache and java, while the installers had grown to read seven more, failed a
// real build four minutes in, immediately after the last toolcache entry, with a
// message about a declaration rather than about a projection.
//
// TestTheProjectionAnswersEveryReaderTheSameWay is what keeps this honest, because
// it RUNS each reader against both the full file and this subset and requires the
// same answer. A key added to the shell without being added here fails it.
var installerKeys = []string{
	"toolcache",         // every tool's version globs, and Ruby's platform_version
	"java",              // the JDK versions and the default
	"cmake",             // a pinned version, not apt's
	"pwsh",              // a line, resolved to the newest non-prerelease tag
	"dotnet",            // the SDK channels
	"node",              // which line a bare `node` on PATH should be
	"pipx",              // the packages and the commands they provide
	"node_modules",      // the globals installed through the default node
	"rubygems",          // the gems installed through the default ruby
	"clang",             // WHICH major the unsuffixed clang must be; apt creates none
	"powershellModules", // installed through PSGallery, two of the four pinned
	"azureModules",      // the same mechanism, a second list upstream maintains apart
	"android",           // the SDK, its platforms, build-tools and NDKs
}

// InstallerToolset is the declaration the installers read, and nothing else.
//
// THE APT SECTIONS STAY BEHIND. `.apt` and the compiler sections are read on THIS
// side and interpolated as a command rather than shipped as data, and the dozen
// upstream keys nothing reads -- android, brew, selenium and the rest -- are what
// makes the full file cost five times this one.
//
// IT EXISTS FOR A BUDGET, AND THE BUDGET IS THE REASON TO BE CAREFUL. The EC2
// backend delivers the whole provisioning script through user data, which EC2
// caps at 16384 bytes; the full declaration costs 1673 of those compressed and
// this costs 329. That is the difference between an image that can carry a
// toolcache for both architectures and one that cannot.
//
// A SUBSET IS A SECOND REPRESENTATION, WHICH IS THE RISK. If this ever drops a key
// an installer reads, the installer sees an empty answer -- and read_toolset_versions
// refuses an empty answer loudly, which is the shape that saves it. The stronger
// guard is TestTheProjectionAnswersEveryReaderTheSameWay, which runs the shell's
// own readers against both this and the full file and requires identical answers.
func InstallerToolset() ([]byte, error) {
	var full map[string]json.RawMessage

	if err := json.Unmarshal(toolset, &full); err != nil {
		return nil, fmt.Errorf("runnerimages: parse the vendored toolset: %w", err)
	}

	// SORTED, SO THE OUTPUT IS THE SAME EVERY TIME. This is hashed and the digest
	// travels beside it; a map iteration order would make the digest differ
	// between two runs of the same build and turn the check into a coin toss.
	out := make(map[string]json.RawMessage, len(installerKeys))

	for _, key := range installerKeys {
		v, ok := full[key]
		if !ok {
			return nil, fmt.Errorf("runnerimages: the vendored toolset has no %q section, "+
				"which an installer reads", key)
		}

		out[key] = v
	}

	// json.Marshal on a map sorts keys, so this is deterministic.
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("runnerimages: render the installer toolset: %w", err)
	}

	// A TRAILING NEWLINE, BECAUSE THE THING THAT GETS HASHED IS A FILE.
	//
	// This travels in a heredoc, which ends the document on a line of its own and
	// therefore terminates the payload's last line whether or not it had one. So a
	// projection without a newline is hashed here as N bytes and lands on the
	// builder as N+1 -- the digests disagree, and the check refuses EVERY build
	// rather than a tampered one. The full declaration ends with a newline the way
	// files do, which is why nothing noticed until it was replaced.
	return append(b, '\n'), nil
}

// InstallerToolsetSHA256 is the digest of what InstallerToolset returns.
//
// NOT PinnedSHA256, AND THE DIFFERENCE IS WORTH STATING. The pinned digest proves
// the vendored file is upstream's; this one proves the script received exactly
// what billet sent. Load has already checked the first before this can be called,
// so the chain is: upstream's file, verified here, projected here, and the
// projection verified on the builder.
func InstallerToolsetSHA256() (string, error) {
	b, err := InstallerToolset()
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:]), nil
}

// ToolsetJSON is the pinned declaration, verbatim.
//
// VERBATIM IS THE POINT. The EC2 build carries these bytes to its builder and
// checks PinnedSHA256 against them THERE, so what the installers read is what the
// pin names rather than something billet reshaped in transit. Callers that want
// the parsed form use Load; this is for delivery.
func ToolsetJSON() string {
	return string(toolset)
}
