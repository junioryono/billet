package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostedToolsArray is HOSTED_TOOLS as check-guest-image.sh declares it, so a
// fixture is built from the gate's own list rather than a copy of it.
func hostedToolsArray(t *testing.T) (string, []string) {
	t.Helper()

	source := readScriptFile(t, "check-guest-image.sh")

	start := strings.Index(source, "HOSTED_TOOLS=(")
	if start < 0 {
		t.Fatal("check-guest-image.sh declares no HOSTED_TOOLS")
	}

	end := strings.Index(source[start:], "\n)\n")
	if end < 0 {
		t.Fatal("HOSTED_TOOLS is not closed where this test expects")
	}

	declaration := source[start : start+end+2]

	return declaration, strings.Fields(strings.TrimPrefix(strings.TrimSuffix(
		strings.TrimSpace(declaration), ")"), "HOSTED_TOOLS=("))
}

// runHostedGate runs check_hosted_tools against an image rooted at root.
func runHostedGate(t *testing.T, root string) string {
	t.Helper()

	declaration, _ := hostedToolsArray(t)
	script := "#!/usr/bin/env bash\nset -euo pipefail\nFAILED=0\n" +
		"pass() { echo \"ok $*\"; }\nfail() { echo \"FAIL $*\"; FAILED=1; }\n" +
		declaration + "\n" + checkImageFunction(t, "check_hosted_tools") + "\n" +
		"check_hosted_tools \"$1\"\necho \"FAILED=$FAILED\"\n"

	path := filepath.Join(t.TempDir(), "gate.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(t.Context(), "bash", path, root).CombinedOutput()
	if err != nil {
		t.Fatalf("the check did not run to its verdict: %v\n%s", err, out)
	}

	return string(out)
}

// hostedImage is an image root carrying every hosted tool except skip.
func hostedImage(t *testing.T, skip string) string {
	t.Helper()

	_, tools := hostedToolsArray(t)
	root := t.TempDir()

	for _, tool := range append(tools, "/usr/lib/postgresql/16/bin/postgres") {
		if tool == skip {
			continue
		}

		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(tool)), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(root, tool), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if skip != "/opt/actionarchivecache" {
		if err := os.MkdirAll(filepath.Join(root, "opt", "actionarchivecache"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

// THE GATE REFUSES AN IMAGE MISSING ANY ONE TOOL GITHUB'S CARRIES, and names it:
// each is removed in turn, so a tool the list names and the check ignores fails.
func TestTheGateRefusesAnImageMissingAHostedTool(t *testing.T) {
	t.Parallel()

	if out := runHostedGate(t, hostedImage(t, "")); !strings.Contains(out, "FAILED=0\n") {
		t.Fatalf("an image with every hosted tool was refused:\n%s", out)
	}

	_, tools := hostedToolsArray(t)
	if len(tools) < 20 {
		t.Fatalf("HOSTED_TOOLS parsed to %d entries, which is not the list the gate declares", len(tools))
	}

	for _, missing := range append(tools, "/usr/lib/postgresql/16/bin/postgres", "/opt/actionarchivecache") {
		out := runHostedGate(t, hostedImage(t, missing))

		name := missing
		if strings.HasPrefix(missing, "/usr/lib/postgresql/") {
			name = "postgres"
		}

		if !strings.Contains(out, "FAILED=1\n") || !strings.Contains(out, name) {
			t.Errorf("an image without %s was not refused by name:\n%s", missing, out)
		}
	}
}

// AND THE GATE ASKS, AND THE BUILD INSTALLS: a check defined and never called
// passes every image, and an installer never called installs nothing.
func TestTheHostedToolsAreInstalledAndGated(t *testing.T) {
	t.Parallel()

	if !hasExactLine(readScriptFile(t, "check-guest-image.sh"), `check_hosted_tools "$MNT"`) {
		t.Error(`check-guest-image.sh never runs check_hosted_tools "$MNT"`)
	}

	body := guestImageFunction(t, "install_toolcache")
	if !hasExactLine(body, "\tinstall_hosted_tools") {
		t.Error("install_toolcache never calls install_hosted_tools, so none of it is installed")
	}

	hosted := guestImageFunction(t, "install_hosted_tools")
	for _, step := range []string{"install_hosted_packages", "install_hosted_binaries", "install_aws_tools",
		"install_hosted_php_tools", "install_bazelisk", "install_action_cache", "install_agentic_tools",
		"install_hosted_environment"} {
		if !hasExactLine(hosted, "\t"+step) {
			t.Errorf("install_hosted_tools does not call %s", step)
		}
	}
}

// A DIGEST IS ONE HEX STRING OF ITS LENGTH OR A REFUSAL. Several of these are read
// out of release notes, and a note that changed shape must stop the build rather
// than hand fetch_verified an empty or partial expectation.
func TestADigestOfTheWrongShapeIsRefused(t *testing.T) {
	t.Parallel()

	fn := guestImageFunction(t, "billet_tc_hex")

	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{strings.Repeat("a", 64), true},
		{"", false},
		{strings.Repeat("a", 63), false},
		{strings.Repeat("A", 64), false},
		{strings.Repeat("a", 64) + " " + strings.Repeat("b", 64), false},
	} {
		script := "#!/usr/bin/env bash\nset -euo pipefail\n" + fn + "\nbillet_tc_hex \"$1\" 64 fixture\n"

		path := filepath.Join(t.TempDir(), "hex.sh")
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}

		out, err := exec.CommandContext(t.Context(), "bash", path, tc.value).CombinedOutput()
		if tc.ok && (err != nil || string(out) != tc.value) {
			t.Errorf("billet_tc_hex refused a valid digest: %v %q", err, out)
		}

		if !tc.ok && err == nil {
			t.Errorf("billet_tc_hex accepted %q", tc.value)
		}
	}
}
