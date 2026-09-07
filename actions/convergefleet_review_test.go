package actions_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// debugLineChecking is debugLine with ansible_host_key_checking rendered too.
func debugLineChecking(host, addr string, checking any) string {
	inner, err := json.Marshal(map[string]any{"host": addr, "port": 22, "common": "", "extra": "", "args": "", "checking": checking})
	if err != nil {
		panic(err)
	}
	outer, err := json.Marshal(map[string]string{"msg": string(inner)})
	if err != nil {
		panic(err)
	}
	return host + " | SUCCESS => " + string(outer)
}

// THE ENVIRONMENT INPUT CANNOT REWRITE A DECISION. A line naming `mode` or
// `inventory` reaches ansible-playbook's environment and nothing else: the
// check stays a check, against the inventory the action was given.
func TestAnEnvironmentLineCannotTurnACheckIntoAConverge(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{mode: "check", environment: "mode=converge\ninventory=/nonexistent/other.yml\nplaybook=other.yml\n"})
	if err != nil {
		t.Fatalf("check failed: %v\n%s", err, out)
	}
	invs := f.playbookInvocations(t)
	if len(invs) != 1 || !strings.Contains(invs[0], "--check\n--diff\n") || strings.Contains(invs[0], "other.yml") {
		t.Fatalf("the environment input changed the run: %d invocations\n%s", len(invs), strings.Join(invs, "\n=====\n"))
	}
	if recordedEnv(t, f.pbEnv)["mode"] != "converge" {
		t.Error("the line did not reach the child's environment, where it is harmless")
	}
}

// A NAME THAT WOULD CHANGE ANSIBLE OR THE RUNNER IS REFUSED BY NAME: the
// host-key policy the action sets cannot be overridden through the input, and
// the refusal names the variable, never its value.
func TestAReservedEnvironmentNameIsRefusedByName(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	for _, line := range []string{"ANSIBLE_HOST_KEY_CHECKING=False", "ANSIBLE_SSH_ARGS=-o StrictHostKeyChecking=no", "PATH=/tmp/evil", "RUNNER_TEMP=/tmp/elsewhere", "GITHUB_OUTPUT=/tmp/out", "SSH_ASKPASS=/tmp/askpass", "SSH_AUTH_SOCK=/tmp/agent", "DISPLAY=:0"} {
		out, err := f.run(t, convergeRun{environment: line + "\n"})
		if err == nil {
			t.Fatalf("%q was accepted:\n%s", line, out)
		}
		name, value, _ := strings.Cut(line, "=")
		if !strings.Contains(out, "sets "+name+",") {
			t.Errorf("the refusal of %q does not name the variable:\n%s", line, out)
		}
		if strings.Contains(out, value) {
			t.Errorf("the refusal of %q printed the value:\n%s", line, out)
		}
		if len(f.playbookInvocations(t)) != 0 {
			t.Errorf("ansible-playbook ran after refusing %q", line)
		}
	}
}

// A MALFORMED LINE IS REPORTED BY NUMBER, because its contents may be a
// credential pasted where a NAME=value line was expected.
func TestAMalformedEnvironmentLineIsReportedWithoutItsContents(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{environment: "BILLET_OK=1\nsk-SECRET-PASTED-VALUE\n"})
	if err == nil {
		t.Fatalf("a malformed line was accepted:\n%s", out)
	}
	if !strings.Contains(out, "line 2 is not NAME=value") {
		t.Errorf("the refusal does not name the line:\n%s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Errorf("the refusal printed the line's contents:\n%s", out)
	}
}

