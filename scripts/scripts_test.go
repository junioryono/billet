package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRepositoryKeyVerifierRequiresTheExactPrimaryKeySet(t *testing.T) {
	t.Parallel()

	pinned := strings.Repeat("A", 40)
	attacker := strings.Repeat("B", 40)
	verifier, err := filepath.Abs(filepath.Join("..", "ansible_collections", "junioryono", "billet", "roles", "development_host", "files", "verify-repository-key.sh"))
	if err != nil {
		t.Fatalf("absolute verifier path: %v", err)
	}
	for _, tc := range []struct {
		name         string
		fingerprints []string
		wantSuccess  bool
	}{
		{name: "one pinned primary", fingerprints: []string{pinned}, wantSuccess: true},
		{name: "pinned plus an extra primary", fingerprints: []string{pinned, attacker}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tools := t.TempDir()
			var output strings.Builder
			output.WriteString("#!/bin/sh\n")
			for _, fingerprint := range tc.fingerprints {
				output.WriteString("printf '%s\\n' 'pub:::::::::' 'fpr:::::::::")
				output.WriteString(fingerprint)
				output.WriteString(":'\n")
			}
			if err := os.WriteFile(filepath.Join(tools, "gpg"), []byte(output.String()), 0o755); err != nil {
				t.Fatalf("write fake gpg: %v", err)
			}
			cmd := exec.CommandContext(t.Context(), verifier, filepath.Join(tools, "bundle.asc"), pinned)
			cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"))
			err := cmd.Run()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("verification error = %v; want success %t", err, tc.wantSuccess)
			}
		})
	}
}

func TestProductionSurfacesUseTheTestedSecurityHelpers(t *testing.T) {
	t.Parallel()

	assertContains(t,
		filepath.Join("..", "ansible_collections", "junioryono", "billet", "roles", "development_host", "tasks", "packages-linux.yml"),
		`- "{{ billet_development_apt_stage }}/verify-repository-key.sh"`)
	assertContains(t, filepath.Join("..", ".github", "workflows", "cut-release.yml"),
		"run: scripts/plan-release.sh")
	assertContains(t, filepath.Join("..", ".github", "workflows", "cut-release.yml"),
		"ansible_collections/junioryono/billet/galaxy.yml")
	assertContains(t, filepath.Join("..", ".github", "workflows", "cut-release.yml"),
		"scripts/check-release-metadata.sh")
	assertContains(t, filepath.Join("..", ".github", "workflows", "cut-release.yml"),
		`[0-9]+\$#version: ${COLLECTION_VERSION}#`)
	assertContains(t, filepath.Join("..", ".github", "workflows", "release.yml"),
		"run: scripts/check-release-metadata.sh")
	assertContains(t, filepath.Join("..", ".github", "workflows", "release.yml"),
		"ref: ${{ github.workflow_sha }}")
	assertContains(t, filepath.Join("..", ".github", "workflows", "release.yml"),
		"run: .billet-release-tools/scripts/verify-release-attestation.sh")
	assertContains(t, filepath.Join("..", ".github", "workflows", "release.yml"),
		"attestations: read")
	assertContains(t, filepath.Join("..", ".github", "workflows", "release.yml"),
		"GH_TOKEN: ${{ github.token }}")
	assertContains(t, filepath.Join("..", ".github", "workflows", "cut-release.yml"),
		"attestations: read")
}

func TestReleaseWorkflowVerifiesThePublishedReleaseAfterGoReleaser(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(body)
	goreleaser := strings.Index(workflow, "uses: goreleaser/goreleaser-action@v7")
	checkout := strings.Index(workflow, "name: Check out release verification code")
	verifyStep := strings.Index(workflow, "name: Verify immutable release attestation")
	verify := strings.Index(workflow, "run: .billet-release-tools/scripts/verify-release-attestation.sh")
	if goreleaser < 0 || checkout <= goreleaser || verifyStep <= checkout || verify <= verifyStep {
		t.Fatalf("release verification steps are missing or out of order")
	}
	checkoutStep := workflow[checkout:verifyStep]
	for _, required := range []string{
		"ref: ${{ github.workflow_sha }}",
		"path: .billet-release-tools",
		"sparse-checkout: scripts/verify-release-attestation.sh",
	} {
		if !strings.Contains(checkoutStep, required) {
			t.Fatalf("release helper checkout is missing %q", required)
		}
	}
	verifyBody := workflow[verifyStep:]
	for _, required := range []string{
		"GH_TOKEN: ${{ github.token }}",
		"RELEASE_TAG: ${{ inputs.tag || github.ref_name }}",
		"run: .billet-release-tools/scripts/verify-release-attestation.sh",
	} {
		if !strings.Contains(verifyBody, required) {
			t.Fatalf("release verification step is missing %q", required)
		}
	}
	if strings.Contains(checkoutStep+verifyBody, "continue-on-error:") {
		t.Fatal("release verification must not continue on error")
	}

	callerBody, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "cut-release.yml"))
	if err != nil {
		t.Fatalf("read cut-release workflow: %v", err)
	}
	caller := string(callerBody)
	releaseJob := strings.Index(caller, "\n  release:\n")
	if releaseJob < 0 || !strings.Contains(caller[releaseJob:], "uses: ./.github/workflows/release.yml") || !strings.Contains(caller[releaseJob:], "attestations: read") {
		t.Fatal("cut-release caller does not delegate attestation-read permission to the reusable release job")
	}
}

func TestGuestImageReleasesCannotTakeOverTheBinaryLatestChannel(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "guest-image.yml"))
	if err != nil {
		t.Fatalf("read guest-image workflow: %v", err)
	}
	workflow := string(body)
	buildJob := strings.Index(workflow, "\n  build:\n")
	publishJob := strings.Index(workflow, "\n  publish:\n")
	if buildJob < 0 || publishJob <= buildJob {
		t.Fatal("separate guest build and publication jobs are missing or out of order")
	}
	buildBody := workflow[buildJob:publishJob]
	for _, required := range []string{
		"contents: read",
		"persist-credentials: false",
		"if: always()",
		"uses: actions/upload-artifact@v4",
	} {
		if !strings.Contains(buildBody, required) {
			t.Fatalf("unprivileged guest build is missing %q", required)
		}
	}
	for _, forbidden := range []string{"contents: write", "id-token: write", "Install cosign", "gh release create"} {
		if strings.Contains(buildBody, forbidden) {
			t.Fatalf("branch-controlled guest build retains publication capability %q", forbidden)
		}
	}
	publishBody := workflow[publishJob:]
	for _, required := range []string{
		"needs: build",
		"if: github.ref == 'refs/heads/main'",
		"contents: write",
		"id-token: write",
		"uses: actions/download-artifact@v5",
		"name: Install cosign",
		"name: Sign the manifest",
		"run: scripts/publish-guest-release.sh out",
		"name: Advance the guest channel",
		"expires_epoch=$((published_epoch + 10 * 24 * 60 * 60))",
		`"release_immutable":true`,
		"--bundle out/current.sigstore.json",
		"out/current.json",
		"git -C \"$channel_dir\" add current.json current.sigstore.json",
		"git -C \"$channel_dir\" push origin HEAD:refs/heads/guest-channel",
	} {
		if !strings.Contains(publishBody, required) {
			t.Fatalf("main-only guest publication is missing %q", required)
		}
	}
	immutableCheck := strings.Index(publishBody, "run: scripts/publish-guest-release.sh out")
	channelAdvance := strings.Index(publishBody, "name: Advance the guest channel")
	if immutableCheck < 0 || channelAdvance <= immutableCheck {
		t.Fatal("guest channel advances before the dated release is proved immutable")
	}
	for _, forbidden := range []string{
		"release create guest-latest",
		"release edit guest-latest",
		"release upload guest-latest",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("an immutable release cannot serve as a rolling guest-image pointer: found %q", forbidden)
		}
	}
}

