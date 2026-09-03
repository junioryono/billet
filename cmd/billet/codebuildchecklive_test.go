package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// THE LIVE CHECK'S VERDICT REACHES ITS CALLER, driven end to end against a fake
// CodeBuild.
//
// A structural test proves the judgement is CALLED; it cannot see whether what it
// returns is then appended to the collections `billet check` prints and fails on.
// Discard both slices and the structural test stays green while an over-capacity
// fleet is reported healthy. So this drives checkCodeBuildLive itself, through the
// real provider client, against an endpoint answering BatchGetProjects and
// BatchGetFleets the way AWS does, and asserts on the error it returns and the
// lines it prints.
func TestTheLiveCodeBuildCheckRefusesADeclaredLimitAboveTheFleet(t *testing.T) {
	// The credential chain reads the environment first; any non-empty pair signs.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")

	fleet := map[string]any{
		"name":            "macs",
		"arn":             "arn:aws:codebuild:us-east-1:123456789012:fleet/macs:00000000-0000-0000-0000-000000000000",
		"environmentType": "MAC_ARM",
		"computeType":     "BUILD_GENERAL1_MEDIUM",
		"baseCapacity":    1,
		"status":          map[string]any{"statusCode": "ACTIVE"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")

		var body any

		switch r.Header.Get("X-Amz-Target") {
		case "CodeBuild_20161006.BatchGetProjects":
			body = map[string]any{"projects": []map[string]any{{
				"name":        "billet-macos",
				"arn":         "arn:aws:codebuild:us-east-1:123456789012:project/billet-macos",
				"environment": map[string]any{"type": "MAC_ARM", "computeType": "BUILD_GENERAL1_MEDIUM"},
				"source":      map[string]any{"type": "NO_SOURCE"},
			}}}
		case "CodeBuild_20161006.BatchGetFleets":
			body = map[string]any{"fleets": []map[string]any{fleet}}
		default:
			http.Error(w, `{"__type":"UnknownOperationException"}`, http.StatusBadRequest)

			return
		}

		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	deployment := func(limit int) *config.Config {
		cfg := &config.Config{
			Node: &config.NodeConfig{
				Name:     "cb-mac",
				Provider: config.ProviderCodeBuild,
				CodeBuild: &config.CodeBuildConfig{
					Region:                     "us-east-1",
					Endpoint:                   srv.URL + "/",
					Project:                    "billet-macos",
					EnvironmentType:            config.CodeBuildMacARM,
					FleetARN:                   "arn:aws:codebuild:us-east-1:123456789012:fleet/macs:00000000-0000-0000-0000-000000000000",
					AcceptExternalBuildCeiling: true,
					JITParameterPath:           "/billet/jit",
					BuildTimeoutMinutes:        30,
					QueuedTimeoutMinutes:       15,
					ComputeTypes: []config.RemoteShape{{
						Type: "BUILD_GENERAL1_MEDIUM", VCPU: 8, Memory: 24 * config.GiB, PriceUSDPerHour: 1,
					}},
				},
			},
			Nodes: []config.NodePolicy{{
				Name:         "cb-mac",
				Provider:     config.ProviderCodeBuild,
				GuestOS:      []config.GuestOS{config.GuestMacOS},
				MacOSVMLimit: &limit,
			}},
		}

		return cfg
	}

	// ONE ABOVE THE FLEET: the error names the two numbers.
	var err error

	out := capture(t, func() {
		err = checkCodeBuildLive(t.Context(), deployment(2), nil)
	})

	if err == nil || !strings.Contains(err.Error(), "macos_vm_limit 2") ||
		!strings.Contains(err.Error(), "set nodes[].macos_vm_limit to 1") {
		t.Errorf("a limit of 2 against a fleet of 1 was not refused by the live check: err=%v\n%s", err, out)
	}

	// EXACTLY THE FLEET: no refusal, and the check still printed the fleet it saw,
	// so the pass is a pass through the same path rather than an early return.
	out = capture(t, func() {
		err = checkCodeBuildLive(t.Context(), deployment(1), nil)
	})

	if err != nil {
		t.Errorf("a limit equal to the fleet's capacity was refused: %v\n%s", err, out)
	}

	if !strings.Contains(out, "fleet    macs (MAC_ARM, BUILD_GENERAL1_MEDIUM), capacity 1") {
		t.Errorf("the check did not report the fleet it described:\n%s", out)
	}

	// AND A SCALING FLEET'S WARNING IS PRINTED, not only returned by the judgement.
	fleet["scalingConfiguration"] = map[string]any{"maxCapacity": 2}

	out = capture(t, func() {
		err = checkCodeBuildLive(t.Context(), deployment(2), nil)
	})

	if err != nil {
		t.Errorf("a limit inside the scaling maximum was refused: %v\n%s", err, out)
	}

	if !strings.Contains(out, "warning  this node declares macos_vm_limit 2") ||
		!strings.Contains(out, "may scale to 2") {
		t.Errorf("the scaling warning did not reach the printed report:\n%s", out)
	}
}
