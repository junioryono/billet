package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/imagesource"
)

const (
	signedManifest = "../../internal/imagesource/testdata/signed-manifest.json"
	signedBundle   = "../../internal/imagesource/testdata/signed-manifest.sigstore.json"
)

// A CARRIED SET PASSES ONLY UNDER BILLET'S OWN POLICY, and only for the machine it is
// carried for. The fixture is a genuine signature from this project's publishing
// workflow, so the refusals below come from what each case changed, and a tampered
// manifest is refused before anything reads it.
func TestACarriedManifestIsHeldToTheNodesPolicyAndItsMachine(t *testing.T) {
	t.Parallel()

	tampered := filepath.Join(t.TempDir(), "manifest.json")

	raw, err := os.ReadFile(signedManifest)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(tampered, []byte(strings.Replace(string(raw), `"arch": "x86_64"`, `"arch": "aarch64"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	garbage := filepath.Join(t.TempDir(), "garbage.json")
	if err := os.WriteFile(garbage, []byte("this is no manifest"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		args []string
		says string
	}{
		"a genuine signature of its machine": {args: []string{"manifest", "x86_64", signedManifest, signedBundle}},
		"another machine's":                  {args: []string{"manifest", "aarch64", signedManifest, signedBundle}, says: "carried for aarch64"},
		"a manifest edited after signing":    {args: []string{"manifest", "aarch64", tampered, signedBundle}, says: "imagesource"},
		"no signature":                       {args: []string{"manifest", "x86_64", signedManifest, empty}, says: "requires a signature"},
		// THE SIGNATURE IS ASKED FIRST: a parse before it would answer about this file.
		"no document, and no signature": {args: []string{"manifest", "x86_64", garbage, empty}, says: "requires a signature"},
		"no manifest":                   {args: []string{"manifest", "x86_64", "gone.json", signedBundle}, says: "could not read the document"},
		"no bundle":                     {args: []string{"manifest", "x86_64", signedManifest, "gone.json"}, says: "could not read the bundle"},
		// A POINTER IS PROVED BY ITS SIGNATURE, and held to what a node holds it to: this
		// fixture is genuinely signed and is no pointer, so only the second can refuse it.
		"past its expiry, by name":       {args: []string{"channel-expired", signedManifest, signedBundle}},
		"a pointer no reader would take": {args: []string{"channel", signedManifest, signedBundle}, says: "guest-image channel"},
		"a pointer edited after":         {args: []string{"channel-expired", tampered, signedBundle}, says: "imagesource"},
		"a pointer with no signature":    {args: []string{"channel-expired", signedManifest, empty}, says: "requires a signature"},
		"a pointer unsigned, held too":   {args: []string{"channel", signedManifest, empty}, says: "requires a signature"},
		"too few arguments":              {args: []string{"manifest", "x86_64"}, says: "usage"},
		"a question it does not answer":  {args: []string{"x86_64", signedManifest, signedBundle}, says: "usage"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := run(tc.args, io.Discard)
			if tc.says == "" {
				if err != nil {
					t.Fatalf("a genuine manifest was refused: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("answered %v, want a refusal saying %q", err, tc.says)
			}
		})
	}
}

// A LAPSED POINTER IS REFUSED, and past that a person's say-so. The signature is the
// fixture's, so this drives the judgement a signed pointer reaches, at the two moments that
// tell an expiry being held from one being ignored.
func TestAChannelPointerIsHeldToItsExpiryUnlessAPersonSaysOtherwise(t *testing.T) {
	t.Parallel()

	published := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	pointer := fmt.Sprintf(`{"schema":1,"tag":"guest-20261101-000000","published_at":%q,"expires_at":%q,"release_immutable":true}`,
		published.Format(time.RFC3339), published.Add(240*time.Hour).Format(time.RFC3339))

	if _, err := imagesource.ParseChannel([]byte(pointer), published.Add(time.Hour)); err != nil {
		t.Fatalf("a current pointer was refused: %v", err)
	}

	_, err := imagesource.ParseChannel([]byte(pointer), published.Add(241*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("a lapsed pointer answered %v", err)
	}
}
