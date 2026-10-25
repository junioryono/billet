package runnerimages

import (
	"strings"
	"testing"
)

// THE GETTER'S CHECK REFUSES BYTES THAT ARE NOT THE PINNED ONES, and names where the
// architecture's own declaration comes from, not the x64 one's.
func TestAToolsetThatIsNotItsPinIsRefusedNamingItsSource(t *testing.T) {
	t.Parallel()

	for _, arch := range []string{ArchX64, ArchArm64} {
		want, err := PinnedSHA256For(arch)
		if err != nil {
			t.Fatal(err)
		}
		data := toolset
		if arch == ArchArm64 {
			data = toolsetArm64
		}
		tampered := append(append([]byte{}, data...), ' ')
		if _, err := verifiedCopy(arch, tampered, want); err == nil {
			t.Fatalf("%s: bytes that are not the pinned ones were accepted", arch)
		} else {
			file, fileErr := ToolsetFileFor(arch)
			if fileErr != nil {
				t.Fatal(fileErr)
			}
			if !strings.Contains(err.Error(), "/"+file) || !strings.Contains(err.Error(), "pinned.txt") {
				t.Fatalf("%s: the refusal does not name %s and pinned.txt: %v", arch, file, err)
			}
		}
		if _, err := verifiedCopy(arch, data, want); err != nil {
			t.Fatalf("%s: the pinned bytes were refused: %v", arch, err)
		}
	}
}
