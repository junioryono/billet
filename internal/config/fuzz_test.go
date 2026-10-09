package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A CONFIGURATION PARSES THE SAME WAY EVERY TIME, AND NO BYTES PANIC IT. Parse
// is what every role and every operator command runs over billet.yaml, so
// whatever an operator writes must come back as a configuration or a refusal:
// never a crash, and never a different answer for the same bytes. Seeded with a
// configuration that parses, which is asserted so the comparison of two
// accepted configurations runs on the seeds, and with the annotated reference
// and the packaged template, which are refused as they ship (their App ids are
// zero).
func FuzzConfigParse(f *testing.F) {
	if _, err := Parse("valid.yaml", []byte(validConfig)); err != nil {
		f.Fatalf("the valid seed does not parse: %v", err)
	}

	f.Add([]byte(validConfig))
	f.Add([]byte(strings.Replace(validConfig, "    image: ubuntu-2404-x64\n  - label: billet-8vcpu",
		"    image: ubuntu-2404-x64\n    cache:\n      docker: {max_size: 9999TiB}\n"+
			"      git: {max_size: 9999TiB}\n  - label: billet-8vcpu", 1)))

	for _, path := range []string{
		filepath.Join("..", "..", "billet.example.yaml"),
		filepath.Join("..", "..", "deploy", "billet.yaml"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatalf("seed %s: %v", path, err)
		}

		f.Add(data)
	}

	f.Add([]byte(""))
	f.Add([]byte("server: {}\n"))
	f.Add([]byte("---\n---\n"))
	f.Add([]byte("a: &a [*a]\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		first, err := Parse("fuzz.yaml", data)
		second, err2 := Parse("fuzz.yaml", data)

		if (err == nil) != (err2 == nil) {
			t.Fatalf("the same bytes parsed once (%v) and were refused once (%v)", err, err2)
		}

		if err != nil {
			if err.Error() != err2.Error() {
				t.Fatalf("the same bytes were refused two ways:\n%v\n%v", err, err2)
			}

			return
		}

		if !reflect.DeepEqual(first, second) {
			t.Fatal("the same bytes parsed to two different configurations")
		}
	})
}
