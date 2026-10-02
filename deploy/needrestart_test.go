package deploy_test

import (
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
)

// TestTheNeedrestartDropInSaysOneThing holds the shipped drop-in to exactly
// one effective line, the exclusion `billet check` looks for. needrestart loads
// it with `defined(do $fn)`, so a trailing statement that evaluates to undef
// would fail every needrestart run on the host; an assignment of 0 does not.
func TestTheNeedrestartDropInSaysOneThing(t *testing.T) {
	t.Parallel()

	var effective []string

	for line := range strings.SplitSeq(deploy.NeedrestartDropIn, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		effective = append(effective, trimmed)
	}

	if len(effective) != 1 || effective[0] != deploy.NeedrestartExclusion {
		t.Fatalf("needrestart.conf's effective lines are %q, want exactly %q",
			effective, deploy.NeedrestartExclusion)
	}

	if !strings.HasSuffix(deploy.NeedrestartDropIn, "\n") {
		t.Error("needrestart.conf does not end in a newline")
	}
}

// TestTheRoleRendersThePackagedNeedrestartDropIn holds the host role's template
// to the package's bytes, so a host converged by the role and one that installed
// the package carry one file, and neither rewrites the other's. The template
// must carry no Jinja delimiter, or its render would differ from its source.
func TestTheRoleRendersThePackagedNeedrestartDropIn(t *testing.T) {
	t.Parallel()

	role, err := os.ReadFile("../ansible_collections/junioryono/billet/roles/host/templates/needrestart-billet.conf.j2")
	if err != nil {
		t.Fatal(err)
	}

	if string(role) != deploy.NeedrestartDropIn {
		t.Errorf("the role's needrestart template differs from deploy/needrestart.conf:\nrole:\n%s\npackage:\n%s",
			role, deploy.NeedrestartDropIn)
	}

	for _, delimiter := range []string{"{{", "{%", "{#"} {
		if strings.Contains(string(role), delimiter) {
			t.Errorf("the role's needrestart template carries the Jinja delimiter %q", delimiter)
		}
	}
}

// TestThePackageSeedsTheNeedrestartDropInItShips ties the three names together:
// the file GoReleaser installs, the template postinstall copies from, and the
// destination it copies to.
func TestThePackageSeedsTheNeedrestartDropInItShips(t *testing.T) {
	t.Parallel()

	release, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(release),
		"- src: deploy/needrestart.conf\n        dst: /usr/share/billet/needrestart.conf\n") {
		t.Error(".goreleaser.yaml does not install deploy/needrestart.conf at /usr/share/billet/needrestart.conf")
	}

	script, err := os.ReadFile("scripts/postinstall.sh")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"\nNEEDRESTART_TEMPLATE=/usr/share/billet/needrestart.conf\n",
		"\nNEEDRESTART_DIR=/etc/needrestart\n",
		"\nNEEDRESTART_DROPIN=\"${NEEDRESTART_DIR}/conf.d/90-billet.conf\"\n",
		"\nif ! install_needrestart_exclusion; then\n",
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("postinstall.sh does not carry %q", strings.TrimSpace(want))
		}
	}

	if deploy.NeedrestartDropInPath != "/etc/needrestart/conf.d/90-billet.conf" {
		t.Errorf("NeedrestartDropInPath is %q, not the path postinstall writes", deploy.NeedrestartDropInPath)
	}
}
