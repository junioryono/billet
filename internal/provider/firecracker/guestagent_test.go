package firecracker

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// THE GUEST AGENT'S DECODE IS RUN HERE, not described here.
//
// billet's half of this contract is `metadata`, and it is easy to test because it is
// Go. The guest's half is a bash loop inside a shell script inside an image, and it
// was WRONG in a way no Go test could have seen: `jq -r '.[]'` is newline-delimited
// and `read` is newline-delimited, so an argument containing a newline arrived as two
// arguments. The metadata was perfect and the argv was not.
//
// So the block between the markers in the build script is extracted VERBATIM and run,
// with billet's own encoder producing its input. What is asserted is the only property
// that matters: the argv the guest reconstructs is the argv billet was given. If
// either side changes the encoding without the other, this fails.
func TestTheGuestAgentReconstructsAnArgvExactly(t *testing.T) {
	t.Parallel()

	decode := agentDecodeBlock(t)

	for _, tc := range []struct {
		name    string
		command []string
	}{
		{"the ordinary case", []string{"./run.sh"}},
		{"a shell command", []string{"/bin/sh", "-c", "echo hello"}},
		{"spaces", []string{"/bin/sh", "-c", "echo one two   three"}},
		// THE ONE THAT WAS BROKEN. A workflow command spanning lines is ordinary.
		{"a newline inside an argument", []string{"/bin/sh", "-c", "echo one\necho two", "tail"}},
		{"an argument ending in a newline", []string{"/bin/sh", "-c", "trailing\n"}},
		{"an empty argument", []string{"/bin/sh", "-c", "", "after"}},
		// THE TRAILING ONES ARE THE DANGEROUS ONES. An empty argument encodes to an
		// empty line and `$()` strips every trailing newline, so a command ending in
		// one lost it — `sh -c '…' arg ''` arriving as `sh -c '…' arg` is a different
		// command with a different $#, and nothing anywhere said so.
		{"a trailing empty argument", []string{"/bin/sh", "-c", "echo", ""}},
		{"several trailing empty arguments", []string{"/bin/sh", "-c", "echo", "", "", ""}},
		{"nothing but empty arguments after the program", []string{"/bin/echo", "", ""}},
		{"quotes and dollars", []string{"/bin/sh", "-c", `echo "$HOME" 'single' $(id)`}},
		{"backslashes", []string{"/bin/sh", "-c", `printf 'a\\b\tc'`}},
		{"a tab", []string{"/bin/sh", "-c", "echo\tone"}},
		{"unicode", []string{"/bin/sh", "-c", "echo héllo → 世界"}},
		{"a leading dash", []string{"/bin/sh", "-c", "echo", "--not-a-flag"}},
		{"something that looks like an option to read", []string{"/bin/sh", "-c", "-r -d x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// BILLET'S OWN ENCODER, so this is the wire format rather than a
			// hand-written approximation of it that could agree with a bug.
			spec := aSpec()
			spec.Command = tc.command

			md, err := metadata(spec)
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}

			raw, ok := md["latest"].(map[string]any)["meta-data"].(map[string]any)["billet"].(map[string]any)["command"].(string)
			if !ok {
				t.Fatalf("the metadata no longer carries the command as a string: %v", md)
			}

			got, err := runAgentDecode(t, decode, raw)
			if err != nil {
				t.Fatalf("the agent's decode refused %q: %v", raw, err)
			}

			if !slices.Equal(got, tc.command) {
				t.Errorf("the guest would run %q and billet was given %q", got, tc.command)
			}
		})
	}
}

