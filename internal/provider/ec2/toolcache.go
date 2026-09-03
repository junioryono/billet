package ec2

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/runnerimages"
)

// Where the build inputs land on the builder. All three are removed before
// poweroff: an image carrying its own build inputs is larger and a puzzle for
// whoever finds them.
// What a toolcache name and a declared version glob may be, before either is
// interpolated into a path in a script that runs as root. The digest on the
// declaration proves provenance, not that its strings are safe shell syntax --
// the same rule aptPackageName states for the package names.
var (
	toolcacheName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]{0,31}$`)
	// A BARE `*` IS ONE OF THE SHAPES THE DECLARATION USES. CodeQL is declared as
	// `*` -- not a line to resolve, but "whatever the action currently pins" -- and
	// the earlier pattern required a leading digit, so it rejected the declaration
	// outright. Everything else is still a version or a version prefix.
	toolcacheGlob = regexp.MustCompile(`^(\*|\d[0-9.]{0,15}\*?)$`)
	// A JAVA VERSION IS NOT A GLOB, and reusing toolcacheGlob for one was wrong.
	// The value is interpolated into a variable NAME (JAVA_HOME_<v>_X64), into a
	// `sed` pattern, and into the apt package `temurin-<v>-jdk`. A declared `8.*`
	// passes the glob pattern, produces an env var name that is not an identifier,
	// and makes the sed match anything. Harmless with today's 8/11/17/21/25 and
	// one upstream edit from not being.
	javaVersion = regexp.MustCompile(`^\d{1,3}$`)
	// A COMMAND NAME FROM THE DECLARATION, which pipx and node_modules both supply
	// and which is interpolated into a SINGLE-QUOTED shell string -- both the
	// diagnostic and, now, the step label. The digest proves where the string came
	// from and says nothing about whether it contains a quote, which is the rule
	// the three patterns above already state. Today's declaration is `yamllint`,
	// `ansible`, `tsc` and the like; one upstream edit is all that stands between
	// that and a name this would paste into the middle of a quoted string.
	globalCommand = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,63}$`)
)

// declaredPSModules is every PowerShell and Azure module the declaration names.
//
// TWO KEYS, ONE LIST, because upstream maintains them apart and installs them the
// same way. The names become part of a single-quoted PowerShell string, so they
// are checked the way every other declared string that reaches a shell is.
func declaredPSModules(ts runnerimages.Toolset) ([]string, error) {
	var out []string

	for _, m := range append(append([]runnerimages.PSModule{}, ts.PowerShellModules...),
		ts.AzureModules...) {
		if m.Name == "" {
			continue
		}

		if !globalCommand.MatchString(m.Name) {
			return nil, fmt.Errorf("ec2: the declaration names a powershell module %q, "+
				"which this build will not paste into a quoted shell string", m.Name)
		}

		out = append(out, m.Name)
	}

	return out, nil
}

// unpublishedFile is where the build records a declared line the vendor
// publishes nothing for. Named here and in install-toolcache.sh, which is one
// name too many -- but the shell writes it and the Go side emits a check that
// reads it, and neither can import from the other.
const unpublishedFile = ".billet-unpublished"

// androidSDKRoot is where the shared installer puts the SDK. Named here and in
// install-toolcache.sh, which is one name too many — but the shell installs it and
// the Go side emits a check that reads it, and neither can import from the other.
const androidSDKRoot = "/usr/local/lib/android/sdk"

const (
	toolcacheAssetPath = "/opt/billet-install-toolcache.sh"
	toolsetPath        = "/opt/billet-toolset.json"
	toolcacheWork      = "/opt/billet-toolcache-work"
)

// stripShellComments removes whole-line comments and the blank lines around
// them, and nothing else.
//
// THE OBVIOUS STRIP IS WRONG IN THIS FILE, and it was measured wrong before it
// was measured right. `s/#.*$//` treats `${v#v}`, `${version#go}` and their
// neighbours as comments -- they are parameter expansions -- and removing them
// leaves a program that fails `bash -n`. An earlier "it fits" measurement was
// taken against exactly that wreckage.
//
// A line whose first non-blank character is `#` is always a comment in shell, so
// dropping whole such lines cannot corrupt an expansion. That is the only strip
// worth trusting, and a test runs `bash -n` over the result rather than assuming
// this reasoning holds for a comment shape nobody has written yet.
func stripShellComments(src string) string {
	var b strings.Builder

	for line := range strings.SplitSeq(src, "\n") {
		if trimmed := strings.TrimLeft(line, " \t"); strings.HasPrefix(trimmed, "#") {
			// THE SHEBANG WOULD GO TOO, and this file has none by design: it is
			// sourced rather than executed. Keeping the check simple is worth more
			// than handling a line that must not appear.
			continue
		}

		b.WriteString(line)
		b.WriteString("\n")
	}

	return b.String()
}

