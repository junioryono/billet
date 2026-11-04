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
		declaration + "\n" + checkImageFunction(t, "image_resolve") + "\n" +
		checkImageFunction(t, "image_executable") + "\n" + checkImageFunction(t, "check_hosted_tools") + "\n" +
		"check_hosted_tools \"$1\"\necho \"FAILED=$FAILED\"\n"

	path := filepath.Join(t.TempDir(), "gate.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(t.Context(), "bash", path, root).CombinedOutput()
	if err != nil {
		t.Fatalf("the check did not run to its verdict: %v\n%s", err, out)
	}

	// A HELPER THE HARNESS FORGOT reads as every tool missing, which would satisfy
	// every refusal below for the wrong reason.
	if strings.Contains(string(out), "command not found") {
		t.Fatalf("the gate called something the harness does not define:\n%s", out)
	}

	return string(out)
}

// extras are the non-command parts check_hosted_tools asks for, by the name a
// missing one is reported under.
var hostedExtras = []string{
	"postgres", "action archive cache", "ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE",
	"USE_BAZEL_FALLBACK_VERSION", "firewall bundle", "Copilot CLI",
	"AZURE_EXTENSION_DIR", "GOROOT_",
}

// hostedDamage are ways an entry can be present and still unusable, each with
// the name the gate reports it under.
var hostedDamage = map[string]string{
	"empty archive directory":    "action archive cache",
	"firewall without .complete": "firewall bundle",
	"empty firewall bundle":      "firewall bundle",
	"copilot without .complete":  "Copilot CLI",
	"copilot not executable":     "Copilot CLI",
	"link through a missing dir": "/usr/local/bin/aws",
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// hostedImage is an image root carrying everything check_hosted_tools asks for
// except skip, a HOSTED_TOOLS path or one of hostedExtras. /usr/local/bin/aws is
// an absolute symlink through a directory symlink, as the AWS CLI installs it.
func hostedImage(t *testing.T, skip string) string {
	t.Helper()

	_, tools := hostedToolsArray(t)
	root := t.TempDir()

	for _, tool := range tools {
		if tool == skip || tool == "/usr/local/bin/aws" {
			continue
		}

		writeFile(t, filepath.Join(root, tool), "#!/bin/sh\n", 0o755)
	}

	if skip == "link through a missing dir" {
		writeFile(t, filepath.Join(root, "usr", "bin", "real-aws"), "#!/bin/sh\n", 0o755)

		if err := os.Symlink("/missing/../usr/bin/real-aws", filepath.Join(root, "usr", "local", "bin", "aws")); err != nil {
			t.Fatal(err)
		}
	} else if skip != "/usr/local/bin/aws" {
		writeFile(t, filepath.Join(root, "usr", "local", "aws-cli", "v2", "2.99.0", "bin", "aws"), "#!/bin/sh\n", 0o755)

		if err := os.Symlink("/usr/local/aws-cli/v2/2.99.0", filepath.Join(root, "usr", "local", "aws-cli", "v2", "current")); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink("/usr/local/aws-cli/v2/current/bin/aws", filepath.Join(root, "usr", "local", "bin", "aws")); err != nil {
			t.Fatal(err)
		}
	}

	if skip != "postgres" {
		writeFile(t, filepath.Join(root, "usr", "lib", "postgresql", "16", "bin", "postgres"), "#!/bin/sh\n", 0o755)
	}

	switch skip {
	case "action archive cache":
	case "empty archive directory":
		if err := os.MkdirAll(filepath.Join(root, "opt", "actionarchivecache", "actions_checkout"), 0o755); err != nil {
			t.Fatal(err)
		}
	default:
		writeFile(t, filepath.Join(root, "opt", "actionarchivecache", "actions_checkout", "v4.tar.gz"), "x", 0o644)
	}

	env := "JAVA_HOME=/usr/lib/jvm/x\n"
	if skip != "ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE" {
		env += "ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE=/opt/actionarchivecache\n"
	}

	if skip != "USE_BAZEL_FALLBACK_VERSION" {
		env += "USE_BAZEL_FALLBACK_VERSION=silent:9.1.1\n"
	}

	if skip != "AZURE_EXTENSION_DIR" {
		env += "AZURE_EXTENSION_DIR=/opt/az/azcliextensions\n"
	}

	if skip != "GOROOT_" {
		env += "GOROOT_1_24_X64=/opt/hostedtoolcache/go/1.24.13/x64\n"
	}

	writeFile(t, filepath.Join(root, "etc", "billet-image-env"), env, 0o644)

	awf := filepath.Join(root, "opt", "hostedtoolcache", "agentic-workflow-firewall-js", "0.1.0")
	switch skip {
	case "firewall bundle":
	case "empty firewall bundle":
		writeFile(t, filepath.Join(awf, "x64", "awf-bundle.js"), "", 0o644)
		writeFile(t, filepath.Join(awf, "x64.complete"), "", 0o644)
	case "firewall without .complete":
		writeFile(t, filepath.Join(awf, "x64", "awf-bundle.js"), "x", 0o644)
	default:
		writeFile(t, filepath.Join(awf, "x64", "awf-bundle.js"), "x", 0o644)
		writeFile(t, filepath.Join(awf, "x64.complete"), "", 0o644)
	}

	copilot := filepath.Join(root, "opt", "hostedtoolcache", "copilot-cli", "1.0.0")
	switch skip {
	case "Copilot CLI":
	case "copilot without .complete":
		writeFile(t, filepath.Join(copilot, "x64", "bin", "copilot"), "x", 0o755)
	case "copilot not executable":
		writeFile(t, filepath.Join(copilot, "x64", "bin", "copilot"), "x", 0o644)
		writeFile(t, filepath.Join(copilot, "x64.complete"), "", 0o644)
	default:
		writeFile(t, filepath.Join(copilot, "x64", "bin", "copilot"), "x", 0o755)
		writeFile(t, filepath.Join(copilot, "x64.complete"), "", 0o644)
	}

	return root
}

// THE GATE REFUSES AN IMAGE MISSING ANY ONE THING GITHUB'S CARRIES, and names it:
// each is removed in turn, so an entry the gate lists and never checks fails here.
func TestTheGateRefusesAnImageMissingAHostedTool(t *testing.T) {
	t.Parallel()

	if out := runHostedGate(t, hostedImage(t, "")); !strings.Contains(out, "FAILED=0\n") {
		t.Fatalf("an image with every hosted tool was refused:\n%s", out)
	}

	_, tools := hostedToolsArray(t)
	if len(tools) < 20 {
		t.Fatalf("HOSTED_TOOLS parsed to %d entries, which is not the list the gate declares", len(tools))
	}

	for _, missing := range append(tools, hostedExtras...) {
		out := runHostedGate(t, hostedImage(t, missing))

		if !strings.Contains(out, "FAILED=1\n") || !strings.Contains(out, missing) {
			t.Errorf("an image without %s was not refused by name:\n%s", missing, out)
		}
	}

	for damage, name := range hostedDamage {
		out := runHostedGate(t, hostedImage(t, damage))

		if !strings.Contains(out, "FAILED=1\n") || !strings.Contains(out, name) {
			t.Errorf("an image with %s was not refused as %s:\n%s", damage, name, out)
		}
	}
}

// A SYMLINK IS RESOLVED INSIDE THE IMAGE. An absolute link to a path the image
// lacks must fail even when the machine running the gate has that path.
func TestTheGateDoesNotFollowALinkOutOfTheImage(t *testing.T) {
	t.Parallel()

	root := hostedImage(t, "/usr/local/bin/aws")
	if err := os.Symlink("/bin/sh", filepath.Join(root, "usr", "local", "bin", "aws")); err != nil {
		t.Fatal(err)
	}

	out := runHostedGate(t, root)
	if !strings.Contains(out, "FAILED=1\n") || !strings.Contains(out, "/usr/local/bin/aws") {
		t.Errorf("an aws linked to the host's /bin/sh passed:\n%s", out)
	}
}

// AND THE GATE ASKS: a check defined and never called passes every image.
func TestTheHostedToolsAreGated(t *testing.T) {
	t.Parallel()

	if !hasExactLine(readScriptFile(t, "check-guest-image.sh"), `check_hosted_tools "$MNT"`) {
		t.Error(`check-guest-image.sh never runs check_hosted_tools "$MNT"`)
	}
}