// AND IT REFUSES ANYTHING THAT IS NOT AN ARGV, rather than running part of one.
//
// The decode reads its input through a process substitution, which neither `set -e`
// nor `pipefail` reaches across — so a jq that emitted two arguments and then failed
// would leave a TRUNCATED argv behind and the agent would run it. A truncated command
// is not a smaller version of the command that was asked for; it is a different one.
func TestTheGuestAgentRefusesMetadataThatIsNotAnArgv(t *testing.T) {
	t.Parallel()

	decode := agentDecodeBlock(t)

	for _, tc := range []struct{ name, raw string }{
		{"not json at all", "this is not json"},
		{"truncated json", `["/bin/sh","-c`},
		{"an object", `{"cmd":"/bin/sh"}`},
		{"a bare string", `"/bin/sh"`},
		{"an empty array", `[]`},
		{"an array holding a number", `["/bin/sh",7]`},
		{"an array holding null", `["/bin/sh",null]`},
		{"an array holding an array", `["/bin/sh",["-c"]]`},
		// TWO DOCUMENTS, WHICH `jq -e` USED TO ACCEPT. It reads a STREAM and reports
		// only the last result, so both arrays validated, the records of both were
		// encoded, and the guest built one argv out of two commands nobody sent.
		{"two json documents", `["/bin/printf","%s"] ["extra"]`},
		{"three json documents", `["/a"] ["/b"] ["/c"]`},
		// A NUL SURVIVES JSON AND DOES NOT SURVIVE BASH. A command substitution
		// cannot hold one, so the argument would arrive shorter than it was sent.
		{"an argument carrying a nul", `["/bin/printf","safe\u0000danger"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := runAgentDecode(t, decode, tc.raw)
			if err == nil {
				t.Errorf("the agent accepted %s and would run %q", tc.raw, got)
			}
		})
	}
}

func TestTheGuestMountsDockerStateBeforeStartingTheDaemon(t *testing.T) {
	t.Parallel()

	buildPath := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")
	source, err := os.ReadFile(buildPath)
	if err != nil {
		t.Fatalf("read guest image builder: %v", err)
	}
	build := string(source)
	assetPath := filepath.Join("..", "..", "guestassets", "docker-cache.sh")
	asset, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatalf("read shared Docker cache helper: %v", err)
	}
	helper := string(asset)

	mountAt := strings.Index(helper, "mount -t ext4 -o noatime \"$device\" /var/lib/docker")
	activateAt := strings.Index(helper, `activate_store "$slot" "$device"`)
	activateBody := strings.Index(helper, "activate_store()")
	startAt := -1
	if activateBody >= 0 {
		startAt = strings.Index(helper[activateBody:], "systemctl start docker.service")
	}
	if mountAt < 0 || activateAt < 0 || activateBody < 0 || startAt < 0 || mountAt >= activateAt {
		t.Fatal("Docker cache mount/start order is not encoded in the shared guest helper")
	}
	if strings.Contains(build, "After=network-online.target docker.service") ||
		strings.Contains(build, "Requires=docker.service") {
		t.Fatal("systemd can start Docker before the guest agent mounts its image store")
	}
	if !strings.Contains(build, "systemctl disable docker.service docker.socket") {
		t.Fatal("the image still permits Docker to autostart before its cache is mounted")
	}
	if !strings.Contains(build, `"containerd-snapshotter": false`) ||
		!strings.Contains(build, `"storage-driver": "overlay2"`) {
		t.Fatal("the image can put pulled images outside the cache-backed Docker data root")
	}
	if !strings.Contains(build, "ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED=true") ||
		!strings.Contains(helper, `[ "$status" = 100 ]`) ||
		!strings.Contains(helper, `operation=ready`) {
		t.Fatal("Docker readiness is not gated on the runner's one-job success code")
	}
	prepareAt := strings.Index(build, "/usr/local/bin/billet-docker-cache prepare")
	runnerAt := strings.Index(build, "BILLET_AGENT_LAUNCH_BEGIN")
	if prepareAt < 0 || runnerAt < 0 || prepareAt >= runnerAt {
		t.Fatal("the guest does not attach the Docker image store before starting the runner")
	}
	if !strings.Contains(build, "BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES") ||
		!strings.Contains(build, "fetch buildkit-cache-mount-limit-bytes") {
		t.Fatal("the tier's BuildKit cache-mount ceiling never reaches workflow actions")
	}
	configureAt := strings.Index(build, "registry-mirrors")
	if configureAt < 0 || configureAt >= prepareAt {
		t.Fatal("the guest does not configure its Docker Hub mirror before Docker starts")
	}
	if !strings.Contains(build, "fetch registry-mirrors") ||
		!strings.Contains(build, "BILLET_REGISTRY_MIRRORS_JSON") {
		t.Fatal("the three registry mirrors do not reach the guest's BuildKit actions")
	}
}

// THE PRIVILEGE DROP IS NOT A LOGIN. systemd starts the agent as root and setpriv
// changes ids without constructing the runner account's environment, so these
// values must cross the same env argv that carries the registration and tool cache.
func TestTheGuestAgentEstablishesTheRunnerAccountEnvironment(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read guest image builder: %v", err)
	}
	text := string(source)

	// ANCHORED ON THE ARRAY'S OWN CLOSE, NOT ON WHAT FOLLOWS IT. The end anchor
	// used to be the next statement in the agent, which made this test fail the
	// moment anything was inserted between the two — reporting the base
	// environment as "no longer uniquely extractable" when it was merely no longer
	// adjacent to the thing the test happened to name.
	const begin = "runner_env=(\n"
	const end = "\n)\n"

	if strings.Count(text, begin) != 1 {
		t.Fatal("the guest agent's base runner environment is no longer uniquely extractable")
	}

	_, after, _ := strings.Cut(text, begin)

	body, _, found := strings.Cut(after, end)
	if !found {
		t.Fatal("the guest agent's runner environment array is never closed")
	}

	block := begin + body + end

	const launchBegin = "BILLET_AGENT_LAUNCH_BEGIN"
	const launchEnd = "BILLET_AGENT_LAUNCH_END"
	if strings.Count(text, launchBegin) != 1 || strings.Count(text, launchEnd) != 1 {
		t.Fatal("the guest agent's real launch stanza is no longer uniquely extractable")
	}
	_, launchAfter, _ := strings.Cut(text, launchBegin)
	launch, _, _ := strings.Cut(launchAfter, launchEnd)
	_, launch, _ = strings.Cut(launch, "\n")

	shadow := t.TempDir()
	setprivLog := filepath.Join(shadow, "setpriv.log")
	stub := `#!/bin/sh
: > "$BILLET_TEST_SETPRIV_LOG"
while [ "$#" -gt 0 ]; do
	printf '%s\n' "$1" >> "$BILLET_TEST_SETPRIV_LOG"
	if [ "$1" = -- ]; then
		shift
		exec "$@"
	fi
	shift
done
exit 97
`
	if err := os.WriteFile(filepath.Join(shadow, "setpriv"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write the validating setpriv stub: %v", err)
	}

	script := `
set -euo pipefail
ACTIONS_RUNNER_INPUT_JITCONFIG=fixture
` + block + `
	cmd=(bash -c 'printf "%s\0" "${HOME-}" "${USER-}" "${LOGNAME-}" "${RUNNER_TOOL_CACHE-}" "${AGENT_TOOLSDIRECTORY-}" "${ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED-}" "${ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE-}" "$(id -u)" "$(id -g)"')
` + launch + `
exit "$job_status"
`
	run := exec.CommandContext(t.Context(), "bash", "-c", script)
	// POISON EVERY VALUE THE HOSTED RUNNER ALREADY SETS CORRECTLY. If the real
	// launch stanza stops passing runner_env, inheriting CI's own runner account
	// would otherwise make the mutation look exactly like production success.
	run.Env = []string{
		"BILLET_TEST_SETPRIV_LOG=" + setprivLog,
		"PATH=" + shadow + ":" + os.Getenv("PATH"),
		"HOME=/poisoned-home",
		"USER=poisoned-user",
		"LOGNAME=poisoned-logname",
		"RUNNER_TOOL_CACHE=/poisoned-tool-cache",
		"AGENT_TOOLSDIRECTORY=/poisoned-agent-tools",
		"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1",
	}
	out, err := run.Output()
	if err != nil {
		t.Fatalf("run the extracted privilege-drop launch: %v", err)
	}
	fields := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	want := []string{
		"/home/runner", "runner", "runner", "/opt/hostedtoolcache", "/opt/hostedtoolcache",
		"true", "1",
		strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()),
	}
	if len(fields) != len(want) {
		t.Fatalf("the runner environment has %d fields, want %d: %q", len(fields), len(want), out)
	}
	for i, value := range fields {
		if string(value) != want[i] {
			t.Errorf("runner environment field %d = %q, want %q", i, value, want[i])
		}
	}

	setprivArgs, err := os.ReadFile(setprivLog)
	if err != nil {
		t.Fatalf("read the privilege-drop argv: %v", err)
	}
	wantSetpriv := "--reuid=runner\n--regid=runner\n--init-groups\n--inh-caps=-all\n--\n"
	if string(setprivArgs) != wantSetpriv {
		t.Errorf("setpriv arguments = %q, want %q", setprivArgs, wantSetpriv)
	}
}

func TestTheGuestAgentInstallsActionsInterceptionForEveryRunnerSurface(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read guest image builder: %v", err)
	}
	text := string(source)

	for _, want := range []string{
		"fetch actions-proxy", "fetch actions-ca-pem", "cat /etc/ssl/certs/ca-certificates.crt",
		"actions_ca_dir=/home/runner/runner/_work/_billet",
		`actions_ca_path="$actions_ca_dir/actions-cache-ca.pem"`,
		// The runner reaches the intercepted origin by a DNS remap, not a proxy
		// variable: only the node CA and the job-started hook are published to it.
		`"NODE_EXTRA_CA_CERTS=$actions_ca_path"`, `"SSL_CERT_FILE=$actions_ca_path"`,
		`"ACTIONS_RUNNER_HOOK_JOB_STARTED=$actions_hook_path"`,
		`target="$RUNNER_TEMP/billet-actions-cache-ca.pem"`,
		`install -m 0444 "$BILLET_ACTIONS_CA_SOURCE" "$target"`,
		`printf 'NODE_EXTRA_CA_CERTS=%s\n' "$target"`,
		`printf 'SSL_CERT_FILE=%s\n' "$target"`,
		`} >>"$GITHUB_ENV"`,
		`--property=Restart=always --property=RestartSec=100ms`,
		// systemd owns the listening socket (a transient .socket unit on the pinned
		// gateway), so PID 1 binds :443 -- no CAP for the runner-uid process -- and
		// the socket survives a service crash. Type=notify makes "active" mean serving.
		`--property=Type=notify --property=NotifyAccess=main`,
		`--socket-property=ListenStream="$docker_gateway:443"`,
		`--socket-property=Accept=no --socket-property=FlushPending=no`,
		`--systemd-socket --upstream "$actions_proxy"`,
		// Readiness is the SERVICE, not the always-listening socket: with Type=notify,
		// `systemctl start` blocks until the process sent READY, so a bare socket
		// probe cannot mark interception active over a not-yet-serving backend.
		`systemctl start billet-actions-proxy.service`,
		`docker_gateway=172.17.0.1`,
		// The passthrough is failed open to the real origin, resolved before the
		// remap, with the gateway excluded so the fallback can never be a loop.
		`--fallback-addr "$results_fallback"`,
		`results_fallback=$(getent ahostsv4 results-receiver.actions.githubusercontent.com`,
		`awk -v gateway="$docker_gateway" '$1 != gateway {print $1}'`,
		// Interception activates only when a fallback resolved, or a later node
		// outage would fail the artifact and log traffic sharing the origin.
		`[ -n "$results_fallback" ] &&`,
		// Daemon-side clients (dockerd, embedded BuildKit) trust the node leaf only
		// if the CA is in the system store, installed before Docker starts.
		`/usr/local/share/ca-certificates/billet-actions-cache.crt`,
		`update-ca-certificates`,
		// The runner is remapped through /etc/hosts, and only the one results origin.
		`printf '%s results-receiver.actions.githubusercontent.com\n' "$docker_gateway" >>/etc/hosts`,
		// Containers do not inherit /etc/hosts, so a guest dnsmasq answers for them
		// and dockerd is pointed at it before it starts.
		`--listen-address="$docker_gateway" --bind-interfaces`,
		`--resolv-file="$upstream_resolv"`,
		`--address="/results-receiver.actions.githubusercontent.com/$docker_gateway"`,
		`'if type == "object" then . + {"dns": $dns} else error("not an object") end'`,
		// The dns list is produced by the behavior-tested filter, which keeps only a
		// real global upstream so no value dockerd rejects reaches daemon.json.
		`dns_json=$(/usr/local/bin/billet-dns-upstreams "$docker_gateway" "$upstream_resolv"`,
		`"$rootfs/usr/local/bin/billet-dns-upstreams"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the guest agent does not establish %q", want)
		}
	}

	// A published proxy variable is exactly the catch-all funnel this design
	// removed: every request the runner and its containers make would route
	// through one guest relay, and bulk transfers stalled through it. And under
	// socket activation the passthrough must NOT ask for the privileged-bind
	// capability nor bind a production port itself -- PID 1 owns the socket.
	for _, forbidden := range []string{
		"HTTPS_PROXY=", "https_proxy=", "actions_guest_proxy", `docker_bridge:7719`,
		"AmbientCapabilities=CAP_NET_BIND_SERVICE", `--listen "$docker_gateway:443"`,
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the passthrough launch still uses the removed mechanism %q", forbidden)
		}
	}

	// Both units are stopped together on failure and after the runner exits, or a
	// late connection would re-activate the service through the orphaned socket.
	if strings.Count(text, "systemctl stop billet-actions-proxy.socket billet-actions-proxy.service") < 2 {
		t.Error("the passthrough is not stopped as both socket and service at every stop site")
	}

	// The container DNS must be set before Docker starts: dockerd reads daemon.json
	// only at start and does not reload "dns" on SIGHUP.
	dnsMergeAt := strings.Index(text, `. + {"dns": $dns}`)
	prepareAt := strings.Index(text, "billet-docker-cache prepare")
	if dnsMergeAt < 0 || prepareAt < 0 || dnsMergeAt >= prepareAt {
		t.Fatal("the guest does not point container DNS at the resolver before Docker starts")
	}

	// container_dns_active is read unconditionally after Docker starts, so it must be
	// initialized BEFORE the interception conditional -- an untrusted job gets no
	// interception metadata and would otherwise read it unset under `set -u` and die.
	dnsInitAt := strings.Index(text, `container_dns_active=""`)
	interceptCondAt := strings.Index(text, "if actions_proxy_candidate=$(fetch actions-proxy")
	if dnsInitAt < 0 || interceptCondAt < 0 || dnsInitAt >= interceptCondAt {
		t.Fatal("container_dns_active is not initialized before the interception conditional")
	}

	// The node CA reaches the system trust store before Docker starts, or dockerd
	// and its embedded BuildKit cannot validate the node leaf for gha traffic.
	caInstallAt := strings.Index(text, "update-ca-certificates")
	if caInstallAt < 0 || caInstallAt >= prepareAt {
		t.Fatal("the node CA is not added to the system trust store before Docker starts")
	}

	// daemon.json is written ONLY when the filter produced a usable list, so it is
	// never a resolver-of-one that takes container DNS down with it when the resolver
	// drops, and no value dockerd rejects can reach the daemon and stop it starting.
	guardAt := strings.Index(text, `[ -n "$dns_json" ]; then`)
	if guardAt < 0 || guardAt >= dnsMergeAt {
		t.Fatal("the container dns list is written without first requiring a usable filtered list")
	}

	// The container resolver runs only when that dns list was configured.
	if !strings.Contains(text, `if [ -n "$container_dns_active" ]; then`) {
		t.Fatal("the container resolver is not gated on container DNS being configured")
	}

	proxyAt := strings.Index(text, "fetch actions-proxy")
	launchAt := strings.Index(text, "BILLET_AGENT_LAUNCH_BEGIN")
	if proxyAt < 0 || launchAt < 0 || proxyAt >= launchAt {
		t.Fatal("the guest does not install Actions interception before starting the runner")
	}
	if strings.Index(text, `cat /etc/ssl/certs/ca-certificates.crt`) >
		strings.Index(text, `printf '\n%s\n' "$actions_ca_candidate"`) {
		t.Fatal("the guest trust bundle does not keep system roots before the Billet authority")
	}
	// The passthrough binds the docker gateway, never the microVM workload interface.
	if strings.Contains(text, "--listen 0.0.0.0:") {
		t.Fatal("the cache-session proxy is exposed on the microVM workload interface")
	}
}

