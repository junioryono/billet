package codebuild

import (
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/runnerrelease"
)

// runnerDir is where the buildspec leaves the Actions runner, and it is what
// config.Tier.RunnerCommandFor's `./run.sh` default resolves against.
//
// A CODEBUILD CURATED IMAGE SHIPS NO ACTIONS RUNNER. CodeBuild's own runner feature
// installs one during DOWNLOAD_SOURCE, and billet does not use that feature — see
// docs/adr-007-codebuild-provider.md — so either the image brought one or this
// buildspec fetches it.
// runnerImageDir is where a CUSTOM IMAGE is expected to ship a runner, and it is
// only ever read.
//
// An operator who builds their own image — or points a reserved fleet at a custom AMI
// — puts one here and pays no download per job.
const runnerImageDir = "/opt/billet/actions-runner"

// runnerDownloadDir is where billet puts a runner it fetches, and it is under $HOME
// BECAUSE THE BUILD USER IS NOT ALWAYS ROOT.
//
// MEASURED, and it cost a macOS acceptance run. A LINUX_CONTAINER build runs as root,
// so writing to /opt worked and nothing suggested otherwise; a MAC_ARM build runs as
// `cbuser` and the same script died on `mkdir: /opt/billet: Permission denied`, then
// on a curl that could not write, then on a checksum of a file that did not exist.
// The build FAILED rather than silently passing — the `test -x` gate did its job — but
// no macOS job could ever have run.
//
// $HOME IS THE ONE PATH BOTH AGREE ON: /root under a root container, /Users/cbuser on
// a managed Mac, and writable by definition in both.
//
// IT IS RELATIVE TO HOME'S RESOLVED PATH rather than to the spelling billet was handed
// — the script sets BILLET_HOME from `cd "$HOME" && pwd -P` first — so nothing above
// `.billet` can still be a symlink when the directory is recursively removed.
const runnerDownloadDir = "$BILLET_HOME/.billet/actions-runner"

// buildspecPhase is the phase billet's commands run in.
//
// PRE_BUILD RATHER THAN BUILD, and the distinction is not cosmetic: whatever runs
// the runner holds the build for the whole job, so putting the fetch in the same
// phase would make an image-provided runner and a fetched one take different
// amounts of the build's time budget for no reason. The runner itself runs in BUILD.
const buildspecPhase = "pre_build"