// writeToolcacheInstall emits the shared installers, the pinned declaration, and
// the bash driver that runs them.
//
// THE SAME CODE THE GUEST BUILD RUNS. scripts/build-guest-image.sh sources
// internal/runnerimages/install-toolcache.sh from disk; the builder has no path
// to it, so it carries those bytes. What differs is the contract the driver
// states: the guest passes a target root and everything goes through chroot,
// while here the builder IS the target and BILLET_TC_ROOT is empty.
func writeToolcacheInstall(b *strings.Builder, arch, url, digest string) error {
	// STAGED, ALWAYS. There is no longer an embedded alternative and the reason is
	// arithmetic rather than preference: the installers and the pinned declaration
	// render to 64248 bytes and 17077 compressed, against EC2's 16384-byte cap for
	// user data. Parity only grows -- every runtime, module and SDK upstream adds
	// lands in the same script -- so the inline path is not tight, it is finished.
	//
	// IT WAS KEPT ONE ROUND TOO LONG. The argument for holding it was that a build
	// with no bucket and a script that still fits needs neither an object nor a new
	// permission, and that a rarely-taken path is where bugs live so both should be
	// exercised. The second half was right and is exactly the problem: a path that
	// cannot carry the real declaration cannot be exercised against real data, so
	// what the tests covered was a fixture, and the operator-facing behaviour was a
	// fallback that would fail every time it was reached.
	if url == "" || digest == "" {
		return fmt.Errorf("ec2: the shared installers must be staged before the " +
			"provisioning script can be rendered; set PayloadBucket (billet ami build " +
			"--payload-bucket) so they are fetched from S3 by a bootstrap. They no " +
			"longer compress into EC2's 16384-byte user data and cannot be embedded")
	}

	// THE DIGEST IS CHECKED BEFORE ANYTHING IS EXTRACTED. A presigned URL is a
	// credential rather than a statement about content: it says who may read the
	// object, not that the object is the one billet uploaded. Verifying first is
	// what makes an interfered-with bucket a failed build rather than a root shell
	// running someone else's installers.
	//
	// TRACING OFF ACROSS THE URL, because the provisioning script runs under
	// `set -x` and a presigned URL is a credential. Traced, it is written to the
	// console output -- readable by anything holding ec2:GetConsoleOutput -- and to
	// /var/log/cloud-init-output.log, which is INSIDE the image being built, so
	// every job that later boots this AMI could read it. It expires in an hour and
	// authorises exactly one GET, which bounds the damage rather than excusing it.
	b.WriteString("set +x\n")
	b.WriteString("curl -fsSL --retry 5 --retry-all-errors -o " + payloadPath +
		" " + strconv.Quote(url) + "\n")
	b.WriteString("set -x\n")
	b.WriteString("echo " + strconv.Quote(digest+"  "+payloadPath) +
		" | sha256sum -c - >/dev/null\n")

	// EXTRACTED FROM /, because the archive carries the absolute paths the driver
	// below names. tar refuses a member that escapes the destination, so the
	// archive billet builds is the only thing that decides where these land.
	b.WriteString("tar -xzf " + payloadPath + " -C /\n")
	b.WriteString("rm -f " + payloadPath + "\n")

	// BASH, EXPLICITLY. The installers are bash -- arrays, namerefs, ${x/y/z} --
	// and this script is /bin/sh, which is dash on Ubuntu. Sourcing rather than
	// executing, in one process, because a function definition does not cross a
	// process boundary; that is why there is a driver at all.
	//
	// BILLET_TC_ROOT IS EMPTY BECAUSE THE BUILDER IS THE TARGET. The guest build
	// passes its mounted rootfs and every in-target command goes through chroot.
	// THE SCRATCH DIRECTORY EXISTS BEFORE ANYTHING DOWNLOADS INTO IT. The
	// installers write each archive to $BILLET_TC_WORK and delete it again; on
	// the guest build that directory is the build's own workspace and already
	// there, so nothing in the shared file creates it.
	b.WriteString("install -d -m 0700 " + toolcacheWork + "\n")

	b.WriteString("bash -c '\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString(". " + toolcacheAssetPath + "\n")
	b.WriteString("BILLET_TC_ROOT=\"\" \\\n")
	b.WriteString("  BILLET_TC_ARCH=" + arch + " \\\n")
	b.WriteString("  BILLET_TC_DIR=" + toolcacheDir + " \\\n")
	b.WriteString("  BILLET_TC_IN_TARGET=" + toolcacheDir + " \\\n")
	b.WriteString("  BILLET_TC_WORK=" + toolcacheWork + " \\\n")
	b.WriteString("  BILLET_TC_TOOLSET=" + toolsetPath + " \\\n")
	b.WriteString("  BILLET_TC_ENV_FILE=" + imageEnvFile + " \\\n")

	// THE ANDROID LICENCE IS ACCEPTED HERE AND NOWHERE ELSE, and the asymmetry is
	// the point. This AMI is built in the operator's own account, from their own
	// `billet ami build`, and stays there — that is USE of Google's SDK. The guest
	// image is published as a GitHub release asset, which is REDISTRIBUTION, and
	// Google's terms treat the two differently. The shared installer does nothing
	// unless this variable says yes, so the guest build gets no android by omission
	// rather than by a second code path that could drift from this one.
	b.WriteString("  BILLET_TC_ANDROID_ACCEPT_LICENSES=yes \\\n")
	b.WriteString("  billet_install_toolcache\n")
	b.WriteString("'\n")

	// NOTHING BILLET WROTE TO BUILD THE IMAGE STAYS IN IT. The installers already
	// remove each archive as they go and run `apt-get clean`; these three are
	// billet's own, and an image carrying its own build inputs is both larger and
	// a puzzle for whoever finds them.
	b.WriteString("rm -rf " + toolcacheAssetPath + " " + toolsetPath + " " + toolcacheWork + "\n")

	ts, err := runnerimages.Load()
	if err != nil {
		return fmt.Errorf("ec2: %w", err)
	}

	return writeToolcacheGate(b, ts, arch, gateOptions{})
}

// gateOptions are the two ways the artifact gate differs from the in-build one.
//
// ZERO IS THE BUILD'S BEHAVIOUR, DELIBERATELY. The build script this package emits
// is measured -- its compressed size is what decides whether a CA bundle fits at
// all -- so the gate has to be extendable without moving a byte of it, and a test
// asserts exactly that.
type gateOptions struct {
	// RunPrefix goes in front of every tool invocation. Empty on the builder, which
	// is root with an inherited environment; on the booted artifact it is the shell
	// function that drops to the runner account under `env -i`, because present on
	// disk is not the same as findable by a job.
	RunPrefix string
	// StepVar, when set, is a shell variable each check assigns its own label to
	// before running. That is what lets a verifier report WHICH declared line failed
	// with a string billet wrote, rather than quoting whatever the machine printed.
	StepVar string
	// Record is told every label as it is emitted, so a reader can be built from
	// the same strings the script carries.
	//
	// A SINK RATHER THAN A SECOND ENUMERATION. The alternative is a function that
	// walks the declaration again to list what the gate WOULD have named, which is
	// two readings of one fact and drifts the first time either loop changes — the
	// two-pins problem this package already has a file dedicated to avoiding.
	Record func(label string)
}

// step emits the assignment naming the check about to run, or nothing.
//
// SINGLE-QUOTED, AND EVERY LABEL IS ALREADY PROVEN TO SURVIVE THAT. The caller
// builds each one from a toolcache name, a version glob or a java version, and
// writeToolcacheGate refuses any of the three that does not match its pattern
// BEFORE reaching here — none of which admits a quote. That is the same rule
// aptPackageName states: a digest proves where a string came from, not that it is
// safe shell syntax.
func (o gateOptions) step(b *strings.Builder, label string) {
	o.record(label)

	if o.StepVar == "" {
		return
	}

	b.WriteString(o.StepVar + "='" + label + "'\n")
}

// record tells the sink about a label without emitting an assignment, for the
// checks whose label is assigned at RUN time by an emitted shell function rather
// than at generation time. The sink still has to learn every label, because it is
// the closed set billet is willing to quote back -- a label the reader has never
// heard of is one it must refuse.
func (o gateOptions) record(label string) {
	if o.Record != nil {
		o.Record(label)
	}
}

// stepAssign is the shell that assigns a label from inside an emitted function,
// or nothing when no verifier is reading. The argument is shell, not a literal.
func (o gateOptions) stepAssign(expr string) string {
	if o.StepVar == "" {
		return ""
	}

	return o.StepVar + "=\"" + expr + "\"; "
}

// writeToolcacheGate asserts the image carries what the declaration promised,
// before poweroff.
//
// THE EXPECTED SET, NOT WHAT HAPPENS TO EXIST. The guest gate's first toolcache
// check walked `$TOOLCACHE/$tool/*` and passed if the count was non-zero, so
// deleting a promised version left it green. What a workflow actually depends on
// is that asking for a DECLARED LINE finds something, so this iterates the globs
// GitHub declares and requires a completed entry matching each -- `22.*` must
// match, by name, or a `node-version: 22` job downloads a runtime on an image
// advertising a toolcache.
//
// IN-BUILD, BEFORE poweroff, WHICH IS THE SHAPE THE DOCKER GATE SET. Under `set
// -e` a failure here aborts before the success signal, so no image is registered.
// That is not the same as booting the produced AMI and checking it -- an artifact
// gate is still worth having -- but it is the difference between a claim about the
// script and a claim about the disk being snapshotted.
func writeToolcacheGate(
	b *strings.Builder, ts runnerimages.Toolset, arch string, opts gateOptions,
) error {
	if len(ts.Toolcache) == 0 {
		return fmt.Errorf("ec2: the pinned toolset declares no toolcache; refusing to build " +
			"an image that would claim one")
	}

	// ONE FUNCTION, CALLED PER LINE, rather than the same fourteen lines of shell
	// repeated for every declared version. With three tools the repetition was
	// affordable; with six it is about twenty copies, and this script's budget is
	// shared with the CA bundle -- the margin test caught it at 514 bytes spare,
	// which is what that assertion exists to do.
	//
	// WHAT IT PROVES IS UNCHANGED. A completed entry matching the line, with a
	// runnable binary inside it: `-f` on the marker, then the tool executed. An
	// entry that is complete on disk and cannot run is the failure that is hard to
	// see, and a marker without its arch directory is what a pathname-only check
	// accepted before a review caught it.
	// billet_tc DERIVES WHAT EVERY CALL WAS SPELLING OUT.
	//
	// The pattern is the glob with `.*` appended unless it already ends in `*`,
	// and the label is "<name> <glob>" -- both computable from the two arguments
	// the caller already passes, and both were being written out at every one of
	// twenty-three call sites. Measured: 2041 bytes of billet_tc_require calls plus
	// most of 1073 bytes of separate step assignments, against roughly 650 through
	// this. The verification script shares this emitter and has a 4 KiB reserve, so
	// the difference is whether the next section added to the declaration turns a
	// test red for a reason two files away from its cause.
	//
	// THE BINARY AND THE FLAG STAY ARGUMENTS because they genuinely vary per tool
	// -- `bin/python --version` against `bin/go version` -- and the inner marker
	// stays an optional fifth for CodeQL alone.
	b.WriteString("billet_tc() {\n")
	b.WriteString("  case \"$2\" in *\\*) billet_pat=\"$2\";; *) billet_pat=\"$2.*\";; esac\n")

	if opts.StepVar != "" {
		b.WriteString("  " + opts.StepVar + "=\"$1 $2\"\n")
	}

	b.WriteString("  billet_tc_require \"" + toolcacheDir + "/$1/$billet_pat\" \"$3\" \"$4\" " +
		"\"$1 $2\" ${5:-}\n")
	b.WriteString("}\n")

	b.WriteString("billet_tc_require() {\n")
	b.WriteString("  billet_found=0\n")
	// UNQUOTED HERE, AND QUOTED AT EVERY CALL SITE, which is the only pairing that
	// works. Unquoted at the call, the shell expands the pattern before the
	// function sees it -- so `CodeQL/*` becomes one argument per match and every
	// later argument shifts, silently turning the binary into a version flag.
	// Quoted here, `for … in "$1"` iterates the literal pattern and matches
	// nothing. The pattern has to arrive whole and expand once, inside.
	//
	// A toolcache path has no spaces, so the word splitting that comes with an
	// unquoted expansion costs nothing here and the glob is the point.
	b.WriteString("  for billet_entry in $1; do\n")
	// AN EXPLICIT SEMVER OR IT IS INVISIBLE, which the guest gate has checked
	// since it was written and this one never did. @actions/tool-cache keeps only
	// directories that parse as a full version when resolving a range, so
	// `Ruby/3.4.0-preview1` satisfies the glob `Ruby/3.4.*` here and is skipped by
	// every lookup -- a gate that passes an image whose entry no job can find.
	//
	// A REGEX ANCHORED AT BOTH ENDS, not a `case` pattern: `*.*.*` accepts
	// `1.2.3junk` and `1.2.3.4`, which are exactly the names a botched extraction
	// leaves. Once in the helper rather than once per line, so the cost to the
	// 16KiB budget does not grow with the declaration.
	//
	// AND A TRAILING `-<digits>` IS ALLOWED, BECAUSE TEMURIN USES IT. This helper
	// checks the JDK entries too, and their real names are `8.0.504-1` --
	// adoptium's build number, not a botched extraction. The first version of this
	// check refused them, which would have failed every correct image: exactly the
	// failure this gate exists to prevent, pointed the other way. The guest gate
	// gets away with the stricter form only because its loop never sees Java.
	b.WriteString("    printf '%s' \"${billet_entry##*/}\" | " +
		"grep -Eq '^[0-9]+[.][0-9]+[.][0-9]+(-[0-9]+)?$' || continue\n")
	b.WriteString("    [ -f \"$billet_entry/" + arch + ".complete\" ] || continue\n")
	// A FIFTH ARGUMENT FOR A MARKER *INSIDE* THE ENTRY, empty for most tools.
	// codeql's action stats `pinned-version` within the entry, separately from the
	// `x64.complete` beside it, and without it re-downloads a bundle that is
	// already there -- the whole cost this bakes in to avoid, paid on every job
	// while every other check here passes. `${5:-}` because the emitted script is
	// not guaranteed to run without `set -u`.
	b.WriteString("    [ -z \"${5:-}\" ] || [ -f \"$billet_entry/" + arch +
		"/$5\" ] || continue\n")
	b.WriteString("    " + opts.RunPrefix + "\"$billet_entry/" + arch +
		"/$2\" $3 >/dev/null 2>&1 || continue\n")
	b.WriteString("    billet_found=1\n")
	b.WriteString("  done\n")
	// THE RECORD EXCUSES A LINE NOBODY PUBLISHES, and nothing else does. github
	// declares Ruby 4.0.* and ruby-builder has no such asset among its 919,
	// because Ruby 4.0 is not released. The build writes what it skipped; this
	// reads it, so a declared line is either installed or recorded and a quiet
	// omission is still a failed build.
	b.WriteString("  [ \"$billet_found\" -eq 1 ] && return 0\n")
	b.WriteString("  grep -qxF \"$4\" " + toolcacheDir + "/" + unpublishedFile +
		" 2>/dev/null && return 0\n")
	b.WriteString("  echo \"the declaration promises $4 and no completed toolcache entry " +
		"matching it has a runnable $2, and the build did not record it as unpublished; " +
		"a job asking for that line would download a runtime on an image advertising a " +
		"toolcache\" >&2\n")
	b.WriteString("  return 1\n")
	b.WriteString("}\n")

	for _, entry := range ts.Toolcache {
		// CHECKED BEFORE THE SWITCH, not after it. Behind it this could only ever
		// see the names billet bakes, so it read as a control over the declaration
		// and was not one.
		if !toolcacheName.MatchString(entry.Name) {
			return fmt.Errorf("ec2: %q is not a toolcache name this build will look for",
				entry.Name)
		}

		// ONLY WHAT THE INSTALLERS BAKE. The declaration names tools billet does
		// not install yet, and asserting those would fail every build for a gap
		// that is documented rather than accidental.
		binary := toolcacheBinary(entry.Name)
		if binary == "" {
			continue
		}

		for _, glob := range entry.Versions {
			if !toolcacheGlob.MatchString(glob) {
				return fmt.Errorf("ec2: the declaration asks for %s version %q, which is not "+
					"a version glob this build will expand", entry.Name, glob)
			}

			// RECORDED, NOT ASSIGNED. The label is computed by the emitted shell
			// from its two arguments, so the sink has to be told about it here --
			// it is still a closed set billet chose, just built in a different
			// place from where it is written.
			opts.record(entry.Name + " " + glob)

			// THE GLOB IS QUOTED AND THE PATTERN IS BUILT INSIDE, which is the same
			// pairing billet_tc_require already documents one level down. CodeQL is
			// declared as a bare `*`; unquoted here the shell expands it against the
			// BUILD DIRECTORY before the function is entered, so `billet_tc CodeQL *`
			// arrived as `CodeQL api.go authorize.go …` and every later argument
			// shifted -- the gate then reported that the declaration promises
			// "CodeQL api.go". Pathname expansion, not word splitting, and the test
			// caught it because a real declaration contains a real glob.
			b.WriteString("billet_tc " + entry.Name + " '" + glob + "' " + binary + " " +
				toolcacheVersionFlag(entry.Name) +
				toolcacheInnerMarker(entry.Name) + "\n")
		}
	}

	// AND THE TOOLCHAINS THAT ARE NOT TOOLCACHE ENTRIES. cmake, pwsh and dotnet go
	// on PATH, so billet_tc_require cannot see them -- it looks under
	// <tool>/<version>/<arch>. Executed rather than stat'ed, for the reason every
	// other entry here is: a binary that exists and does not run is the failure
	// that reaches a job.
	//
	// THROUGH opts, LIKE THE TOOLCACHE ENTRIES ABOVE. On the builder RunPrefix is
	// empty and this is unchanged; on the booted artifact it drops to the runner
	// account under `env -i`, which is the only thing that answers the question a
	// job actually asks. It matters most for clang: its resource directory is
	// installed by apt and "root can read it" is not the same claim as "the runner
	// can", and root-side testing cannot tell those apart.
	for _, tc := range []struct{ name, cmd string }{
		{"cmake", "/usr/local/bin/cmake --version"},
		{"pwsh", "/usr/bin/pwsh --version"},
		{"dotnet", "/usr/bin/dotnet --list-sdks"},
		// apt CREATES NO BARE `clang`: the versioned packages are co-installable,
		// so none of them owns the unsuffixed name and a package-parity check
		// passes against an image where `clang` is not a command.
		{"clang", "/usr/bin/clang --version"},
		{"clang++", "/usr/bin/clang++ --version"},
	} {
		opts.step(b, tc.name)

		b.WriteString(opts.RunPrefix + tc.cmd + " >/dev/null 2>&1 || " +
			"{ echo '" + tc.name + " was installed and does not run' >&2; exit 1; }\n")
	}

	// THE POWERSHELL AND AZURE MODULES. One emitted function and one call each,
	// for the byte reason every other repeated check here has: pwsh's own startup
	// dominates the runtime either way, so what is being saved is script size
	// rather than seconds.
	//
	// Get-Module -ListAvailable IS SATISFIED BY A DIRECTORY, which is weaker than
	// this file's usual standard and is what the install itself already checks more
	// strictly. What this adds is that the module survived into the SNAPSHOT — an
	// AllUsers install writes outside the image's per-user tree, and a check that
	// only ran at build time would not notice if it had not.
	psmods, err := declaredPSModules(ts)
	if err != nil {
		return err
	}

	if len(psmods) > 0 {
		b.WriteString("billet_psmod() { " + opts.stepAssign("psmodule $1") + opts.RunPrefix +
			"/usr/bin/pwsh -NoProfile -NonInteractive -Command " +
			"\"if (-not (Get-Module -ListAvailable -Name '$1')) { exit 1 }\" >/dev/null || " +
			"{ echo \"no powershell module $1\" >&2; exit 1; }; }\n")

		for _, name := range psmods {
			opts.record("psmodule " + name)

			b.WriteString("billet_psmod " + name + "\n")
		}
	}

	// AND ANDROID, WHICH IS ONLY HERE BECAUSE THIS GATE IS THE EC2 ONE. The shared
	// installer does nothing without BILLET_TC_ANDROID_ACCEPT_LICENSES, which the
	// AMI build sets and the guest build does not — so requiring it here is correct
	// and requiring it in the guest gate would fail every guest image. The two gates
	// live in different files, which is what keeps that asymmetry from needing a
	// flag to express.
	if ts.Android.CmdlineTools != "" {
		opts.step(b, "android")

		b.WriteString("test -x " + androidSDKRoot + "/cmdline-tools/latest/bin/sdkmanager || " +
			"{ echo 'the android sdk is not installed' >&2; exit 1; }\n")

		// THE NDK IS THE HALF A JOB ACTUALLY PINS. An SDK with no NDK builds
		// nothing native, and the declaration names a default the environment
		// points at — so a missing one is an ANDROID_NDK naming a directory that
		// does not exist, which is the JAVA_HOME failure one section up.
		opts.step(b, "android ndk")

		b.WriteString("test -d " + androidSDKRoot + "/ndk || " +
			"{ echo 'the android sdk carries no ndk' >&2; exit 1; }\n")
	}

	// THE DEFAULT RUNTIMES AND THE GLOBALS. A toolcache entry is resolved by an
	// action; a workflow step running a bare `node` or a global like `tsc` resolves
	// against PATH, and this image's apt set carries no node and no ruby.
	for _, cmd := range []string{"node", "npm", "ruby", "gem"} {
		opts.step(b, "default "+cmd)

		b.WriteString(opts.RunPrefix + "/usr/local/bin/" + cmd + " --version >/dev/null 2>&1 || " +
			"{ echo 'no default " + cmd + " on PATH' >&2; exit 1; }\n")
	}

	// ONE DEFINITION, ONE CALL PER COMMAND, because this loop is twenty-odd nearly
	// identical lines and every byte is spent TWICE -- once in the build script,
	// whose compressed size decides whether a CA bundle fits, and once in the
	// verification script, which must stay uncompressed, under user data, and with
	// a reserve for the declaration growing. Written out per command it cost about
	// 1.4 KiB more than this and pushed the verifier past its own budget test.
	//
	// The function absorbs the run prefix and the step assignment, so a check still
	// runs as the runner account and still names itself, at twenty bytes a call.
	// `sh -c`, BECAUSE `command` IS A SHELL BUILTIN AND RunPrefix ENDS IN `env`.
	//
	// The prefix drops to the runner account through `setpriv … env -i PATH=… "$@"`,
	// and env can only EXEC a file. Measured on both sides rather than reasoned
	// about: ubuntu:24.04 has no /usr/bin/command, so `env -i PATH=… command -v sh`
	// exits 127 — while macOS ships /usr/bin/command as one of Apple's builtin
	// stubs and the same line exits 0. So this passed every local run and would have
	// failed EVERY verification of EVERY real image, reporting each one as missing a
	// declared global that was installed correctly. A gate that refuses correct
	// artifacts is the one ADR-005 warns about by name, because what people reach
	// for next is deleting the check.
	//
	// `sh -c script name` sets $0 to name, so the command word is an argument to a
	// shell rather than something env must resolve, and nothing needs quoting twice.
	// It is equally correct when RunPrefix is empty, so both gates stay one emission.
	b.WriteString("billet_need() { " + opts.stepAssign("$2 $1") + opts.RunPrefix +
		"sh -c 'command -v \"$0\" >/dev/null' \"$1\" || " +
		"{ echo \"no $1; a declared $2 provides it\" >&2; exit 1; }; }\n")

	for _, e := range ts.Pipx {
		if e.Cmd == "" {
			continue
		}

		if !globalCommand.MatchString(e.Cmd) {
			return fmt.Errorf("ec2: the declaration names a pipx command %q, which this "+
				"build will not paste into a quoted shell string", e.Cmd)
		}

		opts.record("pipx " + e.Cmd)

		b.WriteString("billet_need " + e.Cmd + " pipx\n")
	}

	for _, e := range ts.NodeModules {
		if e.Command == "" {
			continue
		}

		if !globalCommand.MatchString(e.Command) {
			return fmt.Errorf("ec2: the declaration names a node module command %q, which "+
				"this build will not paste into a quoted shell string", e.Command)
		}

		opts.record("npm " + e.Command)

		b.WriteString("billet_need " + e.Command + " npm\n")
	}

	// billet_env_java PROVES ONE DECLARED VARIABLE NAMES A JDK THAT RUNS.
	//
	// The five lines this replaces were emitted once per declared java version and
	// once more for JAVA_HOME -- the largest repeated block in the gate, and the
	// script it shares with the artifact verifier has a 4 KiB reserve. Same trick
	// as billet_tc and billet_need: what varies is the variable name, so that is
	// the argument and everything else is written once.
	//
	// READ OUT OF THE FILE, NOT THE ENVIRONMENT, which is the part that must not
	// be lost in the compaction. The file is what a job reads; this process's
	// environment is the builder's and says nothing about the image.
	b.WriteString("billet_env_java() {\n")
	b.WriteString("  " + opts.stepAssign("$1") + "billet_jdk=$(sed -n \"s/^$1=//p\" " +
		imageEnvFile + " | head -1)\n")
	b.WriteString("  [ -n \"$billet_jdk\" ] && " + opts.RunPrefix +
		"\"$billet_jdk/bin/java\" -version >/dev/null 2>&1 && return 0\n")
	b.WriteString("  echo \"$1 is '$billet_jdk', where no java runs; a build tool reading it " +
		"would trust it instead of installing one\" >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("}\n")

	// AND THE JDKs, whose variables are what setup-java reads. A JAVA_HOME naming
	// a directory the build never created is worse than an absent one, because
	// every build tool downstream trusts it instead of installing a JDK.
	for _, v := range ts.Java.Versions {
		if !javaVersion.MatchString(v) {
			return fmt.Errorf("ec2: the declaration asks for java %q, which is not a version "+
				"this build will look for; it becomes part of a variable name and an apt "+
				"package name, and neither tolerates a glob", v)
		}

		// THE ENTRY setup-java RESOLVES FOR A RANGE, which is a different lookup
		// path from the variable below and was the one this gate left open.
		opts.record("Java_Temurin-Hotspot_jdk " + v)

		b.WriteString("billet_tc Java_Temurin-Hotspot_jdk '" + v + "' bin/java -version\n")

		// THE VALUE, AND IT RUNS. Checking only that the KEY is present accepts a
		// variable pointing at nothing, which is worse than an absent one.
		opts.record("JAVA_HOME_" + v + "_X64")

		b.WriteString("billet_env_java JAVA_HOME_" + v + "_X64\n")
	}

	// THE DEFAULT JAVA_HOME NAMES SOMETHING THAT EXISTS. Read out of the file
	// rather than assumed, because the file is what the job will read.
	opts.record("JAVA_HOME")

	b.WriteString("billet_env_java JAVA_HOME\n")

	return nil
}

// toolcacheBinary and toolcacheVersionFlag are how the gate proves an entry is a
// runtime rather than a directory.
//
// AN EMPTY ANSWER MEANS "billet does not install this", and the caller treats it
// as such rather than interpolating it. The declaration names tools billet has
// not baked yet; emitting `"$entry/x64/" --version` for one would fail every
// entry and refuse every build.
//
// THE PATHS ARE THE ACTIONS' OWN. `setup-node` adds `<entry>/bin` to PATH, and
// `setup-go` and `setup-python` do the same -- so `bin/<tool>` is exactly what a
// job will execute, and running it here is the same question asked earlier.
//
// python's interpreter is `bin/python`, which the installer creates as a symlink
// beside the versioned one: the tarball ships only `python3.13` and friends, so
// an entry without that link is one every `python` in a workflow misses.
func toolcacheBinary(tool string) string {
	switch tool {
	case "node":
		return "bin/node"
	case "go":
		return "bin/go"
	case "Python":
		return "bin/python"
	case "PyPy":
		// THE SYMLINK, NOT `pypy3`. The tarball ships only `pypy3`, and the
		// install adds `python3` and `python` beside it because that is what a
		// workflow runs. Checking the one a job would use is the point.
		return "bin/python"
	case "Ruby":
		return "bin/ruby"
	case "CodeQL":
		// THE BUNDLE'S OWN LAUNCHER, one level down: the archive unpacks a
		// `codeql/` directory into the entry rather than a bare bin/.
		return "codeql/codeql"
	}

	return ""
}

// toolcacheInnerMarker is a file the entry itself must carry, rendered as a
// trailing argument, or "" for a tool that needs none.
//
// SEPARATE FROM x64.complete, WHICH IS A SIBLING. The two markers are stat'ed by
// different code -- @actions/tool-cache looks beside the entry, codeql's action
// looks inside it -- so an entry can satisfy one and not the other, and the
// failure that produces is silent: every check passes and every job pays a
// download the image exists to avoid.
func toolcacheInnerMarker(tool string) string {
	if tool == "CodeQL" {
		return " pinned-version"
	}

	return ""
}

func toolcacheVersionFlag(tool string) string {
	switch tool {
	case "go":
		return "version"
	case "CodeQL":
		// `codeql version` rather than `--version`, which the launcher does not
		// accept.
		return "version"
	}

	return "--version"
}
