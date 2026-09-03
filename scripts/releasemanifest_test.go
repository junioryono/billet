package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE MANIFEST IS PUBLISHED BY GORELEASER, NOT UPLOADED AFTERWARDS.
//
// Release immutability freezes a release's assets the moment it is published, so
// anything a deployment needs in order to verify and install that release has to
// exist BEFORE GoReleaser publishes. `gh release upload` after the fact is
// refused — correctly, and by the property the release pipeline exists to have —
// so the manifest is produced in the `signs` stage and uploaded through
// `release.extra_files`.
//
// A STRUCTURAL TEST BECAUSE THE FAILURE IS SILENT IN THE WRONG DIRECTION. A
// manifest that is built but never uploaded produces a green release with a
// channel pointing at a tag containing nothing to verify, and the first thing
// that notices is a deployment that can no longer update.
func TestTheReleaseManifestIsPublishedWithTheRelease(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read the goreleaser config: %v", err)
	}

	var doc struct {
		Signs []struct {
			ID        string   `yaml:"id"`
			Artifacts string   `yaml:"artifacts"`
			Cmd       string   `yaml:"cmd"`
			Args      []string `yaml:"args"`
			Signature string   `yaml:"signature"`
		} `yaml:"signs"`
		Release struct {
			ExtraFiles []struct {
				Glob string `yaml:"glob"`
			} `yaml:"extra_files"`
		} `yaml:"release"`
	}

	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse the goreleaser config: %v", err)
	}

	var step, sig string

	for _, s := range doc.Signs {
		if s.ID != "release-manifest" {
			continue
		}

		step = strings.Join(s.Args, " ")
		sig = s.Signature

		// ONCE, OVER ONE ARTIFACT. The manifest describes the whole release, so
		// running it per published file would rebuild the same document N times
		// and race on the output.
		if s.Artifacts != "checksum" {
			t.Errorf("the manifest step runs over %q artifacts; it must run once", s.Artifacts)
		}
	}

	if step == "" {
		t.Fatal(".goreleaser.yaml has no release-manifest signing step, so no manifest is " +
			"produced inside the window before the release is frozen")
	}

	for _, want := range []string{
		"./scripts/mkreleasemanifest",
		"cosign sign-blob",
		// WITHOUT THIS, THE SIGNATURE IS UNREADABLE. imagesource's verifier cannot
		// parse cosign's legacy bundle, and that format carries no inclusion proof
		// either — so the signature is genuine and unverifiable at once, which
		// reads to an operator exactly like an attack.
		"--new-bundle-format",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("the manifest step does not run %q:\n%s", want, step)
		}
	}

	// A SNAPSHOT WRITES THE MANIFEST AND DOES NOT SIGN IT, and both halves matter.
	//
	// Keyless signing needs an OIDC identity that exists only in the tagged release
	// workflow, so a snapshot cannot produce a signature whatever is installed —
	// and with the two commands unconditionally joined, `make dist` failed on
	// `cosign: not found` for a signature that could never have been valid. That
	// took the package-lifecycle job with it, which builds real .deb and .rpm
	// packages and never gets near a signature.
	//
	// Generating the manifest must stay OUTSIDE the guard: it is the half a local
	// build can actually check, and moving it inside would mean nothing exercised
	// the generator until a tag was cut.
	cosign := strings.Index(step, "cosign sign-blob")
	guard := strings.Index(step, "{{ if not .IsSnapshot }}")

	switch {
	case guard < 0:
		t.Error("the manifest step signs unconditionally, so `make dist` and the " +
			"package-lifecycle job fail on `cosign: not found` for a signature a " +
			"snapshot could never have produced")
	case cosign < guard:
		t.Error("the cosign call is outside the snapshot guard, so a snapshot still " +
			"tries to sign")
	case strings.Index(step, "./scripts/mkreleasemanifest") > guard:
		t.Error("generating the manifest is inside the snapshot guard, so nothing " +
			"exercises the generator until a tag is cut")
	}

	published := map[string]bool{}
	for _, f := range doc.Release.ExtraFiles {
		published[f.Glob] = true
	}

	if !published["dist/release-manifest.json"] {
		t.Error("dist/release-manifest.json is not in release.extra_files, so it is " +
			"built and never uploaded; a deployment following a channel would find a " +
			"release with nothing in it to verify")
	}

	// THE BUNDLE IS PUBLISHED AS THE SIGNATURE ARTIFACT, NOT AS AN EXTRA FILE, and
	// naming it in both would upload one asset name twice.
	if published["dist/release-manifest.sigstore.json"] {
		t.Error("the bundle is in release.extra_files AND named by signs.signature, " +
			"so the release would upload the same asset twice")
	}

	// THE FIELD WHOSE ABSENCE BROKE THE RELEASE BUTTON. goreleaser looks for a
	// signature file after running the sign command and defaults the name to
	// `${artifact}.sig` -- with `artifacts: checksum` that is `checksums.txt.sig`,
	// which this command has never produced. It fails at PUBLISH, so `make dist`
	// cannot see it: that runs `--skip=publish`, registers the phantom artifact and
	// never uploads it. This assertion is the only thing standing where the local
	// gate does not reach.
	if want := "release-manifest.sigstore.json"; sig != want {
		t.Errorf("signs.signature is %q, want %q: goreleaser would look for "+
			"dist/checksums.txt.sig, which nothing produces, and the release would fail "+
			"at publish with `no such file or directory` after building everything", sig, want)
	}
}