func TestTheManualGuestImagePublisherRunsTheContentsGate(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read guest image builder: %v", err)
	}
	text := string(source)

	checkAt := strings.Index(text, `"$SCRIPT_DIR/check-guest-image.sh" "$img"`)
	publishAt := strings.Index(text, `publish "$img"`)
	if checkAt < 0 || publishAt < 0 || checkAt >= publishAt {
		t.Fatal("the documented manual publisher can write a guest image before its contents gate")
	}
}

func TestTheGuestImageIncludesDockerCLIPluginsWorkflowsUse(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read guest image builder: %v", err)
	}

	if !strings.Contains(string(source), "docker.io docker-buildx docker-compose-v2") {
		t.Fatal("the guest installs Docker without the Buildx and Compose CLI plugins workflows use")
	}

	checkPath := filepath.Join("..", "..", "..", "scripts", "check-guest-image.sh")
	check, err := os.ReadFile(checkPath)
	if err != nil {
		t.Fatalf("read guest image checker: %v", err)
	}
	for _, plugin := range []string{"docker-buildx", "docker-compose"} {
		if !strings.Contains(string(check), "cli-plugins/"+plugin) {
			t.Errorf("the finished-image gate does not inspect the %s plugin", plugin)
		}
	}
}

func TestTheGuestWorkDirectoryBelongsToTheRunner(t *testing.T) {
	t.Parallel()

	buildPath := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")
	buildSource, err := os.ReadFile(buildPath)
	if err != nil {
		t.Fatalf("read guest image builder: %v", err)
	}
	build := string(buildSource)

	const create = `chroot "$rootfs" install -d -m 0755 -o runner -g runner /home/runner/runner/_work`
	if !strings.Contains(build, create) {
		t.Fatal("the image does not pre-create the runner work directory as the runner account")
	}

	// ANCHORED ON THE UNMOUNT, NOT ON A COPY. The build used to assemble a tree on
	// the host and pack it in at the end with `mkfs.ext4 -d`, so "before the image
	// is finalized" meant "before that command". It now creates the filesystem
	// first and writes THROUGH the mount, so the moment the image stops accepting
	// writes is the unmount — and anything after it lands on the host instead of
	// in the guest, which is the same failure under a different mechanism.
	const finalize = "unmount_rootfs\n"

	createAt := strings.Index(build, create)

	finalizeAt := strings.LastIndex(build, finalize)
	if finalizeAt < 0 {
		t.Fatal("the build never unmounts the root filesystem, so nothing finalizes the image")
	}

	if createAt >= finalizeAt {
		t.Fatal("the runner work directory is created after the image is unmounted, so it " +
			"lands on the build host rather than in the guest")
	}

	checkPath := filepath.Join("..", "..", "..", "scripts", "check-guest-image.sh")
	checkSource, err := os.ReadFile(checkPath)
	if err != nil {
		t.Fatalf("read guest image checker: %v", err)
	}
	check := string(checkSource)
	for _, want := range []string{
		`RUNNER_WORK="$RUNNER_DIR/_work"`,
		`work_ids=$(stat -c '%u:%g' "$RUNNER_WORK")`,
		`if [ -n "$runner_ids" ] && [ "$work_ids" = "$runner_ids" ]; then`,
		`fail "the runner work directory belongs to ${work_ids:-no account}, not`,
	} {
		if !strings.Contains(check, want) {
			t.Errorf("the finished-image gate does not enforce %q", want)
		}
	}
}