// Buildspec is the document billet sends as buildspecOverride.
//
// BILLET GENERATES IT, for the reason ADR-002 gives about not shipping a Packer
// template: the contract between billet and its runner is a few lines of shell, and
// a project-owned buildspec is a second place for them to disagree. Terraform
// creates the project with a minimal placeholder so CreateProject validates.
//
// IT NEVER MENTIONS THE REGISTRATION. CodeBuild resolves the PARAMETER_STORE
// variable into the environment before any phase runs, so the runner reads it from
// there and no command here has it to leak. Nothing below refers to jitEnvVar.
//
// AND IT IS EXECUTED IN A TEST rather than pattern-matched. That is the ec2
// boot-script lesson: its first version carried the registration in a quoted
// heredoc inside `$( )`, which reads safer than a plain assignment and is not — a
// single quote in the value confused the shell scanning for the closing paren and
// /bin/sh died with "unexpected EOF" on a later line. A boot script that fails to
// parse is compute that starts, registers nothing, and reports success.
func (p *Provider) Buildspec(spec Spec) (string, error) {
	command, err := shellCommand(spec.Command)
	if err != nil {
		return "", err
	}

	platform, err := runnerPlatform(p.cfg.EnvironmentType)
	if err != nil {
		return "", err
	}

	// UNREACHABLE TODAY, AND DELIBERATELY KEPT.
	//
	// runnerPlatform above refuses an environment billet does not recognise, and
	// every environment it DOES resolve has a pinned checksum — which
	// TestAnUnknownEnvironmentHasNoRunnerPlatform asserts as a property over the
	// whole accepted set rather than case by case. So no input reaches this branch,
	// and a mutation that removes it survives; that survival is redundancy rather
	// than a gap, and it is said out loud here because the alternative is somebody
	// deleting the guard on the strength of the same observation.
	//
	// What it catches is the next environment added to runnerPlatform without a
	// checksum added beside it. Without it the download is handed an EMPTY checksum —
	// which BOTH tools reject as a malformed digest rather than accepting, so the build
	// fails rather than installing something unverified. That makes this a DIAGNOSTIC
	// guard rather than a security one, and saying so matters: the comment here used to
	// claim an empty checksum "passes against anything", which is not what the tools do,
	// and a stated reason that is untrue is worse than none.
	sum, ok := runnerrelease.PinnedSHA256For(platform)
	if !ok {
		return "", fmt.Errorf("codebuild: billet pins no actions/runner checksum for %s, so a "+
			"build on environment_type %s could not verify the runner it downloaded",
			platform, p.cfg.EnvironmentType)
	}

	// EVERY VALUE IS SINGLE-QUOTED, which is the one shell construct with no parsing
	// left inside it. shellQuote refuses a value containing a single quote rather
	// than escaping it, because an escape is a second thing to get right and none of
	// these values has any business containing one.
	assignments := []struct{ name, value string }{
		{"BILLET_RUNNER_IMAGE_DIR", runnerImageDir},
		{"BILLET_RUNNER_VERSION", runnerrelease.Pinned()},
		{"BILLET_RUNNER_SHA256", sum},
		{"BILLET_RUNNER_PLATFORM", platform},
	}

	var b strings.Builder

	b.WriteString("version: 0.2\n")
	b.WriteString("phases:\n")
	b.WriteString("  " + buildspecPhase + ":\n")
	b.WriteString("    commands:\n")

	for _, a := range assignments {
		quoted, err := shellQuote(a.value)
		if err != nil {
			return "", err
		}

		fmt.Fprintf(&b, "      - %s\n", yamlScalar(a.name+"="+quoted))
	}

	for _, line := range fetchRunnerScript() {
		fmt.Fprintf(&b, "      - %s\n", yamlScalar(line))
	}

	b.WriteString("  build:\n")
	b.WriteString("    commands:\n")
	// THE BUILD PHASE IS ONE GUARDED AND-LIST, and the `test -n` at the front of it is
	// there because the previous version's reasoning was wrong.
	//
	// `BILLET_RUNNER_DIR` is exported in pre_build and read here, which depends on the
	// two phases sharing a shell — a real Linux job confirms they do. The comment that
	// used to sit here argued that losing it would still fail closed, because a bare
	// `cd ""` exits ZERO (measured under dash in ubuntu:24.04 — it is a no-op, not a
	// refusal) and the runner command would then exit 127. THE SECOND HALF IS ONLY TRUE
	// FOR `./run.sh`. The tier command is operator-configured, so it can be an absolute
	// path, or any name resolvable from whatever directory the build started in — and
	// then the phase runs something that is not this job's runner and exits zero, which
	// is the worst outcome this file has. The pre_build `test -x` cannot cover it
	// either: it runs before the phase boundary, while the variable is still set.
	//
	// `test -n` makes the loss itself the failure, ahead of any command, under either
	// of CodeBuild's per-command regimes.
	build := `test -n "$BILLET_RUNNER_DIR" && cd "$BILLET_RUNNER_DIR"`

	// THE RUNNER REFUSES TO START AS ROOT, AND A CODEBUILD CONTAINER IS ROOT.
	//
	// MEASURED, and it is the whole reason a real job was needed. GitHub's `run.sh`
	// carries a guard that exits before doing anything when it is invoked by root
	// without this variable — the build downloads the runner, VERIFIES its checksum,
	// prints `Must not run interactively with sudo` and `Exiting runner...`, and then
	// CodeBuild reports the build SUCCEEDED because the script exited zero. Compute
	// that starts, registers nothing and reports success is the exact failure the ec2
	// boot script's own comment warns about, one backend over.
	//
	// NO TEST COULD HAVE CAUGHT IT. The buildspec suite executes the generated script
	// under /bin/sh, which is the right instrument and why two other defects died
	// there — but it substitutes a STAND-IN runner, and a stand-in has no root guard.
	// The guard belongs to the binary billet does not own.
	//
	// SET UNCONDITIONALLY rather than only for a container environment. It is inert
	// where the build does not run as root (a MAC_ARM build runs as `cbuser`), the
	// alternative is a second rule keyed on an environment enum that would have to
	// stay right as AWS adds environments, and a variable that does nothing is
	// cheaper than a launch that silently registers nothing.
	build += ` && export RUNNER_ALLOW_RUNASROOT=1 && ` + command

	fmt.Fprintf(&b, "      - %s\n", yamlScalar(build))

	return b.String(), nil
}

