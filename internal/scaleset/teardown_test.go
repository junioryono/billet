package scaleset

import (
	"net/http"
	"strings"
	"testing"
)

// Teardown deletes the scale set, by id, after finding it by name.
//
// This path has no UI equivalent — a scale set created through the API has no
// delete control in GitHub's org runner list or on its own detail page — so if
// this is wrong the operator has no fallback. Asserted against the request that
// left the process rather than against a returned value.
func TestDeleteScaleSetIssuesTheDelete(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, listJSON(scaleSetJSON(7, "billet-4vcpu", "self-hosted")))

		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	deletedIt, err := client.DeleteScaleSet(t.Context(), "billet-4vcpu", "billet",
		[]string{"billet-4vcpu", "self-hosted"}, false)
	if err != nil {
		t.Fatalf("DeleteScaleSet: %v", err)
	}

	if !deletedIt {
		t.Error("reported nothing deleted while issuing the delete")
	}

	var deleted bool

	for _, c := range fake.calls("runnerscalesets") {
		if c.Method == http.MethodDelete {
			deleted = true

			// By ID — the id GitHub returned, not billet's idea of one.
			if !strings.HasSuffix(c.Path, "/7") {
				t.Errorf("deleted %q, want the scale set the lookup returned (id 7)", c.Path)
			}
		}
	}

	if !deleted {
		t.Error("no DELETE was issued; the scale set would survive a teardown the operator " +
			"has no other way to perform")
	}
}

// Teardown refuses a scale set whose labels are not this tier's.
//
// The same check adoption makes, and it matters more here. Adopting somebody
// else's scale set misroutes work; DELETING it destroys their configuration, and
// there is no UI path for them to have made it any other way.
func TestDeleteScaleSetRefusesForeignLabels(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			// Same NAME, different labels — somebody else's.
			writeJSON(t, w, listJSON(scaleSetJSON(7, "billet-4vcpu", "self-hosted", "gpu")))

		case r.Method == http.MethodDelete:
			t.Error("billet deleted a scale set carrying a label it never asked for")
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.DeleteScaleSet(t.Context(), "billet-4vcpu", "billet",
		[]string{"billet-4vcpu", "self-hosted"}, false)
	if err == nil {
		t.Fatal("deleted a scale set whose labels differ from the tier's")
	}

	if !strings.Contains(err.Error(), "gpu") {
		t.Errorf("the error does not name the label that differs: %v", err)
	}
}

// Deleting something already gone is a success, not an error.
//
// Teardown is the operation most likely to be re-run after a partial failure, so
// a second attempt must not look like a new problem.
func TestDeleteScaleSetIsIdempotent(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, listJSON())

		case r.Method == http.MethodDelete:
			t.Error("billet issued a DELETE for a scale set that does not exist")
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	deletedIt, err := client.DeleteScaleSet(t.Context(), "billet-4vcpu", "billet",
		[]string{"billet-4vcpu"}, false)
	if err != nil {
		t.Errorf("deleting an absent scale set reported an error: %v", err)
	}

	// Reported as NOT deleted, which is the point: absence is scoped to this
	// runner group, so "nothing here" must not read as "removed it".
	if deletedIt {
		t.Error("claimed to have deleted a scale set that was not there")
	}
}

// --force deletes a scale set whose labels drifted, because refusing without an
// escape hatch leaves it unremovable.
//
// This is the trap the refusal above creates if it is the ONLY route. Labels
// drift — somebody edits them in the UI, an older billet used a different set —
// and then billet will not delete the scale set and GitHub's UI cannot either.
// The operator is left with an object nothing can remove. So the refusal is the
// default, force is the way out, and the refusal names it.
func TestForceDeletesAScaleSetWhoseLabelsDrifted(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, listJSON(scaleSetJSON(7, "billet-4vcpu", "self-hosted", "gpu")))

		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	deletedIt, err := client.DeleteScaleSet(t.Context(), "billet-4vcpu", "billet",
		[]string{"billet-4vcpu", "self-hosted"}, true)
	if err != nil {
		t.Fatalf("force could not delete a drifted scale set: %v", err)
	}

	if !deletedIt {
		t.Error("force reported nothing deleted")
	}
}

// The refusal has to point at the way out, or an operator reading it concludes
// the object is stuck. It previously said "use a different tier name", which
// abandons the undeletable scale set rather than removing it.
func TestTheLabelRefusalNamesTheEscapeHatch(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, listJSON(scaleSetJSON(7, "billet-4vcpu", "self-hosted", "gpu")))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.DeleteScaleSet(t.Context(), "billet-4vcpu", "billet",
		[]string{"billet-4vcpu", "self-hosted"}, false)
	if err == nil {
		t.Fatal("deleted a drifted scale set without --force")
	}

	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal does not tell the operator how to proceed, so the scale set "+
			"reads as unremovable: %v", err)
	}
}