func TestTheGuestImageGateKeepsDockerBehindTheCacheMount(t *testing.T) {
	t.Parallel()

	dangling := filepath.Join(t.TempDir(), "docker.service")
	if err := os.Symlink("/billet-test-no-such-systemd-unit/docker.service", dangling); err != nil {
		t.Fatalf("create absolute guest enablement symlink: %v", err)
	}
	if _, err := os.Stat(dangling); !os.IsNotExist(err) {
		t.Fatalf("following the guest symlink error = %v; want not-exist", err)
	}
	if info, err := os.Lstat(dangling); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("inspect the guest symlink: info = %v, error = %v", info, err)
	}

	path := filepath.Join("..", "..", "..", "scripts", "check-guest-image.sh")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read guest image checker: %v", err)
	}
	text := string(source)

	if !strings.Contains(text, `[ -L "$WANTS/docker.service" ]`) ||
		!strings.Contains(text, `[ -L "$DOCKER_SOCKET" ]`) ||
		!strings.Contains(text, "Docker waits for the billet agent to mount its image store") {
		t.Fatal("the image gate does not detect guest Docker enablement symlinks without following them on the host")
	}
	if strings.Contains(text, "for unit in docker.service billet-agent.service") {
		t.Fatal("the image gate still requires Docker to start before the billet agent mounts its cache")
	}
	if !strings.Contains(text, `DOCKER_CACHE="$MNT/usr/local/bin/billet-docker-cache"`) ||
		!strings.Contains(text, `'"ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED=true"'`) {
		t.Fatal("the image gate does not verify the Docker cache helper and authoritative result mode")
	}

	bootPath := filepath.Join("..", "..", "..", "scripts", "boot-guest-image.sh")
	bootSource, err := os.ReadFile(bootPath)
	if err != nil {
		t.Fatalf("read guest image boot gate: %v", err)
	}
	bootText := string(bootSource)

	if !strings.Contains(bootText, "docker_started=0") ||
		!strings.Contains(bootText, `grep -q "Started.*docker.service" "$CONSOLE" 2>/dev/null && docker_started=1`) ||
		!strings.Contains(bootText, `[ "$docker_started" -ne 0 ]`) {
		t.Fatal("the boot gate does not reject Docker starting before the billet agent accepts its metadata")
	}
	if strings.Contains(bootText, "saw_docker") ||
		strings.Contains(bootText, `report "$saw_docker" "docker started"`) {
		t.Fatal("the boot gate still requires Docker to start before the billet agent mounts its cache")
	}
}

