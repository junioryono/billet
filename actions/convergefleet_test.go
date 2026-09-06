package actions_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE SCRIPTS ARE EXECUTED, NEVER PATTERN-MATCHED, against fakes on PATH that
// record every call and judge nothing. A fake ansible-playbook answers
// --list-hosts from a fixture, records its argv and environment, and prints a
// scripted recap per pass; a fake `ansible` answers the debug module's render;
// nc, ssh-keyscan and warp-cli record and exit as told.

type convergeFixture struct {
	dir      string // the temporary tree: fakes, RUNNER_TEMP, HOME, GITHUB_OUTPUT
	bin      string
	calls    string // every fake's invocation, one line each
	pbArgs   string // ansible-playbook argv per invocation, separated by "--"
	pbEnv    string // ansible-playbook's environment on its last invocation
	output   string // GITHUB_OUTPUT
	runnerTm string
	home     string
}

// listHostsFixture is what `ansible-playbook --list-hosts` prints for the
// fleet playbook over a two-host inventory.
const listHostsFixture = `
playbook: fleet.yml

  play #1 (control_plane): billet control plane	TAGS: []
    pattern: ['control_plane']
    hosts (1):
      cp-1

  play #2 (linux): billet Linux hosts	TAGS: []
    pattern: ['linux']
    hosts (1):
      node-a
`

// debugLine is one host's answer from the fake `ansible -m debug -o`: the
// outer document carries the inner one as a JSON string, as the module does.
func debugLine(host, addr string, port int, sshCommon string) string {
	inner, err := json.Marshal(map[string]any{"host": addr, "port": port, "common": sshCommon, "extra": "", "args": ""})
	if err != nil {
		panic(err)
	}
	outer, err := json.Marshal(map[string]string{"msg": string(inner)})
	if err != nil {
		panic(err)
	}
	return host + " | SUCCESS => " + string(outer)
}

func cleanRecap(hosts ...string) string {
	var b strings.Builder
	b.WriteString("PLAY RECAP *********************************************************************\n")
	for _, h := range hosts {
		b.WriteString(h + "                     : ok=12   changed=0    unreachable=0    failed=0    skipped=3    rescued=0    ignored=0   \n")
	}
	return b.String()
}

