package a

import "testing"

// harness makes its caller's subtest parallel. Nothing at a CALL SITE says so,
// which is the entire reason the analyzer carries a Fact rather than looking for
// t.Parallel() in the subtest body.
func harness(t *testing.T) int { // want harness:"parallelHelper"
	t.Parallel()

	return 1
}

// sequentialHelper looks just like harness and is not parallel.
func sequentialHelper(t *testing.T) int {
	t.Helper()

	return 2
}

func TestWritesSharedMapFromParallelSubtest(t *testing.T) {
	results := map[string]int{}

	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			results[name] = 1 // want `results is declared by the enclosing test`
		})
	}
}

func TestWritesSharedCounterThroughHelperFact(t *testing.T) {
	count := 0

	t.Run("one", func(t *testing.T) {
		harness(t)

		count++ // want `count is declared by the enclosing test`
	})
}

func TestWritesSharedStructField(t *testing.T) {
	var state struct{ n int }

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		state.n = 4 // want `state is declared by the enclosing test`
	})
}

func TestDeletesFromSharedMap(t *testing.T) {
	seen := map[string]bool{"x": true}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		delete(seen, "x") // want `seen is declared by the enclosing test`
	})
}

func TestWritesThroughAHelperClosure(t *testing.T) {
	results := map[string]int{}

	record := func(k string) {
		results[k] = 1 // want `results is declared by the enclosing test`
	}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		record("a")
	})
}

func TestWritesSharedStateFromANestedGroup(t *testing.T) {
	results := map[string]int{}

	t.Run("group", func(t *testing.T) {
		t.Run("case", func(t *testing.T) {
			t.Parallel()

			results["a"] = 1 // want `results is declared by the enclosing test`
		})
	})
}

// ----- what must NOT be reported -----

func TestSequentialSubtestMayWriteSharedState(t *testing.T) {
	results := map[string]int{}

	t.Run("one", func(t *testing.T) {
		results["a"] = 1
	})

	t.Run("two", func(t *testing.T) {
		sequentialHelper(t)

		results["b"] = 2
	})
}

func TestParallelSubtestOwningItsState(t *testing.T) {
	t.Run("one", func(t *testing.T) {
		t.Parallel()

		mine := map[string]int{}
		mine["a"] = 1
	})
}

func TestPerIterationStateIsNotShared(t *testing.T) {
	for _, name := range []string{"a", "b"} {
		mine := map[string]int{}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mine[name] = 1
		})
	}
}

func TestReadingSharedStateIsFine(t *testing.T) {
	want := map[string]int{"a": 1}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		if want["a"] != 1 {
			t.Error("no")
		}
	})
}

func TestShadowingIsNotAWrite(t *testing.T) {
	results := map[string]int{"a": 1}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		results := map[string]int{}
		results["b"] = 2
	})

	_ = results
}

func TestASuppressedWriteIsAccepted(t *testing.T) {
	results := map[string]int{}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		//billet:ignore parallelshared // the fixture proves a reasoned exemption is honoured
		results["a"] = 1
	})
}

// ----- shapes an earlier version of the analyzer got wrong -----

// A HELPER THAT CALLS ITSELF used to re-enter its own body until the stack ran
// out, turning the lint gate into a crash.
func TestARecursiveHelperTerminates(t *testing.T) {
	count := 0

	var recurse func(n int)

	recurse = func(n int) {
		if n <= 0 {
			return
		}

		count++ // want `count is declared by the enclosing test`

		recurse(n - 1)
	}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		recurse(3)
	})
}

// A NAMED SUBTEST CALLBACK is the same subtest as an inline literal, and
// accepting only the literal missed it entirely.
func TestANamedSubtestCallbackIsStillASubtest(t *testing.T) {
	shared := map[string]int{}

	caseFn := func(t *testing.T) {
		t.Parallel()

		shared["x"] = 1 // want `shared is declared by the enclosing test`
	}

	t.Run("x", caseFn)
}

// `var f = func(){}` is the same declaration wearing a keyword.
func TestAVarDeclaredHelperIsFollowed(t *testing.T) {
	shared := map[string]int{}

	var record = func(k string) {
		shared[k] = 1 // want `shared is declared by the enclosing test`
	}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		record("a")
	})
}

// A CLOSURE THAT IS ONLY DECLARED writes nothing until something calls it.
func TestAnUncalledClosureIsNotAWrite(t *testing.T) {
	shared := map[string]int{}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		unused := func() {
			shared["a"] = 1
		}

		_ = unused
	})
}

// A DEFERRED LITERAL RUNS, so a write inside it races the sibling subtest.
func TestADeferredLiteralRuns(t *testing.T) {
	shared := map[string]int{}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		defer func() {
			shared["a"] = 1 // want `shared is declared by the enclosing test`
		}()
	})
}