func TestRegistryMirrorsRemainAStringLeafInGuestMetadata(t *testing.T) {
	t.Parallel()

	spec := aSpec()
	spec.RegistryMirrors = config.RegistryMirrors{
		DockerIO: "https://docker-cache.home.example",
		GHCRIO:   "https://ghcr-cache.home.example",
		QuayIO:   "https://quay-cache.home.example",
	}
	md, err := metadata(spec)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	raw, ok := md["latest"].(map[string]any)["meta-data"].(map[string]any)["billet"].(map[string]any)["registry-mirrors"].(string)
	if !ok {
		t.Fatalf("registry mirrors are not an MMDS string leaf: %v", md)
	}
	var got config.RegistryMirrors
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode registry mirrors: %v", err)
	}
	if got != spec.RegistryMirrors {
		t.Errorf("registry mirrors = %+v, want %+v", got, spec.RegistryMirrors)
	}
}

// agentDecodeBlock lifts the decode out of the build script between its markers.
//
// EXTRACTED RATHER THAN COPIED, because a copy is a second implementation: it would
// keep passing while the script it stands for drifted away from it, which is exactly
// the failure this test exists to catch.
func agentDecodeBlock(t *testing.T) string {
	t.Helper()

	const (
		begin = "BILLET_AGENT_DECODE_BEGIN"
		end   = "BILLET_AGENT_DECODE_END"
	)

	path := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the guest image build script is not where this test expects it: %v", err)
	}

	// EXACTLY ONE PAIR. A second pair — left behind by a copied block, or by an
	// agent that grew a second decode — would leave this test silently exercising
	// whichever came first while the guest ran the other.
	if n := strings.Count(string(source), begin); n != 1 {
		t.Fatalf("%s appears %d times in %s and this test can only stand for one decode",
			begin, n, path)
	}

	if n := strings.Count(string(source), end); n != 1 {
		t.Fatalf("%s appears %d times in %s", end, n, path)
	}

	_, after, found := strings.Cut(string(source), begin)
	if !found {
		t.Fatalf("%s no longer marks the start of the agent's decode in %s, so this test "+
			"cannot tell what the guest actually does", begin, path)
	}

	block, _, found := strings.Cut(after, end)
	if !found {
		t.Fatalf("%s no longer marks the end of the agent's decode in %s", end, path)
	}

	// Past the remainder of the marker's own comment line.
	if _, rest, ok := strings.Cut(block, "\n"); ok {
		block = rest
	}

	if strings.TrimSpace(block) == "" {
		t.Fatalf("the marked decode block in %s is empty", path)
	}

	return block
}