func newConvergeFixture(t *testing.T) *convergeFixture {
	t.Helper()

	dir := t.TempDir()
	f := &convergeFixture{
		dir:      dir,
		bin:      filepath.Join(dir, "bin"),
		calls:    filepath.Join(dir, "calls"),
		pbArgs:   filepath.Join(dir, "playbook-args"),
		pbEnv:    filepath.Join(dir, "playbook-env"),
		output:   filepath.Join(dir, "github-output"),
		runnerTm: filepath.Join(dir, "runner-temp"),
		home:     filepath.Join(dir, "home"),
	}
	for _, d := range []string{f.bin, f.runnerTm, f.home, filepath.Join(dir, "collections")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.output, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	fakes := map[string]string{
		"ansible-playbook": `#!/bin/sh
printf 'ansible-playbook %s\n' "$*" >>"$BILLET_FAKE_CALLS"
case " $* " in
  *" --list-hosts "*) cat "$BILLET_FAKE_LIST_HOSTS"; exit 0 ;;
esac
{ printf '%s\n' "$@"; echo "--"; } >>"$BILLET_FAKE_PB_ARGS"
env | sort >"$BILLET_FAKE_PB_ENV"
n=$(cat "$BILLET_FAKE_PASS" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" >"$BILLET_FAKE_PASS"
if [ "$n" = 1 ]; then cat "$BILLET_FAKE_RECAP_1"; else cat "$BILLET_FAKE_RECAP_2"; fi
exit "${BILLET_FAKE_PB_EXIT:-0}"
`,
		"ansible": `#!/bin/sh
printf 'ansible %s\n' "$*" >>"$BILLET_FAKE_CALLS"
cat "$BILLET_FAKE_DEBUG"
`,
		"nc": `#!/bin/sh
printf 'nc %s\n' "$*" >>"$BILLET_FAKE_CALLS"
for a in "$@"; do [ "$a" = "${BILLET_FAKE_NC_FAIL:-none}" ] && exit 1; done
exit 0
`,
		"ssh-keyscan": `#!/bin/sh
printf 'ssh-keyscan %s\n' "$*" >>"$BILLET_FAKE_CALLS"
echo "ssh-keyscan must never be run by the action" >&2
exit 1
`,
		"warp-cli": `#!/bin/sh
printf 'warp-cli %s\n' "$*" >>"$BILLET_FAKE_CALLS"
case " $* " in
  *" status "*) if [ "${BILLET_FAKE_WARP_CONNECTS:-yes}" = yes ]; then echo "Status update: Connected"; else echo "Status update: Registered"; fi ;;
esac
exit "${BILLET_FAKE_WARP_EXIT:-0}"
`,
		"sudo": `#!/bin/sh
printf 'sudo %s\n' "$*" >>"$BILLET_FAKE_CALLS"
# A recorder: nothing here may write under /etc or /var.
cat >/dev/null 2>&1 || true
exit 0
`,
		"sleep":       "#!/bin/sh\nexit 0\n",
		"apt-get":     "#!/bin/sh\nprintf 'apt-get %s\\n' \"$*\" >>\"$BILLET_FAKE_CALLS\"\nexit 0\n",
		"lsb_release": "#!/bin/sh\necho noble\n",
		"curl": `#!/bin/sh
printf 'curl %s\n' "$*" >>"$BILLET_FAKE_CALLS"
out=""
prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out=$a; prev=$a; done
printf '%s' "${BILLET_FAKE_KEY_BODY:-GOOD KEY}" >"$out"
`,
		"gpg": `#!/bin/sh
printf 'gpg %s\n' "$*" >>"$BILLET_FAKE_CALLS"
file=""; for a in "$@"; do file=$a; done
case " $* " in *" --dearmor "*) exit 0 ;; esac
if [ "$(cat "$file")" = "GOOD KEY" ]; then
  printf 'pub:-:4096:1:6E2DD2174FA1C3BA:1700000000::::::scESC::::::23::0:\nfpr:::::::::%s:\n' "$BILLET_FAKE_GOOD_FPR"
else
  printf 'pub:-:4096:1:0000000000000000:1700000000::::::scESC::::::23::0:\nfpr:::::::::DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF:\n'
fi
`,
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(f.bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

type convergeRun struct {
	mode, inventory, playbook, knownHosts, reach, sshKey, environment, appKey, extraVars, limit, prove string
	listHosts, debug, recap1, recap2                                                                   string
	ncFail, runnerName                                                                                 string
	pbExit                                                                                             string
}

// run executes converge.sh with the fixture's fakes and returns combined
// output and the exit error.
func (f *convergeFixture) run(t *testing.T, r convergeRun) (string, error) {
	t.Helper()

	write := func(name, body string) string {
		p := filepath.Join(f.dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if r.listHosts == "" {
		r.listHosts = listHostsFixture
	}
	if r.debug == "" {
		r.debug = debugLine("cp-1", "10.0.0.1", 22, "") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n"
	}
	if r.recap1 == "" {
		r.recap1 = cleanRecap("cp-1", "node-a")
	}
	if r.recap2 == "" {
		r.recap2 = r.recap1
	}
	if r.inventory == "" {
		r.inventory = write("inventory.yml", "all:\n  hosts:\n    cp-1: {}\n    node-a: {}\n")
	}
	if r.mode == "" {
		r.mode = "converge"
	}
	if r.prove == "" {
		r.prove = "true"
	}
	_ = os.Remove(filepath.Join(f.dir, "pass"))
	_ = os.Remove(f.pbArgs)
	_ = os.Remove(f.calls)

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "converge.sh"))
	cmd.Env = []string{
		"PATH=" + f.bin + ":" + os.Getenv("PATH"),
		"HOME=" + f.home,
		"RUNNER_TEMP=" + f.runnerTm,
		"GITHUB_OUTPUT=" + f.output,
		"GITHUB_ACTION_PATH=" + mustAbs(t, "converge-fleet"),
		"BILLET_FAKE_CALLS=" + f.calls,
		"BILLET_FAKE_PB_ARGS=" + f.pbArgs,
		"BILLET_FAKE_PB_ENV=" + f.pbEnv,
		"BILLET_FAKE_PASS=" + filepath.Join(f.dir, "pass"),
		"BILLET_FAKE_LIST_HOSTS=" + write("list-hosts", r.listHosts),
		"BILLET_FAKE_DEBUG=" + write("debug", r.debug),
		"BILLET_FAKE_RECAP_1=" + write("recap1", r.recap1),
		"BILLET_FAKE_RECAP_2=" + write("recap2", r.recap2),
		"BILLET_FAKE_NC_FAIL=" + r.ncFail,
		"BILLET_FAKE_PB_EXIT=" + r.pbExit,
		"BILLET_MODE=" + r.mode,
		"BILLET_INVENTORY=" + r.inventory,
		"BILLET_PLAYBOOK=" + r.playbook,
		"BILLET_KNOWN_HOSTS=" + r.knownHosts,
		"BILLET_REACH=" + r.reach,
		"BILLET_SSH_PRIVATE_KEY=" + r.sshKey,
		"BILLET_ENVIRONMENT=" + r.environment,
		"BILLET_GITHUB_APP_PRIVATE_KEY=" + r.appKey,
		"BILLET_EXTRA_VARS=" + r.extraVars,
		"BILLET_LIMIT=" + r.limit,
		"BILLET_PROVE_IDEMPOTENT=" + r.prove,
	}
	if r.runnerName != "" {
		cmd.Env = append(cmd.Env, "RUNNER_NAME="+r.runnerName)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// readRecorded reads what a fake recorded. An absent file is what a fake that
// was never called leaves, and several assertions are about exactly that, so
// absence reads as nothing recorded; any other error is a failed test rather
// than an empty record.
func readRecorded(t *testing.T, p string) []byte {
	t.Helper()
	body, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", p, err)
	}
	return body
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *convergeFixture) callsOf(t *testing.T, name string) []string {
	t.Helper()
	body := readRecorded(t, f.calls)
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, name+" ") || line == name {
			out = append(out, line)
		}
	}
	return out
}

func (f *convergeFixture) playbookInvocations(t *testing.T) []string {
	t.Helper()
	body := readRecorded(t, f.pbArgs)
	var out []string
	for _, inv := range strings.Split(string(body), "--\n") {
		if strings.TrimSpace(inv) != "" {
			out = append(out, inv)
		}
	}
	return out
}

func TestConvergeRunsTwicAndReportsTheRecap(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	if n := len(f.playbookInvocations(t)); n != 2 {
		t.Fatalf("ansible-playbook ran %d times, want 2 (the converge and the proof)\n%s", n, out)
	}
	body := readRecorded(t, f.output)
	if !strings.Contains(string(body), "recap<<BILLET_RECAP_EOF") || !strings.Contains(string(body), "node-a") {
		t.Errorf("the recap output was not written:\n%s", body)
	}
}

func TestASecondPassWithAChangeFailsNamingTheHost(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	recap2 := strings.Replace(cleanRecap("cp-1", "node-a"), "node-a                     : ok=12   changed=0", "node-a                     : ok=12   changed=1", 1)
	out, err := f.run(t, convergeRun{recap2: recap2})
	if err == nil {
		t.Fatalf("a second pass with changed=1 passed:\n%s", out)
	}
	if !strings.Contains(out, "node-a reported changed=1 on the second pass") {
		t.Errorf("the failure does not name the host and the counter:\n%s", out)
	}
}

func TestAHostThePlaybookListsMustAppearInTheRecap(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{recap2: cleanRecap("cp-1")})
	if err == nil {
		t.Fatalf("a recap missing a listed host passed:\n%s", out)
	}
	if !strings.Contains(out, "node-a was to be converged and has no recap row") {
		t.Errorf("the failure does not name the missing host:\n%s", out)
	}
}

func TestARescuedTaskIsNotAConvergedHost(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	recap2 := strings.Replace(cleanRecap("cp-1", "node-a"), "rescued=0    ignored=0   \n", "rescued=1    ignored=0   \n", 1)
	out, err := f.run(t, convergeRun{recap2: recap2})
	if err == nil {
		t.Fatalf("a rescued task passed the proof:\n%s", out)
	}
	if !strings.Contains(out, "had rescued tasks") {
		t.Errorf("the failure does not say why:\n%s", out)
	}
}

// node.a AND node-a: a pattern built from the hostname reads the dot as any
// character, so a clean row for node-a satisfied an expectation of node.a. The
// rows are compared as literal strings.
func TestHostnamesAreComparedLiterally(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	list := strings.Replace(listHostsFixture, "      node-a\n", "      node.a\n      node-a\n", 1)
	debug := debugLine("cp-1", "10.0.0.1", 22, "") + "\n" + debugLine("node.a", "10.0.0.3", 22, "") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n"
	out, err := f.run(t, convergeRun{listHosts: list, debug: debug, recap1: cleanRecap("cp-1", "node-a"), recap2: cleanRecap("cp-1", "node-a")})
	if err == nil {
		t.Fatalf("node.a had no recap row and the proof passed on node-a's:\n%s", out)
	}
	if !strings.Contains(out, "node.a was to be converged and has no recap row") {
		t.Errorf("the failure does not name node.a:\n%s", out)
	}
}

// With result_format=yaml every task result is printed in full, so a task whose
// output says changed=1 must not fail a clean run: only recap rows are read.
func TestATaskResultMentioningChangedDoesNotFailACleanRun(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	recap2 := "TASK [print] ****\nok: [node-a] => \n  msg: the last run said changed=1 unreachable=0\n" + cleanRecap("cp-1", "node-a")
	out, err := f.run(t, convergeRun{recap2: recap2})
	if err != nil {
		t.Fatalf("a task result above the recap failed the proof: %v\n%s", err, out)
	}
}

func TestALimitedRunExpectsOnlyTheLimitedHosts(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	list := strings.Replace(listHostsFixture, "    hosts (1):\n      node-a\n", "    hosts (0):\n", 1)
	out, err := f.run(t, convergeRun{listHosts: list, limit: "cp-1", debug: debugLine("cp-1", "10.0.0.1", 22, "") + "\n", recap1: cleanRecap("cp-1"), recap2: cleanRecap("cp-1")})
	if err != nil {
		t.Fatalf("a limited run failed: %v\n%s", err, out)
	}
	for _, c := range f.callsOf(t, "nc") {
		if strings.Contains(c, "10.0.0.2") {
			t.Errorf("the unlisted host was probed: %s", c)
		}
	}
	for _, inv := range f.playbookInvocations(t) {
		if !strings.Contains(inv, "--limit\ncp-1\n") {
			t.Errorf("an invocation ran without the limit:\n%s", inv)
		}
	}
}

func TestAPlaybookMatchingNoHostIsRefusedInCheckMode(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	list := "\nplaybook: fleet.yml\n\n  play #1 (control_plane): billet control plane\tTAGS: []\n    pattern: ['control_plane']\n    hosts (0):\n"
	out, err := f.run(t, convergeRun{mode: "check", listHosts: list})
	if err == nil {
		t.Fatalf("a playbook matching no host passed in check mode:\n%s", out)
	}
	if !strings.Contains(out, "matches no host") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	if len(f.playbookInvocations(t)) != 0 {
		t.Error("ansible-playbook ran a pass with no hosts")
	}
}

func TestCheckModePassesExactlyCheckAndDiff(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{mode: "check"})
	if err != nil {
		t.Fatalf("check mode failed: %v\n%s", err, out)
	}
	invs := f.playbookInvocations(t)
	if len(invs) != 1 {
		t.Fatalf("check mode ran ansible-playbook %d times, want 1", len(invs))
	}
	if !strings.Contains(invs[0], "--check\n--diff\n") {
		t.Errorf("check mode did not pass --check --diff:\n%s", invs[0])
	}
}

func TestEnvironmentLinesReachTheChildAndNotItsArgv(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{environment: "BILLET_CLOUDFLARED_TOKEN_NODE_A=tok-SECRET-1\nBILLET_WARP_CONNECTOR_TOKEN_NODE_A=tok-SECRET-2\n"})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	env := readRecorded(t, f.pbEnv)
	for _, want := range []string{"BILLET_CLOUDFLARED_TOKEN_NODE_A=tok-SECRET-1", "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=tok-SECRET-2", "ANSIBLE_HOST_KEY_CHECKING=True"} {
		if !strings.Contains(string(env), want+"\n") {
			t.Errorf("ansible-playbook's environment lacks %q", want)
		}
	}
	args := readRecorded(t, f.pbArgs)
	calls := readRecorded(t, f.calls)
	if strings.Contains(string(args), "SECRET") || strings.Contains(string(calls), "SECRET") {
		t.Errorf("a token reached argv:\n%s\n%s", args, calls)
	}
}

