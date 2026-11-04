package scripts_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/junioryono/billet/scripts"
)

// amiBuildRoots are what scripts/amibuild.go embeds, as paths under scripts/.
var amiBuildRoots = []string{
	"run-runner-images.sh", "check-guest-component.sh",
	"ami/build-ami.sh", "ami/ami-target.sh", "ami/billet-ami-build.service",
	"runner-images/upstream", "runner-images/completer",
	"runner-images/plan.json", "runner-images/plan-arm64.json", "runner-images/differences.tsv",
	"runner-images/pam-environment.awk", "runner-images/write-image-env.sh",
	"runner-images/install-completer.sh", "runner-images/LICENSE",
}

// THE EC2 BUILDER IS SENT THE TREE AS IT IS: every file under what the binary
// embeds, the dot and underscore files go:embed would drop from a directory
// included, byte for byte, and executable exactly where the tree says. An embedded
// file carries no mode, so a script the tree marks executable and the list does
// not is one the builder cannot run, hours into a build.
func TestTheAMIBuildEmbedsTheTreeWithItsModes(t *testing.T) {
	t.Parallel()

	files, err := scripts.AMIBuild()
	if err != nil {
		t.Fatalf("AMIBuild: %v", err)
	}

	embedded := map[string]scripts.AMIBuildFile{}
	for _, f := range files {
		embedded[f.Path] = f
	}

	var onDisk []string

	for _, root := range amiBuildRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}

			onDisk = append(onDisk, filepath.ToSlash(path))

			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(onDisk) < 100 {
		t.Fatalf("only %d files under the embedded roots; the walk read nothing", len(onDisk))
	}

	for _, path := range onDisk {
		f, ok := embedded[path]
		if !ok {
			t.Errorf("%s is in the tree and not embedded, so the builder never gets it", path)

			continue
		}

		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		if !bytes.Equal(body, f.Body) {
			t.Errorf("%s is embedded with other bytes than the tree's", path)
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}

		if exec := info.Mode().Perm()&0o111 != 0; exec != f.Executable {
			t.Errorf("%s is executable=%v in the tree and %v to the builder", path, exec, f.Executable)
		}
	}

	for path := range embedded {
		if !slices.Contains(onDisk, path) {
			t.Errorf("%s is embedded and is not under any root this test walks", path)
		}
	}
}