// refuseRootHome is the shell that refuses a HOME resolving to the root directory.
//
// IT IS A NAMED CONSTANT SO A TEST CAN RUN IT AGAINST VALUES `pwd -P` WILL NOT PRODUCE
// ON THE MACHINE THE TEST RUNS ON. That is not tidiness: the whole reason this check is
// a pattern rather than a comparison against "/" is a platform split, and a test that
// reaches it only through `pwd -P` can only ever see its own platform's half. MEASURED:
// `cd // && pwd -P` answers `/` under dash on ubuntu:24.04 and `//` under /bin/sh on
// macOS, because POSIX leaves a pathname beginning with exactly two slashes
// implementation-defined. So on Linux the old `= "/"` comparison passes every case a
// script-level test can construct, and the regression it exists to catch is invisible
// there — which is a test that agrees with the bug on the platform CI runs.
//
// The pattern asks the general question, "is there any character here that is not a
// slash", so it holds for `/`, `//`, `////` and any spelling a shell this code has not
// been run under might produce.
const refuseRootHome = `case "$BILLET_HOME" in *[!/]*) ;; *) ` +
	`echo "billet: HOME is the root directory, which billet will not treat as a ` +
	`build user's home" >&2; exit 1;; esac`

// fetchRunnerScript is the shell that puts a verified Actions runner at
// BILLET_RUNNER_DIR.
//
// A PREINSTALLED RUNNER WINS, so an operator who builds their own image — or points
// a reserved fleet at a custom AMI — pays no download per job. The fetch is the
// fallback for a curated image, which ships none.
//
// THE CHECKSUM IS VERIFIED AND A MISMATCH FAILS THE BUILD. A runner tarball is
// fetched over the network into a machine that is about to hold a GitHub
// registration; taking whatever arrives is how a compromised mirror becomes a
// runner. `set -e` is not relied on for it — every step is chained explicitly,
// because a buildspec command list is run by CodeBuild rather than by a script
// billet controls, and `!` in front of a pipeline makes `set -e` a no-op anyway
// (the rule CLAUDE.md states for billet's own shell gates).
func fetchRunnerScript() []string {
	// THE CHECKSUM CHECK HAS TWO SPELLINGS BECAUSE THE PLATFORMS DISAGREE, and the
	// gate must not be the thing that gets skipped: a Linux image has sha256sum, a
	// macOS build has `shasum -a 256` and no sha256sum at all. A gate that silently
	// does nothing on one platform is the failure CLAUDE.md records for `command -v`
	// under a privilege drop — it passed every local run and would have failed every
	// real one. No tool at all is a refusal rather than an unverified install.
	// PRINTF RATHER THAN ECHO, because POSIX leaves `echo`'s handling of backslashes
	// IMPLEMENTATION-DEFINED and the two platforms this backend runs on need not agree.
	// The line being built contains a PATHNAME, so a backslash anywhere in it could be
	// interpreted rather than passed through, and the tool would then verify a different
	// pathname from the one `tar` extracts. `printf` interprets nothing in its operands.
	verify := `if command -v sha256sum >/dev/null 2>&1; then ` +
		`printf '%s  %s\n' "$BILLET_RUNNER_SHA256" "$tarpath" | sha256sum -c -; ` +
		`elif command -v shasum >/dev/null 2>&1; then ` +
		`printf '%s  %s\n' "$BILLET_RUNNER_SHA256" "$tarpath" | shasum -a 256 -c -; ` +
		`else echo "billet: no sha256 tool, refusing to install an unverified runner" >&2; ` +
		`exit 1; fi`

	// ONE if/else WITH NO EARLY EXIT, and that is not a style choice.
	//
	// The first version ended the already-installed branch with `exit 0`, which made
	// correctness depend on whether CodeBuild runs each phase in its own shell — if
	// it does not, that `exit 0` skips the BUILD phase and the runner never starts,
	// on exactly the images that already ship a runner. billet has not measured
	// which it is, and the rule about somebody else's behaviour is to pin it or not
	// depend on it. An if/else depends on neither, and the harness that runs both
	// phases in one shell is what found it.
	// WHERE THE RUNNER ENDS UP IS DECIDED HERE, not by a constant, because the two
	// candidates have different owners: the image's copy is read-only and may not
	// exist, and the download target has to be writable by whoever the build runs as.
	// BILLET_RUNNER_DIR is exported so the BUILD phase can cd into whichever won —
	// which relies on a variable surviving from pre_build into build, a dependency
	// this script already had and which a real Linux job confirmed.
	install := `if [ -x "$BILLET_RUNNER_IMAGE_DIR/run.sh" ]; then ` +
		`echo "billet: using the runner already in this image"; ` +
		`export BILLET_RUNNER_DIR="$BILLET_RUNNER_IMAGE_DIR"; ` +
		`else ` +
		// THE FETCH IS `&&`-CHAINED, AND `;` HERE WAS A SECURITY HOLE.
		//
		// MEASURED under dash in ubuntu:24.04, which is the builder rather than this
		// Mac: with the steps separated by `;` and no `set -e`, a FAILING
		// `sha256sum -c -` prints `FAILED`, execution continues into `tar -xzf`, the
		// tarball extracts, and the whole command exits ZERO. The `test -x` gate that
		// follows then passes, because a tarball that extracted has a run.sh. So a
		// mirror serving a runner billet did not pin was installed and executed while
		// every stated protection read as present — and the comment above claimed the
		// chain did not depend on `set -e` while it depended on nothing else.
		//
		// An `&&` chain makes the claim true: any step failing short-circuits the rest
		// and leaves the else-branch — and therefore the `if`, and therefore the
		// buildspec command — non-zero, whether or not CodeBuild sets `-e`. Which of
		// those CodeBuild does is exactly the kind of somebody-else's-behaviour this
		// repo's rule says to pin or not depend on, and this now depends on neither.
		//
		// ONE LINK IS REDUNDANT AND IT IS SAID OUT LOUD rather than left for somebody
		// to rediscover and delete the wrong half of: mutating the `&&` after `curl`
		// back to a `;` SURVIVES, because a download that failed leaves no tarball and
		// the checksum step then refuses a file it cannot read. It is kept because the
		// chain's value is being uniform — a reader should not have to work out which
		// links are load-bearing — and the cost is nothing.
		// $HOME IS PROVED BEFORE ANYTHING IS DELETED UNDER IT, and "not empty" was not
		// that proof — the first version of this guard said it was, which is the
		// deriving-permission-from-what-is-not-broken mistake this repository has now
		// made in four places.
		//
		// EMPTY collapses the path to `/.billet/actions-runner`: a root container
		// CREATES that and works, so the mistake ships and first surfaces on a non-root
		// environment as a filesystem error naming a path no operator recognises.
		// RELATIVE resolves against whatever CodeBuild's working directory happens to
		// be, which is not a place billet has any claim on. And a SYMLINKED `.billet`
		// left by an earlier build on a reserved host — which is not scrubbed, measured
		// — redirects the recursive delete below to whatever it points at.
		//
		// That last one is bounded by this backend refusing untrusted work outright, so
		// it takes a trusted build to plant it and the blast radius stays inside the
		// documented reserved-host boundary. It is refused anyway, because the guard is
		// one `[ -L ]` test and the alternative is a `rm -rf` whose target somebody else
		// chose.
		//
		// `exit 1` rather than a status in the chain, because each of these has
		// something to say and a bare non-zero says none of it.
		//
		// THE EMPTY CHECK IS REDUNDANT WITH THE CANONICAL ONE AND IS KEPT ANYWAY, said
		// out loud so nobody rediscovers it and deletes the wrong one. What it buys is
		// the diagnostic: "HOME is unset" sends an operator to the build environment,
		// where a canonicalisation failure sends them to look at a value that is not
		// there.
		`if [ -z "$HOME" ]; then ` +
		`echo "billet: HOME is unset, so there is no directory this build user can be ` +
		`relied on to write the Actions runner into" >&2; exit 1; fi; ` +
		// BILLET WORKS FROM HOME'S RESOLVED PATH RATHER THAN REQUIRING HOME TO BE ONE.
		//
		// This is the third shape of this guard and the first that is not wrong in one
		// direction or the other, so both mistakes are worth keeping.
		//
		// LEXICAL RULES ADMITTED THE NEXT SPELLING SOMEBODY THOUGHT OF. "Starts with /"
		// plus "no .. component" let through `/.`, `/./`, `//` and a trailing slash —
		// the last of which ALSO defeated the symlink check, since `[ -L "/home/" ]`
		// resolves the slash and asks about the target rather than the link. Symlinked
		// ANCESTORS were never looked at. Each is a spelling of a path billet had not
		// vouched for, ending in `rm -rf`.
		//
		// THEN REQUIRING `pwd -P` TO EQUAL THE LITERAL FIXED THAT AND BROKE SOMETHING
		// ELSE: a custom image whose HOME is deliberately a symlink — an ordinary way
		// to lay out a build image — was refused, and the refusal is a build that never
		// starts a runner. A gate that rejects correct input is the failure ADR-005
		// names, because the next thing anybody does is delete the gate.
		//
		// RESOLVING INSTEAD OF COMPARING gives the same safety envelope with no false
		// refusal. `cd "$HOME" && pwd -P` yields a path with every symlink and every
		// lexical alias already gone, and billet builds the runner directory FROM that
		// rather than from the spelling it was handed. The delete then cannot escape
		// `<resolved home>/.billet/`: nothing above `.billet` is a link any more, the
		// `.billet` component is checked below, and a symlink at the final
		// `actions-runner` component is UNLINKED rather than traversed (measured). What
		// an operator gets is "billet works inside the directory your build user
		// actually lives in", which is what HOME means.
		//
		// It runs in a SUBSHELL so the build's own working directory is untouched, and
		// a HOME that cannot be entered is refused rather than assumed: billet cannot
		// resolve what it cannot reach, and "could not tell" is not "safe to delete
		// under".
		//
		// BILLET_HOME rather than a lowercase name, because it is a plain shell variable
		// in a script an operator's tier command later runs in, and billet should
		// collide only inside its own namespace. It is NOT exported — BILLET_RUNNER_DIR
		// is what the build phase needs and what carries across, and this holds only the
		// value that derived it.
		// AN ABSOLUTE HOME IS REQUIRED BEFORE ANY OF THIS RESOLVES, and leaving it out
		// was a real hole rather than a missing nicety. `cd "$HOME" && pwd -P` happily
		// accepts a RELATIVE HOME that names an existing directory — it resolves against
		// whatever working directory CodeBuild happens to have given the phase, which is
		// not somewhere billet has any claim on, and billet then recursively deletes and
		// installs beneath it. The test that was supposed to cover this passed for the
		// wrong reason: it used a relative path that did not exist, so `cd` failed and
		// the refusal came from the resolution rather than from any rule about
		// relativity.
		`case "$HOME" in /*) ;; *) ` +
		`echo "billet: HOME is not an absolute path, so the directory billet would ` +
		`write the Actions runner into depends on where this build happens to be ` +
		`running from" >&2; exit 1;; esac; ` +
		// THE NEWLINE IS MATCHED WITH SHELL BUILTINS, NOT `wc`, and the first version
		// used `wc` and was wrong in the way this repository keeps having to fix.
		// MEASURED under both /bin/sh on macOS and dash on ubuntu:24.04: with `wc`
		// absent the command substitution is empty, `[ "" -ne 0 ]` is an ERROR rather
		// than a comparison, the `if` reads that non-zero status as false, and the guard
		// SILENTLY DOES NOT REFUSE. That is could-not-tell collapsing into no, in front
		// of a recursive delete.
		//
		// `BILLET_NL` is a literal newline built without one appearing in this file —
		// the buildspec is YAML and a real newline inside a command scalar would end the
		// command. `$( )` strips trailing newlines, so the `x` is there to survive that
		// and is then trimmed off.
		`BILLET_NL=$(printf '\nx'); BILLET_NL=${BILLET_NL%x}; ` +
		// THIS ONE IS REDUNDANT GIVEN THE SENTINEL BELOW AND IS KEPT ANYWAY, said out
		// loud because the mutation survives and the next reader deserves to know which
		// half to leave alone. With `pwd -P` running behind a sentinel the RESOLVED
		// check catches every newline this can, so removing this line alone changes no
		// test. It is the other direction that makes the pair worth having: removing the
		// SENTINEL is caught by this check for a newline in $HOME's own spelling, and by
		// nothing else for one a symlink target brings in. Two cheap `case` patterns
		// covering each other's regression is the right trade in front of a `rm -rf`.
		`case "$HOME" in *"$BILLET_NL"*) ` +
		`echo "billet: HOME contains a newline, which billet will not resolve into a ` +
		`recursive delete" >&2; exit 1;; esac; ` +
		// THE SENTINEL IS WHAT MAKES THE RESOLVED PATH FAITHFUL, and without it the
		// newline check below could be walked straight past.
		//
		// MEASURED in ubuntu:24.04. A CLEAN $HOME symlink can point at a directory whose
		// physical name ENDS in a newline. `$( )` strips every trailing newline — both
		// `pwd`'s own and the one belonging to the pathname — so BILLET_HOME came out a
		// DIFFERENT, newline-free path, which passed every check and is what would have
		// been recursively deleted. In the probe `…/target<newline>` resolved to
		// `…/target`, a directory that existed and held a canary. The guard on $HOME
		// cannot see this because $HOME is a clean name, and the guard on the resolved
		// value cannot see it because the byte is gone by then.
		//
		// `printf x` after `pwd -P` gives the output a non-newline last byte, so nothing
		// of the pathname is stripped. Removing the sentinel and then exactly ONE
		// trailing newline — `pwd`'s own record separator — leaves any newline belonging
		// to the NAME in place for the check to find.
		//
		// THE `%x` CANNOT EAT A PATH'S OWN TRAILING `x`, which is the first thing this
		// looks like it might do. `pwd`'s newline always sits between the pathname and
		// the sentinel, so the two trims take the sentinel and that separator and stop.
		// MEASURED on both /bin/sh (macOS) and dash (ubuntu:24.04): a directory named
		// `…/max` resolves to `…/max`, one named exactly `x` resolves to `…/x`, two
		// trailing newlines are refused, `/tmp` is unchanged, and a resolution that
		// somehow produced NOTHING leaves BILLET_HOME empty — which the all-slashes
		// check below then refuses, so even that degenerate case is fail-closed.
		`BILLET_HOME=$(cd "$HOME" 2>/dev/null && pwd -P && printf x) || { ` +
		`echo "billet: HOME cannot be entered, so billet cannot resolve the directory ` +
		`it would write the Actions runner into" >&2; exit 1; }; ` +
		`BILLET_HOME=${BILLET_HOME%x}; BILLET_HOME=${BILLET_HOME%"$BILLET_NL"}; ` +
		// THE ROOT SURVIVES RESOLUTION, so it is the one alias still to be named — and
		// COMPARING AGAINST "/" IS NOT ENOUGH, which was MEASURED rather than reasoned
		// about and is a platform split of exactly the kind this repo keeps finding.
		//
		// POSIX leaves a pathname beginning with EXACTLY TWO slashes implementation
		// defined, and the two platforms this backend runs on disagree: `cd // &&
		// pwd -P` answers `/` under dash on ubuntu:24.04 and `//` under /bin/sh on
		// macOS. So a `= "/"` test passes `//` straight through on the MAC_ARM half,
		// which is the half with no owned-hardware fallback.
		//
		// The pattern asks the general question instead — "is there any character here
		// that is not a slash" — so it holds for `/`, `//`, and any spelling a shell
		// this code has not been run under might produce.
		refuseRootHome + `; ` +
		// THE .billet COMPONENT IS THE ONE RESOLUTION DOES NOT COVER, because it is a
		// path UNDER home rather than home itself. Which position matters was MEASURED
		// in ubuntu:24.04:
		//
		//   <home>/.billet/actions-runner a symlink -> `rm -rf` UNLINKS IT and the
		//   target is untouched. A check there defends against nothing.
		//
		//   <home>/.billet a symlink -> everything goes THROUGH it. This is that check,
		//   and it is the ONLY ONE LEFT: an earlier version tested two positions and
		//   this comment described two, which was true then and is not now.
		// A BACKSLASH IN THE RESOLVED PATH IS REFUSED, AND THE REASON IS THE CHECKSUM
		// TOOL RATHER THAN `echo` — which matters, because the `echo` argument stopped
		// being true when this switched to `printf`, and a rule whose stated reason has
		// expired is one somebody deletes.
		//
		// MEASURED in ubuntu:24.04: GNU coreutils ESCAPES a path containing a backslash,
		// emitting the line with a leading `\` and the byte doubled. Fed an UNESCAPED
		// one — which is exactly what `printf '%s  %s\n'` produces — it answers
		// `sha256sum: 'standard input': no properly formatted checksum lines found`. So a
		// backslash anywhere in the runner directory makes the VERIFICATION fail, and
		// refusing here turns that into a sentence naming HOME rather than a checksum
		// error naming nothing an operator configured.
		`case "$BILLET_HOME" in *\\*) ` +
		`echo "billet: HOME contains a backslash, which billet will not carry through ` +
		`a shell into a recursive delete" >&2; exit 1;; esac; ` +
		`case "$BILLET_HOME" in *"$BILLET_NL"*) ` +
		`echo "billet: HOME contains a newline, so the path billet resolved is not the ` +
		`one it would delete under" >&2; exit 1;; esac; ` +
		`if [ -L "$BILLET_HOME/.billet" ]; then ` +
		`echo "billet: the .billet directory under HOME is a symlink; billet will not ` +
		`recursively delete through one it did not create" >&2; exit 1; fi; ` +
		`export BILLET_RUNNER_DIR="` + runnerDownloadDir + `" && ` +
		// THE DOWNLOAD DIRECTORY IS EMPTIED FIRST, BECAUSE A RESERVED HOST IS NOT
		// SCRUBBED BETWEEN BUILDS. billet MEASURED that rather than assuming it: a
		// marker written by one build was read intact by the next, same user. So a
		// previous build's runner is still at this exact path.
		//
		// WHAT THAT MATTERS FOR IS THE SUCCESSFUL FETCH, not the failed one — the `&&`
		// chain and the joined gate already stop a failure, and this comment used to
		// claim the cleanup was what kept a failed fetch failed. It is not: mutating it
		// away leaves every failure path still failing. What only this can do is keep a
		// previous build's files from being MIXED INTO a successful extraction, since
		// untarring over a directory replaces the files the archive carries and leaves
		// every other one where it was.
		//
		// WHAT MAKES THE DELETE SAFE IS THE GUARD BLOCK ABOVE, not this line: HOME is
		// non-empty, absolute, enterable, RESOLVED (nothing above `.billet` is still a
		// link), free of newlines and backslashes, not the root, and `.billet` under it
		// is not a symlink — and this is the download branch, so
		// BILLET_RUNNER_DIR is billet's own directory and never the image's read-only
		// copy. `--` because a path is data and this one is built from an environment
		// variable.
		`rm -rf -- "$BILLET_RUNNER_DIR" && ` +
		`mkdir -p "$BILLET_RUNNER_DIR" && ` +
		`tarball="actions-runner-$BILLET_RUNNER_PLATFORM-$BILLET_RUNNER_VERSION.tar.gz" && ` +
		`tarpath="$BILLET_RUNNER_DIR/$tarball" && ` +
		`curl -fsSL --retry 3 -o "$tarpath" ` +
		`"https://github.com/actions/runner/releases/download/v$BILLET_RUNNER_VERSION/$tarball" && ` +
		// THE SEPARATOR AFTER `fi` IS LOAD-BEARING. Without it the next word is
		// parsed as part of the `if` and /bin/sh dies with `syntax error near
		// unexpected token`. That was the first version, and it is precisely the
		// failure a pattern-matching test cannot see: the document is valid YAML, the
		// string contains everything one would grep for, and the build starts and
		// registers nothing.
		verify + ` && ` +
		`tar -xzf "$tarpath" -C "$BILLET_RUNNER_DIR" && ` +
		`rm -f "$tarpath"; ` +
		`fi`

	// THE GATE IS `&&`-JOINED TO THE INSTALL RATHER THAN FOLLOWING IT, and as a
	// separate command it could ANSWER FOR a failed install instead of gating it.
	//
	// It still gates both branches, which is why it sits outside the if/else: an image
	// whose run.sh has gone and a fetch that unpacked something unexpected are the same
	// failure from here. What changed is what happens when the install itself fails.
	// As its own buildspec command, with no `set -e`, it RAN ANYWAY — so a `rm -rf`
	// that failed while leaving the previous build's runner in place (an unremovable
	// file, a full or read-only filesystem) short-circuited the chain, and then this
	// test found the stale executable and returned zero, making the whole thing exit
	// zero and handing the build phase a runner this job never verified. That is the
	// exact outcome the `rm -rf` was added to prevent, arriving through the gate.
	//
	// `&&` makes the install's failure the compound's failure, under either of
	// CodeBuild's possible per-command regimes — the same standard as the chain above.
	return []string{
		install + ` && test -x "$BILLET_RUNNER_DIR/run.sh"`,
	}
}

