package images

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
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

// recordSilentControlPlane gives a test's node the record a registration with a
// control plane that does not say what it accepts leaves, under the deployment
// the control plane's directory beside it holds, so the node completes with its
// own images.accept alone, as every node did before the control plane said.
func recordSilentControlPlane(t *testing.T, serverDir, nodeDir string) {
	t.Helper()

	if _, err := state.AdoptDeploymentID(serverDir, acceptanceDeployment); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := imageaccept.Write(nodeDir, acceptanceDeployment, false, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// recordFor writes the control plane's acceptance into the node state directory
// a pull fixture's config names, under the deployment its server directory is
// given.
func recordFor(t *testing.T, cfgPath string, components ...string) {
	t.Helper()

	dir := filepath.Dir(cfgPath)
	if _, err := state.AdoptDeploymentID(filepath.Join(dir, "server"), acceptanceDeployment); err != nil {
		t.Fatal(err)
	}

	nodeDir := filepath.Join(dir, "node")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := imageaccept.Write(nodeDir, acceptanceDeployment, true, components, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// WHAT THE CONTROL PLANE ACCEPTS IS ADDED TO WHAT THE NODE'S OWN CONFIG DOES,
// for the deployment the node is in, and a control plane that does not say
// leaves the file's list alone.
func TestTheDeploymentsAcceptanceIsAddedToTheNodesOwn(t *testing.T) {
	for name, tc := range map[string]struct {
		local      string
		deployment string
		carried    bool
		recorded   []string
		want       []config.ImageComponent
	}{
		"both, merged": {"images:\n  accept: [codeql]\n", acceptanceDeployment, true,
			[]string{"firefox", "android-sdk"},
			[]config.ImageComponent{config.ComponentAndroidSDK, config.ComponentCodeQL, config.ComponentFirefox}},
		"the deployment's alone": {"", acceptanceDeployment, true, []string{"android-sdk"},
			[]config.ImageComponent{config.ComponentAndroidSDK}},
		"a control plane that does not say": {"images:\n  accept: [codeql]\n", acceptanceDeployment, false, nil,
			[]config.ImageComponent{config.ComponentCodeQL}},
	} {
		t.Run(name, func(t *testing.T) {
			path, nodeDir := acceptanceConfig(t, tc.local)
			if err := imageaccept.Write(nodeDir, tc.deployment, tc.carried, tc.recorded, time.Now()); err != nil {
				t.Fatal(err)
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

// WHAT CANNOT BE TOLD REFUSES, never the node's own list alone: no record of
// this deployment's, whether or not the node's file describes a control plane
// beside it, a record that does not parse, a name this build does not install,
// and a record with no deployment identity to compare it with.
func TestWhatCannotBeToldRefusesRatherThanInstallLess(t *testing.T) {
	remote := func(t *testing.T, cfg *config.Config) {
		t.Helper()

		// A NODE WHOSE FILE DOES NOT DESCRIBE ITS CONTROL PLANE: its deployment is
		// the one its own state directory holds.
		cfg.Server = nil
		if _, err := state.AdoptDeploymentID(cfg.Node.StateDir, acceptanceDeployment); err != nil {
			t.Fatal(err)
		}
	}

	for name, tc := range map[string]struct {
		stage func(t *testing.T, cfg *config.Config, nodeDir string)
		says  string
	}{
		"no record, beside a control plane in the same file": {func(t *testing.T, _ *config.Config, _ string) {
			t.Helper()
		}, "has not recorded what its control plane accepts"},
		"another deployment's record, beside a control plane in the same file": {func(t *testing.T,
			_ *config.Config, nodeDir string,
		) {
			t.Helper()

			if err := imageaccept.Write(nodeDir, "fedcba9876543210fedcba9876543210", true,
				[]string{"codeql"}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}, "has not recorded what its control plane accepts"},
		"no record, dialing elsewhere": {func(t *testing.T, cfg *config.Config, _ string) {
			t.Helper()

			remote(t, cfg)
		}, "has not recorded what its control plane accepts"},
		"another deployment's record, dialing elsewhere": {func(t *testing.T, cfg *config.Config, nodeDir string) {
			t.Helper()

			remote(t, cfg)
			if err := imageaccept.Write(nodeDir, "fedcba9876543210fedcba9876543210", true,
				[]string{"codeql"}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}, "has not recorded what its control plane accepts"},
		"no record, a certificate beside a server section": {func(t *testing.T, cfg *config.Config, _ string) {
			t.Helper()

			// A NODE WITH A CERTIFICATE MAY DIAL ANOTHER MACHINE'S CONTROL PLANE, so
			// the server section in its file is not proved to be its own.
			cfg.Node.TLS = &config.NodeTLS{CertPath: "node.crt", KeyPath: "node.key", CAPath: "ca.crt"}
		}, "has not recorded what its control plane accepts"},
		"a record that does not parse": {func(t *testing.T, _ *config.Config, nodeDir string) {
			t.Helper()

			if err := os.WriteFile(imageaccept.Path(nodeDir), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "is not a record this node wrote"},
		"a component this build does not install": {func(t *testing.T, _ *config.Config, nodeDir string) {
			t.Helper()

			if err := imageaccept.Write(nodeDir, acceptanceDeployment, true,
				[]string{"android-sdk", "visual-studio"}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}, `"visual-studio", which this billet does not install; upgrade billet on this node`},
		"no deployment identity": {func(t *testing.T, cfg *config.Config, nodeDir string) {
			t.Helper()

			cfg.Server.IdentityDir = t.TempDir()
			if err := imageaccept.Write(nodeDir, acceptanceDeployment, true, []string{"codeql"},
				time.Now()); err != nil {
				t.Fatal(err)
			}
		}, "has no deployment identity to compare it with"},
	} {
		t.Run(name, func(t *testing.T) {
			path, nodeDir := acceptanceConfig(t, "images:\n  accept: [codeql]\n")

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}

			tc.stage(t, cfg, nodeDir)

			if got, err := acceptedComponents(cfg); err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("answered %v, %v; want a refusal saying %q", got, err, tc.says)
			}
		})
	}
}

// THE REFRESH COMPLETES WITH WHAT THE CONTROL PLANE ACCEPTS: a newest generation
// that lacks a component only the deployment's record accepts is pulled again.
func TestARefreshPullsForAComponentOnlyTheControlPlaneAccepts(t *testing.T) {
	record := ceph.GenerationComponents{LeftOut: []string{"android-sdk"}}
	run := stageRefresh(t, fakeRefreshStore{newest: newer, found: true, components: &record}, channelBuilt)

	path, nodeDir := acceptanceConfig(t, "")
	if err := imageaccept.Write(nodeDir, acceptanceDeployment, true, []string{"android-sdk"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := cmdImagesRefresh(t.Context(), processEnv(), []string{"--config", path}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if len(run.pulled) != 1 || run.pulled[0] != "ubuntu-2404-x64" {
		t.Fatalf("pulled %v, want the image completed with what the control plane accepts", run.pulled)
	}
}

// A REFRESH THAT CANNOT TELL WHAT IS ACCEPTED PULLS NOTHING and says why, rather
// than complete with the node's own list.
func TestARefreshThatCannotTellWhatIsAcceptedPullsNothing(t *testing.T) {
	run := stageRefresh(t, fakeRefreshStore{newest: older, found: true}, channelBuilt)

	path, nodeDir := acceptanceConfig(t, "")
	if err := os.WriteFile(imageaccept.Path(nodeDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := cmdImagesRefresh(t.Context(), processEnv(), []string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "billet images refresh: ") ||
		!strings.Contains(err.Error(), "is not a record this node wrote") {
		t.Fatalf("refresh answered %v, want the unreadable record named", err)
	}

	if len(run.pulled) != 0 {
		t.Fatalf("pulled %v with acceptance unknown", run.pulled)
	}
}

// A PULL COMPLETES WITH WHAT ONLY THE CONTROL PLANE ACCEPTS: the node's own list
// is empty, and the record's google-chrome is installed before the import.
func TestAPullCompletesWithWhatOnlyTheControlPlaneAccepts(t *testing.T) {
	store := &fakeCompletionStore{verified: ceph.Generation{Name: "g20260901000000"}, found: true}
	run := newCompletionPull(t, "", completionReportFor("installed=google-chrome 155.0.8059.84"), store)
	recordFor(t, run.fixture.cfgPath, "google-chrome")

	if err := run.pull(t); err != nil {
		t.Fatalf("images pull: %v", err)
	}

	if len(run.backend.launched) != 1 ||
		!strings.Contains(strings.Join(run.backend.launched[0].Command, " "), "/opt/billet/complete/run google-chrome") {
		t.Fatalf("the pull did not complete google-chrome: %+v", run.backend.launched)
	}

	if got := run.fixture.publisher.last().components; got == nil || got.Installed["google-chrome"] != "155.0.8059.84" {
		t.Fatalf("the generation records %+v", got)
	}
}

// A PULL WHOSE ACCEPTANCE CHANGED BEFORE ITS IMPORT IMPORTS NOTHING: the control
// plane withdraws google-chrome while the image is being completed with it, or
// after, while the pull installs the kernel under the directory's lock, and the
// completed image is not published either way.
func TestAPullWhoseAcceptanceChangedBeforeItsImportImportsNothing(t *testing.T) {
	for name, during := range map[string]func(withdraw func()){
		// newCompletionPull's cleanup restores the seam.
		"during the completion": func(withdraw func()) {
			attach := attachLoop
			attachLoop = func(ctx context.Context, path string) (string, func(context.Context) error, error) {
				withdraw()

				return attach(ctx, path)
			}
		},
		// usePullSeams' cleanup restores the seam; the kernel is installed once,
		// under the lock, after the completion.
		"under the kernel directory's lock": func(withdraw func()) {
			kernelInstaller = durablefile.Installer{Rename: func(from, to string) error {
				withdraw()

				return os.Rename(from, to)
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeCompletionStore{verified: ceph.Generation{Name: "g20260901000000"}, found: true}
			run := newCompletionPull(t, "", completionReportFor("installed=google-chrome 155.0.8059.84"), store)
			recordFor(t, run.fixture.cfgPath, "google-chrome")

			nodeDir := filepath.Join(filepath.Dir(run.fixture.cfgPath), "node")
			during(func() {
				if err := imageaccept.Write(nodeDir, acceptanceDeployment, true, nil, time.Now()); err != nil {
					t.Error(err)
				}
			})

			err := run.pull(t)
			if err == nil || !strings.Contains(err.Error(), "changed while ubuntu-2404-x64 was completed, from "+
				"[google-chrome] to [], so nothing was imported") {
				t.Fatalf("images pull answered %v, want it to import nothing", err)
			}

			if len(run.backend.launched) != 1 {
				t.Fatalf("the completion ran %d times, want once before the acceptance was asked again",
					len(run.backend.launched))
			}

			if got := run.fixture.publisher.calls(); got != 0 {
				t.Fatalf("ImportGeneration was reached %d times, want never", got)
			}
		})
	}
}

// A NODE'S FIRST PULL, BEFORE IT HAS REGISTERED, INSTALLS THE KERNEL AND IMPORTS
// NOTHING: the node cannot start without the kernel, and a generation without
// what the deployment accepts could be booted by a tier pinning it.
func TestAFirstPullBeforeRegistrationInstallsTheKernelAndImportsNothing(t *testing.T) {
	store := &fakeCompletionStore{verified: ceph.Generation{Name: "g20260901000000"}, found: true}
	run := newCompletionPull(t, "google-chrome", completionReportFor("installed=google-chrome 155.0.8059.84"), store)

	if err := imageaccept.Remove(filepath.Join(filepath.Dir(run.fixture.cfgPath), "node")); err != nil {
		t.Fatal(err)
	}

	kernel := filepath.Join(run.fixture.kernelDir, kernelFileName(run.fixture.manifest))

	err := run.pull(t)
	if err == nil || !strings.Contains(err.Error(), "installed the kernel "+kernel+", and imported no "+
		"generation: this node has not recorded what its control plane accepts") ||
		!strings.Contains(err.Error(), "point node.firecracker.kernel_image at that kernel") {
		t.Fatalf("a first pull before registration answered %v, want the kernel installed and nothing imported", err)
	}

	if info, err := os.Stat(kernel); err != nil || info.Size() == 0 {
		t.Fatalf("the kernel %s is not installed: %v", kernel, err)
	}

	if len(run.backend.launched) != 0 {
		t.Fatalf("a pull that cannot tell what is accepted completed %+v", run.backend.launched)
	}

	if got := run.fixture.publisher.calls(); got != 0 {
		t.Fatalf("ImportGeneration was reached %d times, want never", got)
	}
}

// VERIFY AND PROMOTE REFUSE A GENERATION LACKING WHAT ONLY THE CONTROL PLANE
// ACCEPTS.
func TestVerifyAndPromoteRefuseWhatOnlyTheControlPlaneAccepts(t *testing.T) {
	open := openComponentsReader
	t.Cleanup(func() { openComponentsReader = open })
	openComponentsReader = func(*config.Config) (componentsReader, error) {
		return fakeComponentsReader{record: ceph.GenerationComponents{LeftOut: []string{"google-chrome"}}, found: true}, nil
	}

	for name, run := range map[string]func(cfgPath string) error{
		"billet images verify": func(cfgPath string) error {
			return cmdImagesVerify(t.Context(), processEnv(), []string{"--config", cfgPath,
				"ubuntu-2404-x64@g20260901000000"})
		},
		"billet images promote": func(cfgPath string) error {
			return cmdImagesPromote(t.Context(), processEnv(), []string{"--config", cfgPath,
				"ubuntu-2404-x64@g20260901000000"}, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPullFixture(t)
			recordFor(t, fixture.cfgPath, "google-chrome")

			err := run(fixture.cfgPath)
			if err == nil || !strings.Contains(err.Error(), name+": ubuntu-2404-x64@g20260901000000 lacks google-chrome") {
				t.Fatalf("got %v, want it refused for lacking what the control plane accepts", err)
			}
		})
	}
}

// `billet check` DESCRIBES A GENERATION AGAINST THE DEPLOYMENT'S ACCEPTANCE TOO,
// and says when it cannot tell what that is.
func TestCheckCountsWhatTheControlPlaneAccepts(t *testing.T) {
	store := fakeComponentsChecker{verified: completedGeneration, records: map[string]ceph.GenerationComponents{
		completedGeneration: {LeftOut: []string{"android-sdk"}, Installed: map[string]string{}},
	}}

	restore := openComponentsChecker
	t.Cleanup(func() { openComponentsChecker = restore })
	openComponentsChecker = func(*config.Config) (componentsChecker, error) { return store, nil }

	check := func(t *testing.T, body string) string {
		t.Helper()

		path, nodeDir := acceptanceConfig(t, "")
		if err := os.WriteFile(imageaccept.Path(nodeDir), []byte(body), 0o600); err != nil {
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

		return out.String()
	}

	if out := check(t, `{"schema":1,"deployment":"`+acceptanceDeployment+
		`","carried":true,"components":["android-sdk"],"received_at":""}`); !strings.Contains(out,
		"lacks android-sdk, which this deployment accepts") {
		t.Fatalf("the check left out what the control plane accepts:\n%s", out)
	}

	if out := check(t, "{"); !strings.Contains(out, "images   could not tell what this deployment accepts: ") {
		t.Fatalf("the check did not say it could not tell:\n%s", out)
	}
}
