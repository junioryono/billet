package config

import (
	"fmt"
	"strings"
)

// tartVerifiedSuffix asks a Tart tier for a copy of what billet booted (ADR-021), the
// spelling a Firecracker tier uses for the newest generation proved to boot.
const tartVerifiedSuffix = "@verified"

// tartDefaultTag is what tart reads a remote reference with no tag as.
const tartDefaultTag = "latest"

// TartVerifiedImage is the reference a `<image>@verified` names, and whether image is one.
func TartVerifiedImage(image string) (string, bool) {
	base, verified := strings.CutSuffix(strings.TrimSpace(image), tartVerifiedSuffix)
	if !verified || base == "" {
		return "", false
	}

	return base, true
}

// TartImageIsRemote reports whether a reference names a registry rather than a local VM.
// A local VM's name cannot hold a path separator, and tart refuses one that does.
func TartImageIsRemote(image string) bool {
	return strings.Contains(image, "/")
}

// TartImageKey is a reference normalised the way tart resolves it, so two spellings of
// one image share one verification: the default tag named, nothing else changed. A local
// VM's name is its own key.
func TartImageKey(image string) string {
	base, verified := TartVerifiedImage(image)
	if !verified {
		base = strings.TrimSpace(image)
	}

	if !TartImageIsRemote(base) || strings.Contains(base, "@") {
		return base
	}

	// THE LAST SEGMENT ALONE: a colon before it is a registry's port.
	if !strings.Contains(base[strings.LastIndex(base, "/"):], ":") {
		return base + ":" + tartDefaultTag
	}

	return base
}

// checkTartVerifiedImage refuses a `@verified` a Tart tier cannot mean: a digest already
// says which bytes, and `@verified` asks billet to choose, so together they say two
// things about one image.
func checkTartVerifiedImage(image string) error {
	if _, verified := strings.CutSuffix(strings.TrimSpace(image), tartVerifiedSuffix); !verified {
		return nil
	}

	base, ok := TartVerifiedImage(image)
	if !ok {
		return fmt.Errorf("%q names no image before %s", image, tartVerifiedSuffix)
	}

	if strings.Contains(base, "@") {
		return fmt.Errorf("%q combines %s with a digest: a digest says which bytes, and %s asks "+
			"billet for the newest it proved to boot, so name one of the two", image,
			tartVerifiedSuffix, tartVerifiedSuffix)
	}

	return nil
}

// NamesTartVerified reports whether the image this tier boots on Tart is `<image>@verified`.
// It says nothing of another backend, where the same spelling means something else.
func (t Tier) NamesTartVerified() bool {
	_, verified := TartVerifiedImage(t.ImageFor(ProviderTart))

	return verified
}

// CatalogueNamesTartVerified reports whether any tier a Tart node could launch names
// `@verified`, which a node from before ADR-021 hands to `tart fqn` as a name.
func CatalogueNamesTartVerified(tiers []Tier) bool {
	for i := range tiers {
		if tiers[i].AcceptsProvider(ProviderTart) && tiers[i].NamesTartVerified() {
			return true
		}
	}

	return false
}
