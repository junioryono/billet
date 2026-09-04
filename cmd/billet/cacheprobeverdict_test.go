package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE CACHE PROBE'S INCONCLUSIVE VERDICT IS TAKEN FROM THE ANSWER, NOT THE WORDS.
//
// `billet check` reads a 403 from the ebs-s3 bucket probe as INCONCLUSIVE rather
// than as a broken bucket, because billet's own minimal grant conditions
// s3:ListBucket on s3:prefix — a context key a GetObject request does not carry —
// so a healthy miss can answer 403 under exactly the policy billet generates.
// That branch used to be selected by looking for the substring "HTTP 403" in the
// probe error's rendered message, which made every message on the path
// load-bearing: reword one and a refused identity becomes a hard failure, or a
// real fault becomes an advisory line an operator scrolls past.
//
// A STRUCTURAL TEST BECAUSE THE CALL SITE CANNOT BE REACHED. `ebss3.New` builds
// its endpoint from the region and the bucket with no override, so nothing in a
// unit test can put a fake S3 behind ec2Preflight; and the branch is one
// fmt.Printf, so a run-time test would be asserting on stdout after standing up a
// config, an identity and a credential chain. What is being defended here is an
// ABSENCE — delete the awss3.StatusOf call and every other test in this change
// stays green while the operator-facing regression comes back.
func TestTheCacheProbeClassifiesARefusalByItsStatus(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	preflight := functionNamed(file, "ec2Preflight")
	if preflight == nil {
		t.Fatal("ec2Preflight is gone from main.go; this guard is checking nothing")
	}

	var asksTheStatus bool

	ast.Inspect(preflight, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "StatusOf" {
			return true
		}

		if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "awss3" {
			asksTheStatus = true
		}

		return true
	})

	if !asksTheStatus {
		t.Error("ec2Preflight no longer asks awss3.StatusOf what S3 answered, so the cache " +
			"probe cannot tell a refused identity from a broken bucket")
	}

	// AND IT MUST NOT GO BACK TO THE MESSAGE. A strings.Contains over anything's
	// .Error() in this function is the shape that was removed, and it would pass
	// the assertion above while sitting beside it.
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

// functionNamed finds one top-level function declaration.
func functionNamed(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}

	return nil
}
