package regularfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHoldsJudgesByMountWhereTheKernelSaysAndByDeviceOtherwise(t *testing.T) {
	t.Parallel()

	dir := Mount{Dev: 64, Known: true, ID: 7}

	for _, tc := range []struct {
		name  string
		dir   Mount
		entry Mount
		want  bool
	}{
		{"same mount and device", dir, Mount{Dev: 64, Known: true, ID: 7}, true},
		{"an overlay layer's device on the directory's mount", dir, Mount{Dev: 99, Known: true, ID: 7}, true},
		{"a bind mount from the same filesystem", dir, Mount{Dev: 64, Known: true, ID: 31}, false},
		{"a bind mount from another filesystem", dir, Mount{Dev: 99, Known: true, ID: 31}, false},
		{"no mount identity for the entry: same device", dir, Mount{Dev: 64}, true},
		{"no mount identity for the entry: another device", dir, Mount{Dev: 99, ID: 7}, false},
		{"no mount identity for the directory", Mount{Dev: 64, ID: 7}, Mount{Dev: 99, Known: true, ID: 7}, false},
	} {
		if got := tc.dir.Holds(tc.entry); got != tc.want {
			t.Errorf("%s: Holds = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAFileIsOnItsDirectorysMount(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "authority.lock")

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	dirMount, err := MountOf(d)
	if err != nil {
		t.Fatal(err)
	}

	fileMount, err := MountOf(f)
	if err != nil {
		t.Fatal(err)
	}

	if !dirMount.Holds(fileMount) {
		t.Errorf("a file created in its directory is not on that directory's mount: %+v %+v",
			dirMount, fileMount)
	}

	if fileMount.Dev == 0 {
		t.Errorf("no device was read for %s: %+v", path, fileMount)
	}
}
