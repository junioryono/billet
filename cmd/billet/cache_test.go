package main

import "testing"

func TestCacheNamespacesIncludeTheSite(t *testing.T) {
	t.Parallel()

	if got := cacheNamespace("deployment-1", "home"); got != "deployment-1/home" {
		t.Errorf("named site namespace = %q", got)
	}
	if got := cacheNamespace("deployment-1", ""); got != "deployment-1/local" {
		t.Errorf("implicit site namespace = %q", got)
	}
	if cacheNamespace("deployment-1", "home") == cacheNamespace("deployment-1", "aws-us-west-2") {
		t.Fatal("two sites share one cache namespace")
	}
}

func TestCachePolicyScopeRequiresOneUnambiguousTarget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		org, repository string
		wantOwner       string
		wantRepository  string
		wantError       bool
	}{
		{org: "acme", wantOwner: "acme"},
		{repository: "acme/api", wantOwner: "acme", wantRepository: "api"},
		{wantError: true},
		{org: "acme", repository: "acme/api", wantError: true},
		{repository: "api", wantError: true},
		{repository: "acme/api/other", wantError: true},
	} {
		scope, _, err := cachePolicyScope(tc.org, tc.repository)
		if (err != nil) != tc.wantError {
			t.Errorf("cachePolicyScope(%q, %q) error=%v, want error %t",
				tc.org, tc.repository, err, tc.wantError)
		}
		if err == nil && (scope.Owner != tc.wantOwner || scope.Repository != tc.wantRepository) {
			t.Errorf("cachePolicyScope(%q, %q) = %+v", tc.org, tc.repository, scope)
		}
	}
}
