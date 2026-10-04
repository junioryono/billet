package lease

import (
	"reflect"
	"strings"
	"testing"
)

// untaggedKeys names each non-embedded field of typ whose json tag names no key:
// absent, or empty before its options, which leaves the Go name as the wire key.
func untaggedKeys(typ reflect.Type) []string {
	var missing []string

	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Anonymous {
			continue
		}

		tag, _ := field.Tag.Lookup("json")
		if name, _, _ := strings.Cut(tag, ","); name == "" {
			missing = append(missing, field.Name)
		}
	}

	return missing
}

// EVERY FIELD OF THE LEASE NAMES ITS WIRE KEY. The lease travels in a node
// command, so an untagged field's Go name is its wire name, and renaming it would
// rename a field an older node or a strict plane decoder still expects. With the
// tag explicit, a Go rename keeps the wire as it was; internal/nodeapi's golden
// fixtures prove the encoding itself. The embedded BuildCaches is the exception:
// its own fields carry tags, and embedding promotes them.
func TestEveryLeaseFieldNamesItsWireKey(t *testing.T) {
	t.Parallel()

	for _, name := range untaggedKeys(reflect.TypeFor[Lease]()) {
		t.Errorf("Lease.%s names no wire key in its json tag, so its Go name is its wire name", name)
	}

	// AN EMPTY TAG IS NOT A KEY: `json:""` and `json:",omitempty"` leave the Go
	// name on the wire as surely as no tag at all.
	type probe struct {
		Keyed    string `json:"Keyed"`
		Untagged string
		Empty    string `json:""`
		Optioned string `json:",omitempty"`
		BuildCaches
	}

	got := untaggedKeys(reflect.TypeFor[probe]())
	if want := []string{"Untagged", "Empty", "Optioned"}; !reflect.DeepEqual(got, want) {
		t.Errorf("untaggedKeys(probe) = %v, want %v", got, want)
	}
}
