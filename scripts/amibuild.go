// Package scripts carries the part of this directory a billet binary ships: what
// the EC2 builder runs to make an AMI from GitHub's template (ADR-022).
//
// The guest image build runs from a checkout of the release tree; `billet ami
// build` runs from an installed binary on an operator's machine, which has no
// checkout. So the files that build needs are embedded here and staged to the
// builder at the paths they have in the tree, where they find each other.
package scripts

import (
	"embed"
	"io/fs"
	"slices"
)

// amiBuild is every file the EC2 builder runs, at its path under scripts/.
//
//go:embed run-runner-images.sh check-guest-component.sh
//go:embed ami/build-ami.sh ami/ami-target.sh ami/billet-ami-build.service
//go:embed runner-images/upstream/helpers runner-images/upstream/images/ubuntu runner-images/completer
//go:embed runner-images/plan.json runner-images/plan-arm64.json runner-images/differences.tsv
//go:embed runner-images/pam-environment.awk runner-images/write-image-env.sh
//go:embed runner-images/install-completer.sh runner-images/LICENSE
var amiBuild embed.FS

// amiBuildWindows is every file the EC2 builder runs on a Windows build, at its path
// under scripts/ (ADR-023): the entry points and driver billet writes, the Windows tree
// of GitHub's template, the plan derived from it and what billet does differently.
//
//go:embed windows/build-ami.ps1 windows/ec2-layer.ps1 windows/billet-runner.ps1
//go:embed windows/billet-runner-service.ps1 windows/billet-job-started.ps1 windows/prepare-aws-tools.ps1
//go:embed runner-images/upstream/images/windows/assets runner-images/upstream/images/windows/scripts
//go:embed runner-images/plan-windows-2025.json runner-images/differences-windows.tsv
//go:embed runner-images/LICENSE
var amiBuildWindows embed.FS

// WindowsPlan is the plan's path under scripts/ in what AMIBuildWindows returns.
const WindowsPlan = "runner-images/plan-windows-2025.json"

// The entry points' paths under scripts/, which the verifier holds a booted image's
// copies to.
const (
	WindowsRunnerEntry   = "windows/billet-runner.ps1"
	WindowsRunnerService = "windows/billet-runner-service.ps1"
	WindowsJobStarted    = "windows/billet-job-started.ps1"
)

// amiBuildExecutables are the embedded files the tree marks executable. An
// embedded file carries no mode, so the stager is told which they are, and a test
// holds this list to the modes the files have in the tree.
var amiBuildExecutables = []string{
	"ami/ami-target.sh",
	"ami/build-ami.sh",
	"check-guest-component.sh",
	"run-runner-images.sh",
	"runner-images/completer/run",
	"runner-images/install-completer.sh",
	"runner-images/upstream/images/ubuntu/scripts/build/install-copilot-cli.sh",
	"runner-images/write-image-env.sh",
}

// AMIBuildService is the systemd unit's path under scripts/; the builder installs
// it as a unit rather than beside the scripts.
const AMIBuildService = "ami/billet-ami-build.service"

// AMIBuildFile is one file the EC2 builder runs.
type AMIBuildFile struct {
	// Path is the file's path under scripts/, with forward slashes.
	Path       string
	Body       []byte
	Executable bool
}

// AMIBuild is every file the EC2 builder runs, in path order.
func AMIBuild() ([]AMIBuildFile, error) {
	return walkBuild(amiBuild)
}

// AMIBuildWindows is every file the EC2 builder runs on a Windows build, in path
// order. PowerShell reads a script whatever its mode, so none is marked executable.
func AMIBuildWindows() ([]AMIBuildFile, error) {
	return walkBuild(amiBuildWindows)
}

func walkBuild(tree embed.FS) ([]AMIBuildFile, error) {
	var out []AMIBuildFile

	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		body, err := tree.ReadFile(path)
		if err != nil {
			return err
		}

		out = append(out, AMIBuildFile{
			Path:       path,
			Body:       body,
			Executable: slices.Contains(amiBuildExecutables, path),
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
