package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A CONFIGURATION PARSES THE SAME WAY EVERY TIME, AND NO BYTES PANIC IT. Parse
// is what every role and every operator command runs over billet.yaml, so
// whatever an operator writes must come back as a configuration or a refusal:
// never a crash, and never a different answer for the same bytes. Seeded with
// the annotated reference and the packaged template.
func FuzzConfigParse(f *testing.F) {
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