// runAgentDecode runs the extracted block over one metadata value and returns the argv
// it built.
func runAgentDecode(t *testing.T, decode, raw string, pathPrefix ...string) ([]string, error) {
	t.Helper()

	// MISSING TOOLS FAIL RATHER THAN SKIP. A skip here is indistinguishable from a
	// pass in the one place it matters — CI, where nobody reads the log of a green
	// run — and it would hide every case below. All three are on the CI image and on
	// any machine that can run scripts/build-guest-image.sh, which requires jq for
	// exactly this reason.
	for _, tool := range []string{"bash", "jq", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not installed, and it is what the guest uses to rebuild the argv; "+
				"install it rather than letting this test stop checking (apt-get install jq)", tool)
		}
	}

	// THE VALUE ARRIVES IN THE ENVIRONMENT, never interpolated into the script. A
	// test that built its own shell quoting could be defeated by the very inputs it
	// exists to check, and would be asserting about its quoting rather than about
	// the agent.
	//
	// NUL-separated on the way out for the same reason the decode does not read
	// newline-separated input.
	script := `
set -euo pipefail
log() { echo "billet-agent: $*" >&2; }
cmd=()
raw="$BILLET_AGENT_TEST_RAW"
` + decode + `
printf '%s\0' "${cmd[@]}"
`

	run := exec.CommandContext(t.Context(), "bash", "-c", script)
	run.Env = append(os.Environ(), "BILLET_AGENT_TEST_RAW="+raw)

	// A SHADOW DIRECTORY IN FRONT OF PATH, so a test can stage a tool that fails the
	// way a real broken one does.
	if len(pathPrefix) > 0 {
		run.Env = append(run.Env, "PATH="+strings.Join(pathPrefix, ":")+":"+os.Getenv("PATH"))
	}

	var out, errs bytes.Buffer

	run.Stdout = &out
	run.Stderr = &errs

	if err := run.Run(); err != nil {
		return nil, err
	}

	fields := bytes.Split(out.Bytes(), []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}

	argv := make([]string, 0, len(fields))
	for _, field := range fields {
		argv = append(argv, string(field))
	}

	return argv, nil
}

