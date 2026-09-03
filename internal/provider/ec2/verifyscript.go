package ec2

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/runnerimages"
)

// The report's frame.
//
// A NONCE MINTED PER RUN, AND IT BRACKETS THE BLOCK RATHER THAN SIGNING IT. What
// it buys is that the block billet reads is THIS verification's: a console buffer
// holds 64 KiB of whatever the machine printed, so a fixed marker could be
// satisfied by an image that echoes it, or by a stale read of an earlier run. It
// buys nothing against an image that is lying on purpose — see VerifyImage, where
// that limit is stated rather than papered over.
const (
	reportBegin = "billet-verify-begin"
	reportEnd   = "billet-verify-end"
)

// reportVerdictKey is the one field that decides the outcome; reportStepKey names
// the check that was running when the script stopped.
//
// FROM A CLOSED SET BILLET ITSELF WROTE INTO THE SCRIPT, which is the same rule
// the tart backend follows for its launch status file: whatever a machine prints
// is the machine's to choose, so what billet renders back to an operator is a
// token out of a vocabulary billet emitted, never a line lifted off the console.
const (
	reportVerdictKey = "verdict"
	reportStepKey    = "step"
)

// The two verdicts the script can write. Anything else is a machine saying
// something billet has no word for, and is discarded rather than rendered.
const (
	verdictOK   = "ok"
	verdictFail = "fail"
)

// reportSchema is exactly what billet will read out of a console: the keys the
// script it sent emits, and the step labels that script can report.
//
// BUILT WHILE THE SCRIPT IS WRITTEN, NOT BESIDE IT. A key shape alone is not an
// allowlist — `secret=…` matches it perfectly — and a hand-kept list of names is a
// second reading of what the emitter does, which drifts silently the first time a
// field is added. So every writer below records what it emitted, and this is
// simply what came back.
//
// PASSED BY VALUE AND STILL RECORDED INTO, because the two maps are what the
// struct is: a copy shares them, so a writer given a copy adds to the same sets
// the caller will read. Said out loud because a value receiver that mutates reads
// like a mistake, and the alternative — threading a pointer through five writers
// — buys nothing but a chance to pass a nil one.
//
// A ZERO reportSchema ALLOWS NOTHING, which is the safe direction for the value a
// caller gets from a var declaration or a failed construction. verifyScript's own
// error paths do NOT return a zero one — they return whatever had been recorded
// by then, which for a gate failure is already several keys and labels — and no
// caller may use it, because a schema is only meaningful beside the script it was
// built from. Every caller discards it when generation fails.
type reportSchema struct {
	keys  map[string]struct{}
	steps map[string]struct{}
}

func newReportSchema() reportSchema {
	return reportSchema{
		keys:  map[string]struct{}{},
		steps: map[string]struct{}{},
	}
}

// allows reports whether a report line is one this script could have written.
//
// THE VERDICT AND THE STEP ARE CHECKED BY VALUE, not merely by name, because they
// are the two fields billet acts on and renders. Every other field is data for an
// operator to read, bounded by the key set and the printable-ASCII rule.
func (s reportSchema) allows(key, value string) bool {
	if _, ok := s.keys[key]; !ok {
		return false
	}

	switch key {
	case reportVerdictKey:
		return value == verdictOK || value == verdictFail
	case reportStepKey:
		_, ok := s.steps[value]

		return ok
	}

	return true
}

// verifyStepVar is the shell variable holding that label.
const verifyStepVar = "billet_step"

// verifyRunAsRunner is the shell function every tool is executed through.
//
// UNDER THE JOB'S OWN PRIVILEGE DROP, WHICH IS MOST OF THE POINT OF DOING THIS AT
// ALL. The in-build gate runs each binary as root with the builder's environment
// inherited, and that answers a question no job asks. A job reaches the toolcache
// as the unprivileged runner account under `env -i` carrying only the image's own
// declared variables, so present on disk is not the same as findable by a job: a
// directory only root can traverse, a variable only root's environment carried,
// or a missing supplementary group are each invisible to the build and fatal to a
// workflow.
const verifyRunAsRunner = "billet_as_runner"

