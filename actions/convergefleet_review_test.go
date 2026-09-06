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
	env := readRecorded(t, f.pbEnv)
	if !strings.Contains(string(env), "mode=converge\n") {
		t.Error("the line did not reach the child's environment, where it is harmless")
	}
}

// A NAME THAT WOULD CHANGE ANSIBLE OR THE RUNNER IS REFUSED BY NAME: the
// host-key policy the action sets cannot be overridden through the input, and
// the refusal names the variable, never its value.
func TestAReservedEnvironmentNameIsRefusedByName(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	for _, line := range []string{"ANSIBLE_HOST_KEY_CHECKING=False", "ANSIBLE_SSH_ARGS=-o StrictHostKeyChecking=no", "PATH=/tmp/evil", "RUNNER_TEMP=/tmp/elsewhere", "GITHUB_OUTPUT=/tmp/out"} {
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
	if !strings.Contains(out, "cp-1 sets stricthostkeychecking") {
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

// A FAILING PASS BEHIND tee FAILS THE RUN: pipefail is what carries the status.
func TestAFailingPassBehindTeeFailsTheRun(t *testing.T) {
	t.Parallel()
	f := newConvergeFixture(t)

	out, err := f.run(t, convergeRun{pbExit: "2"})
	if err == nil {
		t.Fatalf("a failing ansible-playbook behind tee passed:\n%s", out)
	}
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

// THE ENVIRONMENT INPUT REACHES THE SECOND PASS TOO, through the same env(1)
// path; a proof that ran without the tokens would find the connector roles
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
	env := readRecorded(t, f.pbEnv)
	if !strings.Contains(string(env), "BILLET_CLOUDFLARED_TOKEN_NODE_A=tok-SECRET-1\n") {
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
	if !strings.Contains(string(out), "already holds a WARP registration") {
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
