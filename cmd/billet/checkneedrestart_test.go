package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
)

// needrestartTree plants files under a fresh root, each path relative to it.
func needrestartTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func reportedNeedrestart(root string) string {
	var out bytes.Buffer
	reportNeedrestart(&out, root)

	return out.String()
}

func TestCheckSaysNothingWhereNeedrestartIsNotInstalled(t *testing.T) {
	t.Parallel()

	if got := reportedNeedrestart(needrestartTree(t, nil)); got != "" {
		t.Errorf("a host without needrestart reported %q", got)
	}
}

func TestCheckWarnsWhenNeedrestartIsInstalledWithoutTheExclusion(t *testing.T) {
	t.Parallel()

	for name, files := range map[string]map[string]string{
		"executable alone": {"usr/sbin/needrestart": "#!/bin/sh\n"},
		"stock configuration": {
			"etc/needrestart/needrestart.conf":          "#$nrconf{restart} = 'i';\n",
			"etc/needrestart/conf.d/README.needrestart": deploy.NeedrestartDropIn,
		},
		"exclusion commented out": {
			"etc/needrestart/conf.d/90-billet.conf": "# " + deploy.NeedrestartExclusion + "\n",
		},
		"exclusion in a file needrestart does not load": {
			"etc/needrestart/conf.d/90-billet.conf.bak": deploy.NeedrestartDropIn,
			"etc/needrestart/conf.d/.90-billet.conf":    deploy.NeedrestartDropIn,
		},
	} {
		got := reportedNeedrestart(needrestartTree(t, files))
		if !strings.Contains(got, "WARNING: needrestart is installed and nothing excludes billet's services") ||
			!strings.Contains(got, deploy.NeedrestartDropInPath) {
			t.Errorf("%s: reported %q, not the warning naming %s", name, got, deploy.NeedrestartDropInPath)
		}
	}
}

func TestCheckRecognisesTheExclusionWhereverNeedrestartLoadsIt(t *testing.T) {
	t.Parallel()

	for name, path := range map[string]string{
		"the shipped drop-in": "etc/needrestart/conf.d/90-billet.conf",
		"another drop-in":     "etc/needrestart/conf.d/50-local.conf",
		"the main file":       "etc/needrestart/needrestart.conf",
	} {
		root := needrestartTree(t, map[string]string{path: deploy.NeedrestartDropIn})

		got := reportedNeedrestart(root)
		if got != "restarts needrestart leaves billet's services alone ("+filepath.Join(root, path)+")\n" {
			t.Errorf("%s: reported %q", name, got)
		}
	}
}

// An unreadable configuration is could-not-tell, never "no exclusion": here a
// directory where needrestart would load a drop-in.
func TestCheckCannotTellThroughAnUnreadableConfiguration(t *testing.T) {
	t.Parallel()

	root := needrestartTree(t, map[string]string{"etc/needrestart/needrestart.conf": "\n"})
	if err := os.Mkdir(filepath.Join(root, "etc/needrestart/conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "etc/needrestart/conf.d/90-billet.conf"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := reportedNeedrestart(root)
	if !strings.HasPrefix(got, "restarts could not tell whether needrestart excludes billet's services: ") {
		t.Errorf("an unreadable drop-in reported %q", got)
	}
}
