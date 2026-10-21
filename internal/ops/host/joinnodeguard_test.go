package host

import (
	"go/ast"
	"slices"
	"strings"
	"testing"
)

// THE DIRECTORIES COME BEFORE EVERY STEP THAT USES THEM (#181): the lifecycle
// lock is taken first, then the planned directories are created, and only then
// is the identity read, the host prepared (which writes the identity directory
// and the locks inside them) and the node's drain released. The order trace
// cannot see those three, which record nothing in the fake, so this reads
// runLocalUp's own source.
func TestUpCreatesTheDirectoriesBeforeAnythingUsesThem(t *testing.T) {
	fn := findFunc(t, "runLocalUp")

	at := map[string]int{}
	ast.Inspect(fn, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if name := calleeName(call); at[name] == 0 {
				at[name] = int(call.Pos())
			}
		}

		return true
	})

	order := []string{"lifecycleLock", "ApplyDirectories", "Identity", "prepareHostForUp", "releaseNodeDrain", "ApplyOwnership"}
	for i, name := range order {
		if at[name] == 0 {
			t.Fatalf("runLocalUp no longer calls %s", name)
		}
		if i > 0 && at[order[i-1]] > at[name] {
			t.Errorf("runLocalUp calls %s before %s", name, order[i-1])
		}
	}
}

// THE ENROLLMENT ADDRESS IS USED WHERE ONE IS SERVED: with --bootstrap-addr the
// authority is fetched there and the enrollment asks there, because the node
// wire refuses a machine with no certificate.
func TestJoinFetchesAndEnrollsAtTheBootstrapAddress(t *testing.T) {
	onPlatform(t, "darwin", 501)

	f := &joinFakes{generated: joinedNodeConfig}
	stageJoin(t, f)

	if err := Join(t.Context(), processEnv(), append(slices.Clone(joinArgs), "--bootstrap-addr", "cp.example:7718")); err != nil {
		t.Fatalf("join: %v", err)
	}

	if !strings.HasPrefix(f.trace[0], "fetch https://cp.example:7718 ") {
		t.Errorf("the authority was fetched from %q, want the bootstrap address", f.trace[0])
	}
	if !slices.ContainsFunc(f.trace, func(s string) bool { return strings.HasPrefix(s, "enroll mac-1 https://cp.example:7718 ") }) {
		t.Errorf("the enrollment did not ask at the bootstrap address: %v", f.trace)
	}
}

// INIT'S OWN FLAGS REACH THE GENERATION THE JOIN RUNS, after its own.
func TestJoinHandsInitsFlagsToTheGeneration(t *testing.T) {
	onPlatform(t, "darwin", 501)

	f := &joinFakes{generated: joinedNodeConfig}
	stageJoin(t, f)

	if err := Join(t.Context(), processEnv(), append(slices.Clone(joinArgs), "--", "--macos-image", "ghcr.io/x/y:1")); err != nil {
		t.Fatalf("join: %v", err)
	}
	if n := len(f.initArgs); n < 2 || !slices.Equal(f.initArgs[n-2:], []string{"--macos-image", "ghcr.io/x/y:1"}) {
		t.Errorf("init was given %v, want the flags after -- at the end", f.initArgs)
	}
}

// A CONFIG THAT IS NOT THIS JOIN'S NODE STOPS THE JOIN, on each thing it must
// say: no control plane, this join's server, and this join's name.
func TestJoinStopsOnEveryWayTheConfigIsNotItsNode(t *testing.T) {
	const notThisNode = "is not the node config this join generated"

	// A LOOPBACK CONTROL PLANE is the one node config that loads with no
	// bundle, so the last case joins one.
	loopback := slices.Clone(joinArgs)
	loopback[1] = "127.0.0.1:7717"
	// A CONTROL PLANE THAT LOADS beside the node needs a network listen for the
	// node's bundle to mean anything, so that case joins one.
	network := slices.Clone(joinArgs)
	network[1] = "10.0.0.1:7717"

	for name, c := range map[string]struct {
		generated, want string
		args            []string
	}{
		"a control plane": {"server:\n  listen: 10.0.0.1:7717\n  state_dir: /tmp/billet-join-test\n" +
			"  max_vcpu: 8\n  max_memory: 32GiB\ngithub:\n  org: acme\n  app_id: 1\n  installation_id: 2\n" +
			"  private_key_path: /tmp/billet-join-test/app.pem\n" +
			strings.Replace(joinedNodeConfig, "cp.example:7717", "10.0.0.1:7717", 1), notThisNode, network},
		"another server": {strings.Replace(joinedNodeConfig, "cp.example:7717", "other.example:7717", 1), notThisNode, nil},
		"another node":   {strings.Replace(joinedNodeConfig, "name: mac-1", "name: mac-2", 1), notThisNode, nil},
		"no bundle at all": {strings.Replace(strings.Split(joinedNodeConfig, "  tls:\n")[0], "cp.example:7717",
			"127.0.0.1:7717", 1), "names no node.tls", loopback},
	} {
		t.Run(name, func(t *testing.T) {
			onPlatform(t, "darwin", 501)

			f := &joinFakes{generated: c.generated}
			stageJoin(t, f)

			args := joinArgs
			if c.args != nil {
				args = c.args
			}

			err := Join(t.Context(), processEnv(), args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("a config with %s answered %v, want a refusal saying %q", name, err, c.want)
			}
			if slices.ContainsFunc(f.trace, func(s string) bool { return strings.HasPrefix(s, "enroll") || s == "up" }) {
				t.Errorf("a config with %s went on to %v", name, f.trace)
			}
		})
	}
}