func TestAMalformedEnvironmentLineIsRefused(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{environment: "not a variable\n"})
	if err == nil {
		t.Fatalf("a malformed environment line was accepted:\n%s", out)
	}
	if !strings.Contains(out, "not NAME=value") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
}

func TestTheCredentialsAreWrittenReadOnlyToTheOwnerAndExported(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{sshKey: "-----BEGIN KEY-----\nSECRET-KEY\n-----END KEY-----", appKey: "-----BEGIN RSA PRIVATE KEY-----\nSECRET-APP\n-----END RSA PRIVATE KEY-----"})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	for name, envName := range map[string]string{"billet-ssh-key": "ANSIBLE_PRIVATE_KEY_FILE", "billet-app-key.pem": "BILLET_GITHUB_PRIVATE_KEY_PATH"} {
		info, err := os.Stat(filepath.Join(f.runnerTm, name))
		if err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is mode %o, want 0600", name, info.Mode().Perm())
		}
		env := readRecorded(t, f.pbEnv)
		if !strings.Contains(string(env), envName+"="+filepath.Join(f.runnerTm, name)+"\n") {
			t.Errorf("ansible-playbook's environment lacks %s", envName)
		}
	}
	args := readRecorded(t, f.pbArgs)
	if strings.Contains(string(args), "SECRET") {
		t.Errorf("a key reached argv:\n%s", args)
	}
}