// verifyDwellRounds and verifyDwellSeconds are how long the verifier keeps
// re-announcing its answer before it powers off.
//
// IT REPEATS BECAUSE THE CONSOLE LAGS. EC2 posts buffered console output around a
// state transition rather than continuously, and the live read (`Latest`) is
// documented as Nitro-only. Running the procedure by hand while this verifier was
// written, two probes powered off before AWS had flushed and their answers were
// lost — which is the worst shape a check can have: an empty console is
// indistinguishable from a boot that printed nothing, so the failure is
// intermittent and teaches whoever meets it to re-run rather than to look.
//
// MEASURED NOW, ON A REAL BUILD: `Latest=true` was accepted on the c7i.large this
// picks — refusedLatestConsole never fired, so nothing fell back to the buffered
// read — and billet had a complete block 4m40s after RunInstances, terminating the
// verifier immediately. The dwell was nowhere near spent. What that 4m40s does NOT
// separate is boot from gate from console lag: the script runs about twenty-five
// tool invocations through the privilege drop and a `du` over 5.1GiB of toolcache
// before it prints anything, so most of it is work rather than waiting, and the
// console's own latency is still unmeasured.
//
// SO THE VERIFIER OUTLIVES ITS OWN ANSWER. billet terminates it the moment it
// reads a complete block, so the dwell costs nothing on the path where everything
// works; what it buys is that the instance is still RUNNING while billet reads,
// which is the state both console paths can answer from.
//
// AND THE POWEROFF AT THE END IS TWO THINGS AT ONCE.
//
// It is MOST of a leak bound: the verifier launches with
// InstanceInitiatedShutdownBehavior=terminate — the opposite of the builder,
// which has to leave a disk behind to snapshot — so an instance billet loses
// track of between RunInstances and its terminate usually ends itself rather than
// running until somebody notices. It is not ALL of one, and the gap is exactly
// the case this command exists for: an image whose cloud-init is broken, or that
// panics on boot, never reaches this script. That instance is found the same way
// a leaked builder is, by the per-build owner tag; see VerifyImage.
//
// And on the BUFFERED console path the shutdown is expected to be the flush. AWS
// documents that buffer as posted around a state transition rather than
// continuously, so a shape with no live read should answer little while the
// machine runs and more once it stops. That is a reading of the documentation,
// not something billet has measured, which is why the dwell is deliberately
// SHORTER than verifyWindow rather than tuned: whichever path delivers, the
// report lands with minutes of billet's poll still to run.
const (
	verifyDwellRounds  = 24
	verifyDwellSeconds = 20
)

// minVerifiedFreeGiB is what a booted image must have left on its root.
//
// THE CHECK WITHOUT IT PROVED ONLY THAT df ANSWERED. A verifier that reads a
// number and compares it to nothing passes an image with one kilobyte free, which
// then fails every job on ENOSPC — and the standalone `billet ami verify` takes an
// image id from an operator, so the image is not necessarily one this billet just
// sized.
//
// FOUR, AND THE MEASUREMENT IT IS SET AGAINST IS THIS VERIFICATION'S OWN REPORT.
// A live build on 2026-08-28 read, from a machine booted off the AMI it had just
// produced: root_total_kib=29378688, root_used_kib=10785452,
// root_free_kib=18576852 — 28GiB usable, 10.3 used, 17.7 free, with the toolcache
// alone at 5.1GiB. So the floor sits far below what a real image leaves.
//
// AND IT IS NOT THE UNGROWN-FILESYSTEM ARGUMENT, WHICH THAT MEASUREMENT KILLED.
// The first version of this reasoned that cloud-init failing to grow the root onto
// the larger volume leaves Canonical's declared 8GiB holding the same content, and
// that 4 separates the two. It does not: 10.3GiB of content does not fit on an
// 8GiB root at all, so that build fails long before anything boots it. The honest
// statement is narrower — this catches an artifact whose root is nearly full,
// which is a state `billet ami verify` can be handed by an operator and a build
// cannot produce. A number derived from a story the numbers do not support is the
// proxy mistake one line down, so it is corrected rather than kept.
//
// NOT minBuilderFreeGiB, WHICH MEASURES A DIFFERENT THING. That is what the BUILD
// needs before it starts unpacking; this is what the finished artifact must have
// left. Reusing one for the other is the proxy-for-the-property mistake this
// package keeps finding.
const minVerifiedFreeGiB = 4