// So does one handed to t.Cleanup.
func TestACleanupLiteralRuns(t *testing.T) {
	shared := map[string]int{}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		t.Cleanup(func() {
			shared["a"] = 1 // want `shared is declared by the enclosing test`
		})
	})
}

// A DIRECTIVE THAT SUPPRESSES NOTHING is a standing exemption for whatever lands
// on this line next, carrying a reason written for something else.
func TestAnUnusedDirectiveIsReported(t *testing.T) {
	mine := map[string]int{}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		own := map[string]int{}
		//billet:ignore parallelshared // want `suppresses nothing here`
		own["a"] = 1
		_ = own
	})

	_ = mine
}

// ----- shapes the second review round named -----

// A NAMED CLEANUP is the case that showed the previous fixture was passing for
// the wrong reason: it was entered because the walker fell into every inline
// literal, not because anything understood t.Cleanup. Passed by NAME, it was
// missed entirely.
func TestANamedCleanupIsFollowed(t *testing.T) {
	shared := map[string]int{}

	cleanup := func() {
		shared["a"] = 1 // want `shared is declared by the enclosing test`
	}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		t.Cleanup(cleanup)
	})
}

// A SUBTEST CALLBACK THAT NAMES ITSELF used to recurse until the analyzer
// exhausted its stack -- the same crash the helper closures had, one function up.
func TestASelfNamingSubtestCallbackTerminates(t *testing.T) {
	shared := map[string]int{}

	var f func(t *testing.T)

	f = func(t *testing.T) {
		t.Run("again", f)
	}

	t.Run("first", f)

	_ = shared
}

// A LITERAL IN A SLICE is stored, not run: whether any of it executes is decided
// somewhere else, and reporting it would be a write that may never happen.
func TestALiteralInASliceIsNotAWrite(t *testing.T) {
	shared := map[string]int{}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		handlers := []func(){
			func() { shared["a"] = 1 },
		}

		_ = handlers
	})
}

// A LITERAL HANDED TO SOMETHING ELSE may be called, stored or dropped, and this
// cannot tell which. The miss is deliberate and documented; the alternative is a
// false positive on every callback-taking helper.
func TestALiteralPassedToAnUnknownFunctionIsNotFollowed(t *testing.T) {
	shared := map[string]int{}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		register(func() { shared["a"] = 1 })
	})
}

func register(fn func()) { _ = fn }

// A REASSIGNED CLOSURE VARIABLE resolves to nothing: what the call reaches is
// unknowable from here, and following the first literal would report code that
// does not run.
func TestAReassignedClosureIsNotFollowed(t *testing.T) {
	shared := map[string]int{}

	record := func() { shared["a"] = 1 }
	record = func() {}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		record()
	})
}

// A NESTED SUBTEST OF A PARALLEL SUBTEST runs inside it, so a write there races
// the sibling exactly the same way.
func TestANestedSubtestOfAParallelSubtestIsFollowed(t *testing.T) {
	shared := map[string]int{}

	t.Run("outer", func(t *testing.T) {
		t.Parallel()

		t.Run("inner", func(t *testing.T) {
			shared["a"] = 1 // want `shared is declared by the enclosing test`
		})
	})
}

// A PARENTHESISED INLINE CALLBACK is one subtest, not two: comparing the
// argument against the resolved literal missed the ParenExpr and walked the body
// a second time from the parent's scope.
func TestAParenthesisedCallbackIsOneSubtest(t *testing.T) {
	shared := map[string]int{}

	t.Run("one", (func(t *testing.T) {
		t.Parallel()

		shared["a"] = 1 // want `shared is declared by the enclosing test`
	}))
}

// ----- shapes the third review round named -----

// A CAPTURED OUTER t IS NOT THE SUBTEST'S OWN. This cleanup belongs to the outer
// test and runs after its subtests finish, so the write is not the child's and
// reporting it would be a false positive. Recognising t.Cleanup by the TYPE of
// its receiver did exactly that.
func TestACapturedOuterCleanupIsNotTheSubtestsWrite(t *testing.T) {
	shared := map[string]int{}

	t.Run("child", func(child *testing.T) {
		child.Parallel()

		t.Cleanup(func() {
			shared["x"] = 1
		})
	})
}

// AND A CAPTURED OUTER t.Parallel() DOES NOT MAKE THE CHILD PARALLEL. The child
// here is sequential, so its write is fine.
func TestACapturedOuterParallelDoesNotMakeTheChildParallel(t *testing.T) {
	shared := map[string]int{}

	t.Run("child", func(child *testing.T) {
		// The OUTER t, directly in the child's body. Nothing but the identity
		// rule can decide this one: the literal boundary does not apply, and
		// matching by type would make the child parallel and report the write.
		t.Parallel()

		shared["x"] = 1
	})
}