func TestSSHKeyscanIsNeverRun(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	pins := filepath.Join(f.dir, "known_hosts")
	if err := os.WriteFile(pins, []byte("# pins\n10.0.0.1 ssh-ed25519 AAAA1\n10.0.0.2 ssh-ed25519 AAAA2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(t, convergeRun{knownHosts: pins})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	if calls := f.callsOf(t, "ssh-keyscan"); len(calls) != 0 {
		t.Errorf("ssh-keyscan was run: %v", calls)
	}
}

func TestThePinsAreAppendedAndAnOperatorsLineSurvives(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	if err := os.MkdirAll(filepath.Join(f.home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(f.home, ".ssh", "known_hosts")
	if err := os.WriteFile(existing, []byte("github.com ssh-ed25519 AAAAGH\n10.0.0.1 ssh-ed25519 AAAA1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pins := filepath.Join(f.dir, "known_hosts")
	if err := os.WriteFile(pins, []byte("10.0.0.1 ssh-ed25519 AAAA1\n10.0.0.2 ssh-ed25519 AAAA2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(t, convergeRun{knownHosts: pins})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	body := readRecorded(t, existing)
	got := string(body)
	if !strings.Contains(got, "github.com ssh-ed25519 AAAAGH\n") {
		t.Errorf("the operator's line was lost:\n%s", got)
	}
	if strings.Count(got, "10.0.0.1 ssh-ed25519 AAAA1\n") != 1 || !strings.Contains(got, "10.0.0.2 ssh-ed25519 AAAA2\n") {
		t.Errorf("the pins were not appended exactly once each:\n%s", got)
	}
}

func TestReachCloudflareWarpRequiresPins(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{reach: "cloudflare-warp"})
	if err == nil {
		t.Fatalf("reach cloudflare-warp without known-hosts was accepted:\n%s", out)
	}
	if !strings.Contains(out, "needs known-hosts") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
}

func TestAnInventoryThatTurnsHostKeyCheckingOffIsRefused(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	debug := debugLine("cp-1", "10.0.0.1", 22, "-o StrictHostKeyChecking=no") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n"
	out, err := f.run(t, convergeRun{debug: debug})
	if err == nil {
		t.Fatalf("StrictHostKeyChecking=no was accepted:\n%s", out)
	}
	if !strings.Contains(out, "disable or redirect host-key checking") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	if len(f.playbookInvocations(t)) != 0 {
		t.Error("ansible-playbook ran a pass after the refusal")
	}
}

func TestAProxyCommandIsAccepted(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	debug := debugLine("cp-1", "10.0.0.1", 22, `-o ProxyCommand="aws ec2-instance-connect open-tunnel --instance-id %h"`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n"
	out, err := f.run(t, convergeRun{debug: debug})
	if err != nil {
		t.Fatalf("a ProxyCommand was refused: %v\n%s", err, out)
	}
}

// The reference deployment's controller spells ansible_host as a template of
// the address its connector advertises; the rendered value is what is probed.
func TestATemplatedAnsibleHostIsRenderedAndProbed(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	debug := debugLine("cp-1", "10.3.1.117", 2222, "") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n"
	out, err := f.run(t, convergeRun{debug: debug})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	found := false
	for _, c := range f.callsOf(t, "nc") {
		if strings.Contains(c, "10.3.1.117 2222") {
			found = true
		}
	}
	if !found {
		t.Errorf("the rendered address was not probed on the inventory's port: %v", f.callsOf(t, "nc"))
	}
}

func TestAFailingProbeStopsBeforeAnsiblePlaybookRuns(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{ncFail: "10.0.0.2"})
	if err == nil {
		t.Fatalf("a failing probe did not fail the run:\n%s", out)
	}
	if !strings.Contains(out, "No path to node-a at 10.0.0.2:22") {
		t.Errorf("the failure does not name the host and address:\n%s", out)
	}
	if len(f.playbookInvocations(t)) != 0 {
		t.Error("ansible-playbook ran a pass after the probe failed")
	}
}

func TestProveIdempotentCanBeTurnedOff(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{prove: "false", recap2: "unused"})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	if n := len(f.playbookInvocations(t)); n != 1 {
		t.Errorf("ansible-playbook ran %d times with the proof off, want 1", n)
	}
}

// prepare.sh refuses before anything is enrolled or installed.
func TestPrepareRefusesABilletManagedRunnerAndAnUnknownMode(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mode, reach, runner string
		want                string
	}{
		"billet runner": {mode: "check", runner: "billet-lease-abc123", want: "a runner billet itself manages"},
		"unknown mode":  {mode: "apply", want: "mode must be check or converge"},
		"unknown reach": {mode: "check", reach: "ssm", want: "reach must be none or cloudflare-warp"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			output := filepath.Join(t.TempDir(), "output")
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "prepare.sh"))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_OUTPUT=" + output, "BILLET_MODE=" + tc.mode, "BILLET_REACH=" + tc.reach, "BILLET_ACTION_REF=v0.10.0"}
			if tc.runner != "" {
				cmd.Env = append(cmd.Env, "RUNNER_NAME="+tc.runner)
			}
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("prepare accepted %s:\n%s", name, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("the refusal does not say why:\n%s", out)
			}
		})
	}

	t.Run("an ordinary runner passes and reports the ref", func(t *testing.T) {
		t.Parallel()
		output := filepath.Join(t.TempDir(), "output")
		cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "prepare.sh"))
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_OUTPUT=" + output, "BILLET_MODE=converge", "BILLET_REACH=none", "BILLET_ACTION_REF=v0.10.0", "RUNNER_NAME=GitHub Actions 12"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("prepare refused an ordinary runner: %v\n%s", err, out)
		}
		body := readRecorded(t, output)
		if !strings.Contains(string(body), "billet_ref=v0.10.0\n") {
			t.Errorf("the ref was not reported:\n%s", body)
		}
	})
}

