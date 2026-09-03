package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/initconfig"
)

// TestNoGeneratedProfilePutsTheAppKeyWhereARestoreRefusesIt proves the broad
// refusal cannot fire on a deployment billet itself generated.
//
// A restore refuses an App key path anywhere inside the state directory,
// because that directory is billet's: it creates, renames and DELETES files
// there by name — including staging names `billet ca rotate` clears — and
// GitHub issues the App key exactly once. The refusal is deliberately broad,
// and a broad refusal has a cost this test exists to bound: if billet's OWN
// generated configuration ever put the key there, the refusal would fire on a
// correct deployment at the exact moment it must not, which is disaster
// recovery.
//
// Today every profile puts the key BESIDE the state directory rather than in
// it. That is a property of two pieces of code that know nothing about each
// other, so nothing but this test would notice either of them moving.
//
// IT DRIVES THE REAL REFUSAL rather than re-deriving the rule: a test that
// compared the paths itself would agree with a broken check.
func TestNoGeneratedProfilePutsTheAppKeyWhereARestoreRefusesIt(t *testing.T) {
	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	a, err := deployarchive.Open(t.Context(), archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// THE PLATFORM IS A DIMENSION OF THE SERVICE SHAPE ONLY, and pretending
	// otherwise was vacuous: the user-session paths come from THIS machine's user
	// config directory, so `local/linux` and `local/darwin` produced identical
	// paths on any one runner — two cases proving one thing, and neither of them
	// the platform they were labelled with.
	cases := []struct {
		profile initconfig.Profile
		goos    string
	}{
		{initconfig.ProfileLocal, ""},
		{initconfig.ProfileLocalService, "linux"},
		{initconfig.ProfileLocalService, "darwin"},
	}

	for _, tc := range cases {
		profile, goos := tc.profile, tc.goos

		name := string(profile)
		if goos != "" {
			name += "/" + goos
		}

		t.Run(name, func(t *testing.T) {
			stateDir, keyPath := generatedPaths(t, profile, goos)

			plan, err := deployarchive.PlanRestore(t.Context(), a, deployarchive.Target{
				ConfigPath: "generated",
				StateDir:   stateDir,
				AppKeyPath: keyPath,
				// The App identity is the archive's, so the only refusal this
				// case can produce is the one under test.
				GitHub: a.Manifest.GitHub,
			})
			if err != nil {
				t.Fatalf("PlanRestore: %v", err)
			}

			for _, r := range plan.Refusals {
				if strings.Contains(r.What, "inside the state directory") {
					t.Errorf(
						"a generated %s profile on %s puts the App key at %s, inside the state "+
							"directory %s — so a restore would refuse a deployment billet itself "+
							"produced:\n  %s",
						profile, goos, keyPath, stateDir, r.What)
				}
			}

			// AND THE APP KEY WAS ACTUALLY PLANNED. Asserting only that one
			// refusal string is absent would pass if App-key planning were
			// removed altogether, or if an earlier refusal short-circuited
			// before it ran — neither of which says anything about the
			// property this test is named for.
			var planned bool

			for _, act := range plan.Actions {
				if act.Entry == deployarchive.EntryAppKey && act.Path == keyPath {
					planned = true
				}
			}

			if !planned {
				t.Errorf("the plan produced no App-key action for %s; this test cannot see "+
					"the collision it is checking for", keyPath)
			}
		})
	}
}

// generatedPaths renders one profile and reads back the two paths under test.
// An empty goos leaves the platform alone, which is the only meaningful case for
// the user-session shape.
//
// A NARROW YAML READ RATHER THAN config.Load, for the reason init.go already
// records: a generated config carries app_id 0 until `github-app create` fills
// it in, so it does not validate yet. initconfig proves validity itself against
// a separate render with non-zero ids; what this test needs is the two paths as
// an operator receives them.
func generatedPaths(t *testing.T, profile initconfig.Profile, goos string) (string, string) {
	t.Helper()

	if goos != "" {
		prev := hostOS
		hostOS = goos

		t.Cleanup(func() { hostOS = prev })
	}

	body, _, err := initconfig.Generate(initconfig.Params{
		Profile:     profile,
		Org:         "acme",
		Provider:    config.ProviderDocker,
		VCPU:        8,
		Memory:      32 << 30,
		RunnerGroup: "billet",
		Workflows:   []string{"acme/ci/.github/workflows/ci.yml@refs/heads/main"},
		GOOS:        goos,
	})
	if err != nil {
		t.Fatalf("Generate(%s, %s): %v", profile, goos, err)
	}

	var doc struct {
		Server struct {
			StateDir string `yaml:"state_dir"`
		} `yaml:"server"`
		GitHub struct {
			PrivateKeyPath string `yaml:"private_key_path"`
		} `yaml:"github"`
	}

	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse the generated %s profile: %v", profile, err)
	}

	if doc.Server.StateDir == "" || doc.GitHub.PrivateKeyPath == "" {
		t.Fatalf("the generated %s profile names state_dir %q and private_key_path %q; this "+
			"test cannot check a collision it cannot see",
			profile, doc.Server.StateDir, doc.GitHub.PrivateKeyPath)
	}

	return doc.Server.StateDir, doc.GitHub.PrivateKeyPath
}
