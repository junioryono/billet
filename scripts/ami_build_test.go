package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// amiBuildNonce is the build identity a fixture's build.env names.
const amiBuildNonce = "0123456789abcdef0123456789abcdef"

// The stand-ins build-ami.sh runs beside, in a repository of their own. Each logs
// what it was asked to $FAKE/calls; the runner announces its steps as the real one
// does, leaves what GitHub's template leaves (its staging under
// /var/tmp/billet-ri), fails at FAKE_FAIL_AT, and reboots once at FAKE_REBOOT_AT
// through the real ami-target.sh, after which it exits as the reboot's SIGTERM
// would end it.
const (
	fakeAMIRunner = `#!/bin/bash
set -eu
echo "runner scope=$BILLET_RI_SCOPE from=$BILLET_RI_FROM tmp=${TMPDIR:-}" >>"$FAKE/calls"
grep -qxF '$nrconf{override_rc}{qr(^billet-ami-build)} = 0;' "$BILLET_AMI_ROOT/etc/needrestart/conf.d/billet-ami-build.conf" &&
	echo "runner needrestart-exempt" >>"$FAKE/calls"
[ -z "${FAKE_FAIL_FIRST:-}" ] || exit 1
mkdir -p "$BILLET_AMI_ROOT/var/tmp/billet-ri"
echo staged >"$BILLET_AMI_ROOT/var/tmp/billet-ri/step.sh"
# A STEP THAT LEAVES A PROCESS BEHIND holding the build's output.
if [ -n "${FAKE_LINGER:-}" ]; then
	/bin/sleep "$FAKE_LINGER" &
	echo $! >"$FAKE/linger.pid"
fi
for n in $(seq "$BILLET_RI_FROM" 9); do
	echo "=== [$n/9] step-$n ==="
	[ "$n" != "${FAKE_FAIL_AT:-}" ] || exit 1
	if [ "$n" = "${FAKE_REBOOT_AT:-}" ] && [ ! -e "$FAKE/rebooted" ]; then
		touch "$FAKE/rebooted"
		BILLET_RI_NEXT_STEP=$((n + 1)) "$BILLET_RI_TARGET" reboot </dev/null || exit 1
		exit 143
	fi
done
`
	fakeInstallCompleter = `#!/bin/bash
set -eu
echo "install-completer $*" >>"$FAKE/calls"
case "$1" in "$BILLET_AMI_ROOT"/var/lib/billet-ami-build/*) ;; *) echo "completer outside the build's directory" >>"$FAKE/calls" ;; esac
mkdir -p "$1/opt/billet/complete"
cat >"$1/opt/billet/complete/run" <<'EOF'
#!/bin/bash
echo "complete $*" >>"$FAKE/calls"
for name in "$@"; do
	if [ "$name" = "${FAKE_COMPLETION_FAILS:-}" ]; then
		echo "failed=$name"
	else
		echo "installed=$name 140.0"
	fi
done >"$BILLET_COMPLETION_REPORT"
EOF
chmod 0755 "$1/opt/billet/complete/run"
`
	fakeWriteImageEnv = "#!/bin/sh\necho \"write-image-env $*\" >>\"$FAKE/calls\"\ncp \"$1\" \"$2\"\n"
	// check-guest-component.sh's `present` chroots, so it is stood in for; every
	// other question goes to the real script.
	fakeCheckComponent = `#!/bin/bash
if [ "$1" = present ]; then
	echo "present $2" >>"$FAKE/calls"
	exit "${FAKE_PRESENT_STATUS:-0}"
fi
if [ "$1" = names ] && [ -n "${FAKE_NAMES:-}" ]; then
	# fail: the list fails part way, after a name; empty: it loads and names nothing.
	[ "$FAKE_NAMES" = empty ] || { echo firefox; exit 1; }
	exit 0
fi
if [ "$1" = absent ]; then
	echo "absent $2" >>"$FAKE/calls"
	[ "$2" != "${FAKE_CARRIED:-}" ] || exit 1
	exit 0
fi
exec bash "$REAL_CHECK_COMPONENT" "$@"
`
	fakeLayer = "#!/bin/sh\necho layer >>\"$FAKE/calls\"\nexit \"${FAKE_LAYER_STATUS:-0}\"\n"
	// tee WRITES LATE, as a real one may: the build must wait for it before it
	// reads its own log.
	fakeTee = `#!/bin/bash
# tee -a FILE, a line at a time, each a little late.
while IFS= read -r line; do
	/bin/sleep 0.1
	printf '%s\n' "$line"
	printf '%s\n' "$line" >>"$2"
done
`
	// A command the build runs on the machine: logged, and successful.
	fakeMachineCommand = "#!/bin/sh\necho \"$(basename \"$0\") $*\" >>\"$FAKE/calls\"\n"
	// systemctl is logged too, and says the machine is going down once a reboot
	// was asked for and did not fail (FAKE_REBOOT_FAILS refuses it).
	fakeSystemctl = `#!/bin/sh
case "$1" in
is-system-running)
	if [ -e "$FAKE/rebooting" ]; then echo stopping; else echo running; fi
	exit 0
	;;
enable)
	echo "systemctl $*" >>"$FAKE/calls"
	if ! grep -qx phase=rebooting "$BILLET_AMI_ROOT/var/lib/billet-ami-build/state" 2>/dev/null; then
		echo "enabled before the reboot was recorded" >>"$FAKE/calls"
	fi
	exit 0
	;;
reboot)
	echo "systemctl $*" >>"$FAKE/calls"
	[ -z "${FAKE_REBOOT_FAILS:-}" ] || exit 1
	touch "$FAKE/rebooting"
	exit 0
	;;
esac
echo "systemctl $*" >>"$FAKE/calls"
`
	// poweroff records whether the success verdict was already on the console.
	fakePoweroff = `#!/bin/sh
if grep -q '^verdict=ok$' "$BILLET_AMI_CONSOLE" 2>/dev/null; then
	echo "poweroff after the verdict" >>"$FAKE/calls"
else
	echo "poweroff before any verdict" >>"$FAKE/calls"
fi
`
	// sleep ENDS A FAILED BUILD'S VERDICT LOOP, which on a real builder runs until
	// billet terminates it: after a few rounds it signals the build's shell, as the
	// termination would.
	fakeSleep = `#!/bin/sh
[ "$1" = infinity ] && exit 0
n=$(($(cat "$FAKE/sleeps" 2>/dev/null || echo 0) + 1))
echo "$n" >"$FAKE/sleeps"
[ "$n" -lt 4 ] || kill -TERM "$PPID"
`
)

