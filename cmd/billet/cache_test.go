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