// verifyNonce is what may be interpolated into the script's marker lines.
//
// CHECKED EVEN THOUGH BILLET MINTS IT. It reaches a script that runs as root on a
// machine billet pays for, and this package has already had one injection through
// a value that "could only be" well-formed: --runner-version reached a URL
// through strconv.Quote, which is GO quoting, and `$(poweroff)` ran.
var verifyNonce = regexp.MustCompile(`^[0-9a-f]{32}$`)

// reportKey is what a report line's key must look like before billet will render
// it. Lowercase, because the toolcache names it is derived from are not.
var reportKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// verifyScript is what a machine booted from the produced AMI runs.
//
// NO `set -x`, AND THAT IS NOT A STYLE CHOICE. EC2 keeps the last 64 KiB of
// console output; a trace of the toolcache gate would push the report out of that
// window and leave billet reading a boot log with no answer in it.
func verifyScript(arch string, contract int, nonce string) (string, reportSchema, error) {
	schema := newReportSchema()

	if arch != "x64" && arch != "arm64" {
		return "", schema, fmt.Errorf("ec2: cannot verify an image for architecture %q, which "+
			"is not x64 or arm64", arch)
	}

	if !verifyNonce.MatchString(nonce) {
		return "", schema, fmt.Errorf("ec2: %q is not a verification nonce", nonce)
	}

	var (
		b     strings.Builder
		ts    runnerimages.Toolset
		tools []toolReport
		err   error
	)

	if contract >= 2 {
		if ts, err = runnerimages.Load(); err != nil {
			return "", schema, fmt.Errorf("ec2: %w", err)
		}

		if tools, err = toolReports(ts); err != nil {
			return "", schema, err
		}
	}

	// ONE WRITER FOR THE STEP LABEL, so a label that reaches the script is the same
	// string that reaches the schema. Two of these — one emitting and one listing —
	// is how a reader ends up refusing a label the script really does write.
	step := func(label string) {
		schema.steps[label] = struct{}{}
		b.WriteString(verifyStepVar + "='" + label + "'\n")
	}

	b.WriteString("#!/bin/sh\nset -eu\n")

	// THE VERDICT IS fail UNTIL THE LAST LINE SETS IT, so every way out of this
	// script that is not "reached the end" reports a failure. `set -e` plus an EXIT
	// trap is what makes that true of an assertion that aborts as well as one that
	// returns; checking each command's status instead is a list that has to stay
	// complete, and this script's whole job is to be complete.
	b.WriteString("billet_verdict=" + verdictFail + "\n")

	writeVerifyReport(&b, schema, nonce, tools)

	step("start")

	b.WriteString("trap billet_dwell EXIT\n")

	writeRunAsRunner(&b)

	// FREE SPACE, WHICH ONLY A BOOTED MACHINE CAN ANSWER. cloud-init's growpart and
	// resizefs run in the `init` stage of a FIRST boot, so what the builder measured
	// says nothing about what an instance launched from the image is given.
	step("disk")
	b.WriteString("billet_root_total_kib=$(df -Pk / | awk 'NR == 2 { print $2 }')\n")
	b.WriteString("billet_root_used_kib=$(df -Pk / | awk 'NR == 2 { print $3 }')\n")
	b.WriteString("billet_root_free_kib=$(df -Pk / | awk 'NR == 2 { print $4 }')\n")

	// A VALUE THAT IS NOT A NUMBER MUST FAIL, NOT PASS — the same rule, and the same
	// measured reason, as the builder's own free-space gate. `[ "$x" -lt N ]` exits
	// 2 on garbage, and a `case` in front of it is what stops that being read as
	// "not less than".
	b.WriteString("case \"${billet_root_free_kib:-}\" in\n")
	b.WriteString("  ''|*[!0-9]*)\n")
	b.WriteString("    echo \"df reported '${billet_root_free_kib:-}' free on this image's " +
		"root, which is not a number\" >&2\n")
	b.WriteString("    exit 1 ;;\n")
	b.WriteString("esac\n")

	// AND THE NUMBER IS COMPARED TO SOMETHING. See minVerifiedFreeGiB: a check that
	// reads a value and compares it to nothing proves that df ran.
	b.WriteString("if [ \"$billet_root_free_kib\" -lt " +
		strconv.Itoa(minVerifiedFreeGiB*1024*1024) + " ]; then\n")
	b.WriteString("  echo \"this image has ${billet_root_free_kib}KiB free on its root and a " +
		"job needs at least " + strconv.Itoa(minVerifiedFreeGiB) + "GiB; the filesystem was " +
		"probably never grown onto the volume (growpart, resizefs)\" >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")

	// THE RUNNER EXECUTES, THROUGH THE PATH A JOB TAKES. A missing libicu, a file
	// the snapshot lost, or a permission the builder had and the runner account does
	// not all land here rather than on somebody's first job.
	//
	// ASSERTED SEPARATELY FROM WHAT IT PRINTED, because a pipeline's status is the
	// LAST command's: `v=$(cmd | head -1)` reports head's success whatever cmd did,
	// so the assertion has to be the bare invocation.
	step("runner")
	b.WriteString(verifyRunAsRunner +
		" /opt/actions-runner/bin/Runner.Listener --version >/dev/null\n")
	b.WriteString("billet_runner=$(" + verifyRunAsRunner +
		" /opt/actions-runner/bin/Runner.Listener --version 2>&1 | head -1)\n")

	if contract >= 1 {
		writeVerifyDocker(&b, step)
	}

	if contract >= 2 {
		step("toolcache")
		b.WriteString("billet_toolcache_kib=$(du -sk " + toolcacheDir +
			" 2>/dev/null | awk '{ print $1 }')\n")

		// THE SAME GATE THE BUILD RUNS, WITH THE TWO DIFFERENCES THAT MATTER: every
		// tool is executed through the privilege drop, and each check names itself
		// first so a failure can be reported as the declared line it was about.
		if err := writeToolcacheGate(&b, ts, arch, gateOptions{
			RunPrefix: verifyRunAsRunner + " ",
			StepVar:   verifyStepVar,
			Record:    func(label string) { schema.steps[label] = struct{}{} },
		}); err != nil {
			return "", schema, err
		}

		writeVerifyToolcacheInventory(&b, tools)
	}

	step("done")
	b.WriteString("billet_verdict=" + verdictOK + "\n")

	return b.String(), schema, nil
}