func TestGuestReleasePublisherRefusesAConflictingTag(t *testing.T) {
	publisher, err := filepath.Abs("publish-guest-release.sh")
	if err != nil {
		t.Fatalf("absolute guest release publisher path: %v", err)
	}
	for _, tc := range []struct {
		name        string
		mode        string
		immutable   string
		tag         string
		wantSuccess bool
		wantRelease bool
	}{
		{name: "new immutable release", mode: "success", immutable: "true", tag: "guest-20260821-000000", wantSuccess: true, wantRelease: true},
		{name: "conflicting existing tag", mode: "conflict", immutable: "true", tag: "guest-20260821-000000"},
		{name: "mutable release", mode: "success", immutable: "false", tag: "guest-20260821-000000", wantRelease: true},
		{name: "invalid tag", mode: "success", immutable: "true", tag: "guest-latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := t.TempDir()
			calls := filepath.Join(tools, "calls.log")
			writeExecutable(t, filepath.Join(tools, "git"), `#!/bin/sh
printf 'git %s\n' "$*" >> "$BILLET_TEST_CALLS"
if [ "$BILLET_TEST_MODE" = conflict ] && [ "$1" = tag ]; then exit 1; fi
exit 0
`)
			writeExecutable(t, filepath.Join(tools, "gh"), `#!/bin/sh
printf 'gh %s\n' "$*" >> "$BILLET_TEST_CALLS"
if [ "$1 $2" = "release create" ]; then
    case " $* " in *" --verify-tag "*) ;; *) exit 95 ;; esac
    case " $* " in *" --target "*) exit 94 ;; esac
    exit 0
fi
if [ "$1 $2" = "release view" ]; then printf '%s\n' "$BILLET_TEST_IMMUTABLE"; exit 0; fi
exit 97
`)

			cmd := exec.CommandContext(t.Context(), publisher, t.TempDir())
			cmd.Env = append(os.Environ(),
				"PATH="+tools+":"+os.Getenv("PATH"),
				"BILLET_TEST_CALLS="+calls,
				"BILLET_TEST_MODE="+tc.mode,
				"BILLET_TEST_IMMUTABLE="+tc.immutable,
				"GITHUB_REPOSITORY=junioryono/billet",
				"GITHUB_SHA="+strings.Repeat("a", 40),
				"TAG="+tc.tag,
			)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("publication error = %v; want success %t\n%s", err, tc.wantSuccess, output)
			}
			callBody, err := os.ReadFile(calls)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read calls: %v", err)
			}
			callsText := string(callBody)
			releaseCall := "gh release create " + tc.tag
			if strings.Contains(callsText, releaseCall) != tc.wantRelease {
				t.Fatalf("release call presence does not match want %t\n%s", tc.wantRelease, callsText)
			}
			if tc.wantRelease {
				push := "git push origin refs/tags/" + tc.tag
				if !strings.Contains(callsText, push) || strings.Index(callsText, push) >= strings.Index(callsText, releaseCall) {
					t.Fatalf("release was not preceded by the exact tag push\n%s", callsText)
				}
			}
		})
	}
}

func TestReleaseMetadataMustMatchTheTag(t *testing.T) {
	t.Parallel()

	checker, err := filepath.Abs("check-release-metadata.sh")
	if err != nil {
		t.Fatalf("absolute metadata checker path: %v", err)
	}
	for _, tc := range []struct {
		name              string
		tag               string
		collectionVersion string
		actionVersion     string
		wantSuccess       bool
	}{
		{name: "matching release", tag: "v0.4.3", collectionVersion: "0.4.3", actionVersion: "v0.4.3", wantSuccess: true},
		{name: "stale collection", tag: "v0.4.3", collectionVersion: "0.4.2", actionVersion: "v0.4.3"},
		{name: "stale action", tag: "v0.4.3", collectionVersion: "0.4.3", actionVersion: "v0.4.2"},
		{name: "invalid tag", tag: "release-4", collectionVersion: "4.0.0", actionVersion: "release-4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repository := t.TempDir()
			collection := filepath.Join(repository, "ansible_collections", "junioryono", "billet")
			actions := filepath.Join(repository, "actions")
			if err := os.MkdirAll(collection, 0o755); err != nil {
				t.Fatalf("create collection directory: %v", err)
			}
			if err := os.MkdirAll(actions, 0o755); err != nil {
				t.Fatalf("create actions directory: %v", err)
			}
			if err := os.WriteFile(filepath.Join(collection, "galaxy.yml"), []byte("version: "+tc.collectionVersion+"\n"), 0o600); err != nil {
				t.Fatalf("write galaxy metadata: %v", err)
			}
			actionBody := fmt.Sprintf("  uses: junioryono/billet/actions/one@%s\n  uses: junioryono/billet/actions/two@%s\n  uses: junioryono/billet/actions/three@%s\n", tc.actionVersion, tc.actionVersion, tc.actionVersion)
			if err := os.WriteFile(filepath.Join(actions, "action.yml"), []byte(actionBody), 0o600); err != nil {
				t.Fatalf("write action metadata: %v", err)
			}
			cmd := exec.CommandContext(t.Context(), checker)
			cmd.Dir = repository
			cmd.Env = append(os.Environ(), "RELEASE_TAG="+tc.tag)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("metadata check error = %v; want success %t\n%s", err, tc.wantSuccess, output)
			}
		})
	}
}