// runnerPlatform maps a CodeBuild environment to the actions/runner asset name.
//
// NOT ONE OF THEM IS DERIVABLE FROM ANOTHER, which is the same trap the toolcache
// installers record: every vendor spells an architecture differently. actions/runner
// uses `linux-x64`, `linux-arm64` and `osx-arm64`, so an environment billet has not
// been taught about is REFUSED rather than defaulted — a default would fetch an x64
// runner onto an arm64 build, where every file is structurally correct and nothing
// fails until a job execs one.
func runnerPlatform(env config.CodeBuildEnvironment) (string, error) {
	switch env {
	case config.CodeBuildLinuxContainer, config.CodeBuildLinuxGPUContainer,
		config.CodeBuildLinuxEC2:
		return "linux-x64", nil

	case config.CodeBuildARMContainer, config.CodeBuildARMEC2:
		return "linux-arm64", nil

	case config.CodeBuildMacARM:
		return "osx-arm64", nil

	default:
		return "", fmt.Errorf("codebuild: environment_type %q names no actions/runner platform "+
			"billet knows, and defaulting would install a runner for the wrong architecture — "+
			"every file structurally correct and nothing failing until a job execs one", env)
	}
}

// yamlScalar renders one command as a YAML scalar that cannot be reinterpreted.
//
// DOUBLE-QUOTED WITH ESCAPES, because a buildspec is YAML and a shell command is
// full of characters YAML gives meaning to: a leading `-`, a `:` followed by a
// space, a `#`, a `{`. A plain scalar carrying any of them parses as something else
// or fails to parse at all, and a buildspec that fails to parse is a build that
// starts and registers nothing.
func yamlScalar(command string) string {
	var b strings.Builder

	b.WriteByte('"')

	for _, r := range command {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}

	b.WriteByte('"')

	return b.String()
}

// shellQuote wraps a value in single quotes, refusing one that contains a single
// quote.
//
// REFUSED RATHER THAN ESCAPED, the rule the ec2 boot script follows: a single-quoted
// assignment is the one construct with no parsing left in it, and an escape is a
// second thing to get right on a path where getting it wrong produces a build that
// starts and registers nothing. Nothing billet puts through here has any business
// containing a quote.
func shellQuote(v string) (string, error) {
	if strings.Contains(v, "'") {
		return "", fmt.Errorf("codebuild: %q contains a single quote, which cannot be carried "+
			"safely in the generated buildspec", v)
	}

	if strings.ContainsAny(v, "\n\r\x00") {
		return "", fmt.Errorf("codebuild: %q contains a newline or NUL, which would end the "+
			"buildspec command it is written into", v)
	}

	return "'" + v + "'", nil
}

// shellCommand renders a tier's argv as one safely quoted command line.
func shellCommand(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errNoCommand
	}

	quoted := make([]string, 0, len(argv))

	for _, arg := range argv {
		q, err := shellQuote(arg)
		if err != nil {
			return "", err
		}

		quoted = append(quoted, q)
	}

	return strings.Join(quoted, " "), nil
}