// toolReport is one toolcache directory the report lists, under a name that is a
// legal shell variable and a legal report key.
//
// NEITHER IS FREE. The declaration's own names carry capitals and a hyphen —
// `Java_Temurin-Hotspot_jdk` is not a shell identifier at all, and
// `billet_tc_Java_Temurin-Hotspot_jdk=…` is a command, not an assignment. So the
// path keeps the declaration's spelling and the variable and key get a derived
// one, checked rather than assumed to fit.
type toolReport struct {
	dir string
	key string
}

func toolReports(ts runnerimages.Toolset) ([]toolReport, error) {
	names := make([]string, 0, len(ts.Toolcache)+1)

	for _, entry := range ts.Toolcache {
		// ONLY WHAT THE INSTALLERS BAKE, matching the gate: listing a tool billet does
		// not install would report an empty line as though something were missing.
		if toolcacheBinary(entry.Name) == "" {
			continue
		}

		names = append(names, entry.Name)
	}

	if len(ts.Java.Versions) > 0 {
		names = append(names, "Java_Temurin-Hotspot_jdk")
	}

	out := make([]toolReport, 0, len(names))

	for _, name := range names {
		if !toolcacheName.MatchString(name) {
			return nil, fmt.Errorf("ec2: %q is not a toolcache name this verification will "+
				"look for", name)
		}

		key := "tc_" + strings.Map(func(r rune) rune {
			switch {
			case r >= 'A' && r <= 'Z':
				return r + ('a' - 'A')
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				return r
			default:
				return '_'
			}
		}, name)

		// REFUSED RATHER THAN DROPPED. A key billet cannot render is a line the
		// report would silently lose, and a silently shorter report reads exactly
		// like a passing one.
		if !reportKey.MatchString(key) {
			return nil, fmt.Errorf("ec2: the toolcache name %q becomes the report key %q, which "+
				"billet will not render", name, key)
		}

		out = append(out, toolReport{dir: name, key: key})
	}

	return out, nil
}