// amiBuildFixture is a builder build-ami.sh can run on: the real build-ami.sh and
// ami-target.sh in a repository of stand-ins, a machine under root, and a bin of
// the machine's own commands.
type amiBuildFixture struct {
	dir, repo, root, fake, console, bootID string
}

func newAMIBuildFixture(t *testing.T, scope, accept string) amiBuildFixture {
	t.Helper()

	dir := t.TempDir()
	fx := amiBuildFixture{
		dir:     dir,
		repo:    filepath.Join(dir, "repo"),
		root:    filepath.Join(dir, "root"),
		fake:    filepath.Join(dir, "fake"),
		console: filepath.Join(dir, "console"),
		bootID:  filepath.Join(dir, "boot_id"),
	}

	for _, d := range []string{
		filepath.Join(fx.repo, "scripts", "ami"), filepath.Join(fx.repo, "scripts", "runner-images"),
		filepath.Join(fx.root, "var", "lib", "billet-ami"), filepath.Join(fx.root, "etc", "systemd", "system"),
		fx.fake, filepath.Join(dir, "bin"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for name, body := range map[string]string{
		"scripts/ami/build-ami.sh":                            readScriptFile(t, filepath.Join("ami", "build-ami.sh")),
		"scripts/ami/ami-target.sh":                           readScriptFile(t, filepath.Join("ami", "ami-target.sh")),
		"scripts/run-runner-images.sh":                        fakeAMIRunner,
		"scripts/runner-images/install-completer.sh":          fakeInstallCompleter,
		"scripts/runner-images/write-image-env.sh":            fakeWriteImageEnv,
		"scripts/check-guest-component.sh":                    fakeCheckComponent,
		"scripts/runner-images/plan.json":                     readScriptFile(t, filepath.Join("runner-images", "plan.json")),
		"scripts/runner-images/plan-arm64.json":               readScriptFile(t, filepath.Join("runner-images", "plan-arm64.json")),
		"../root/var/lib/billet-ami/ec2-layer.sh":             fakeLayer,
		"../root/etc/environment":                             "PATH=/usr/bin\n",
		"../root/etc/systemd/system/billet-ami-build.service": "[Unit]\n",
		"../root/var/lib/billet-ami/build.env": "BILLET_AMI_NONCE=" + amiBuildNonce + "\n" +
			"BILLET_AMI_SCOPE=" + scope + "\nBILLET_AMI_ACCEPT='" + accept + "'\n",
	} {
		if err := forkSafeWriteFile(filepath.Join(fx.repo, filepath.FromSlash(name)), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := forkSafeWriteFile(filepath.Join(dir, "bin", "systemctl"), []byte(fakeSystemctl), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := forkSafeWriteFile(filepath.Join(dir, "bin", "poweroff"), []byte(fakePoweroff), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := forkSafeWriteFile(filepath.Join(dir, "bin", "tee"), []byte(fakeTee), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"cloud-init", "sync"} {
		if err := forkSafeWriteFile(filepath.Join(dir, "bin", name), []byte(fakeMachineCommand), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := forkSafeWriteFile(filepath.Join(dir, "bin", "sleep"), []byte(fakeSleep), 0o755); err != nil {
		t.Fatal(err)
	}

	fx.boot(t, "boot-one")

	return fx
}

// boot gives the machine a new boot id, as a reboot does.
func (fx amiBuildFixture) boot(t *testing.T, id string) {
	t.Helper()

	if err := os.WriteFile(fx.bootID, []byte(id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"sleeps", "rebooting"} {
		if err := os.Remove(filepath.Join(fx.fake, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// run runs build-ami.sh run as the unit would, with env added.
func (fx amiBuildFixture) run(t *testing.T, env ...string) (string, error) {
	t.Helper()

	here, err := filepath.Abs("check-guest-component.sh")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(fx.repo, "scripts", "ami", "build-ami.sh"), "run")
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(fx.dir, "bin")+":"+os.Getenv("PATH"),
		"FAKE="+fx.fake, "REAL_CHECK_COMPONENT="+here,
		"BILLET_AMI_ROOT="+fx.root, "BILLET_AMI_CONSOLE="+fx.console, "BILLET_AMI_BOOT_ID="+fx.bootID,
		"BILLET_AMI_DWELL_ROUNDS=2", "BILLET_AMI_DWELL_SECONDS=0")
	cmd.Env = append(cmd.Env, env...)

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// verdict is the last framed block on the console, as billet reads it.
func (fx amiBuildFixture) verdict(t *testing.T) map[string]string {
	t.Helper()

	raw, err := os.ReadFile(fx.console)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	console := string(raw)
	end := strings.LastIndex(console, "billet-build-end "+amiBuildNonce)
	if end < 0 {
		return nil
	}

	start := strings.LastIndex(console[:end], "billet-build-begin "+amiBuildNonce)
	if start < 0 {
		return nil
	}

	out := map[string]string{}
	for _, line := range strings.Split(console[start:end], "\n")[1:] {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}

	return out
}

// calls is every logged call, in order.
func (fx amiBuildFixture) calls(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(fx.fake, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return string(raw)
}

// state is the build's record, or "" once it is gone.
func (fx amiBuildFixture) state(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(fx.root, "var", "lib", "billet-ami-build", "state"))
	if os.IsNotExist(err) {
		return ""
	}

	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// A BUILD THAT FINISHES SAYS SO, WITH WHAT IT INSTALLED, AND ONLY THEN POWERS OFF:
// the template run from step 1 under its scope, each accepted component the
// template offers completed and proved, the job's environment written, the layer
// run, and nothing of the build left behind.
func TestTheAMIBuildRunsTheTemplateCompletesAndAnnounces(t *testing.T) {
	t.Parallel()

	fx := newAMIBuildFixture(t, "ami-x64", "android-sdk firefox")

	out, err := fx.run(t)
	if err != nil {
		t.Fatalf("the build failed: %v\n%s", err, out)
	}

	v := fx.verdict(t)
	if v["verdict"] != "ok" || v["step"] != "done" || v["components"] != "android-sdk:140.0,firefox:140.0" {
		t.Errorf("the verdict is %v, want ok at done with both components", v)
	}

	calls := fx.calls(t)
	for _, want := range []string{
		"runner scope=ami-x64 from=1 tmp=" + fx.root + "/var/lib/billet-ami-build/tmp", "runner needrestart-exempt",
		"complete android-sdk firefox", "present android-sdk",
		"absent codeql", "absent google-chrome", "absent microsoft-edge",
		"present firefox", "write-image-env " + fx.root + "/etc/environment " + fx.root + "/etc/billet-image-env",
		"layer", "systemctl disable billet-ami-build.service", "cloud-init clean --logs --machine-id",
	} {
		if !strings.Contains(calls, want+"\n") {
			t.Errorf("the build never ran %q:\n%s", want, calls)
		}
	}

	if i, j := strings.Index(calls, "cloud-init clean"), strings.Index(calls, "poweroff"); j < 0 || j < i {
		t.Errorf("the builder powered off at %d, before the image was cleaned at %d:\n%s", j, i, calls)
	}

	if strings.Contains(calls, "completer outside the build's directory") || strings.Contains(calls, "absent android-sdk") {
		t.Errorf("the completer ran outside the build's own directory, or a recorded component was "+
			"asked to be absent:\n%s", calls)
	}

	if !strings.Contains(calls, "poweroff after the verdict\n") {
		t.Errorf("the builder powered off before announcing its verdict:\n%s", calls)
	}

	record, err := os.ReadFile(filepath.Join(fx.root, "etc", "billet-image-components"))
	if err != nil || string(record) != "android-sdk:140.0,firefox:140.0\n" {
		t.Errorf("the image records %q (%v)", record, err)
	}

	for _, gone := range []string{"var/lib/billet-ami", "var/lib/billet-ami-build", "opt/billet/complete",
		"var/tmp/billet-ri", "etc/needrestart/conf.d/billet-ami-build.conf",
		"etc/systemd/system/billet-ami-build.service"} {
		if _, err := os.Stat(filepath.Join(fx.root, gone)); !os.IsNotExist(err) {
			t.Errorf("the image keeps %s (%v)", gone, err)
		}
	}
}

// AN ACCEPTED COMPONENT THIS ARCHITECTURE'S TEMPLATE DOES NOT INSTALL IS LEFT OUT
// AND SAID, as GitHub's own image for it leaves it out; one it does is installed.
func TestTheAMIBuildCompletesOnlyWhatItsTemplateOffers(t *testing.T) {
	t.Parallel()

	fx := newAMIBuildFixture(t, "ami-arm64", "android-sdk firefox")

	out, err := fx.run(t)
	if err != nil {
		t.Fatalf("the build failed: %v\n%s", err, out)
	}

	if v := fx.verdict(t); v["verdict"] != "ok" || v["components"] != "firefox:140.0" {
		t.Errorf("the verdict is %v, want ok with firefox alone", v)
	}

	if !strings.Contains(fx.calls(t), "complete firefox\n") {
		t.Errorf("the completer was not asked for firefox alone:\n%s", fx.calls(t))
	}

	if !strings.Contains(out, "android-sdk is accepted and GitHub's template for ami-arm64 does not install it") {
		t.Errorf("the build did not say it left the Android SDK out:\n%s", out)
	}
}

// EVERY FAILURE IS A VERDICT NAMING ITS PART, AND NO POWEROFF: billet reads the
// failure while the builder is still there, and a stop is never mistaken for
// success.
func TestTheAMIBuildReportsEachFailureAndKeepsTheMachine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		scope        string
		accept       string
		env          []string
		removePlan   bool
		wantStep     string
		wantTemplate string
	}{
		// A BUILD THAT CANNOT READ ITS OWN SETTINGS STILL SAYS SO, rather than
		// leaving billet to its deadline.
		{name: "the settings name no scope", scope: "ami-riscv64", wantStep: "start"},
		{name: "the runner fails before its first step", env: []string{"FAKE_FAIL_FIRST=1"}, wantStep: "template"},
		// A PLAN THAT CANNOT BE READ IS NOT A PLAN WITHOUT THE STEP: the accepted
		// component would be dropped and the build would succeed without it.
		{name: "the plan cannot be read", accept: "firefox", removePlan: true, wantStep: "components"},
		{name: "a template step fails", env: []string{"FAKE_FAIL_AT=4"}, wantStep: "template", wantTemplate: "4"},
		{name: "the completer installs nothing", accept: "firefox",
			env: []string{"FAKE_COMPLETION_FAILS=firefox"}, wantStep: "components"},
		{name: "a completed component is not present", accept: "firefox",
			env: []string{"FAKE_PRESENT_STATUS=1"}, wantStep: "components"},
		{name: "the layer fails", env: []string{"FAKE_LAYER_STATUS=1"}, wantStep: "layer"},
		// THE BASE IMAGE CARRIES A COMPONENT NOBODY ACCEPTED: a record of none would
		// be a claim the image does not meet.
		{name: "the base image carries a component nobody accepted", env: []string{"FAKE_CARRIED=google-chrome"},
			wantStep: "components"},
		// A LIST THAT CANNOT BE LOADED PROVES NOTHING ABSENT, failed or empty.
		{name: "the component list cannot be loaded", env: []string{"FAKE_NAMES=fail"}, wantStep: "components"},
		{name: "the component list names nothing", env: []string{"FAKE_NAMES=empty"}, wantStep: "components"},
		// A PROCESS A STEP LEFT HOLDING THE BUILD'S OUTPUT does not hold back the
		// verdict past BILLET_AMI_TEE_WAIT: the build stops waiting for its log.
		{name: "a step leaves a process holding the output", env: []string{"FAKE_FAIL_AT=4",
			"FAKE_LINGER=30", "BILLET_AMI_TEE_WAIT=1"}, wantStep: "template", wantTemplate: "4"},
		// THE RECORD SAYS REBOOTING AND THE MACHINE IS NOT GOING DOWN: a reboot
		// request that failed is a failure, not the planned path.
		{name: "the reboot request fails", env: []string{"FAKE_REBOOT_AT=5", "FAKE_REBOOT_FAILS=1"},
			wantStep: "template", wantTemplate: "5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scope := tc.scope
			if scope == "" {
				scope = "ami-x64"
			}

			fx := newAMIBuildFixture(t, scope, tc.accept)

			if tc.removePlan {
				if err := os.Remove(filepath.Join(fx.repo, "scripts", "runner-images", "plan.json")); err != nil {
					t.Fatal(err)
				}
			}

			// THE LINGERING PROCESS IS THE TEST'S TO END, not left running after it.
			t.Cleanup(func() {
				raw, err := os.ReadFile(filepath.Join(fx.fake, "linger.pid"))
				if err != nil {
					return
				}

				if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
					if p, err := os.FindProcess(pid); err == nil {
						if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
							t.Logf("the lingering process %d: %v", pid, err)
						}
					}
				}
			})

			began := time.Now()

			out, err := fx.run(t, tc.env...)
			if err == nil {
				t.Fatalf("a failed build exited 0:\n%s", out)
			}

			// A FAILURE IS ANNOUNCED PROMPTLY: a wait without its bound would sit
			// out the lingering process's thirty seconds.
			if took := time.Since(began); took > 20*time.Second {
				t.Errorf("the failure took %s to be announced", took)
			}

			v := fx.verdict(t)
			if v["verdict"] != "fail" || v["step"] != tc.wantStep || v["template_step"] != tc.wantTemplate {
				t.Errorf("the verdict is %v, want fail at %s (template step %q)\n%s",
					v, tc.wantStep, tc.wantTemplate, out)
			}

			if strings.Contains(fx.calls(t), "poweroff") {
				t.Errorf("a failed build powered the builder off:\n%s", fx.calls(t))
			}
		})
	}
}

// THE PLANNED REBOOT IS RECORDED, THE UNIT ENABLED FOR THE NEXT BOOT, AND THAT BOOT
// CONTINUES AT THE NEXT STEP, saying nothing in between: the reboot's end of the
// first boot's build is not a failure.
func TestTheAMIBuildResumesAfterItsPlannedReboot(t *testing.T) {
	t.Parallel()

	fx := newAMIBuildFixture(t, "ami-x64", "")

	out, err := fx.run(t, "FAKE_REBOOT_AT=5")
	if err == nil {
		t.Fatalf("the build's first boot ended 0 across a reboot:\n%s", out)
	}

	if v := fx.verdict(t); v != nil {
		t.Errorf("the planned reboot announced a verdict: %v", v)
	}

	state := fx.state(t)
	for _, want := range []string{"nonce=" + amiBuildNonce, "phase=rebooting", "next=6", "boot=boot-one"} {
		if !strings.Contains(state, want+"\n") {
			t.Errorf("the record does not say %s:\n%s", want, state)
		}
	}

	calls := fx.calls(t)
	enable := strings.Index(calls, "systemctl enable billet-ami-build.service")
	reboot := strings.Index(calls, "systemctl reboot")

	if enable < 0 || reboot < enable {
		t.Errorf("the unit was enabled at %d and the reboot asked at %d:\n%s", enable, reboot, calls)
	}

	// THE RECORD BEFORE THE UNIT: a crash between them leaves a record no boot
	// acts on, where the other order leaves an enabled unit with no record.
	if strings.Contains(calls, "enabled before the reboot was recorded") {
		t.Errorf("the unit was enabled before the reboot was recorded:\n%s", calls)
	}

	fx.boot(t, "boot-two")

	out, err = fx.run(t)
	if err != nil {
		t.Fatalf("the resumed build failed: %v\n%s", err, out)
	}

	if !strings.Contains(fx.calls(t), "runner scope=ami-x64 from=6 tmp="+fx.root+"/var/lib/billet-ami-build/tmp\n") {
		t.Errorf("the second boot did not continue at step 6:\n%s", fx.calls(t))
	}

	if v := fx.verdict(t); v["verdict"] != "ok" {
		t.Errorf("the resumed build's verdict is %v", v)
	}
}

// THE BUILD UNIT RUNS AFTER CLOUD-INIT'S LAST STAGE AND IS WANTED BY CLOUD-INIT'S
// TARGET. cloud-final.service is ordered after multi-user.target, so the same unit
// wanted by multi-user.target is an ordering cycle, and systemd breaks it by
// deleting the build's start job: the boot after the template's reboot ran
// nothing (measured on real builders, 2026-10-25). And it is a oneshot whose start
// never times out, because the build takes hours.
func TestTheAMIBuildUnitResumesWithoutAnOrderingCycle(t *testing.T) {
	t.Parallel()

	unit := readScriptFile(t, filepath.Join("ami", "billet-ami-build.service"))

	for _, line := range []string{
		"After=network-online.target cloud-final.service", "WantedBy=cloud-init.target",
		"Type=oneshot", "TimeoutStartSec=infinity", "ExecStart=/var/lib/billet-ami/repo/scripts/ami/build-ami.sh run",
		"StandardOutput=journal", "StandardError=journal",
	} {
		if !hasExactLine(unit, line) {
			t.Errorf("the build unit does not say %q", line)
		}
	}

	if strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Error("the build unit is wanted by multi-user.target, which cloud-final runs after")
	}
}

// A RESUMED RUN THAT FAILS BEFORE ITS FIRST STEP NAMES NO STEP: the log of the
// boot before is not this run's, and its last step is not where this one failed.
func TestAResumedFailureNamesOnlyItsOwnSteps(t *testing.T) {
	t.Parallel()

	fx := newAMIBuildFixture(t, "ami-x64", "")

	work := filepath.Join(fx.root, "var", "lib", "billet-ami-build")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"state":     "nonce=" + amiBuildNonce + "\nphase=rebooting\nnext=6\nboot=boot-zero\n",
		"build.log": "=== [5/9] step-5 ===\n",
	} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, err := fx.run(t, "FAKE_FAIL_FIRST=1")
	if err == nil {
		t.Fatalf("a failed build exited 0:\n%s", out)
	}

	if v := fx.verdict(t); v["verdict"] != "fail" || v["step"] != "template" || v["template_step"] != "" {
		t.Errorf("the verdict is %v, want fail at template naming no step\n%s", v, out)
	}
}

// record-reboot WRITES ONLY THIS BUILD'S RECORD: a record of another build is
// left as it was and the reboot is refused.
func TestARebootIsRecordedOnlyForThisBuild(t *testing.T) {
	t.Parallel()

	fx := newAMIBuildFixture(t, "ami-x64", "")

	state := filepath.Join(fx.root, "var", "lib", "billet-ami-build", "state")
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}

	other := "nonce=fedcba9876543210fedcba9876543210\nphase=started\nboot=boot-one\n"
	if err := os.WriteFile(state, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(fx.repo, "scripts", "ami", "build-ami.sh"),
		"record-reboot", "5")
	cmd.Env = append(os.Environ(), "BILLET_AMI_ROOT="+fx.root, "BILLET_AMI_BOOT_ID="+fx.bootID,
		"BILLET_AMI_CONSOLE="+fx.console, "FAKE="+fx.fake, "PATH="+filepath.Join(fx.dir, "bin")+":"+os.Getenv("PATH"))

	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("a reboot was recorded over another build's record:\n%s", out)
	}

	if got := fx.state(t); got != other {
		t.Errorf("the record became %q", got)
	}

	if v := fx.verdict(t); v != nil {
		t.Errorf("record-reboot announced a verdict of its own: %v", v)
	}
}

// ANY OTHER START WITH A RECORD IS AN UNPLANNED RESTART, AND FAILS without running
// the template again: a build resumed from a record no reboot wrote installs onto
// a machine in a state nothing knows.
func TestTheAMIBuildRefusesAnUnplannedRestart(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, record string
	}{
		{name: "a build that had resumed", record: "nonce=" + amiBuildNonce + "\nphase=resumed\nboot=boot-zero\n"},
		{name: "a build that had started", record: "nonce=" + amiBuildNonce + "\nphase=started\nboot=boot-zero\n"},
		{name: "a reboot recorded in this boot",
			record: "nonce=" + amiBuildNonce + "\nphase=rebooting\nnext=3\nboot=boot-one\n"},
		{name: "another build's record",
			record: "nonce=fedcba9876543210fedcba9876543210\nphase=rebooting\nnext=3\nboot=boot-zero\n"},
		// AN INCOMPLETE RECORD IS NOT A PLANNED REBOOT: no boot reads as "another
		// boot", and no step would restart the template from the first.
		{name: "a reboot recorded in no boot", record: "nonce=" + amiBuildNonce + "\nphase=rebooting\nnext=3\n"},
		{name: "a reboot recorded at no step", record: "nonce=" + amiBuildNonce + "\nphase=rebooting\nboot=boot-zero\n"},
		{name: "a reboot recorded at step zero",
			record: "nonce=" + amiBuildNonce + "\nphase=rebooting\nnext=0\nboot=boot-zero\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fx := newAMIBuildFixture(t, "ami-x64", "")

			state := filepath.Join(fx.root, "var", "lib", "billet-ami-build", "state")
			if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(state, []byte(tc.record), 0o600); err != nil {
				t.Fatal(err)
			}

			out, err := fx.run(t)
			if err == nil {
				t.Fatalf("an unplanned restart exited 0:\n%s", out)
			}

			if v := fx.verdict(t); v["verdict"] != "fail" || v["step"] != "resume" {
				t.Errorf("the verdict is %v, want fail at resume\n%s", v, out)
			}

			if strings.Contains(fx.calls(t), "runner ") {
				t.Errorf("the template ran again:\n%s", fx.calls(t))
			}
		})
	}
}
