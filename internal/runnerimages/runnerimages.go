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
// that is wrong. Both now run GitHub's own template against these declarations
// (ADR-022), the guest gate reads them through `jq`, and the AMI verifier lists the
// toolcache they name.
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
	"errors"
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
// THE ARM64 DECLARATION IS THE SECOND LINE, `arm64 <sha256>`, at the same commit:
// GitHub's arm64 template reads a toolset of its own (ADR-022), and one commit for
// both keeps the two architectures' images from two upstream revisions. The first
// line stays exactly what it was, because the guest build reads it.
//
//go:embed pinned.txt
var pinned string

// toolset is GitHub's declaration of the ubuntu-24.04 image contents, verbatim.
//
//go:embed toolset-2404.json
var toolset []byte

// toolsetArm64 is GitHub's declaration of the ubuntu-24.04 arm64 image, verbatim.
//
//go:embed toolset-2404-arm64.json
var toolsetArm64 []byte

// aptAliases maps a declared package name to one apt can actually install.
//
// BILLET'S OWN FILE, DELIBERATELY NOT THE VENDORED ONE. The toolset is upstream's
// and is pinned by digest so it cannot be edited; this is the narrow reviewed
// mapping that makes a name in it installable, and keeping the two apart means
// every deviation from what GitHub declared is one line in a file that exists to
// list deviations.
//
// APPLIED HERE AS WELL AS IN THE GUEST GATE, so the gate's own reading of the
// declaration is held to this one by test: a mapping applied by one reader and not
// the other is the two-pins problem this package was created to remove.
//
//go:embed apt-aliases.json
var aptAliases []byte

// PinnedCommit is the actions/runner-images revision both vendored toolsets came from.
func PinnedCommit() string {
	commit, _, _ := strings.Cut(strings.TrimSpace(pinned), " ")

	return commit
}

// PinnedFile is pinned.txt as it is vendored, which the template runner checks a
// toolset against when it runs from the EC2 builder's payload rather than a checkout.
func PinnedFile() []byte {
	return []byte(pinned)
}

// PinnedSHA256 is the digest of the vendored x64 toolset as it should be on disk.
func PinnedSHA256() string {
	first, _, _ := strings.Cut(strings.TrimSpace(pinned), "\n")
	_, sum, _ := strings.Cut(strings.TrimSpace(first), " ")

	return strings.TrimSpace(sum)
}

// The architectures GitHub publishes an ubuntu-24.04 declaration for, spelled as
// billet's AMI and the tool cache spell them.
const (
	ArchX64   = "x64"
	ArchArm64 = "arm64"
)

// PinnedSHA256For is the digest the vendored toolset of arch must have, or an
// error for an architecture billet vendors no declaration for.
func PinnedSHA256For(arch string) (string, error) {
	switch arch {
	case ArchX64:
		return PinnedSHA256(), nil
	case ArchArm64:
		for _, line := range strings.Split(strings.TrimSpace(pinned), "\n")[1:] {
			name, sum, ok := strings.Cut(strings.TrimSpace(line), " ")
			if ok && name == ArchArm64 {
				return strings.TrimSpace(sum), nil
			}
		}

		return "", fmt.Errorf("runnerimages: pinned.txt names no digest for the arm64 toolset")
	default:
		return "", fmt.Errorf("runnerimages: %q is not an architecture billet vendors a toolset for", arch)
	}
}

// ToolsetFileFor is the vendored declaration's file name for arch, as GitHub names it.
func ToolsetFileFor(arch string) (string, error) {
	switch arch {
	case ArchX64:
		return "toolset-2404.json", nil
	case ArchArm64:
		return "toolset-2404-arm64.json", nil
	default:
		return "", fmt.Errorf("runnerimages: %q is not an architecture billet vendors a toolset for", arch)
	}
}

// ToolsetBytesFor is the vendored declaration of arch, verified against its pin and
// returned as a copy.
func ToolsetBytesFor(arch string) ([]byte, error) {
	data := toolset
	if arch == ArchArm64 {
		data = toolsetArm64
	}

	want, err := PinnedSHA256For(arch)
	if err != nil {
		return nil, err
	}

	return verifiedCopy(arch, data, want)
}

// verifiedCopy is data held to want and copied, or an error naming where arch's
// declaration comes from: the check ToolsetBytesFor makes, apart from the embedded
// bytes so a test can hand it wrong ones.
func verifiedCopy(arch string, data []byte, want string) ([]byte, error) {
	if err := verifyToolsetBytes(data, want); err != nil {
		source, urlErr := SourceURLFor(arch)
		if urlErr != nil {
			return nil, errors.Join(err, urlErr)
		}

		return nil, fmt.Errorf("%w; refresh both together from %s, or restore the file", err, source)
	}

	out := make([]byte, len(data))
	copy(out, data)

	return out, nil
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

// SourceURLFor names where the vendored declaration of arch came from.
func SourceURLFor(arch string) (string, error) {
	file, err := ToolsetFileFor(arch)
	if err != nil {
		return "", err
	}

	return "https://raw.githubusercontent.com/actions/runner-images/" + PinnedCommit() +
		"/images/ubuntu/toolsets/" + file, nil
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
	if err := verifyToolsetBytes(data, wantDigest); err != nil {
		return fmt.Errorf("%w; refresh both together from %s, or restore the file", err, SourceURL())
	}

	return nil
}

// verifyToolsetBytes is the comparison alone, naming no architecture's source.
func verifyToolsetBytes(data []byte, wantDigest string) error {
	sum := sha256.Sum256(data)

	if got := hex.EncodeToString(sum[:]); got != wantDigest {
		return fmt.Errorf("runnerimages: the vendored toolset hashes to %s and pinned.txt names %s",
			got, wantDigest)
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

// LoadFor is the vendored declaration of arch, verified against its pin and
// parsed, with unknown fields allowed for the reason Load gives.
func LoadFor(arch string) (Toolset, error) {
	data, err := ToolsetBytesFor(arch)
	if err != nil {
		return Toolset{}, err
	}

	var out Toolset

	if err := json.Unmarshal(data, &out); err != nil {
		return Toolset{}, fmt.Errorf("runnerimages: parse the vendored %s toolset: %w", arch, err)
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
