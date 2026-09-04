package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"testing"

	"github.com/junioryono/billet/internal/awss3"
)

// THE VERDICT COMES FROM THE ANSWER S3 SENT, NOT FROM THE WORDS OF A MESSAGE.
//
// `billet check` reads a 403 from the ebs-s3 bucket probe as INCONCLUSIVE rather
// than as a broken bucket, because billet's minimal grant conditions
// s3:ListBucket on s3:prefix — a context key a GetObject request does not carry —
// so a healthy miss can answer 403 under exactly the policy billet generates.
//
// THE DECEPTIVE CASE IS THE ONE THAT MATTERS. This branch was selected by looking
// for the substring "HTTP 403" in the probe error's rendered message, so an error
// that merely CONTAINS those characters was read as a refused identity, and any
// reword of a diagnostic on the path changed the verdict. That error is in the
// table below and must be a failure.
func TestTheCacheProbeVerdictReadsTheRefusalRatherThanTheMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want cacheProbeVerdict
	}{
		{
			name: "a bucket that answered", err: nil, want: cacheProbeAnswered,
		},
		{
			name: "a refusal S3 sent",
			err: fmt.Errorf("ebs-s3: the cache bucket did not answer a probe read: %w",
				fmt.Errorf("ebs-s3: S3 GET returned %w",
					&awss3.Refusal{Status: http.StatusForbidden, Code: "AccessDenied"})),
			want: cacheProbeInconclusive,
		},
		{
			// PROSE THAT LOOKS LIKE A REFUSAL IS NOT ONE. Nothing here carries an
			// S3 answer, so it must not reach the advisory branch.
			name: "a message that merely says HTTP 403",
			err:  errors.New("ebs-s3: something else entirely went wrong: HTTP 403"),
			want: cacheProbeFailed,
		},
		{
			name: "a bucket that does not exist",
			err: fmt.Errorf("ebs-s3: S3 GET returned %w",
				&awss3.Refusal{Status: http.StatusNotFound, Code: awss3.CodeNoSuchBucket}),
			want: cacheProbeFailed,
		},
		{
			// A transport failure carries no S3 answer either, and reporting it
			// as inconclusive would hide an unreachable bucket behind an
			// advisory line.
			name: "a host that could not be dialled",
			err:  errors.New("ebs-s3: call S3: dial tcp: connection refused"),
			want: cacheProbeFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := judgeCacheProbe(tc.err); got != tc.want {
				t.Errorf("judgeCacheProbe = %d, want %d", got, tc.want)
			}
		})
	}
}

// AND `billet check` ASKS THAT JUDGEMENT, WITH THE PROBE'S OWN ERROR.
//
// PROVING THE MECHANISM IS NOT PROVING IT IS USED. The verdict above is a plain
// function; what turns it into something an operator meets is one call in
// ec2Preflight. Delete it and the table stays green while the probe prints that a
// bucket S3 has never heard of answered.
//
// A STRUCTURAL TEST BECAUSE THE CALL SITE CANNOT BE REACHED. ebss3.New builds its
// endpoint from the region and the bucket with no override, so nothing in a unit
// test can put a fake S3 behind ec2Preflight. The ARGUMENT is asserted as well as
// the call, because judging an error other than the probe's judges nothing.
func TestTheCheckCommandJudgesTheCacheProbesOwnAnswer(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var preflight *ast.FuncDecl

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "ec2Preflight" {
			preflight = fn
		}
	}

	// A WALK THAT FOUND NOTHING PASSES FOR THE WRONG REASON, which is the failure
	// every structural test in this repository is arranged against.
	if preflight == nil {
		t.Fatal("ec2Preflight is gone from main.go; this guard is checking nothing")
	}

	// THE CALL MUST BE THE SWITCH'S SUBJECT, not merely present. `_ =
	// judgeCacheProbe(probeErr)` beside a switch that decides some other way
	// satisfies "the call exists", which is the mechanism-versus-use gap this
	// whole file is about.
	var verdict *ast.SwitchStmt

	ast.Inspect(preflight, func(n ast.Node) bool {
		stmt, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}

		call, ok := stmt.Tag.(*ast.CallExpr)
		if !ok {
			return true
		}

		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "judgeCacheProbe" || len(call.Args) != 1 {
			return true
		}

		if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == "probeErr" {
			verdict = stmt
		}

		return true
	})

	if verdict == nil {
		t.Fatal("ec2Preflight does not switch on judgeCacheProbe(probeErr), so a refused " +
			"identity and a bucket that does not exist are not told apart by the judgement " +
			"that knows the difference")
	}

	// AND EACH VERDICT MUST STILL DO ITS THING. A switch naming all three and
	// acting on none is the same silence as no switch at all; the one that has to
	// be a REFUSAL is the failure, because that is the branch a misaddressed
	// bucket now reaches.
	clauses := map[string]*ast.CaseClause{}

	for _, stmt := range verdict.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}

		for _, expr := range clause.List {
			if name, ok := expr.(*ast.Ident); ok {
				clauses[name.Name] = clause
			}
		}
	}

	handled := []string{"cacheProbeAnswered", "cacheProbeInconclusive", "cacheProbeFailed"}
	for _, want := range handled {
		if clauses[want] == nil {
			t.Errorf("the cache probe's switch does not handle %s", want)
		}
	}

	if failed := clauses["cacheProbeFailed"]; failed != nil && !returnsSomething(failed) {
		t.Error("the cacheProbeFailed branch does not return, so a bucket that does not exist " +
			"leaves `billet check` reporting success")
	}

	// AND IT MUST NOT GO BACK TO THE MESSAGE. A strings.Contains over anything's
	// .Error() in this function is the shape that was removed, and it would sit
	// beside the call above while deciding the verdict instead of it.
	ast.Inspect(preflight, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Contains" {
			return true
		}

		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "strings" {
			return true
		}

		for _, arg := range call.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}

			if method, ok := inner.Fun.(*ast.SelectorExpr); ok && method.Sel.Name == "Error" {
				t.Errorf("ec2Preflight classifies an error by its rendered message at %s; "+
					"a reworded diagnostic must not be able to change a verdict",
					fset.Position(call.Pos()))
			}
		}

		return true
	})
}

// returnsSomething reports whether a case clause returns a value.
func returnsSomething(clause *ast.CaseClause) bool {
	for _, stmt := range clause.Body {
		ret, ok := stmt.(*ast.ReturnStmt)
		if ok && len(ret.Results) > 0 {
			return true
		}
	}

	return false
}
