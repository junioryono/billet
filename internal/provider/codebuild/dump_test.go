package codebuild

import (
	"os"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// TestDumpBuildspec writes the real generated pre_build script to a file, so it can be
// executed on a real builder image rather than only under the shell this suite runs on.
//
// IT EXISTS BECAUSE A PROBE ON THE DEVELOPMENT MAC IS NOT EVIDENCE ABOUT THE BUILDER,
// which is a rule this repository learned four times in one piece of work. Everything
// else in this file runs under `/bin/sh` on whatever machine `go test` was invoked from;
// that is the right instrument for the shape of the script and it cannot speak for the
// image AWS actually boots. Two of the defects in this file's history were platform
// splits — `command -v` resolving differently, and `pwd -P` answering `/` under dash and
// `//` under macOS `/bin/sh` — and neither was visible from one platform.
//
// It skips unless BILLET_DUMP_BUILDSPEC names a path, the same shape as the real-tart
// and real-codebuild suites. To use it:
//
//	BILLET_DUMP_BUILDSPEC=/tmp/prebuild.sh go test -run TestDumpBuildspec ./internal/provider/codebuild/
//	docker run --rm -v /tmp/prebuild.sh:/prebuild.sh:ro \
//	  public.ecr.aws/amazonlinux/amazonlinux:2023 \
//	  sh -c 'dnf install -y tar gzip >/dev/null; HOME=/ sh /prebuild.sh'
//
// EVERY GUARD WAS RUN THAT WAY, on `amazonlinux:2023`, with no `set -e` — which is the
// condition the gates have to hold under, since CodeBuild runs the commands rather than
// a script billet controls. A mismatched tarball exited 1 and installed nothing; an
// unset HOME, a RELATIVE HOME naming an existing directory, `HOME=/`, `HOME=//` and a
// symlinked `.billet` were each refused with their own message and left the redirect
// target intact; a correctly checksummed tarball exited 0 and installed the runner; and
// a file left by a previous build did not survive into the directory the runner runs
// out of.
func TestDumpBuildspec(t *testing.T) {
	out := os.Getenv("BILLET_DUMP_BUILDSPEC")
	if out == "" {
		t.Skip("set BILLET_DUMP_BUILDSPEC to write the generated pre_build script")
	}

	p := providerFor(t, config.CodeBuildLinuxContainer)

	body, err := p.Buildspec(Spec{Name: "billet-abc", Command: []string{"./run.sh"}})
	if err != nil {
		t.Fatalf("Buildspec: %v", err)
	}

	doc := parseBuildspec(t, body)

	var script string

	for _, c := range doc.Phases[buildspecPhase].Commands {
		script += c + "\n"
	}

	if err := os.WriteFile(out, []byte(script), 0o600); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
}