// SSH OPTIONS ARE CASE-INSENSITIVE, and the refusal names the option rather
// than printing the arguments, which may carry a ProxyCommand nobody wants in a
// log.
func TestHostKeyRefusalIsCaseInsensitiveAndNamesOnlyTheOption(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	debug := debugLine("cp-1", "10.0.0.1", 22, `-o ProxyCommand="proxy --token PROXY-SECRET" -o stricthostkeychecking=no`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n"
	out, err := f.run(t, convergeRun{debug: debug})
	if err == nil {
		t.Fatalf("a lowercase stricthostkeychecking=no was accepted:\n%s", out)
	}
	if !strings.Contains(out, "cp-1 sets StrictHostKeyChecking") {
		t.Errorf("the refusal does not name the host and the option:\n%s", out)
	}
	if strings.Contains(out, "PROXY-SECRET") {
		t.Errorf("the refusal printed the SSH arguments:\n%s", out)
	}
}

// ansible_host_key_checking IS THE INVENTORY'S OTHER WAY TO TURN CHECKING OFF.
func TestAnInventoryHostKeyCheckingFalseIsRefused(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	debug := debugLineChecking("cp-1", "10.0.0.1", false) + "\n" + debugLineChecking("node-a", "10.0.0.2", true) + "\n"
	out, err := f.run(t, convergeRun{debug: debug})
	if err == nil {
		t.Fatalf("ansible_host_key_checking: false was accepted:\n%s", out)
	}
	if !strings.Contains(out, "cp-1 sets ansible_host_key_checking") {
		t.Errorf("the refusal does not name the variable:\n%s", out)
	}
	// The SSH plugin's own alias outranks the generic variable, and the render
	// must ask for it first; the fake answers from a fixture, so the recorded
	// render expression is the only evidence the alias is read.
	render := strings.Join(f.callsOf(t, "ansible"), "\n")
	if !strings.Contains(render, "'checking': ansible_ssh_host_key_checking | default(ansible_host_key_checking | default(true))") {
		t.Errorf("the render does not read the SSH plugin's alias before the generic variable:\n%s", render)
	}
}

// NO SHELL ASSIGNMENT SEES A VALUE: a name bash treats specially (RANDOM's
// value is evaluated as arithmetic, where a subscript can run a command) and a
// name that collides with a launcher's own bookkeeping both reach the child as
// plain variables holding exactly their text.
func TestEnvironmentValuesAreNeverEvaluated(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	canary := filepath.Join(f.dir, "canary")
	out, err := f.run(t, convergeRun{environment: "RANDOM=x[$(touch " + canary + ")0]=0\nkv=first\nTOKEN=second\nSECONDS=$(touch " + canary + ")\n"})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("a value in the environment input was evaluated by a shell")
	}
	// RANDOM and SECONDS are bash's own in the fake's shell and are not
	// re-exported by it, so what the record can show is that the canary was
	// never touched and that the ordinary names arrived whole.
	env := recordedEnv(t, f.pbEnv)
	for name, want := range map[string]string{"kv": "first", "TOKEN": "second"} {
		if env[name] != want {
			t.Errorf("the child has %s=%q, want the literal %q", name, env[name], want)
		}
	}
	// NO env(1) EVER CARRIED A VALUE: the recorder in front of the real env
	// logs every argv it was handed.
	for _, call := range strings.Split(string(readRecorded(t, f.calls)), "\n") {
		if strings.HasPrefix(call, "env ") && (strings.Contains(call, "first") || strings.Contains(call, "second")) {
			t.Errorf("a value reached env(1)'s argv: %s", call)
		}
	}
}