func TestReleaseAttestationVerifierRetriesOnlyPropagationDelay(t *testing.T) {
	t.Parallel()

	verifier, err := filepath.Abs("verify-release-attestation.sh")
	if err != nil {
		t.Fatalf("absolute attestation verifier path: %v", err)
	}
	for _, tc := range []struct {
		name         string
		mode         string
		failures     int
		attempts     int
		immutable    string
		wantAPICalls int
		wantVerify   int
		wantSuccess  bool
	}{
		{name: "attestation appears", mode: "eventual", failures: 2, attempts: 4, immutable: "true", wantAPICalls: 3, wantVerify: 1, wantSuccess: true},
		{name: "release attestation is on a later page", mode: "later-page", attempts: 4, immutable: "true", wantAPICalls: 1, wantVerify: 1, wantSuccess: true},
		{name: "unrelated attestation precedes release attestation", mode: "unrelated-first", attempts: 4, immutable: "true", wantAPICalls: 2, wantVerify: 2, wantSuccess: true},
		{name: "attestation stays absent", mode: "eventual", failures: 3, attempts: 2, immutable: "true", wantAPICalls: 2},
		{name: "mutable release", mode: "eventual", attempts: 4, immutable: "false"},
		{name: "authentication failure", mode: "401", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "authorization failure", mode: "403", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "throttled", mode: "429", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "server failure", mode: "500", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "transport failure", mode: "transport", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "empty successful response exhausts its bound", mode: "empty", attempts: 2, immutable: "true", wantAPICalls: 2},
		{name: "malformed API response", mode: "malformed", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "missing page", mode: "missing-page", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "non-object page", mode: "nonobject-page", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "missing attestations array", mode: "missing-attestations", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "non-array attestations", mode: "nonarray-attestations", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "non-object attestation", mode: "nonobject-attestation", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "missing initiator", mode: "missing-initiator", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "null initiator", mode: "null-initiator", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "empty initiator", mode: "empty-initiator", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "missing bundle URL", mode: "missing-bundle-url", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "null bundle URL", mode: "null-bundle-url", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "non-string bundle URL", mode: "nonstring-bundle-url", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "empty bundle URL", mode: "empty-bundle-url", attempts: 4, immutable: "true", wantAPICalls: 1},
		{name: "malformed verified response", mode: "malformed-verification", attempts: 4, immutable: "true", wantAPICalls: 1, wantVerify: 1},
		{name: "verified response names another tag", mode: "mismatched-verification", attempts: 4, immutable: "true", wantAPICalls: 1, wantVerify: 1},
		{name: "final verification failure", mode: "verify-failure", attempts: 4, immutable: "true", wantAPICalls: 1, wantVerify: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tools := t.TempDir()
			calls := filepath.Join(tools, "calls.log")
			apiCalls := filepath.Join(tools, "api-calls")
			if err := os.WriteFile(apiCalls, []byte("0\n"), 0o600); err != nil {
				t.Fatalf("initialize API calls: %v", err)
			}
			fakeGH := `#!/bin/sh
printf 'gh %s\n' "$*" >> "$BILLET_TEST_CALLS"
if [ "$1 $2" = "release view" ]; then
    printf '%s\n' "$BILLET_TEST_IMMUTABLE"
    exit 0
fi
if [ "$1" = api ]; then
    count=$(sed -n '1p' "$BILLET_TEST_API_CALLS")
    count=$((count + 1))
    printf '%s\n' "$count" > "$BILLET_TEST_API_CALLS"
    case "$BILLET_TEST_MODE" in
			eventual)
            if [ "$count" -le "$BILLET_TEST_FAILURES" ]; then
                printf 'gh: Not Found (HTTP 404)\n' >&2
                exit 1
            fi
			printf '[{"attestations":[{"initiator":"github","bundle_url":"https://attestations.example/v0.2.0.json.sn"}]}]\n'
            ;;
        401|403|429|500)
			printf 'gh: failure (HTTP %s)\n' "$BILLET_TEST_MODE" >&2
            exit 1
            ;;
        transport)
            printf 'connection reset by peer\n' >&2
            exit 1
            ;;
        empty)
			printf '[{"attestations":[]}]\n'
            ;;
        malformed)
			printf '{not json\n'
            ;;
		missing-page)
			printf '[]\n'
			;;
		nonobject-page)
			printf '[null]\n'
			;;
		missing-attestations)
			printf '[{}]\n'
			;;
		nonarray-attestations)
			printf '[{"attestations":{}}]\n'
			;;
		nonobject-attestation)
			printf '[{"attestations":[null]}]\n'
			;;
		missing-initiator)
			printf '[{"attestations":[{}]}]\n'
			;;
		null-initiator)
			printf '[{"attestations":[{"initiator":null}]}]\n'
			;;
		empty-initiator)
			printf '[{"attestations":[{"initiator":""}]}]\n'
			;;
		missing-bundle-url)
			printf '[{"attestations":[{"initiator":"github"}]}]\n'
			;;
		null-bundle-url)
			printf '[{"attestations":[{"initiator":"github","bundle_url":null}]}]\n'
			;;
		nonstring-bundle-url)
			printf '[{"attestations":[{"initiator":"github","bundle_url":42}]}]\n'
			;;
		empty-bundle-url)
			printf '[{"attestations":[{"initiator":"github","bundle_url":""}]}]\n'
			;;
		verify-failure|malformed-verification|mismatched-verification)
			printf '[{"attestations":[{"initiator":"github","bundle_url":"https://attestations.example/v0.2.0.json.sn"}]}]\n'
			;;
		unrelated-first)
			if [ "$count" -eq 1 ]; then
				printf '[{"attestations":[{"initiator":"github","bundle_url":"https://attestations.example/v0.1.0.json.sn"}]}]\n'
			else
				printf '[{"attestations":[{"initiator":"github","bundle_url":"https://attestations.example/v0.2.0.json.sn"}]}]\n'
			fi
			;;
		later-page)
			printf '[{"attestations":[{"initiator":"github","bundle_url":"https://attestations.example/v0.1.0.json.sn"}]},{"attestations":[{"initiator":"github","bundle_url":"https://attestations.example/v0.2.0.json.sn"}]}]\n'
			;;
    esac
    exit 0
fi
if [ "$1 $2" = "release verify" ]; then
	if [ "$BILLET_TEST_MODE" = unrelated-first ] && [ "$(sed -n '1p' "$BILLET_TEST_API_CALLS")" -eq 1 ]; then
		printf 'no attestations found for release v0.2.0 in billet\n' >&2
		exit 1
	fi
    if [ "$BILLET_TEST_MODE" = verify-failure ]; then
        printf 'digest mismatch\n' >&2
        exit 1
    fi
	if [ "$BILLET_TEST_MODE" = malformed-verification ]; then
		printf '{not json\n'
		exit 0
	fi
	verified_tag=v0.2.0
	if [ "$BILLET_TEST_MODE" = mismatched-verification ]; then verified_tag=v0.1.0; fi
	printf '{"attestation":{"bundle_url":"https://attestations.example/%s.json.sn","initiator":"github"},"verificationResult":{"statement":{"predicate":{"tag":"%s"}}}}\n' "$verified_tag" "$verified_tag"
    exit 0
fi
exit 97
`
			writeExecutable(t, filepath.Join(tools, "gh"), fakeGH)
			writeExecutable(t, filepath.Join(tools, "git"), `#!/bin/sh
printf 'git %s\n' "$*" >> "$BILLET_TEST_CALLS"
case "$*" in
    "rev-parse --show-object-format") printf 'sha1\n' ;;
    "rev-parse refs/tags/v0.2.0") printf '256ab3dd4414643c5acc57055f7a81cff99bc4d1\n' ;;
    *) exit 98 ;;
esac
`)
			writeExecutable(t, filepath.Join(tools, "timeout"), "#!/bin/sh\nprintf 'timeout %s\\n' \"$*\" >> \"$BILLET_TEST_CALLS\"\n[ \"$1\" = --signal=KILL ] || exit 96\nshift\nshift\nexec \"$@\"\n")
			cmd := exec.CommandContext(t.Context(), verifier)
			cmd.Env = append(os.Environ(),
				"PATH="+tools+":"+os.Getenv("PATH"),
				"BILLET_TEST_CALLS="+calls,
				"BILLET_TEST_API_CALLS="+apiCalls,
				"BILLET_TEST_MODE="+tc.mode,
				fmt.Sprintf("BILLET_TEST_FAILURES=%d", tc.failures),
				"BILLET_TEST_IMMUTABLE="+tc.immutable,
				"GITHUB_REPOSITORY=junioryono/billet",
				"RELEASE_TAG=v0.2.0",
				fmt.Sprintf("RELEASE_VERIFY_ATTEMPTS=%d", tc.attempts),
				"RELEASE_VERIFY_INTERVAL_SECONDS=0",
				"RELEASE_VERIFY_DEADLINE_SECONDS=30",
				"RELEASE_VERIFY_ATTEMPT_TIMEOUT_SECONDS=15",
				"GH_TOKEN=sentinel-secret-that-must-not-leak",
			)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("verification error = %v; want success %t\n%s", err, tc.wantSuccess, output)
			}
			apiBody, err := os.ReadFile(apiCalls)
			if err != nil {
				t.Fatalf("read API calls: %v", err)
			}
			if string(apiBody) != fmt.Sprintf("%d\n", tc.wantAPICalls) {
				t.Fatalf("API calls = %q; want %d\n%s", apiBody, tc.wantAPICalls, output)
			}
			callBody, err := os.ReadFile(calls)
			if err != nil {
				t.Fatalf("read calls: %v", err)
			}
			callsText := string(callBody)
			if strings.Contains(string(output)+callsText, "sentinel-secret-that-must-not-leak") {
				t.Fatal("GH_TOKEN leaked into output or command arguments")
			}
			verifyCall := "gh release verify v0.2.0 --repo junioryono/billet --format json\n"
			verifyCalls := 0
			for _, line := range strings.Split(callsText, "\n") {
				if line+"\n" == verifyCall {
					verifyCalls++
				}
			}
			if verifyCalls != tc.wantVerify {
				t.Fatalf("verification calls = %d; want %d\n%s", verifyCalls, tc.wantVerify, callsText)
			}
			if tc.wantAPICalls > 0 {
				wantAPI := "gh api --paginate --slurp repos/junioryono/billet/attestations/sha1:256ab3dd4414643c5acc57055f7a81cff99bc4d1?predicate_type=release&per_page=100\n"
				if !strings.Contains(callsText, wantAPI) {
					t.Fatalf("missing exact attestation API call\n%s", callsText)
				}
				if tc.wantVerify > 0 && strings.LastIndex(callsText, wantAPI) >= strings.LastIndex(callsText, verifyCall) {
					t.Fatalf("verification did not follow successful attestation discovery\n%s", callsText)
				}
			}
		})
	}
}