// ONE CALLBACK USED AS TWO SUBTESTS is one write site, reported once. The cycle
// stack cannot help: a parallel callback never enters it.
func TestOneCallbackUsedTwiceIsReportedOnce(t *testing.T) {
	shared := map[string]int{}

	cb := func(t *testing.T) {
		t.Parallel()

		shared["x"] = 1 // want `shared is declared by the enclosing test`
	}

	t.Run("one", cb)
	t.Run("two", cb)
}

// A MULTI-RESULT REASSIGNMENT is still a reassignment. Stopping at the arity
// mismatch left the stale first literal standing, so the call resolved to code
// that may no longer be what runs.
func TestAMultiResultReassignmentIsNotFollowed(t *testing.T) {
	shared := map[string]int{}

	f := func() { shared["x"] = 1 }
	_, f = pair()

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		f()
	})
}

func pair() (int, func()) { return 0, func() {} }

// A CLOSURE REACHED THROUGH A STRUCT FIELD is a documented miss: the call proves
// it runs, and resolving it needs more than this lexical analyzer does.
func TestAFieldInvokedClosureIsADocumentedMiss(t *testing.T) {
	shared := map[string]int{}

	holder := struct{ fn func() }{fn: func() { shared["x"] = 1 }}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		holder.fn()
	})
}

// ----- shapes the fourth review round named -----

// AN ALIAS IS THE SAME HANDLE UNDER ANOTHER NAME. Comparing raw variable
// identities treated `alias` as somebody else's t, so this subtest was never
// found at all -- a whole file that aliased its handles would have been silently
// unanalysed while the fixture stayed green.
func TestAnAliasedRunIsStillASubtest(t *testing.T) {
	shared := map[string]int{}

	alias := t

	alias.Run("child", func(child *testing.T) {
		child.Parallel()

		shared["x"] = 1 // want `shared is declared by the enclosing test`
	})
}

// And an aliased Parallel still makes the child parallel.
func TestAnAliasedParallelStillCounts(t *testing.T) {
	shared := map[string]int{}

	t.Run("child", func(child *testing.T) {
		alias := child
		alias.Parallel()

		shared["x"] = 1 // want `shared is declared by the enclosing test`
	})
}

// `t := t` is the same shape wearing the same name, and it occurs in this
// repository.
func TestAShadowedSelfAliasIsStillTheSameHandle(t *testing.T) {
	shared := map[string]int{}

	// Inside a block, which is the only place `t := t` is legal -- and the shape
	// this repository actually has, in a range body.
	for range 1 {
		t := t

		t.Run("child", func(child *testing.T) {
			child.Parallel()

			shared["x"] = 1 // want `shared is declared by the enclosing test`
		})
	}
}

// An aliased Cleanup belongs to whoever the alias came from -- here the outer
// test, so the write is not the child's.
func TestAnAliasedOuterCleanupIsStillTheOuterTests(t *testing.T) {
	shared := map[string]int{}

	alias := t

	t.Run("child", func(child *testing.T) {
		child.Parallel()

		alias.Cleanup(func() {
			shared["x"] = 1
		})
	})
}

// ----- shapes the fifth review round named -----

// AN UNCALLED LITERAL DOES NOT MAKE ITS SCOPE PARALLEL, even when it calls
// Parallel on the scope's OWN t. This pins the literal boundary in makesParallel:
// without it the child is classified parallel and the write is reported.
func TestAnUncalledLiteralDoesNotMakeTheScopeParallel(t *testing.T) {
	shared := map[string]int{}

	t.Run("child", func(child *testing.T) {
		_ = func() { child.Parallel() }

		shared["x"] = 1
	})
}

// REBINDING THE PARAMETER DOES NOT CHANGE WHAT THE SCOPE OWNS. Parallel is called
// on the OUTER handle here, so the child is sequential -- which only holds
// because ownT is the formal parameter and is never normalised through
// assignments.
func TestReboundParameterDoesNotChangeOwnership(t *testing.T) {
	shared := map[string]int{}

	t.Run("child", func(child *testing.T) {
		child = t
		child.Parallel()

		shared["x"] = 1
	})
}

// A PARENTHESISED REASSIGNMENT IS STILL A REASSIGNMENT. Asserting the identifier
// directly skipped it, leaving `record` looking singly-assigned and resolving to
// a literal that no longer runs.
func TestAParenthesisedReassignmentIsNotFollowed(t *testing.T) {
	shared := map[string]int{}

	record := func() { shared["x"] = 1 }
	(record) = func() {}

	t.Run("one", func(t *testing.T) {
		t.Parallel()

		record()
	})
}
