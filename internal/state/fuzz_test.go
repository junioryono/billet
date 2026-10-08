package state

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"
)

// A MIGRATION'S STATEMENTS ARE ITS OWN BYTES, ALL OF THEM, IN ORDER. Two
// properties over one input. Read as a migration file: each statement the
// parser returns is a stretch of the file as written, and written back
// between its markers parses to the same statements. Read as statement bodies
// separated by NUL and written between markers: if the parser accepts that
// document, it returns exactly those bodies, every one, byte for byte and in
// order, so a parser that dropped, merged, truncated or reordered statements
// fails. The published bytes a checksum is taken over cannot be altered by the
// reading. Seeded with every migration of both engines.
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
	f.Add([]byte("CREATE TABLE a (x INTEGER);\x00CREATE TABLE b (\n\ty INTEGER\n);"))

	f.Fuzz(func(t *testing.T, data []byte) {
		asFile(t, data)
		asBodies(t, strings.Split(string(data), "\x00"))
	})
}

// render writes bodies between their markers, as a migration file holds them.
func render(bodies []string) string {
	var doc strings.Builder

	for _, body := range bodies {
		doc.WriteString(stmtOpenMarker + "\n" + body + "\n" + stmtCloseMarker + "\n")
	}

	return doc.String()
}

// asFile holds the parse of data as a migration file to its own bytes.
func asFile(t *testing.T, data []byte) {
	t.Helper()

	stmts, err := parseMigrationStatements("fuzz.sql", data)
	if err != nil {
		return
	}

	for _, stmt := range stmts {
		if !strings.Contains(string(data), stmt) {
			t.Fatalf("a statement that is not in the file: %q", stmt)
		}
	}

	again, err := parseMigrationStatements("fuzz.sql", []byte(render(stmts)))
	if err != nil {
		t.Fatalf("the statements written back between their markers were refused: %v\n%s", err, render(stmts))
	}

	if len(stmts) != len(again) || (len(stmts) > 0 && !reflect.DeepEqual(stmts, again)) {
		t.Fatalf("the statements read back differently:\n%q\n%q", stmts, again)
	}
}

// asBodies holds the parse of a document built from bodies to those bodies.
func asBodies(t *testing.T, bodies []string) {
	t.Helper()

	// A BODY HOLDING A MARKER LINE IS NOT ONE STATEMENT'S BODY: written between
	// markers, its own marker is structure, and splitting there is right.
	for _, body := range bodies {
		for line := range strings.SplitSeq(body, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed == stmtOpenMarker || trimmed == stmtCloseMarker {
				return
			}
		}
	}

	stmts, err := parseMigrationStatements("fuzz.sql", []byte(render(bodies)))
	if err != nil {
		return
	}

	if len(stmts) != len(bodies) {
		t.Fatalf("%d statements written, %d read back:\n%q\n%q", len(bodies), len(stmts), bodies, stmts)
	}

	for i := range bodies {
		if stmts[i] != bodies[i] {
			t.Fatalf("statement %d was written as %q and read back as %q", i, bodies[i], stmts[i])
		}
	}
}