func TestReleaseAttestationVerifierAcceptsALightweightTag(t *testing.T) {
	verifier, err := filepath.Abs("verify-release-attestation.sh")
	if err != nil {
		t.Fatalf("absolute attestation verifier path: %v", err)
	}
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.name", "Billet Test")
	runGit(t, repository, "config", "user.email", "billet@example.invalid")
	runGit(t, repository, "commit", "--allow-empty", "--quiet", "-m", "fixture")
	runGit(t, repository, "tag", "v0.2.0")

	tools := t.TempDir()
	writeExecutable(t, filepath.Join(tools, "gh"), `#!/bin/sh
case "$1 $2" in
    "release view") printf 'true\n' ;;
    "api --paginate") printf '[{"attestations":[{"initiator":"github","bundle_url":"https://attestations.example/v0.2.0.json.sn"}]}]\n' ;;
    "release verify") printf '{"attestation":{"bundle_url":"https://attestations.example/v0.2.0.json.sn","initiator":"github"},"verificationResult":{"statement":{"predicate":{"tag":"v0.2.0"}}}}\n' ;;
    *) exit 97 ;;
esac
`)
	writeExecutable(t, filepath.Join(tools, "timeout"), "#!/bin/sh\n[ \"$1\" = --signal=KILL ] || exit 96\nshift\nshift\nexec \"$@\"\n")

	cmd := exec.CommandContext(t.Context(), verifier)
	cmd.Dir = repository
	cmd.Env = append(os.Environ(),
		"PATH="+tools+":"+os.Getenv("PATH"),
		"GITHUB_REPOSITORY=junioryono/billet",
		"RELEASE_TAG=v0.2.0",
		"RELEASE_VERIFY_ATTEMPTS=1",
		"RELEASE_VERIFY_INTERVAL_SECONDS=0",
		"RELEASE_VERIFY_DEADLINE_SECONDS=30",
		"RELEASE_VERIFY_ATTEMPT_TIMEOUT_SECONDS=15",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lightweight tag was refused: %v\n%s", err, output)
	}
}

func TestReleaseAttestationVerifierBoundsABlockedAPIRequest(t *testing.T) {
	verifier, err := filepath.Abs("verify-release-attestation.sh")
	if err != nil {
		t.Fatalf("absolute attestation verifier path: %v", err)
	}
	tools := t.TempDir()
	writeExecutable(t, filepath.Join(tools, "gh"), `#!/bin/sh
if [ "$1 $2" = "release view" ]; then printf 'true\n'; exit 0; fi
if [ "$1" = api ]; then exec /usr/bin/perl -e '$SIG{TERM} = "IGNORE"; while (1) {}'; fi
exit 97
`)
	writeExecutable(t, filepath.Join(tools, "git"), `#!/bin/sh
case "$*" in
    "rev-parse --show-object-format") printf 'sha1\n' ;;
    "rev-parse refs/tags/v0.2.0") printf '256ab3dd4414643c5acc57055f7a81cff99bc4d1\n' ;;
    *) exit 98 ;;
esac
`)
	cmd := exec.CommandContext(t.Context(), verifier)
	cmd.Env = append(os.Environ(),
		"PATH="+tools+":"+os.Getenv("PATH"),
		"GITHUB_REPOSITORY=junioryono/billet",
		"RELEASE_TAG=v0.2.0",
		"RELEASE_VERIFY_ATTEMPTS=4",
		"RELEASE_VERIFY_INTERVAL_SECONDS=0",
		"RELEASE_VERIFY_DEADLINE_SECONDS=1",
		"RELEASE_VERIFY_ATTEMPT_TIMEOUT_SECONDS=15",
	)
	started := time.Now()
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("blocked API request unexpectedly succeeded\n%s", output)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("blocked API request ran for %s; want no more than 3s\n%s", elapsed, output)
	}
}

func TestReleasePlannerOrdersNewSeriesWithoutBlockingMaintainedHotfixes(t *testing.T) {
	t.Parallel()

	planner, err := filepath.Abs("plan-release.sh")
	if err != nil {
		t.Fatalf("absolute planner path: %v", err)
	}
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.name", "Billet Test")
	runGit(t, repository, "config", "user.email", "billet@example.invalid")
	runGit(t, repository, "commit", "--allow-empty", "--quiet", "-m", "fixture")
	runGit(t, repository, "tag", "v0.4.2")
	runGit(t, repository, "tag", "v0.5.0")

	for _, tc := range []struct {
		name        string
		requested   string
		wantOutput  string
		wantSuccess bool
	}{
		{name: "backward new series", requested: "v0.3.0"},
		{name: "forward new series", requested: "v0.6.0", wantOutput: "tag=v0.6.0\nbranch=release/v0.6\ncollection_version=0.6.0\n", wantSuccess: true},
		{name: "maintained older series hotfix", requested: "v0.4.3", wantOutput: "tag=v0.4.3\nbranch=release/v0.4\ncollection_version=0.4.3\n", wantSuccess: true},
		{name: "backward older series patch", requested: "v0.4.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			output := filepath.Join(t.TempDir(), "github-output")
			cmd := exec.CommandContext(t.Context(), planner)
			cmd.Dir = repository
			cmd.Env = append(os.Environ(), "REQUESTED="+tc.requested, "GITHUB_OUTPUT="+output)
			err := cmd.Run()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("plan error = %v; want success %t", err, tc.wantSuccess)
			}
			if !tc.wantSuccess {
				return
			}
			body, err := os.ReadFile(output)
			if err != nil {
				t.Fatalf("read workflow output: %v", err)
			}
			if string(body) != tc.wantOutput {
				t.Fatalf("workflow output = %q; want %q", body, tc.wantOutput)
			}
		})
	}
}

func TestInstallerSelectsAndDoesNotExecuteACrossTargetBinary(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:     "Darwin",
		hostArch:   "arm64",
		targetOS:   stringPtr("linux"),
		targetArch: stringPtr("amd64"),
		binary:     "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\nprintf 'foreign fixture\\n'\n",
	})
	if !strings.Contains(run.output, "Installed linux_amd64 billet to "+run.installed) {
		t.Fatalf("output = %q; want cross-target installation report", run.output)
	}
	if !strings.Contains(run.output, "Supply "+run.installed+" to your provisioning tool") {
		t.Fatalf("output = %q; want cross-target next step", run.output)
	}
	if strings.Contains(run.output, "billet github-app create") {
		t.Fatalf("output = %q; foreign binary must not be presented as runnable here", run.output)
	}
	if strings.Contains(run.requests, "darwin_arm64.tar.gz") || !strings.Contains(run.requests, "linux_amd64.tar.gz") {
		t.Fatalf("requests = %q; want only the linux_amd64 archive", run.requests)
	}
	if _, err := os.Stat(run.marker); !os.IsNotExist(err) {
		t.Fatalf("foreign binary execution marker error = %v; want not-exist", err)
	}
	assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\nprintf 'foreign fixture\\n'\n")
}