func runReach(t *testing.T, f *convergeFixture, connects bool, keyBody string) (string, error) {
	t.Helper()
	fpr := "C068A2B5771775193CBE1F2F6E2DD2174FA1C3BA"
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "reach-cloudflare-warp.sh"))
	cmd.Env = []string{
		"PATH=" + f.bin + ":" + os.Getenv("PATH"),
		"HOME=" + f.home,
		"RUNNER_TEMP=" + f.runnerTm,
		"GITHUB_ACTION_PATH=" + mustAbs(t, "converge-fleet"),
		"BILLET_FAKE_CALLS=" + f.calls,
		"BILLET_FAKE_GOOD_FPR=" + fpr,
		"BILLET_FAKE_KEY_BODY=" + keyBody,
		"BILLET_FAKE_WARP_CONNECTS=" + map[bool]string{true: "yes", false: "no"}[connects],
		"CF_TEAM=example", "CF_CLIENT_ID=id-SECRET", "CF_CLIENT_SECRET=secret-SECRET",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReachVerifiesTheKeyAgainstTheRolesPinAndMarksTheRegistration(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := runReach(t, f, true, "GOOD KEY")
	if err != nil {
		t.Fatalf("reach failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(f.runnerTm, "billet-warp-registered")); err != nil {
		t.Error("the registration marker was not written")
	}
	calls := readRecorded(t, f.calls)
	if strings.Contains(string(calls), "SECRET") {
		t.Errorf("the service token reached argv:\n%s", calls)
	}
	if !strings.Contains(string(calls), "sudo install -m 0600 -o root -g root /dev/stdin /var/lib/cloudflare-warp/mdm.xml") {
		t.Errorf("mdm.xml was not written from stdin with install:\n%s", calls)
	}
}

func TestReachRefusesAKeyWithAnotherFingerprint(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := runReach(t, f, true, "SOMEBODY ELSES KEY")
	if err == nil {
		t.Fatalf("a key with another fingerprint was trusted:\n%s", out)
	}
	if !strings.Contains(out, "not exactly one primary key with the pinned fingerprint") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	calls := readRecorded(t, f.calls)
	if strings.Contains(string(calls), "apt-get install") {
		t.Errorf("the package was installed after a refused key:\n%s", calls)
	}
}

func TestReachTimesOutNamingTheEnrolmentPolicy(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := runReach(t, f, false, "GOOD KEY")
	if err == nil {
		t.Fatalf("a device that never connected passed:\n%s", out)
	}
	if !strings.Contains(out, "enrolment policy") {
		t.Errorf("the failure does not name the enrolment policy:\n%s", out)
	}
}

func runCleanup(t *testing.T, f *convergeFixture, warpExit string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "cleanup.sh"))
	cmd.Env = []string{"PATH=" + f.bin + ":" + os.Getenv("PATH"), "RUNNER_TEMP=" + f.runnerTm, "BILLET_FAKE_CALLS=" + f.calls, "BILLET_FAKE_WARP_EXIT=" + warpExit}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCleanupRemovesOnlyWhatThisRunCreated(t *testing.T) {
	t.Parallel()

	t.Run("reach none touches no registration", func(t *testing.T) {
		t.Parallel()
		f := newConvergeFixture(t)
		key := filepath.Join(f.runnerTm, "billet-ssh-key")
		if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := runCleanup(t, f, "0"); err != nil {
			t.Fatalf("cleanup failed: %v\n%s", err, out)
		}
		if calls := f.callsOf(t, "warp-cli"); len(calls) != 0 {
			t.Errorf("warp-cli was called with no registration of this run's: %v", calls)
		}
		if _, err := os.Stat(key); !os.IsNotExist(err) {
			t.Error("the key file survived cleanup")
		}
	})

	t.Run("a registration this run made is deleted even when warp-cli fails, and the keys go first", func(t *testing.T) {
		t.Parallel()
		f := newConvergeFixture(t)
		for _, name := range []string{"billet-ssh-key", "billet-app-key.pem", "billet-warp-registered"} {
			if err := os.WriteFile(filepath.Join(f.runnerTm, name), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := runCleanup(t, f, "1"); err != nil {
			t.Fatalf("cleanup failed on a failing warp-cli: %v\n%s", err, out)
		}
		calls := f.callsOf(t, "warp-cli")
		joined := strings.Join(calls, "\n")
		if !strings.Contains(joined, "registration delete") {
			t.Errorf("the registration was not deleted: %v", calls)
		}
		for _, name := range []string{"billet-ssh-key", "billet-app-key.pem"} {
			if _, err := os.Stat(filepath.Join(f.runnerTm, name)); !os.IsNotExist(err) {
				t.Errorf("%s survived cleanup", name)
			}
		}
	})
}

// THE PIN CI TESTS THE ROLE AGAINST IS THE PIN THE ACTION INSTALLS. Both read
// one file; a CI workflow that spelled a version of its own would test the
// role on one ansible-core and ship the action on another.
func TestTheActionAndCIPinOneAnsibleCore(t *testing.T) {
	t.Parallel()

	tests := filepath.Join("..", "ansible_collections", "junioryono", "billet", "tests")
	for _, name := range []string{"ansible-core-version", "ansible-posix-version"} {
		body, err := os.ReadFile(filepath.Join(tests, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if v := strings.TrimSpace(string(body)); v == "" || strings.ContainsAny(v, " \n~>=<*") {
			t.Errorf("%s must hold one exact version, got %q", name, v)
		}
	}

	ci, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	if !strings.Contains(string(ci), "ansible_collections/junioryono/billet/tests/ansible-core-version") ||
		!strings.Contains(string(ci), "ansible_collections/junioryono/billet/tests/ansible-posix-version") {
		t.Error("ci.yml does not install ansible-core and ansible.posix from the collection's pin files")
	}
	if strings.Contains(string(ci), "ansible-core==2.") || strings.Contains(string(ci), "ansible.posix:2.") {
		t.Error("ci.yml spells a version of its own beside the pin files")
	}
	install, err := os.ReadFile(filepath.Join("converge-fleet", "install-ansible.sh"))
	if err != nil {
		t.Fatalf("read install-ansible.sh: %v", err)
	}
	if !strings.Contains(string(install), "ansible-core-version") || !strings.Contains(string(install), "ansible-posix-version") {
		t.Error("install-ansible.sh does not read the collection's pin files")
	}
}
