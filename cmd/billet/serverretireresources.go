package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/junioryono/billet/internal/retirement"
)

// retireInvocationForConfig permits only the exact replacement recorded at the
// rewrite boundary. The original resource remains authoritative through archive.
func retireInvocationForConfig(j retirement.Journal) (*retirement.RetainedInvocation, *retireRefusal) {
	want := j.RetainedInvocation
	if want == nil || want.ConfigReplacement == nil ||
		(j.Phase != retirement.PhaseArchived && j.Phase != retirement.PhaseConfigRewritten &&
			j.Phase != retirement.PhaseNodeRestarted && j.Phase != retirement.PhaseDone) {
		return want, nil
	}
	fact, r := retireConfigFact(want.ConfigPath, j)
	if r != nil {
		return nil, r
	}
	if fact != retirement.ConfigStaged {
		return want, nil
	}
	expected := *want
	expected.Resources = slices.Clone(want.Resources)
	for index, resource := range expected.Resources {
		if resource.Path == want.ConfigPath {
			expected.Resources[index] = *want.ConfigReplacement
		}
	}
	return &expected, nil
}

// Required resources are checked even across the node's authorized stop. Only
// its recreatable records and its invocation may change at that operation.
func proveRetireRequiredResources(ctx context.Context, j retirement.Journal) *retireRefusal {
	want, r := retireInvocationForConfig(j)
	if r != nil || want == nil {
		return r
	}
	refuse := func(why string) *retireRefusal { return retireUnknown(retireReasonStopped, why, "") }
	insp := retireOperationInspector()
	if r := proveRetireConfigLeaf(want.ConfigPath); r != nil {
		return refuse(r.Reason + ": " + r.Why)
	}
	if err := insp.AdmitRetainedInputs(retainedRequiredInputs(want), j.IdentityDir); err != nil {
		return refuse(err.Error())
	}
	if err := insp.AdmitRetainedUnitPaths(ctx, nodeUnit, j.IdentityDir); err != nil {
		return refuse(err.Error())
	}
	if j.InstalledSHA256 != "" {
		fact, r := retireConfigFact(want.ConfigPath, j)
		if r != nil {
			return r
		}
		if fact != retirement.ConfigInstalled && fact != retirement.ConfigStaged {
			return refuse("retained-input-evidence: configuration no longer names the captured inputs")
		}
	}
	environment, err := requiredRetireEnvironmentFiles(ctx)
	if err != nil {
		return refuse(err.Error())
	}
	required := append(slices.Clone(environment), want.ConfigPath)
	if err := insp.AdmitRetainedInputs(required, j.IdentityDir); err != nil {
		return refuse(err.Error())
	}
	captured := make(map[string]retirement.RetainedResource)
	for _, expected := range want.Resources {
		if expected.Runtime && !expected.GuestNetwork {
			continue
		}
		captured[expected.Path] = expected
	}
	if r := proveRetireRequiredIdentities(want); r != nil {
		return r
	}
	for _, path := range required {
		resource, ok := captured[path]
		if !ok || resource.Absent {
			return refuse("retained-input-evidence: required input has no captured identity: " + path)
		}
		// Use the bounded regular-file reader; contents never enter evidence or
		// diagnostics. A required environment file must be readable at restart.
		limit := int64(maxEnvironmentBytes)
		if path == want.ConfigPath {
			limit = maxConfigBytes
		}
		if _, _, err := hashRegular(path, limit); err != nil {
			return refuse(fmt.Sprintf("retained-input-unreadable: %s: %v", path, err))
		}
	}
	after, err := requiredRetireEnvironmentFiles(ctx)
	if err != nil {
		return refuse(err.Error())
	}
	if !slices.Equal(environment, after) {
		return refuse("retained-input-environment-unknown: EnvironmentFiles changed during observation")
	}
	if err := insp.AdmitRetainedInputs(retainedRequiredInputs(want), j.IdentityDir); err != nil {
		return refuse(err.Error())
	}
	if err := insp.AdmitRetainedUnitPaths(ctx, nodeUnit, j.IdentityDir); err != nil {
		return refuse(err.Error())
	}
	return proveRetireRequiredIdentities(want)
}

func proveRetireRequiredIdentities(want *retirement.RetainedInvocation) *retireRefusal {
	refuse := func(why string) *retireRefusal { return retireUnknown(retireReasonStopped, why, "") }
	for _, expected := range want.Resources {
		if expected.Runtime && !expected.GuestNetwork {
			continue
		}
		now, err := observeRetireResource(expected.Path)
		if err != nil {
			return refuse(err.Error())
		}
		if now.ResolvedPath != expected.ResolvedPath {
			return refuse("retained path resolution changed: " + expected.Path)
		}
		now.GuestNetwork = expected.GuestNetwork
		if now != expected {
			return refuse("retained node resource changed: " + expected.Path)
		}
	}
	return nil
}