// A CARRIAGE RETURN INSIDE A VALUE IS REFUSED BY BOTH VALIDATORS: the shell
// sees one line, and a launcher reading with newline translation would have
// seen two, the second setting PATH. Each validator is tested on its own.
func TestAControlCharacterInsideAValueIsRefused(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{environment: "TOKEN=x\rPATH=" + f.dir + "\nkv=first\n"})
	if err == nil {
		t.Fatalf("a value with a carriage return was accepted:\n%s", out)
	}
	if !strings.Contains(out, "line 1 carries a control character") {
		t.Errorf("the refusal does not name the line:\n%s", out)
	}
	if strings.Contains(out, "PATH=") {
		t.Errorf("the refusal printed the line:\n%s", out)
	}
	if len(f.playbookInvocations(t)) != 0 {
		t.Error("ansible-playbook ran after the refusal")
	}

	file := filepath.Join(f.dir, "child-env")
	if err := os.WriteFile(file, []byte("TOKEN=x\rPATH="+f.dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), realPython3(t), filepath.Join(actionDir(t), "with-environment.py"), file, "--", "true")
	launcherOut, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the launcher accepted a line with a carriage return:\n%s", launcherOut)
	}
	if !strings.Contains(string(launcherOut), "line 1 of") || !strings.Contains(string(launcherOut), "carries a control character") {
		t.Errorf("the launcher's refusal does not name the line:\n%s", launcherOut)
	}
}

// EVERY EXPECTED HOST RENDERS EXACTLY ONCE, or nothing is probed and nothing
// runs: a render that answered for one host of two is a partial probe.
func TestAPartialRenderStopsBeforeAnyProbe(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{debug: debugLine("cp-1", "10.0.0.1", 22, "") + "\n"})
	if err == nil {
		t.Fatalf("a render missing node-a passed:\n%s", out)
	}
	if !strings.Contains(out, "node-a was listed by the playbook but rendered 0 times") {
		t.Errorf("the failure does not name the unrendered host:\n%s", out)
	}
	if calls := f.callsOf(t, "nc"); len(calls) != 0 {
		t.Errorf("a host was probed on a partial render: %v", calls)
	}
	if len(f.playbookInvocations(t)) != 0 {
		t.Error("ansible-playbook ran on a partial render")
	}
}

// A HOST NAME THE PATTERN OR THE RECAP CANNOT CARRY IS REFUSED: an IPv6 literal
// as an inventory name would be read as two hosts by the pattern.
func TestAnInventoryNameWithAPatternSeparatorIsRefused(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	list := strings.Replace(listHostsFixture, "      node-a\n", "      fd00::2\n", 1)
	out, err := f.run(t, convergeRun{listHosts: list})
	if err == nil {
		t.Fatalf("an IPv6 literal as an inventory name was accepted:\n%s", out)
	}
	if !strings.Contains(out, "names a host as 'fd00::2'") {
		t.Errorf("the refusal does not name the host:\n%s", out)
	}
}