// AND A DECODER THAT FAILS STOPS THE AGENT, rather than contributing an empty
// argument to a command that then runs.
//
// `decoded=$(… | base64 -d; printf X)` reports the status of the FINAL printf, which
// is always 0, so base64 could fail and nothing would notice. `&&` makes the status
// base64's. Nothing in the ordinary tests can catch that, because jq only ever
// produces base64 that decodes — so this puts a base64 on PATH that behaves the way a
// broken one does: some output, then a non-zero exit.
func TestTheGuestAgentStopsWhenTheDecoderFails(t *testing.T) {
	t.Parallel()

	decode := agentDecodeBlock(t)

	shadow := t.TempDir()

	stub := "#!/bin/sh\nprintf 'partial'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(shadow, "base64"), []byte(stub), 0o755); err != nil {
		t.Fatalf("stage a failing base64: %v", err)
	}

	got, err := runAgentDecode(t, decode, `["/bin/sh","-c","echo hello"]`, shadow)
	if err == nil {
		t.Errorf("the agent ran %q despite its decoder failing", got)
	}
}

// AND THE TWO HALVES OF THE CONTRACT ARE THE SAME NUMBER.
//
// billet states the contract and the guest agent checks it, and they live in
// different files in different languages. Bumping one alone is the failure the
// contract exists to prevent, turned on itself: every guest would refuse every
// launch, and no test would have said so.
func TestBothSidesOfTheGuestContractAgree(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the guest image build script: %v", err)
	}

	match := regexp.MustCompile(`(?m)^WANT_CONTRACT=(\S+)$`).FindSubmatch(source)
	if match == nil {
		t.Fatalf("%s no longer declares WANT_CONTRACT, so nothing pins the guest to "+
			"billet's contract", path)
	}

	if got := string(match[1]); got != GuestContract {
		t.Errorf("the guest image understands contract %s and billet speaks %s, so every "+
			"guest built from this script would refuse every launch", got, GuestContract)
	}
}