func TestInstallerExecutesANativeBinaryForItsVersion(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:   "Darwin",
		hostArch: "arm64",
		binary:   "#!/bin/sh\nprintf 'billet fixture 0.0.0\\n'\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\n",
	})
	if !strings.Contains(run.output, "Installed: billet fixture 0.0.0") {
		t.Fatalf("output = %q; want native version report", run.output)
	}
	if !strings.Contains(run.requests, "darwin_arm64.tar.gz") {
		t.Fatalf("requests = %q; want darwin_arm64 archive", run.requests)
	}
	assertFileEquals(t, run.marker, "executed\n")
	if run.unameRequests != "-s\n-m\n" {
		t.Fatalf("uname calls = %q; want one OS and one architecture lookup", run.unameRequests)
	}
}

func TestInstallerValidatesAnExplicitNativeTarget(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:     "Darwin",
		hostArch:   "arm64",
		targetOS:   stringPtr("darwin"),
		targetArch: stringPtr("arm64"),
		binary:     "#!/bin/sh\nprintf 'billet explicit native\\n'\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\n",
	})
	if !strings.Contains(run.output, "Installed: billet explicit native") {
		t.Fatalf("output = %q; want explicit native version report", run.output)
	}
	assertFileEquals(t, run.marker, "executed\n")
}

func TestInstallerSelectsLinuxARM64(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:     "Darwin",
		hostArch:   "arm64",
		targetOS:   stringPtr("linux"),
		targetArch: stringPtr("arm64"),
		binary:     "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\n",
	})
	if !strings.Contains(run.requests, "linux_arm64.tar.gz") {
		t.Fatalf("requests = %q; want linux_arm64 archive", run.requests)
	}
	if _, err := os.Stat(run.marker); !os.IsNotExist(err) {
		t.Fatalf("foreign binary execution marker error = %v; want not-exist", err)
	}
	assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\n")
}

func TestInstallerRefusesEmptyOrPartialTargetPlatforms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		targetOS   *string
		targetArch *string
		want       string
	}{
		{name: "only os", targetOS: stringPtr("linux"), want: "must be set together"},
		{name: "only arch", targetArch: stringPtr("amd64"), want: "must be set together"},
		{name: "empty os", targetOS: stringPtr(""), targetArch: stringPtr("amd64"), want: "must not be empty"},
		{name: "empty arch", targetOS: stringPtr("linux"), targetArch: stringPtr(""), want: "must not be empty"},
		{name: "both empty", targetOS: stringPtr(""), targetArch: stringPtr(""), want: "must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:     "Darwin",
				hostArch:   "arm64",
				targetOS:   tc.targetOS,
				targetArch: tc.targetArch,
				binary:     "unused\n",
			})
			if !strings.Contains(run.output, tc.want) {
				t.Fatalf("output = %q; want %q", run.output, tc.want)
			}
		})
	}
}

func TestInstallerStagesForAnUnsupportedControlHost(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:     "Darwin",
		hostArch:   "x86_64",
		targetOS:   stringPtr("linux"),
		targetArch: stringPtr("amd64"),
		binary:     "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\nprintf 'foreign fixture\\n'\n",
	})
	if !strings.Contains(run.output, "Installed linux_amd64 billet to "+run.installed) {
		t.Fatalf("output = %q; want cross-target installation report", run.output)
	}
	if strings.Contains(run.output, "does not build for macOS on Intel") {
		t.Fatalf("output contains a fatal host diagnostic: %q", run.output)
	}
	assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\nprintf 'foreign fixture\\n'\n")
	if _, err := os.Stat(run.marker); !os.IsNotExist(err) {
		t.Fatalf("foreign binary execution marker error = %v; want not-exist", err)
	}
}

func TestInstallerRefusesHostDetectionFailureForAnExplicitTarget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		failFlag  string
		wantError string
		wantCalls string
	}{
		{name: "operating system", failFlag: "-s", wantError: "could not detect the host operating system", wantCalls: "-s\n"},
		{name: "architecture", failFlag: "-m", wantError: "could not detect the host architecture", wantCalls: "-s\n-m\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:        "Darwin",
				hostArch:      "arm64",
				targetOS:      stringPtr("linux"),
				targetArch:    stringPtr("amd64"),
				binary:        "unused\n",
				unameFailFlag: tc.failFlag,
			})
			if !strings.Contains(run.output, tc.wantError) {
				t.Fatalf("output = %q; want %q", run.output, tc.wantError)
			}
			if run.unameRequests != tc.wantCalls {
				t.Fatalf("uname calls = %q; want %q", run.unameRequests, tc.wantCalls)
			}
			if _, err := os.Stat(run.installed); !os.IsNotExist(err) {
				t.Fatalf("installed binary error = %v; want not-exist", err)
			}
		})
	}
}

func TestInstallerCannotBeDisabledByTheFormerTestHook(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:       "Darwin",
		hostArch:     "arm64",
		binary:       "#!/bin/sh\nprintf 'billet fixture 0.0.0\\n'\n",
		legacyBypass: true,
	})
	if !strings.Contains(run.output, "Installed: billet fixture 0.0.0") {
		t.Fatalf("output = %q; want production entrypoint to run", run.output)
	}
	assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'billet fixture 0.0.0\\n'\n")
}

func TestInstallerPreservesAnExistingBinaryWhenNativeValidationFails(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		binary string
		want   string
	}{
		{name: "command fails", binary: "#!/bin/sh\nexit 7\n", want: "could not run"},
		{name: "version is empty", binary: "#!/bin/sh\nexit 0\n", want: "reported no version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:    "Darwin",
				hostArch:  "arm64",
				binary:    tc.binary,
				oldBinary: "working billet\n",
			})
			if !strings.Contains(run.output, tc.want) {
				t.Fatalf("output = %q; want %q", run.output, tc.want)
			}
			assertFileEquals(t, run.installed, "working billet\n")
			matches, err := filepath.Glob(filepath.Join(filepath.Dir(run.installed), ".billet.incoming.*"))
			if err != nil {
				t.Fatalf("glob staging files: %v", err)
			}
			if len(matches) != 0 {
				t.Fatalf("staging residue = %v; want none", matches)
			}
		})
	}
}

func TestInstallerRefusesAChecksumMismatch(t *testing.T) {
	t.Parallel()

	run := runInstallerExpectingFailure(t, installerFixture{
		hostOS:      "Darwin",
		hostArch:    "arm64",
		binary:      "unused\n",
		badChecksum: true,
	})
	if !strings.Contains(run.output, "checksum mismatch") {
		t.Fatalf("output = %q; want checksum mismatch", run.output)
	}
	if _, err := os.Stat(run.installed); !os.IsNotExist(err) {
		t.Fatalf("installed binary error = %v; want not-exist", err)
	}
}

func TestInstallerRefusesUnsafeArchiveNames(t *testing.T) {
	t.Parallel()

	for _, archive := range []string{
		"../escape_darwin_arm64.tar.gz",
		"billét_darwin_arm64.tar.gz",
	} {
		t.Run(archive, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:            "Darwin",
				hostArch:          "arm64",
				binary:            "unused\n",
				darwinArchiveName: archive,
			})
			if !strings.Contains(run.output, "unsafe archive name") {
				t.Fatalf("output = %q; want unsafe archive refusal", run.output)
			}
			if strings.Contains(run.requests, archive) {
				t.Fatalf("requests = %q; unsafe archive must not be downloaded", run.requests)
			}
		})
	}
}

func TestInstallerStreamsOnlyTheExpectedArchiveMember(t *testing.T) {
	t.Parallel()

	t.Run("traversal entry", func(t *testing.T) {
		t.Parallel()

		run := runInstaller(t, installerFixture{
			hostOS:        "Darwin",
			hostArch:      "arm64",
			binary:        "#!/bin/sh\nprintf 'billet safe archive fixture\n'\n",
			archiveThreat: "traversal",
		})
		if _, err := os.Stat(run.escaped); !os.IsNotExist(err) {
			t.Fatalf("archive escape path error = %v; want not-exist", err)
		}
		assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'billet safe archive fixture\n'\n")
	})

	t.Run("symlink payload", func(t *testing.T) {
		t.Parallel()

		run := runInstallerExpectingFailure(t, installerFixture{
			hostOS:        "Darwin",
			hostArch:      "arm64",
			binary:        "unused\n",
			archiveThreat: "symlink",
		})
		assertFileEquals(t, run.escaped, "outside stays unchanged\n")
		if _, err := os.Stat(run.installed); !os.IsNotExist(err) {
			t.Fatalf("installed binary error = %v; want not-exist", err)
		}
	})
}

