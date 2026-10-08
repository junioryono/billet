package state

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"
)

// A MIGRATION'S STATEMENTS ARE ITS OWN BYTES, AND READ BACK THE SAME. Whatever
// file the parser accepts, each statement it returns is a stretch of that file
// exactly as written, and writing the statements back between their markers
// parses to the same statements: the published bytes a checksum is taken over
// cannot be altered, merged or dropped by the reading. Seeded with every
// migration of both engines.
func FuzzParseMigrationStatements(f *testing.F) {
	for _, fsys := range []fs.FS{migrationFS, pgMigrationFS} {
		err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}

			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}

			f.Add(data)

			return nil
		})
		if err != nil {
			f.Fatal(err)
		}
	}

	f.Add([]byte(stmtOpenMarker + "\n" + stmtCloseMarker + "\n"))
	f.Add([]byte(" " + stmtOpenMarker + "\nSELECT 1;\n" + stmtCloseMarker + "\n"))
	f.Add([]byte(stmtOpenMarker + "\nSELECT 1;\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		stmts, err := parseMigrationStatements("fuzz.sql", data)
		if err != nil {
			return
		}

		for _, stmt := range stmts {
			if !strings.Contains(string(data), stmt) {
				t.Fatalf("a statement that is not in the file: %q", stmt)
			}
		}

		var rendered strings.Builder

		for _, stmt := range stmts {
			rendered.WriteString(stmtOpenMarker + "\n" + stmt + "\n" + stmtCloseMarker + "\n")
		}

		again, err := parseMigrationStatements("fuzz.sql", []byte(rendered.String()))
		if err != nil {
			t.Fatalf("the statements written back between their markers were refused: %v\n%s", err, rendered.String())
		}

		if len(stmts) == 0 && len(again) == 0 {
			return
		}

		if !reflect.DeepEqual(stmts, again) {
			t.Fatalf("the statements read back differently:\n%q\n%q", stmts, again)
		}
	})
}
