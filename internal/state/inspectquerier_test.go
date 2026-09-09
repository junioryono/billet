package state

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// AN INSPECTION'S VIEW ADMITS ONLY THE NAMED READS SQLC GENERATED. A READ ONLY
// transaction refuses a write and nothing else: a callback with the bare
// transaction could COMMIT it, turn the session's read-only default off and
// write in autocommit, or take a session advisory lock rollback never releases.
// So every statement that is not one of ReadOps' generated reads is refused
// before it reaches the engine, on every engine.
func TestAnInspectionsViewAdmitsOnlyTheGeneratedReads(t *testing.T) {
	dir := ledgerWithRow(t)

	db, err := OpenInspect(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenInspect: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	refused := map[string]string{
		"transaction control":          "COMMIT",
		"a session setting":            "SELECT set_config('default_transaction_read_only', 'off', false)",
		"a session advisory lock":      "SELECT pg_try_advisory_lock(1)",
		"a bare read":                  "SELECT provider FROM nodes WHERE name = 'epyc-1'",
		"a forged header on a write":   "-- name: ListRolloutNodes :many\nUPDATE nodes SET provider = 'tart' RETURNING name",
		"a forged header on a session": "-- name: ListRolloutNodes :many\nSELECT pg_advisory_lock(1)",
		"a write's own header":         "-- name: UpsertNodeRegistration :one\nSELECT 1",
		"an exec's header":             "-- name: MarkNodeNotLive :exec\nSELECT 1",
	}

	for name, stmt := range refused {
		if err := db.View(t.Context(), func(q Querier) error {
			if err := queryOne(t, querierWith(q, stmt)); !errors.Is(err, ErrInspect) {
				t.Errorf("%s (%q) through QueryContext: err = %v, want ErrInspect", name, stmt, err)
			}

			var v string
			if err := q.QueryRowContext(t.Context(), stmt).Scan(&v); err == nil ||
				!strings.Contains(err.Error(), "admits only the named reads") {
				t.Errorf("%s (%q) through QueryRowContext: err = %v, want the refusal", name, stmt, err)
			}

			return nil
		}); err != nil {
			t.Fatalf("View: %v", err)
		}
	}

	// The generated read still answers, through the same querier.
	if err := db.View(t.Context(), func(q Querier) error {
		if _, err := ReadQueries(q).ReadDeploymentBinding(t.Context()); err != nil && !strings.Contains(err.Error(), "no rows") {
			t.Errorf("a generated read through an inspection's View: %v", err)
		}

		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	if got := providerVia(t, db, "epyc-1"); got != "docker" {
		t.Errorf("the sentinel row reads %q after the refused statements", got)
	}
}

// querierWith runs one fixed statement through QueryContext whatever the
// querier is asked, so queryOne's draining and closing apply to it.
func querierWith(q Querier, stmt string) Querier { return fixedQuerier{q: q, stmt: stmt} }

type fixedQuerier struct {
	q    Querier
	stmt string
}

func (f fixedQuerier) QueryContext(ctx context.Context, _ string, args ...any) (*sql.Rows, error) {
	return f.q.QueryContext(ctx, f.stmt, args...)
}

func (f fixedQuerier) QueryRowContext(ctx context.Context, _ string, args ...any) *sql.Row {
	return f.q.QueryRowContext(ctx, f.stmt, args...)
}

// EVERY GENERATED READ IS ADMITTED AND EVERY GENERATED WRITE IS REFUSED, read
// from the generated constants themselves: the admission rule must never bite
// a legitimate read, and must bite every statement ReadOps does not hold.
func TestTheInspectionAdmissionMatchesTheGeneratedQueries(t *testing.T) {
	t.Parallel()

	consts := generatedQueryConstants(t)
	if len(consts) < 100 {
		t.Fatalf("found %d generated query constants, which is not the query set", len(consts))
	}

	admitted, refused := 0, 0

	for name, text := range consts {
		header, _, _ := strings.Cut(text, "\n")
		fields := strings.Fields(strings.TrimPrefix(header, "-- name: "))

		if len(fields) != 2 {
			t.Fatalf("%s: unexpected header %q", name, header)
		}

		_, isRead := readOpsType.MethodByName(fields[0])
		err := admitInspectRead(text)

		switch {
		case isRead && err != nil:
			t.Errorf("%s is a read ReadOps holds and the inspection refuses it: %v", fields[0], err)
		case !isRead && err == nil:
			t.Errorf("%s is not a read ReadOps holds and the inspection admits it", fields[0])
		case isRead:
			admitted++
		default:
			refused++
		}
	}

	if admitted == 0 || refused == 0 {
		t.Fatalf("admitted %d and refused %d generated statements; the rule was not exercised both ways", admitted, refused)
	}
}

// generatedQueryConstants reads every string constant sqlc generated, keyed
// by its Go name.
func generatedQueryConstants(t *testing.T) map[string]string {
	t.Helper()

	out := map[string]string{}

	files, err := filepath.Glob(filepath.Join("ledgerdb", "*.sql.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no generated query files: %v", err)
	}

	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		f, err := parser.ParseFile(token.NewFileSet(), file, body, 0)
		if err != nil {
			t.Fatal(err)
		}

		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}

			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) != 1 {
					continue
				}

				lit, ok := vs.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}

				text, err := strconv.Unquote(lit.Value)
				if err != nil || !strings.HasPrefix(text, "-- name: ") {
					continue
				}

				out[vs.Names[0].Name] = text
			}
		}
	}

	return out
}