// KEYLESS SIGNING NEEDS THE OIDC TOKEN, and the release job holds no signing key
// on purpose: there is nothing to rotate and nothing to leak, and the
// certificate's SAN names the workflow that requested it — which is what
// internal/releasesource pins against.
func TestTheReleaseJobCanRequestASigningCertificate(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read the release workflow: %v", err)
	}

	var doc struct {
		Jobs map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
				Uses string `yaml:"uses"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}

	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse the release workflow: %v", err)
	}

	job, ok := doc.Jobs["release"]
	if !ok {
		t.Fatalf("release.yml has no release job; jobs are %v", sortedKeys(doc.Jobs))
	}

	if job.Permissions["id-token"] != "write" {
		t.Errorf("the release job has id-token: %q, want write: without it cosign cannot "+
			"exchange an OIDC token for a signing certificate and every release publishes "+
			"an unsigned manifest", job.Permissions["id-token"])
	}

	// THE CHANNEL ADVANCES AFTER THE IMMUTABILITY GATE, because the signed
	// statement ASSERTS that the release is immutable and that assertion has to be
	// a finding rather than a claim. GitHub's immutability applies only to
	// releases created after it was enabled, so it is proved per release.
	var gate, advance = -1, -1

	for i, step := range job.Steps {
		switch {
		case strings.Contains(step.Run, "verify-release-attestation.sh"):
			gate = i
		case strings.Contains(step.Run, "advance-release-channel.sh"):
			advance = i
		}
	}

	switch {
	case gate < 0:
		t.Error("the release job no longer verifies the immutable release attestation")
	case advance < 0:
		t.Error("the release job never advances the release channel, so nothing following " +
			"`stable` ever learns about a new release")
	case advance < gate:
		t.Errorf("the channel is advanced at step %d and immutability is proved at step %d; "+
			"advancing first publishes a signed assertion nobody checked", advance, gate)
	}
}

// THE CHANNEL SCRIPT MUST NOT ROLL A FLEET BACKWARDS.
//
// `stable` names the newest release, and cutting a hotfix on an older minor after
// a newer one exists is an ordinary thing to do. Pointing the channel at it would
// roll every deployment following stable back a minor version, silently, as a
// side effect of a patch release.
func TestTheChannelScriptRefusesToRegress(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("advance-release-channel.sh")
	if err != nil {
		t.Fatalf("read the channel script: %v", err)
	}

	script := string(body)

	for _, want := range []string{
		// The newest tag, not the current statement: the statement can be missing
		// the first time this runs, or stale, and neither is a reason to publish a
		// regression.
		"--sort=-v:refname",
		"not advancing the",
		// The digest is taken from the PUBLISHED manifest, so it names the bytes a
		// deployment will actually download rather than a local copy nobody else
		// can see.
		"gh release download",
		"sha256sum",
		"./scripts/mkchannelstatement",
		"cosign sign-blob",
		"--new-bundle-format",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("advance-release-channel.sh does not contain %q", want)
		}
	}
}
