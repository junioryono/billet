package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gateLiteral is one `grep -Fq [--] '<text>' "$VAR"` in the publication gate. The
// option terminator is captured rather than assumed, because whether it is there
// decides whether the grep runs at all.
var gateLiteral = regexp.MustCompile(`grep -Fq (--)? ?'([^']*)' "\$([A-Z_]+)(/[a-z-]+)?"`)

// gateSubjects maps the gate's file variable to the source of the file it will
// find in a built image.
var gateSubjects = map[string]string{
	"ACTIONS_PROXY":  "../internal/guestassets/actions-proxy.py",
	"DNS_UPSTREAMS":  "../internal/guestassets/dns-upstreams.py",
	"DOCKER_CACHE":   "../internal/guestassets/docker-cache.sh",
	"DOCKER_SHIM":    "../internal/guestassets/docker-shim.sh",
	"CONTAINER_HOOK": "../internal/guestassets/container-hook.js",
	"RUNNER_DIR":     "../internal/guestassets/runner-service.sh",
	"AGENT":          "", // the agent is a heredoc inside build-guest-image.sh
	"IMAGE_ENV_FILE": "",
}

// THE GATE GREPS FOR TEXT THAT LIVES IN ANOTHER FILE, AND NOTHING TOLD IT WHEN
// THAT TEXT MOVED.
//
// check-guest-image.sh is the last thing between a built rootfs and a published
// generation, and every one of its interception checks is a fixed-string grep
// against a script this repository ships. So a rename in the script -- extracting
// one constant, renaming one function -- turns the gate into a check that can
// only fail, and the failure lands on whoever next builds an image with a message
// about a missing feature that is present. It happened while the loopback adapter
// was written: pulling `RESULTS_AUTHORITY` apart into a host plus a port removed
// the literal `results-receiver.actions.githubusercontent.com:443` from the file
// the gate greps, and nothing in `make check` could see it.
//
// This asserts the relationship rather than the text: every literal the gate looks
// for is present in the thing that will carry it.
func TestThePublicationGateGrepsForTextTheImageWillActuallyCarry(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("check-guest-image.sh")
	if err != nil {
		t.Fatalf("read check-guest-image.sh: %v", err)
	}

	bodies := map[string]string{"AGENT": guestAgentBody(t)}
	for variable, source := range gateSubjects {
		if source == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(source))
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		bodies[variable] = string(body)
	}

	checked := map[string]int{}
	for _, match := range gateLiteral.FindAllStringSubmatch(string(raw), -1) {
		terminator, literal, variable := match[1], match[2], match[3]
		// A PATTERN THAT BEGINS WITH A DASH IS AN OPTION, and grep says so:
		// measured, `grep -Fq '--mode x' file` exits 2 with "unrecognized option"
		// on GNU, BSD and ugrep alike. Inside the gate's `&&` chain that is not a
		// mismatch, it is a check that CANNOT PASS -- so every image built after
		// it lands is refused for a feature it carries, which is the failure that
		// gets a gate deleted rather than fixed.
		if strings.HasPrefix(literal, "-") && terminator != "--" {
			t.Errorf("the gate greps $%s for %q without `--`; grep reads that as an option "+
				"and exits 2, so the check can only fail", variable, literal)
		}
		body, known := bodies[variable]
		if !known {
			if _, expected := gateSubjects[variable]; !expected {
				t.Errorf("the gate greps $%s, which this test cannot map to a shipped file; "+
					"add it to gateSubjects so its literals stay checked", variable)
			}

			continue
		}
		checked[variable]++
		if !strings.Contains(body, literal) {
			t.Errorf("check-guest-image.sh greps $%s for %q, which %s does not contain; the "+
				"gate would refuse every image built from it",
				variable, literal, gateSubjects[variable])
		}
	}

	// NON-VACUITY. A regex that stopped matching would report a clean run over
	// nothing, which is the same green as a correct one.
	for _, variable := range []string{"ACTIONS_PROXY", "AGENT", "DOCKER_CACHE"} {
		if checked[variable] == 0 {
			t.Errorf("no $%s literal was extracted from the gate; the pattern no longer matches "+
				"how it is written", variable)
		}
	}
}

// guestAgentBody returns the agent exactly as it is installed: the quoted heredoc
// in the build script, which is the only place that text exists.
func guestAgentBody(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatalf("read build-guest-image.sh: %v", err)
	}

	_, rest, found := strings.Cut(string(raw), "\"$rootfs/usr/local/bin/billet-agent\" <<'AGENT'\n")
	if !found {
		t.Fatal("build-guest-image.sh no longer installs the agent from a quoted AGENT heredoc")
	}
	body, _, found := strings.Cut(rest, "\nAGENT\n")
	if !found {
		t.Fatal("the agent heredoc has no terminator")
	}

	return body
}
