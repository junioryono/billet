package imagesource

import (
	"fmt"
	"strings"
	"testing"
)

// validCompletableManifest is a schema 3 image: the multipart layout, with the
// components it leaves out and the completer's contract.
func validCompletableManifest() Manifest {
	m := validMultipartManifest()
	m.Schema = SchemaV3
	m.CompletionContract = "1"
	m.Components = []Component{
		{Name: "android-sdk", Space: 12 << 30},
		{Name: "google-chrome", Space: 600 << 20},
	}

	return m
}

func TestASchema3ManifestParsesWithItsComponents(t *testing.T) {
	got, err := ParseManifest(mustJSON(t, validCompletableManifest()))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	if len(got.Components) != 2 || got.Components[0].Name != "android-sdk" ||
		got.Components[0].Space != 12<<30 || got.CompletionContract != "1" {
		t.Fatalf("parsed components %+v and contract %q", got.Components, got.CompletionContract)
	}
}

// An image that leaves something out is refused by every reader that cannot put
// it back, which is the whole reason the field is a schema rather than optional.
func TestComponentsAreDeclaredOnlyUnderSchema3(t *testing.T) {
	t.Run("components under schema 2", func(t *testing.T) {
		m := validCompletableManifest()
		m.Schema = SchemaV2
		m.CompletionContract = ""

		requireRefused(t, m, "only schema 3 describes")
	})

	t.Run("a contract under schema 2", func(t *testing.T) {
		m := validMultipartManifest()
		m.CompletionContract = "1"

		requireRefused(t, m, "only schema 3 describes")
	})

	t.Run("schema 3 declaring nothing", func(t *testing.T) {
		m := validCompletableManifest()
		m.Components = nil

		requireRefused(t, m, "declares no components")
	})

	t.Run("schema 3 without a contract", func(t *testing.T) {
		m := validCompletableManifest()
		m.CompletionContract = ""

		requireRefused(t, m, "is not a completion contract version")
	})

	t.Run("schema 3 as a single asset", func(t *testing.T) {
		m := validCompletableManifest()
		m.RootfsMultipart = nil
		m.Rootfs = validManifest().Rootfs

		requireRefused(t, m, "schema 1 layout")
	})
}

func TestAComponentIsRefusedUnlessWellFormed(t *testing.T) {
	for name, tc := range map[string]struct {
		components []Component
		want       string
	}{
		"an uppercase name":   {[]Component{{Name: "Google-Chrome", Space: 1}}, `"Google-Chrome" is not a component name`},
		"a name with a space": {[]Component{{Name: "google chrome", Space: 1}}, "is not a component name"},
		"an empty name":       {[]Component{{Name: "", Space: 1}}, `"" is not a component name`},
		"a repeat": {[]Component{{Name: "codeql", Space: 1}, {Name: "codeql", Space: 2}},
			"declares component codeql twice"},
		"no space":   {[]Component{{Name: "codeql", Space: 0}}, "asks for 0 bytes"},
		"a negative": {[]Component{{Name: "codeql", Space: -1}}, "asks for -1 bytes"},
		"too much": {[]Component{{Name: "codeql", Space: MaxComponentBytes + 1}},
			fmt.Sprintf("asks for %d bytes", MaxComponentBytes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			m := validCompletableManifest()
			m.Components = tc.components

			requireRefused(t, m, tc.want)
		})
	}

	t.Run("too many", func(t *testing.T) {
		m := validCompletableManifest()
		m.Components = nil
		for i := range MaxComponents + 1 {
			m.Components = append(m.Components, Component{Name: fmt.Sprintf("c%d", i), Space: 1})
		}

		requireRefused(t, m, fmt.Sprintf("declares %d components", MaxComponents+1))
	})
}

// A build that reads schema 3 but cannot complete an image refuses it as unusable,
// naming what it leaves out, so a deployment is never moved onto an image without
// what its tiers expect.
func TestAnImageThisBuildCannotCompleteIsNotUsable(t *testing.T) {
	m := validCompletableManifest()

	err := m.Usable(m.GuestContract, m.Arch)
	if err == nil || !strings.Contains(err.Error(), `leaves out android-sdk, google-chrome for each `+
		`deployment to install, through completion contract "1", which this billet cannot run`) {
		t.Fatalf("got %v, want the image refused as one this build cannot complete", err)
	}

	full := validMultipartManifest()
	if err := full.Usable(full.GuestContract, full.Arch); err != nil {
		t.Fatalf("a schema 2 image was refused: %v", err)
	}
}

// The bound allows what the largest real component needs.
func TestTheComponentBoundFitsTheAndroidSDK(t *testing.T) {
	m := validCompletableManifest()
	m.Components = []Component{{Name: "android-sdk", Space: MaxComponentBytes}}

	if _, err := ParseManifest(mustJSON(t, m)); err != nil {
		t.Fatalf("a component at the bound was refused: %v", err)
	}
}
