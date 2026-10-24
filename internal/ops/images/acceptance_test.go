package images

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imageaccept"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/store/ceph"
)

const acceptanceDeployment = "0123456789abcdef0123456789abcdef"

// acceptanceConfig is firecrackerNodeConfig with extra before its node block and
// its state directories in a fresh place: the control plane's beside it holds
// acceptanceDeployment, and the node's is returned for a record.
func acceptanceConfig(t *testing.T, extra string) (string, string) {
	t.Helper()

	serverDir, nodeDir := t.TempDir(), t.TempDir()
	if _, err := state.AdoptDeploymentID(serverDir, acceptanceDeployment); err != nil {
		t.Fatal(err)
	}

	body := strings.Replace(firecrackerNodeConfig, "node:", extra+"node:", 1)
	body = strings.Replace(body, "state_dir: /var/lib/billet/server", "state_dir: "+serverDir, 1)
	body = strings.Replace(body, "state_dir: /var/lib/billet/node", "state_dir: "+nodeDir, 1)

	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path, nodeDir
}

// WHAT THE CONTROL PLANE ACCEPTS IS ADDED TO WHAT THE NODE'S OWN CONFIG DOES,
// only for the deployment the node is in; nothing recorded is the node's own
// list alone.
func TestTheDeploymentsAcceptanceIsAddedToTheNodesOwn(t *testing.T) {
	for name, tc := range map[string]struct {
		local      string
		deployment string
		recorded   []string
		want       []config.ImageComponent
	}{
		"both, merged": {"images:\n  accept: [codeql]\n", acceptanceDeployment,
			[]string{"firefox", "android-sdk"},
			[]config.ImageComponent{config.ComponentAndroidSDK, config.ComponentCodeQL, config.ComponentFirefox}},
		"the deployment's alone": {"", acceptanceDeployment, []string{"android-sdk"},
			[]config.ImageComponent{config.ComponentAndroidSDK}},
		"another deployment's": {"images:\n  accept: [codeql]\n", "fedcba9876543210fedcba9876543210",
			[]string{"android-sdk"}, []config.ImageComponent{config.ComponentCodeQL}},
		"nothing recorded": {"images:\n  accept: [codeql]\n", "", nil,
			[]config.ImageComponent{config.ComponentCodeQL}},
	} {
		t.Run(name, func(t *testing.T) {
			path, nodeDir := acceptanceConfig(t, tc.local)
			if tc.deployment != "" {
				if err := imageaccept.Write(nodeDir, tc.deployment, tc.recorded, time.Now()); err != nil {
					t.Fatal(err)
				}
			}

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}

			got, err := acceptedComponents(cfg)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("accepted %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}

// A RECORD THAT CANNOT BE USED IS NEVER THE NODE'S OWN LIST ALONE: one that does
// not parse, and one naming a component this build does not install, each
// refuse, the second saying to upgrade.
func TestAnUnusableRecordRefusesRatherThanInstallLess(t *testing.T) {
	path, nodeDir := acceptanceConfig(t, "images:\n  accept: [codeql]\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := imageaccept.Write(nodeDir, acceptanceDeployment, []string{"android-sdk", "visual-studio"},
		time.Now()); err != nil {
		t.Fatal(err)
	}

	if got, err := acceptedComponents(cfg); err == nil || !strings.Contains(err.Error(), `"visual-studio"`) ||
		!strings.Contains(err.Error(), "upgrade billet on this node") {
		t.Fatalf("a component this build does not install answered %v, %v", got, err)
	}

	if err := os.WriteFile(imageaccept.Path(nodeDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := acceptedComponents(cfg); err == nil {
		t.Fatalf("a record that does not parse answered %v", got)
	}
}

// THE REFRESH COMPLETES WITH WHAT THE CONTROL PLANE ACCEPTS: a newest generation
// that lacks a component only the deployment's record accepts is pulled again.
func TestARefreshPullsForAComponentOnlyTheControlPlaneAccepts(t *testing.T) {
	record := ceph.GenerationComponents{LeftOut: []string{"android-sdk"}}
	run := stageRefresh(t, fakeRefreshStore{newest: newer, found: true, components: &record}, channelBuilt)

	path, nodeDir := acceptanceConfig(t, "")
	if err := imageaccept.Write(nodeDir, acceptanceDeployment, []string{"android-sdk"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := cmdImagesRefresh(t.Context(), processEnv(), []string{"--config", path}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if len(run.pulled) != 1 || run.pulled[0] != "ubuntu-2404-x64" {
		t.Fatalf("pulled %v, want the image completed with what the control plane accepts", run.pulled)
	}
}

// `billet check` DESCRIBES A GENERATION AGAINST THE DEPLOYMENT'S ACCEPTANCE TOO.
func TestCheckCountsWhatTheControlPlaneAccepts(t *testing.T) {
	store := fakeComponentsChecker{verified: completedGeneration, records: map[string]ceph.GenerationComponents{
		completedGeneration: {LeftOut: []string{"android-sdk"}, Installed: map[string]string{}},
	}}

	restore := openComponentsChecker
	t.Cleanup(func() { openComponentsChecker = restore })
	openComponentsChecker = func(*config.Config) (componentsChecker, error) { return store, nil }

	path, nodeDir := acceptanceConfig(t, "")
	if err := imageaccept.Write(nodeDir, acceptanceDeployment, []string{"android-sdk"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	env := processEnv()
	env.Stdout = &out
	ReportImageComponents(t.Context(), env, cfg)

	if !strings.Contains(out.String(), "lacks android-sdk, which this deployment accepts") {
		t.Fatalf("the check left out what the control plane accepts:\n%s", out.String())
	}
}