// writeRunAsRunner emits the function that runs a command exactly as a job's
// first process runs it.
//
// ONE INVOCATION, SHARED WITH THE ENTRY POINT THE IMAGE CARRIES. privilegeDrop is
// what /usr/local/bin/billet-runner execs through and imageEnvFile is where it
// reads its variables; spelling either a second way here would let this pass
// while every job failed, which is exactly what a hand-written second copy of
// privilegeDrop caused once already.
//
// THE ROTATION IS HOW A SHELL WITH NO ARRAYS PUTS THE ENVIRONMENT IN FRONT OF THE
// COMMAND. `env` needs its assignments before the program, the function receives
// the program in `$@`, and building the assignments with `set --` would destroy
// it. So the assignments are APPENDED and then the leading command words are
// rotated to the end — which costs a loop and survives a value containing a
// space, where every shorter version word-splits.
func writeRunAsRunner(b *strings.Builder) {
	b.WriteString(verifyRunAsRunner + "() {\n")
	b.WriteString("  billet_argc=$#\n")
	b.WriteString("  if [ -r " + imageEnvFile + " ]; then\n")
	b.WriteString("    while IFS= read -r billet_line; do\n")
	// THE SAME [A-Za-z_]*=* FILTER AS THE GUEST AND THE ENTRY POINT, so a comment
	// or a blank line in that file is skipped rather than handed to env as a
	// malformed assignment.
	b.WriteString("      case \"$billet_line\" in\n")
	b.WriteString("        [A-Za-z_]*=*) set -- \"$@\" \"$billet_line\" ;;\n")
	b.WriteString("      esac\n")
	b.WriteString("    done <" + imageEnvFile + "\n")
	b.WriteString("  fi\n")
	b.WriteString("  billet_i=0\n")
	b.WriteString("  while [ \"$billet_i\" -lt \"$billet_argc\" ]; do\n")
	b.WriteString("    billet_head=$1; shift; set -- \"$@\" \"$billet_head\"\n")
	b.WriteString("    billet_i=$((billet_i+1))\n")
	b.WriteString("  done\n")
	b.WriteString("  " + privilegeDrop + " \\\n")
	b.WriteString("    \"$@\"\n")
	b.WriteString("}\n")
}

// writeVerifyDocker asserts what AMIContract 1 is about, on the daemon the image
// actually starts.
//
// THIS IS THE CHECK THE WHOLE VERIFIER IS ABOUT. The in-build version asserted a
// driver on a daemon apt had already started before daemon.json was written, read
// `overlayfs` where the image itself reports `overlay2`, and failed every build
// against a correct artifact. Restarting fixed the build; asking a machine booted
// FROM the image is what makes the answer about the image.
//
// THE DATA ROOT FIRST AND THE DRIVER LAST, which is the in-build gate's ordering
// and its reason: the cache attaches a fenced filesystem at /var/lib/docker, so
// where the bytes land is the property and the driver string is Docker's to
// rename. Both sides are canonicalised because a LEXICAL comparison accepts
// /var/lib/docker/../containerd, which resolves to exactly the directory this
// exists to keep Docker out of.
func writeVerifyDocker(b *strings.Builder, step func(string)) {
	step("docker")

	// A FRESH BOOT IS ALLOWED TO BE SLOW. A verification run of a finished image
	// measured docker inactive at the instant a check ran and a container running
	// seven seconds later, so without this the assertion is about a daemon that has
	// not started yet — a flake that reports a good image as broken.
	b.WriteString("i=0\n")
	b.WriteString("while [ $i -lt 120 ] && ! docker info >/dev/null 2>&1; do\n")
	b.WriteString("  i=$((i+1)); sleep 1\n")
	b.WriteString("done\n")
	b.WriteString("docker info >/dev/null\n")

	// AND THE RUNNER CAN REACH IT, WHICH IS A DIFFERENT QUESTION FROM WHETHER IT IS
	// UP. Everything else here is a property of the DAEMON and reads the same
	// whoever asks; this is a property of the IMAGE's accounts, and it is the one a
	// job depends on. The build does `usermod -aG docker runner`, and a supplementary
	// group that did not take is invisible to every check that runs as root — an
	// image where `docker info` is perfect and every workflow dies on "permission
	// denied while trying to connect to the Docker daemon socket".
	//
	// `docker ps` RATHER THAN `docker info`, because info answers from the client
	// for some fields and this has to be a request the daemon serves: talking to the
	// socket is the whole content of the check.
	b.WriteString(verifyRunAsRunner + " docker ps -q >/dev/null\n")

	b.WriteString("billet_docker_server=$(docker info -f '{{.ServerVersion}}')\n")
	b.WriteString("billet_docker_driver=$(docker info -f '{{.Driver}}')\n")
	b.WriteString("billet_docker_root=$(realpath -m \"$(docker info -f '{{.DockerRootDir}}')\")\n")
	b.WriteString("billet_cache_root=$(realpath -m /var/lib/docker)\n")
	b.WriteString("case \"$billet_docker_root\" in\n")
	b.WriteString("  \"$billet_cache_root\"|\"$billet_cache_root\"/*) ;;\n")
	b.WriteString("  *) echo \"docker data root $billet_docker_root resolves outside " +
		"$billet_cache_root, so the cache would publish without the images\" >&2; " +
		"exit 1 ;;\n")
	b.WriteString("esac\n")
	b.WriteString("test \"$billet_docker_driver\" = overlay2\n")

	// BOTH HALVES, because they answer different questions: the file is what the
	// snapshot preserved and therefore what the AMI carries, and `docker info` is
	// that file having taken effect on a daemon that read it at start.
	b.WriteString("jq -e '.features[\"containerd-snapshotter\"] == false and " +
		".[\"storage-driver\"] == \"overlay2\"' /etc/docker/daemon.json >/dev/null\n")
}

