package scaleset

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Provisioning a tier that GitHub has never heard of creates the scale set, with
// the labels billet asked for.
//
// Asserted against the REQUEST BODY rather than the returned struct, because the
// struct is billet's own type and would agree with billet's own mistake. What
// matters is what went on the wire.
func TestEnsureScaleSetCreatesWhatDoesNotExist(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		// No scale set by that name yet.
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, listJSON())

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, scaleSetJSON(7,
				"billet-4vcpu", "self-hosted"))

		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	set, err := client.EnsureScaleSet(t.Context(), "billet-4vcpu", "billet",
		[]string{"billet-4vcpu", "self-hosted"})
	if err != nil {
		t.Fatalf("EnsureScaleSet: %v", err)
	}

	if set.ID != 7 {
		t.Errorf("adopted scale set %d, want the one the service returned (7)", set.ID)
	}

	creates := fake.calls("runnerscalesets")

	var body string

	for _, c := range creates {
		if c.Method == http.MethodPost {
			body = c.Body
		}
	}

	if body == "" {
		t.Fatal("billet never asked the service to create the scale set")
	}

	var sent struct {
		Name   string `json:"name"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		RunnerSetting struct {
			DisableUpdate bool `json:"disableUpdate"`
		} `json:"RunnerSetting"`
	}

	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("the create body is not valid json (%v): %s", err, body)
	}

	if sent.Name != "billet-4vcpu" {
		t.Errorf("created a scale set named %q", sent.Name)
	}

	got := make(map[string]bool, len(sent.Labels))
	for _, l := range sent.Labels {
		got[l.Name] = true
	}

	for _, want := range []string{"billet-4vcpu", "self-hosted"} {
		if !got[want] {
			t.Errorf("label %q was not sent; runs-on would not route here", want)
		}
	}

	// Runner self-update stays ENABLED on purpose: GitHub stops queuing to
	// runners older than about a month, so disabling it turns maintenance into an
	// outage on someone else's schedule.
	if sent.RunnerSetting.DisableUpdate {
		t.Error("runner self-update was disabled; GitHub will stop queuing to these runners")
	}
}

// An existing scale set with the labels billet wants is adopted, not duplicated.
func TestEnsureScaleSetAdoptsAMatchingSet(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			// Same labels, different order, which must not matter.
			writeJSON(t, w, listJSON(scaleSetJSON(9,
				"self-hosted", "billet-4vcpu")))

		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	set, err := client.EnsureScaleSet(t.Context(), "billet-4vcpu", "billet",
		[]string{"billet-4vcpu", "self-hosted"})
	if err != nil {
		t.Fatalf("EnsureScaleSet: %v", err)
	}

	if set.ID != 9 {
		t.Errorf("adopted %d, want the existing set 9", set.ID)
	}

	for _, c := range fake.calls("runnerscalesets") {
		if c.Method == http.MethodPost {
			t.Error("billet created a second scale set instead of adopting the existing one")
		}
	}
}

// A scale set that already exists with DIFFERENT labels is refused rather than
// adopted or rewritten. Labels decide which runs-on values route to this tier,
// and the tier's capacity is sized for its own.
func TestEnsureScaleSetRefusesForeignLabels(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, listJSON(scaleSetJSON(9,
				"billet-4vcpu", "self-hosted", "gpu")))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.EnsureScaleSet(t.Context(), "billet-4vcpu", "billet",
		[]string{"billet-4vcpu", "self-hosted"})
	if err == nil {
		t.Fatal("adopted a scale set carrying a label billet never asked for")
	}

	if !strings.Contains(err.Error(), "gpu") {
		t.Errorf("the error does not name the offending label: %v", err)
	}
}