func TestInstallerCleansUpAfterSignals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		signal   syscall.Signal
		exitCode int
	}{
		{name: "interrupt", signal: syscall.SIGINT, exitCode: 130},
		{name: "terminate", signal: syscall.SIGTERM, exitCode: 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:    "Darwin",
				hostArch:  "arm64",
				binary:    "#!/bin/sh\nprintf 'ready\n' > \"$BILLET_TEST_MARKER\"\nwhile :; do sleep 1; done\n",
				oldBinary: "working billet\n",
				signal:    tc.signal,
			})
			if run.exitCode != tc.exitCode {
				t.Fatalf("exit code = %d; want %d; output = %q", run.exitCode, tc.exitCode, run.output)
			}
			assertFileEquals(t, run.installed, "working billet\n")
			assertNoInstallerResidue(t, run)
		})
	}
}

func TestInstallerUsesPrivilegedStagingAndCleanup(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		run := runInstaller(t, installerFixture{
			hostOS:     "Darwin",
			hostArch:   "arm64",
			binary:     "#!/bin/sh\nprintf 'billet privileged fixture\n'\n",
			oldBinary:  "working billet\n",
			privileged: true,
		})
		assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'billet privileged fixture\n'\n")
		for _, command := range []string{"mktemp", "cp", "chmod", "mv"} {
			if !strings.Contains(run.sudoRequests, command+"\n") {
				t.Fatalf("sudo requests = %q; want %s", run.sudoRequests, command)
			}
		}
	})

	t.Run("validation failure", func(t *testing.T) {
		t.Parallel()

		run := runInstallerExpectingFailure(t, installerFixture{
			hostOS:     "Darwin",
			hostArch:   "arm64",
			binary:     "#!/bin/sh\nexit 7\n",
			oldBinary:  "working billet\n",
			privileged: true,
		})
		assertFileEquals(t, run.installed, "working billet\n")
		if !strings.Contains(run.sudoRequests, "rm\n") {
			t.Fatalf("sudo requests = %q; want cleanup through sudo", run.sudoRequests)
		}
		assertNoInstallerResidue(t, run)
	})
}

func TestInstallerRefusesADirectoryDestination(t *testing.T) {
	t.Parallel()

	run := runInstallerExpectingFailure(t, installerFixture{
		hostOS:               "Darwin",
		hostArch:             "arm64",
		binary:               "#!/bin/sh\nprintf 'billet fixture\\n'\n",
		destinationDirectory: true,
	})
	if !strings.Contains(run.output, "is a directory") {
		t.Fatalf("output = %q; want directory refusal", run.output)
	}
	entries, err := os.ReadDir(run.installed)
	if err != nil {
		t.Fatalf("read destination directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("destination directory contains %v; want empty", entries)
	}
}

func TestInstallerDoesNotReuseTheFormerFixedStagingName(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:             "Darwin",
		hostArch:           "arm64",
		binary:             "#!/bin/sh\nprintf 'billet fixture\\n'\n",
		fixedStageSentinel: "do not overwrite\n",
	})
	assertFileEquals(t, filepath.Join(filepath.Dir(run.installed), ".billet.incoming"), "do not overwrite\n")
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(run.installed), ".billet.incoming.*"))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("staging residue = %v; want none", matches)
	}
}

type installerFixture struct {
	hostOS               string
	hostArch             string
	targetOS             *string
	targetArch           *string
	binary               string
	badChecksum          bool
	oldBinary            string
	legacyBypass         bool
	unameFailFlag        string
	darwinArchiveName    string
	destinationDirectory bool
	fixedStageSentinel   string
	archiveThreat        string
	privileged           bool
	signal               syscall.Signal
}

type installerRun struct {
	output        string
	requests      string
	installed     string
	marker        string
	unameRequests string
	escaped       string
	tempParent    string
	sudoRequests  string
	exitCode      int
}

func runInstaller(t *testing.T, fixture installerFixture) installerRun {
	t.Helper()
	run, err := executeInstaller(t, fixture)
	if err != nil {
		t.Fatalf("run installer: %v\n%s", err, run.output)
	}
	return run
}

func runInstallerExpectingFailure(t *testing.T, fixture installerFixture) installerRun {
	t.Helper()
	run, err := executeInstaller(t, fixture)
	if err == nil {
		t.Fatalf("installer succeeded unexpectedly:\n%s", run.output)
	}
	return run
}

func executeInstaller(t *testing.T, fixture installerFixture) (installerRun, error) {
	t.Helper()

	installer, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("absolute installer path: %v", err)
	}
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	installDir := filepath.Join(root, "install")
	tempParent := filepath.Join(root, "tmp")
	escaped := filepath.Join(root, "escaped")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatalf("create tools: %v", err)
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("create install directory: %v", err)
	}
	if err := os.MkdirAll(tempParent, 0o755); err != nil {
		t.Fatalf("create temporary parent: %v", err)
	}

	selectedPlatform := fixturePlatform(fixture)
	linuxBinary := "#!/bin/sh\nprintf 'wrong linux fixture\\n'\n"
	linuxARMBinary := "#!/bin/sh\nprintf 'wrong linux arm fixture\\n'\n"
	darwinBinary := "#!/bin/sh\nprintf 'wrong darwin fixture\\n'\n"
	switch selectedPlatform {
	case "linux_amd64":
		linuxBinary = fixture.binary
	case "linux_arm64":
		linuxARMBinary = fixture.binary
	default:
		darwinBinary = fixture.binary
	}
	linuxArchive := writeInstallerArchive(t, root, "linux-amd64", linuxBinary)
	linuxARMArchive := writeInstallerArchive(t, root, "linux-arm64", linuxARMBinary)
	darwinArchive := writeInstallerArchive(t, root, "darwin", darwinBinary)
	if fixture.archiveThreat != "" {
		switch selectedPlatform {
		case "linux_amd64":
			linuxArchive = writeHostileInstallerArchive(t, root, "linux-amd64-hostile", fixture.archiveThreat, linuxBinary, escaped)
		case "linux_arm64":
			linuxARMArchive = writeHostileInstallerArchive(t, root, "linux-arm64-hostile", fixture.archiveThreat, linuxARMBinary, escaped)
		default:
			darwinArchive = writeHostileInstallerArchive(t, root, "darwin-hostile", fixture.archiveThreat, darwinBinary, escaped)
		}
	}
	linuxSum := fileSHA256(t, linuxArchive)
	linuxARMSum := fileSHA256(t, linuxARMArchive)
	darwinSum := fileSHA256(t, darwinArchive)
	if fixture.badChecksum {
		linuxSum = sha256.Sum256([]byte("different linux archive\n"))
		linuxARMSum = sha256.Sum256([]byte("different linux arm archive\n"))
		darwinSum = sha256.Sum256([]byte("different darwin archive\n"))
	}
	darwinArchiveName := fixture.darwinArchiveName
	if darwinArchiveName == "" {
		darwinArchiveName = "billet_0.0.0_darwin_arm64.tar.gz"
	}
	checksums := filepath.Join(root, "checksums.txt")
	checksumBody := fmt.Sprintf("%x  billet_0.0.0_linux_amd64.tar.gz\n%x  billet_0.0.0_linux_arm64.tar.gz\n%x  %s\n", linuxSum, linuxARMSum, darwinSum, darwinArchiveName)
	if err := os.WriteFile(checksums, []byte(checksumBody), 0o600); err != nil {
		t.Fatalf("write checksums: %v", err)
	}
	if fixture.oldBinary != "" {
		if err := os.WriteFile(filepath.Join(installDir, "billet"), []byte(fixture.oldBinary), 0o755); err != nil {
			t.Fatalf("write existing binary: %v", err)
		}
	}
	if fixture.destinationDirectory {
		if err := os.Mkdir(filepath.Join(installDir, "billet"), 0o755); err != nil {
			t.Fatalf("create destination directory: %v", err)
		}
	}
	if fixture.fixedStageSentinel != "" {
		if err := os.WriteFile(filepath.Join(installDir, ".billet.incoming"), []byte(fixture.fixedStageSentinel), 0o600); err != nil {
			t.Fatalf("write fixed staging sentinel: %v", err)
		}
	}

	uname := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$1" >> "$BILLET_TEST_UNAME_LOG"
