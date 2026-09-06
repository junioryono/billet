package main

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imagesource"
	"github.com/junioryono/billet/internal/releasesource"
)

func TestReleasePolicyBindsTheRepositoryUnlessTheSignerIsOverridden(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		release  *config.ReleaseConfig
		wantRepo string
	}{
		{name: "default", wantRepo: "https://github.com/" + releasesource.DefaultRepo},
		{name: "explicit default", release: &config.ReleaseConfig{
			SigningIdentity: releasesource.DefaultSigningIdentity,
			SigningIssuer:   imagesource.GitHubOIDCIssuer,
		}, wantRepo: "https://github.com/" + releasesource.DefaultRepo},
		{name: "custom signer", release: &config.ReleaseConfig{
			SigningIdentity: "https://example.org/release",
			SigningIssuer:   "https://example.org/issuer",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			policy, err := releasePolicyFor(&config.Config{Release: tc.release}, false)
			if err != nil {
				t.Fatal(err)
			}
			if !policy.Required || policy.SourceRepositoryURI != tc.wantRepo {
				t.Fatalf("release policy = %+v, want required with repository %q", policy, tc.wantRepo)
			}
			if tc.release != nil && (policy.Identity != tc.release.SigningIdentity ||
				policy.Issuer != tc.release.SigningIssuer) {
				t.Fatalf("release policy lost the configured signer: %+v", policy)
			}
		})
	}
}