// writeVerifyToolcacheInventory records what the image holds, per tool.
//
// FOR THE OPERATOR TO READ, NOT FOR THE VERDICT. The gate above decides whether a
// declared line resolves and runs; this lists the entries beside it so a report
// says "node 22.23.2 24.20.0" rather than only "ok", which is the difference
// between a check and something worth looking at.
func writeVerifyToolcacheInventory(b *strings.Builder, tools []toolReport) {
	for _, t := range tools {
		b.WriteString("billet_" + t.key + "=$(ls " + toolcacheDir + "/" + t.dir +
			" 2>/dev/null | tr '\\n' ' ')\n")
	}
}

// writeVerifyReport emits the function that announces the answer, over and over,
// and then ends the machine.
//
// TO /dev/console EXPLICITLY. cloud-init's own output goes to its log and to
// whatever systemd does with the unit's stdout, neither of which is reliably the
// serial device EC2 captures. Writing to the console is the one path that is
// about the thing being read.
//
// AND IT RUNS UNDER `set +e`, because it is the EXIT trap: the ordinary way to
// arrive here is an assertion that failed, and a report that aborts halfway
// through collecting facts reports nothing at all.
//
// EVERY FIELD IS ${x:-}, because the trap can fire before the variable that would
// have held it was ever assigned — which is precisely the case worth reporting.
func writeVerifyReport(b *strings.Builder, schema reportSchema, nonce string, tools []toolReport) {
	b.WriteString("billet_report() {\n")
	b.WriteString("  set +e\n")
	b.WriteString("  echo '" + reportBegin + " " + nonce + "'\n")

	schema.keys[reportVerdictKey] = struct{}{}
	b.WriteString("  echo \"" + reportVerdictKey + "=$billet_verdict\"\n")

	schema.keys[reportStepKey] = struct{}{}
	b.WriteString("  echo \"" + reportStepKey + "=$" + verifyStepVar + "\"\n")

	for _, field := range []string{
		"root_total_kib", "root_used_kib", "root_free_kib",
		"docker_server", "docker_driver", "docker_root",
		"toolcache_kib", "runner",
	} {
		schema.keys[field] = struct{}{}
		b.WriteString("  echo \"" + field + "=${billet_" + field + ":-}\"\n")
	}

	for _, t := range tools {
		schema.keys[t.key] = struct{}{}
		b.WriteString("  echo \"" + t.key + "=${billet_" + t.key + ":-}\"\n")
	}

	b.WriteString("  echo '" + reportEnd + " " + nonce + "'\n")
	b.WriteString("}\n")

	b.WriteString("billet_dwell() {\n")
	b.WriteString("  set +e\n")
	b.WriteString("  billet_round=0\n")
	b.WriteString("  while [ $billet_round -lt " + strconv.Itoa(verifyDwellRounds) + " ]; do\n")
	// STDOUT AS WELL AS THE CONSOLE, because a machine whose /dev/console cannot be
	// written to would otherwise report nothing at all and look like an image that
	// never booted.
	b.WriteString("    billet_report > /dev/console 2>&1 || billet_report\n")
	b.WriteString("    billet_round=$((billet_round+1))\n")
	b.WriteString("    sleep " + strconv.Itoa(verifyDwellSeconds) + "\n")
	b.WriteString("  done\n")
	b.WriteString("  poweroff\n")
	b.WriteString("}\n")
}