[ "$1" != "$BILLET_TEST_UNAME_FAIL" ] || exit 1
case "$1" in
    -s) printf '%%s\n' %q ;;
    -m) printf '%%s\n' %q ;;
    *) exit 2 ;;
esac
`, fixture.hostOS, fixture.hostArch)
	writeExecutable(t, filepath.Join(tools, "uname"), uname)
	writeExecutable(t, filepath.Join(tools, "curl"), `#!/bin/sh
url=
output=
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) shift; output=$1 ;;
        https://*) url=$1 ;;
    esac
    shift
done
printf '%s\n' "$url" >> "$BILLET_TEST_CURL_LOG"
case "$url" in
    */checksums.txt) cp "$BILLET_TEST_CHECKSUMS" "$output" ;;
    *_linux_amd64.tar.gz) cp "$BILLET_TEST_LINUX_ARCHIVE" "$output" ;;
    *_linux_arm64.tar.gz) cp "$BILLET_TEST_LINUX_ARM_ARCHIVE" "$output" ;;
    *_darwin_arm64.tar.gz) cp "$BILLET_TEST_DARWIN_ARCHIVE" "$output" ;;
    *) exit 2 ;;
esac
`)

	requests := filepath.Join(root, "requests")
	unameRequests := filepath.Join(root, "uname-requests")
	sudoRequests := filepath.Join(root, "sudo-requests")
	marker := filepath.Join(root, "executed")
	if fixture.privileged {
		writeExecutable(t, filepath.Join(tools, "sudo"), `#!/bin/sh