// A PINS FILE WITHOUT A FINAL NEWLINE INSTALLS ITS LAST PIN, and a runner's
// known_hosts without one does not get the first pin glued onto its last line.
func TestPinsWithoutFinalNewlinesAreAppendedWhole(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	if err := os.MkdirAll(filepath.Join(f.home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(f.home, ".ssh", "known_hosts")
	if err := os.WriteFile(existing, []byte("github.com ssh-ed25519 AAAAGH"), 0o600); err != nil {
		t.Fatal(err)
	}
	pins := filepath.Join(f.dir, "known_hosts")
	if err := os.WriteFile(pins, []byte("10.0.0.1 ssh-ed25519 AAAA1\r\n10.0.0.2 ssh-ed25519 AAAA2"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(t, convergeRun{knownHosts: pins})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	got := string(readRecorded(t, existing))
	want := "github.com ssh-ed25519 AAAAGH\n10.0.0.1 ssh-ed25519 AAAA1\n10.0.0.2 ssh-ed25519 AAAA2\n"
	if got != want {
		t.Errorf("known_hosts is\n%q\nwant\n%q", got, want)
	}
}

// A FAILING PASS BEHIND tee FAILS THE RUN, on its own: the first pass with no
// proof after it, and a check, so the status is converge.sh's pipefail and not
// prove-idempotent.sh's. The recap is published either way.
func TestAFailingPassBehindTeeFailsTheRun(t *testing.T) {
	t.Parallel()

	for name, run := range map[string]convergeRun{
		"first pass": {pbExit: "2", prove: "false"},
		"check":      {pbExit: "2", mode: "check"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newConvergeFixture(t)
			out, err := f.run(t, run)
			if err == nil {
				t.Fatalf("a failing ansible-playbook behind tee passed:\n%s", out)
			}
			if n := len(f.playbookInvocations(t)); n != 1 {
				t.Errorf("ansible-playbook ran %d times, want 1", n)
			}
			body := string(readRecorded(t, f.output))
			if !strings.Contains(body, "recap<<BILLET_RECAP_EOF") || !strings.Contains(body, "node-a") {
				t.Errorf("the failed pass's recap was not published:\n%s", body)
			}
		})
	}
}

// THE RENDER SEES THE ENVIRONMENT INPUT, as the play will: an inventory that
// reads an SSH option through lookup('env') must render with the same value
// the play runs with, or the judgement is made on different arguments.
func TestTheRenderRunsUnderTheEnvironmentInput(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{environment: "BILLET_CLOUDFLARED_TOKEN_NODE_A=tok-SECRET-1\nFLEET_SSH_OPTS=-o ProxyCommand=x\n"})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	env := recordedEnv(t, filepath.Join(f.dir, "ansible-env"))
	if env["FLEET_SSH_OPTS"] != "-o ProxyCommand=x" {
		t.Errorf("the render did not run under the environment input: %v", env["FLEET_SSH_OPTS"])
	}
	if strings.Contains(string(readRecorded(t, f.calls)), "SECRET") {
		t.Error("a token reached argv")
	}
}

// OPENSSH'S OWN SPELLINGS: `false` is a value, and whitespace around `=` is
// allowed; the SSH plugin's alias outranks the generic variable.
func TestOtherSpellingsOfCheckingOffAreRefused(t *testing.T) {
	t.Parallel()

	for name, debug := range map[string]string{
		"false":          debugLine("cp-1", "10.0.0.1", 22, "-o StrictHostKeyChecking=false") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"spaced":         debugLine("cp-1", "10.0.0.1", 22, `-o "StrictHostKeyChecking = no"`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"ssh alias":      debugLineChecking("cp-1", "10.0.0.1", "False") + "\n" + debugLineChecking("node-a", "10.0.0.2", true) + "\n",
		"checkhostip":    debugLine("cp-1", "10.0.0.1", 22, "-o CheckHostIP=false") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"quoted value":   debugLine("cp-1", "10.0.0.1", 22, `-o StrictHostKeyChecking="no"`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"inner quotes":   debugLine("cp-1", "10.0.0.1", 22, `-o 'StrictHostKeyChecking="no"'`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"tab":            debugLine("cp-1", "10.0.0.1", 22, "-o 'StrictHostKeyChecking\tno'") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"no separator":   debugLine("cp-1", "10.0.0.1", 22, "-oStrictHostKeyChecking=off") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"config file":    debugLine("cp-1", "10.0.0.1", 22, "-F /tmp/ssh_config") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"include":        debugLine("cp-1", "10.0.0.1", 22, "-o Include=/tmp/ssh_config") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"global file":    debugLine("cp-1", "10.0.0.1", 22, "-o GlobalKnownHostsFile=/tmp/keys") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"keys command":   debugLine("cp-1", "10.0.0.1", 22, "-o KnownHostsCommand=/tmp/keys.sh") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"single quotes":  debugLine("cp-1", "10.0.0.1", 22, `-o "StrictHostKeyChecking='no'"`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"fragments":      debugLine("cp-1", "10.0.0.1", 22, `-o 'StrictHostKeyChecking=n"o"'`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"comment":        debugLine("cp-1", "10.0.0.1", 22, `-o 'StrictHostKeyChecking=no # comment'`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"cluster":        debugLine("cp-1", "10.0.0.1", 22, "-4oStrictHostKeyChecking=no") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"cluster file":   debugLine("cp-1", "10.0.0.1", 22, "-4F/tmp/ssh_config") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"localhost":      debugLine("cp-1", "10.0.0.1", 22, "-o NoHostAuthenticationForLocalhost=yes") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"dns":            debugLine("cp-1", "10.0.0.1", 22, "-o VerifyHostKeyDNS=yes") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"alias":          debugLine("cp-1", "10.0.0.1", 22, "-o HostKeyAlias=other") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"quoted keyword": debugLine("cp-1", "10.0.0.1", 22, `-o '"StrictHostKeyChecking"no # comment'`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"split keyword":  debugLine("cp-1", "10.0.0.1", 22, `-o 'Strict"Host"KeyChecking=no'`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"control path":   debugLine("cp-1", "10.0.0.1", 22, "-o ControlPath=/tmp/master -o ControlMaster=auto") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"control master": debugLine("cp-1", "10.0.0.1", 22, "-o ControlMaster=auto") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"leading space":  debugLine("cp-1", "10.0.0.1", 22, `' -oStrictHostKeyChecking=no'`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"spaced socket":  debugLine("cp-1", "10.0.0.1", 22, `' -oControlPath=/tmp/master'`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
		"gssapi":         debugLine("cp-1", "10.0.0.1", 22, "-o GSSAPIKeyExchange=yes") + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newConvergeFixture(t)
			out, err := f.run(t, convergeRun{debug: debug})
			if err == nil {
				t.Fatalf("%s was accepted:\n%s", name, out)
			}
			// THE REFUSAL NAMES THE OPTION THAT CAUSED IT, so a walker that
			// answered "cannot read" for every quoted spelling would fail here.
			want, ok := refusedOption[name]
			if !ok || want == "" {
				t.Fatalf("%s has no canonical refusal in refusedOption, so its assertion would be vacuous", name)
			}
			if !strings.Contains(out, "cp-1 sets "+want) {
				t.Errorf("the refusal does not name %s:\n%s", want, out)
			}
			if len(f.playbookInvocations(t)) != 0 {
				t.Error("ansible-playbook ran after the refusal")
			}
		})
	}
}

// refusedOption is the canonical name each spelling above must be refused by.
var refusedOption = map[string]string{
	"false":          "StrictHostKeyChecking",
	"spaced":         "StrictHostKeyChecking",
	"ssh alias":      "ansible_host_key_checking (or its ansible_ssh_ alias)",
	"checkhostip":    "CheckHostIP",
	"quoted value":   "StrictHostKeyChecking",
	"inner quotes":   "StrictHostKeyChecking",
	"tab":            "StrictHostKeyChecking",
	"no separator":   "StrictHostKeyChecking",
	"config file":    "-F (an ssh flag the action does not interpret",
	"include":        "Include",
	"global file":    "GlobalKnownHostsFile",
	"keys command":   "KnownHostsCommand",
	"single quotes":  "StrictHostKeyChecking",
	"fragments":      "StrictHostKeyChecking",
	"comment":        "StrictHostKeyChecking",
	"cluster":        "-4 (an ssh flag the action does not interpret",
	"cluster file":   "-4 (an ssh flag the action does not interpret",
	"localhost":      "NoHostAuthenticationForLocalhost",
	"dns":            "VerifyHostKeyDNS",
	"alias":          "HostKeyAlias",
	"quoted keyword": "StrictHostKeyChecking",
	"split keyword":  "an SSH option the action cannot read",
	"control path":   "ControlPath",
	"control master": "ControlMaster",
	"leading space":  "StrictHostKeyChecking",
	"spaced socket":  "ControlPath",
	"gssapi":         "GSSAPIKeyExchange",
}

// THE READER DECODES AS OPENSSH DOES, observed directly: the policy never
// compares an IdentityFile, so an escape rule dropped from the reader would
// leave every policy case green while ssh read a different file.
func TestTheSSHOptionReaderDecodesAsOpenSSHDoes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ opt, want string }{
		{`IdentityFile=key\ name`, "identityfile\tkey name"},
		{`StrictHostKeyChecking=n"o"`, "stricthostkeychecking\tno"},
		{`StrictHostKeyChecking='no'`, "stricthostkeychecking\tno"},
		{`StrictHostKeyChecking=no # comment`, "stricthostkeychecking\tno"},
		{`StrictHostKeyChecking=no#comment`, "stricthostkeychecking\tno#comment"},
		{`"StrictHostKeyChecking"no`, "stricthostkeychecking\tno"},
		{"StrictHostKeyChecking\tno", "stricthostkeychecking\tno"},
		{`IdentityFile="key\"name"`, "identityfile\tkey\"name"},
		{`IdentityFile=key\name`, "identityfile\tkey\\name"},
		{`ForwardX11=no`, "forwardx11\tno"},
		{`Strict"Host"KeyChecking=no`, "unreadable"},
		{`IdentityFile="open`, "unreadable"},
		{`=no`, "unreadable"},
	} {
		out, err := exec.CommandContext(t.Context(), realPython3(t), filepath.Join(actionDir(t), "judge-ssh-options.py"), "--decode", tc.opt).CombinedOutput()
		if err != nil {
			t.Fatalf("decode %q: %v\n%s", tc.opt, err, out)
		}
		if got := strings.TrimSuffix(string(out), "\n"); got != tc.want {
			t.Errorf("decode %q = %q, want %q", tc.opt, got, tc.want)
		}
	}
}

// QUOTED VALUES THAT KEEP CHECKING ON ARE ACCEPTED, escaped quotes, an escaped
// space and digit-bearing keywords included: a walker that refused every quoted
// spelling as unreadable, or every keyword that is not letters, would pass the
// table above and fail here.
func TestQuotedValuesThatKeepCheckingOnAreAccepted(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	debug := debugLine("cp-1", "10.0.0.1", 22, `-o 'IdentityFile="key\"name"' -o "StrictHostKeyChecking='yes'" -o VerifyHostKeyDNS=no -o 'ProxyCommand=nc -w -1 %h %p' -o ForwardX11=no -o PKCS11Provider=/opt/p11.so -o 'IdentityFile=key\ name'`) + "\n" + debugLine("node-a", "10.0.0.2", 22, "") + "\n"
	out, err := f.run(t, convergeRun{debug: debug})
	if err != nil {
		t.Fatalf("quoted values that keep checking on were refused: %v\n%s", err, out)
	}
}

// A CLIENT WHOSE REGISTRATION STATE COULD NOT BE READ IS NOT A CLIENT WITH NONE:
// a stopped daemon still has its registration on disk.
func TestReachRefusesWhenTheRegistrationStateCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "reach-cloudflare-warp.sh"))
	cmd.Env = []string{
		"PATH=" + f.bin + ":" + os.Getenv("PATH"),
		"HOME=" + f.home,
		"RUNNER_TEMP=" + f.runnerTm,
		"GITHUB_ACTION_PATH=" + actionDir(t),
		"BILLET_FAKE_CALLS=" + f.calls,
		"BILLET_FAKE_GOOD_FPR=C068A2B5771775193CBE1F2F6E2DD2174FA1C3BA",
		"BILLET_FAKE_KEY_BODY=GOOD KEY",
		"BILLET_FAKE_WARP_REGISTERED=error",
		"CF_TEAM=example", "CF_CLIENT_ID=id-SECRET", "CF_CLIENT_SECRET=secret-SECRET",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an unreadable registration state was taken as none:\n%s", out)
	}
	if !strings.Contains(string(out), "registration state is not 'missing'") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(f.runnerTm, "billet-warp-registered")); err == nil {
		t.Error("a marker was written without proof there was no registration")
	}
}

// THE MARKER OUTLIVES A HALF-DONE CLEANUP, and a retry that finds the
// registration already gone finishes the job.
func TestCleanupKeepsItsMarkerUntilBothTheRegistrationAndTheTokenAreGone(t *testing.T) {
	t.Parallel()

	t.Run("token removal fails", func(t *testing.T) {
		t.Parallel()
		f := newConvergeFixture(t)
		if err := os.WriteFile(filepath.Join(f.runnerTm, "billet-warp-registered"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := runCleanup(t, f, "0", "BILLET_FAKE_SUDO_RM_FAIL=yes")
		if err == nil {
			t.Fatalf("cleanup reported success with the token file left behind:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(f.runnerTm, "billet-warp-registered")); err != nil {
			t.Error("the marker was removed although the token file stayed")
		}
	})

	t.Run("a retry meets a registration already gone", func(t *testing.T) {
		t.Parallel()
		f := newConvergeFixture(t)
		if err := os.WriteFile(filepath.Join(f.runnerTm, "billet-warp-registered"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := runCleanup(t, f, "0", "BILLET_FAKE_WARP_DELETE_SAYS=missing")
		if err != nil {
			t.Fatalf("a retry over a gone registration failed: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(f.runnerTm, "billet-warp-registered")); !os.IsNotExist(err) {
			t.Error("the marker survived a cleanup that left nothing behind")
		}
	})
}

// A RECAP ROW MISSING A COUNTER IS NOT A CLEAN ROW, and a row for a host the
// playbook did not list is a converge of something unexpected.
func TestARecapWithAMissingCounterOrAnExtraHostFails(t *testing.T) {
	t.Parallel()

	t.Run("missing counter", func(t *testing.T) {
		t.Parallel()
		f := newConvergeFixture(t)
		recap2 := strings.Replace(cleanRecap("cp-1", "node-a"), "node-a                     : ok=12   changed=0    unreachable=0    failed=0    skipped=3    rescued=0    ignored=0   \n", "node-a                     : ok=12   changed=0    unreachable=0    failed=0    skipped=3\n", 1)
		out, err := f.run(t, convergeRun{recap2: recap2})
		if err == nil {
			t.Fatalf("a row without rescued and ignored passed:\n%s", out)
		}
		if !strings.Contains(out, "node-a reports rescued 0 times") {
			t.Errorf("the failure does not name the missing counter:\n%s", out)
		}
	})

	t.Run("extra host", func(t *testing.T) {
		t.Parallel()
		f := newConvergeFixture(t)
		out, err := f.run(t, convergeRun{recap2: cleanRecap("cp-1", "node-a", "stranger")})
		if err == nil {
			t.Fatalf("a row for an unlisted host passed:\n%s", out)
		}
		if !strings.Contains(out, "stranger was converged but the playbook listing did not include it") {
			t.Errorf("the failure does not name the stranger:\n%s", out)
		}
	})
}

// THE ENVIRONMENT INPUT REACHES THE SECOND PASS TOO, through the same
// launcher; a proof that ran without the tokens would find the connector roles
// skipping and call that idempotent.
func TestTheSecondPassCarriesTheEnvironmentInput(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{environment: "BILLET_CLOUDFLARED_TOKEN_NODE_A=tok-SECRET-1\n"})
	if err != nil {
		t.Fatalf("converge failed: %v\n%s", err, out)
	}
	// The fake overwrites its environment record on every call, so what is
	// left is the second pass's.
	if recordedEnv(t, f.pbEnv)["BILLET_CLOUDFLARED_TOKEN_NODE_A"] != "tok-SECRET-1" {
		t.Error("the second pass did not carry the environment input")
	}
	if n := len(f.playbookInvocations(t)); n != 2 {
		t.Fatalf("ansible-playbook ran %d times, want 2", n)
	}
}

// install-ansible.sh CONSUMES THE PINS: the venv's pip is asked for exactly the
// ansible-core the file names and galaxy for exactly the ansible.posix, which
// a substring check on the script's text could not prove.
func TestInstallAnsibleConsumesThePins(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	tests := filepath.Join("..", "ansible_collections", "junioryono", "billet", "tests")
	core := strings.TrimSpace(string(readRecorded(t, filepath.Join(tests, "ansible-core-version"))))
	posix := strings.TrimSpace(string(readRecorded(t, filepath.Join(tests, "ansible-posix-version"))))
	if core == "" || posix == "" {
		t.Fatal("the pin files are empty")
	}
	ghPath := filepath.Join(f.dir, "github-path")
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "install-ansible.sh"))
	cmd.Env = []string{
		"PATH=" + f.bin + ":" + os.Getenv("PATH"),
		"RUNNER_TEMP=" + f.runnerTm,
		"GITHUB_PATH=" + ghPath,
		"GITHUB_ACTION_PATH=" + actionDir(t),
		"BILLET_FAKE_CALLS=" + f.calls,
		"BILLET_REAL_PYTHON3=" + realPython3(t),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install-ansible.sh failed: %v\n%s", err, out)
	}
	calls := string(readRecorded(t, f.calls))
	if !strings.Contains(calls, "ansible-core=="+core+"\n") {
		t.Errorf("pip was not asked for ansible-core==%s:\n%s", core, calls)
	}
	if !strings.Contains(calls, "ansible.posix:"+posix+"\n") {
		t.Errorf("galaxy was not asked for ansible.posix:%s:\n%s", posix, calls)
	}
	if body := string(readRecorded(t, ghPath)); !strings.Contains(body, filepath.Join(f.runnerTm, "billet-ansible", "bin")+"\n") {
		t.Errorf("the venv's bin was not put on GITHUB_PATH:\n%s", body)
	}
}

// A RUNNER THAT ALREADY HOLDS A REGISTRATION IS REFUSED before anything is
// written: cleanup would otherwise delete somebody's registration.
func TestReachRefusesARunnerThatAlreadyHoldsARegistration(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join("converge-fleet", "reach-cloudflare-warp.sh"))
	cmd.Env = []string{
		"PATH=" + f.bin + ":" + os.Getenv("PATH"),
		"HOME=" + f.home,
		"RUNNER_TEMP=" + f.runnerTm,
		"GITHUB_ACTION_PATH=" + actionDir(t),
		"BILLET_FAKE_CALLS=" + f.calls,
		"BILLET_FAKE_GOOD_FPR=C068A2B5771775193CBE1F2F6E2DD2174FA1C3BA",
		"BILLET_FAKE_KEY_BODY=GOOD KEY",
		"BILLET_FAKE_WARP_REGISTERED=yes",
		"CF_TEAM=example", "CF_CLIENT_ID=id-SECRET", "CF_CLIENT_SECRET=secret-SECRET",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a runner with a registration was enrolled:\n%s", out)
	}
	if !strings.Contains(string(out), "registration state is not 'missing'") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	calls := string(readRecorded(t, f.calls))
	if strings.Contains(calls, "install -m 0600") || strings.Contains(calls, "apt-get install") {
		t.Errorf("something was written after the refusal:\n%s", calls)
	}
	if _, err := os.Stat(filepath.Join(f.runnerTm, "billet-warp-registered")); err == nil {
		t.Error("a marker was written for a registration this run did not make")
	}
}

// CHECK MODE PUBLISHES THE RECAP TOO.
func TestCheckModePublishesTheRecap(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{mode: "check"})
	if err != nil {
		t.Fatalf("check failed: %v\n%s", err, out)
	}
	body := string(readRecorded(t, f.output))
	if !strings.Contains(body, "recap<<BILLET_RECAP_EOF") || !strings.Contains(body, "node-a") {
		t.Errorf("check mode wrote no recap output:\n%s", body)
	}
}