printf '%s\n' "$1" >> "$BILLET_TEST_SUDO_LOG"
chmod u+w "$BILLET_TEST_INSTALL_DIR"
"$@"
status=$?
chmod u-w "$BILLET_TEST_INSTALL_DIR"
exit "$status"
`)
		if err := os.Chmod(installDir, 0o555); err != nil {
			t.Fatalf("make install directory non-writable: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(installDir, 0o755); err != nil && !os.IsNotExist(err) {
				t.Errorf("restore install directory permissions: %v", err)
			}
		})
		writable := exec.CommandContext(t.Context(), "sh", "-c", `[ -w "$1" ]`, "sh", installDir)
		if err := writable.Run(); err == nil {
			t.Skip("this environment still grants write access to a mode-0555 directory")
		}
	}
	cmd := exec.CommandContext(t.Context(), "sh", installer)
	cmd.Env = append(environmentWithout(
		"BILLET_ARCH",
		"BILLET_INSTALL_DIR",
		"BILLET_INSTALL_SH_TEST",
		"BILLET_OS",
		"BILLET_VERSION",
		"TMPDIR",
	),
		"PATH="+tools+":"+os.Getenv("PATH"),
		"BILLET_INSTALL_DIR="+installDir,
		"BILLET_TEST_CHECKSUMS="+checksums,
		"BILLET_TEST_CURL_LOG="+requests,
		"BILLET_TEST_DARWIN_ARCHIVE="+darwinArchive,
		"BILLET_TEST_LINUX_ARM_ARCHIVE="+linuxARMArchive,
		"BILLET_TEST_LINUX_ARCHIVE="+linuxArchive,
		"BILLET_TEST_INSTALL_DIR="+installDir,
		"BILLET_TEST_MARKER="+marker,
		"BILLET_TEST_SUDO_LOG="+sudoRequests,
		"BILLET_TEST_UNAME_FAIL="+fixture.unameFailFlag,
		"BILLET_TEST_UNAME_LOG="+unameRequests,
		"TMPDIR="+tempParent,
	)
	if fixture.legacyBypass {
		cmd.Env = append(cmd.Env, "BILLET_INSTALL_SH_TEST=1")
	}
	if fixture.targetOS != nil {
		cmd.Env = append(cmd.Env, "BILLET_OS="+*fixture.targetOS)
	}
	if fixture.targetArch != nil {
		cmd.Env = append(cmd.Env, "BILLET_ARCH="+*fixture.targetArch)
	}
	var output []byte
	var runErr error
	if fixture.signal != 0 {
		var combined bytes.Buffer
		cmd.Stdout = &combined
		cmd.Stderr = &combined
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start installer for signal: %v", err)
		}
		waitForFile(t, marker)
		if err := syscall.Kill(-cmd.Process.Pid, fixture.signal); err != nil {
			t.Fatalf("signal installer process group: %v", err)
		}
		runErr = cmd.Wait()
		output = combined.Bytes()
	} else {
		output, runErr = cmd.CombinedOutput()
	}
	requestsBody, err := os.ReadFile(requests)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read requests: %v", err)
	}
	unameRequestsBody, err := os.ReadFile(unameRequests)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read uname requests: %v", err)
	}
	sudoRequestsBody, err := os.ReadFile(sudoRequests)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read sudo requests: %v", err)
	}
	exitCode := 0
	var exitError *exec.ExitError
	if errors.As(runErr, &exitError) {
		exitCode = exitError.ExitCode()
	}
	return installerRun{
		output:        string(output),
		requests:      string(requestsBody),
		installed:     filepath.Join(installDir, "billet"),
		marker:        marker,
		unameRequests: string(unameRequestsBody),
		escaped:       escaped,
		tempParent:    tempParent,
		sudoRequests:  string(sudoRequestsBody),
		exitCode:      exitCode,
	}, runErr
}

func fixturePlatform(fixture installerFixture) string {
	if fixture.targetOS != nil && fixture.targetArch != nil && *fixture.targetOS != "" && *fixture.targetArch != "" {
		return *fixture.targetOS + "_" + *fixture.targetArch
	}
	if fixture.hostOS == "Linux" && (fixture.hostArch == "x86_64" || fixture.hostArch == "amd64") {
		return "linux_amd64"
	}
	if fixture.hostOS == "Linux" && (fixture.hostArch == "aarch64" || fixture.hostArch == "arm64") {
		return "linux_arm64"
	}
	return "darwin_arm64"
}

func writeInstallerArchive(t *testing.T, root, name, binary string) string {
	t.Helper()
	directory := filepath.Join(root, name+"-archive")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("create %s archive directory: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "billet"), []byte(binary), 0o755); err != nil {
		t.Fatalf("write %s archive binary: %v", name, err)
	}
	archive := filepath.Join(root, name+".tar.gz")
	cmd := exec.CommandContext(t.Context(), "tar", "-czf", archive, "-C", directory, "billet")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create %s archive: %v\n%s", name, err, output)
	}
	return archive
}

func writeHostileInstallerArchive(t *testing.T, root, name, threat, binary, escaped string) string {
	t.Helper()

	archive := filepath.Join(root, name+".tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatalf("create hostile archive: %v", err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	writeRegular := func(name, body string, mode int64) {
		t.Helper()
		header := &tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write hostile archive header: %v", err)
		}
		if _, err := tarWriter.Write([]byte(body)); err != nil {
			t.Fatalf("write hostile archive body: %v", err)
		}
	}

	switch threat {
	case "traversal":
		writeRegular("../escaped", "archive escaped\n", 0o644)
		writeRegular("billet", binary, 0o755)
	case "symlink":
		if err := os.WriteFile(escaped, []byte("outside stays unchanged\n"), 0o600); err != nil {
			t.Fatalf("write symlink target: %v", err)
		}
		header := &tar.Header{Name: "billet", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: escaped}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write hostile symlink header: %v", err)
		}
	default:
		t.Fatalf("unknown archive threat %q", threat)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close hostile tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close hostile gzip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close hostile archive: %v", err)
	}
	return archive
}

func waitForFile(t *testing.T, path string) {
	t.Helper()

	// A READINESS BOUND, NOT AN INSTALLER PERFORMANCE ASSERTION. Under the full
	// race-and-coverage suite this helper competes with every package and the
	// staged fixture has twice taken just over five seconds to be scheduled, while
	// its isolated run completes in under a second. Thirty seconds still catches a
	// process that never reaches the marker without making machine load the verdict.
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !os.IsNotExist(err) {
			t.Fatalf("wait for %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertNoInstallerResidue(t *testing.T, run installerRun) {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(filepath.Dir(run.installed), ".billet.incoming.*"))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("staging residue = %v; want none", matches)
	}
	entries, err := os.ReadDir(run.tempParent)
	if err != nil {
		t.Fatalf("read temporary parent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary residue = %v; want none", entries)
	}
}

func fileSHA256(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256.Sum256(body)
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertFileEquals(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := string(body); got != want {
		t.Fatalf("%s = %q; want %q", path, got, want)
	}
}

func stringPtr(value string) *string {
	return &value
}

func environmentWithout(names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[name] = struct{}{}
	}
	var environment []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, found := blocked[name]; !found {
			environment = append(environment, entry)
		}
	}
	return environment
}

func TestCacheConformanceFixturesKeepEveryEmbeddedClientInOneLane(t *testing.T) {
	t.Parallel()

	script, err := filepath.Abs(filepath.Join("..", ".github", "actions",
		"cache-conformance-fixtures", "fixtures.sh"))
	if err != nil {
		t.Fatalf("fixture script path: %v", err)
	}
	root := t.TempDir()
	run := exec.CommandContext(t.Context(), "bash", script, "prepare", "pinned", "123-2")
	run.Dir = root
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("prepare fixtures: %v\n%s", err, output)
	}
	for _, path := range []string{
		"node/package-lock.json", "python/requirements.txt", "java/pom.xml",
		"dotnet/packages.lock.json", "go.sum",
	} {
		body, err := os.ReadFile(filepath.Join(root, "conformance", "embedded", path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(body), "billet-cache-conformance-pinned-123-2") {
			t.Errorf("%s is not scoped to the pinned lane and run", path)
		}
	}

	invalid := exec.CommandContext(t.Context(), "bash", script, "prepare", "../../escape", "123-2")
	invalid.Dir = root
	if err := invalid.Run(); err == nil {
		t.Fatal("a fixture lane that can escape its cache namespace was accepted")
	}
	invalid = exec.CommandContext(t.Context(), "bash", script, "prepare", "pinned", "../../escape")
	invalid.Dir = root
	if err := invalid.Run(); err == nil {
		t.Fatal("a fixture run salt that can escape its cache namespace was accepted")
	}
}

func TestCacheConformanceReusableWorkflowUsesItsOwnRevision(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "cache-conformance.yml"))
	if err != nil {
		t.Fatalf("read cache conformance workflow: %v", err)
	}
	workflow := string(body)
	const scaleSetRunner = "runs-on: ${{ inputs.runner_label }}"
	var jobRunners, scaleSetRunners int
	for line := range strings.SplitSeq(workflow, "\n") {
		if !strings.HasPrefix(line, "    runs-on: ") {
			continue
		}
		jobRunners++
		if line == "    "+scaleSetRunner {
			scaleSetRunners++
		}
	}
	if jobRunners == 0 || scaleSetRunners != jobRunners {
		t.Fatalf("%d of %d conformance jobs target the single runner scale-set label",
			scaleSetRunners, jobRunners)
	}
	if strings.Contains(workflow, "runs-on: [self-hosted") {
		t.Fatal("runner scale sets expose one label, so a self-hosted conjunction never matches")
	}
	if strings.Count(workflow,
		"uses: ./.billet-conformance/.github/actions/cache-conformance-fixtures") != 8 {
		t.Fatal("embedded cache lanes do not all use the called Billet revision's fixture action")
	}
	if strings.Contains(workflow, "scripts/cache-conformance-fixtures.sh") {
		t.Fatal("reusable workflow still depends on a fixture script from the caller checkout")
	}
	for _, required := range []string{
		"steps.poison-actions.outputs.cache-hit }}' != true",
		"steps.poison-tls.outputs.cache-hit }}' != true",
		"steps.poison-proxy.outputs.cache-hit }}' != true",
		"steps.poison-process.outputs.cache-hit }}' != true",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("poison conformance is missing non-vacuity proof %q", required)
		}
	}
	// The NODE_OPTIONS poison was removed: the runner strips step-env NODE_OPTIONS
	// from JS actions, so the poison never reached the client and the "must fail"
	// assertion could not hold. The guard is keyed on the poison itself, not a step
	// name -- a NODE_OPTIONS env key under ANY id reintroduces the permanently red
	// lane. Parse the workflow and walk every mapping key so every spelling the YAML
	// loader accepts (bare, single- or double-quoted, flow mapping) is caught while a
	// NODE_OPTIONS token inside a `run:` block scalar or a comment -- a value, not a
	// key -- is not a false positive.
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		t.Fatalf("parse cache conformance workflow as YAML: %v", err)
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i]
				if key.Kind == yaml.AliasNode && key.Alias != nil {
					key = key.Alias // an aliased key resolves to its anchored scalar
				}
				if key.Value == "NODE_OPTIONS" {
					t.Fatal("the NODE_OPTIONS poison is untestable (the runner strips it); " +
						"it must not return as an env key under any step")
				}
			}
		}
		for _, child := range n.Content {
			walk(child)
		}
	}
	walk(&root)
	if strings.Contains(workflow, "steps.poison-node") ||
		strings.Contains(workflow, "id: poison-node") {
		t.Fatal("the removed poison-node step must not return")
	}
	if strings.Count(workflow, "lookup-only: true") != 4 {
		t.Fatal("every poisoned cache key must be checked for a miss from the next VM")
	}
	for _, jobs := range [][2]string{
		{"save-embedded-current", "restore-embedded-current"},
		{"restore-embedded-current", "save-embedded-pinned"},
		{"save-embedded-pinned", "restore-embedded-pinned"},
		{"restore-embedded-pinned", "poisoned-clients"},
	} {
		start := strings.Index(workflow, "\n  "+jobs[0]+":\n")
		if start < 0 {
			t.Fatalf("workflow is missing %s", jobs[0])
		}
		rest := workflow[start+1:]
		end := strings.Index(rest, "\n  "+jobs[1]+":\n")
		if end > 0 {
			rest = rest[:end]
		}
		for _, required := range []string{
			"uses: actions/checkout@v6",
			"repository: ${{ inputs.billet_repository }}",
			"ref: ${{ inputs.billet_ref }}",
			"path: .billet-conformance",
			"sparse-checkout: .github/actions/cache-conformance-fixtures",
			"persist-credentials: false",
			"salt: ${{ github.run_id }}-${{ github.run_attempt }}",
		} {
			if !strings.Contains(rest, required) {
				t.Fatalf("%s does not fetch or use the exact called Billet revision: missing %q",
					jobs[0], required)
			}
		}
		if strings.Contains(rest, "repository: ${{ github.repository }}") {
			t.Fatalf("%s is caller-dependent or does not force a fresh run-scoped cache", jobs[0])
		}
	}
}

func assertContains(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(body), want) {
		t.Fatalf("%s is not wired to tested helper %q", path, want)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = directory
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
