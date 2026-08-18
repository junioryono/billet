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

	path := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read guest image builder: %v", err)
	}
	text := string(source)

	mountAt := strings.Index(text, "mount -t ext4 -o noatime /dev/vdb /var/lib/docker")
	startAt := strings.Index(text, "systemctl start docker.service")
	if mountAt < 0 || startAt < 0 || mountAt >= startAt {
		t.Fatalf("Docker cache mount/start order is not encoded in the guest agent")
	}
	if strings.Contains(text, "After=network-online.target docker.service") ||
		strings.Contains(text, "Requires=docker.service") {
		t.Fatal("systemd can start Docker before the guest agent mounts its image store")
	}
	if !strings.Contains(text, "systemctl disable docker.service docker.socket") {
		t.Fatal("the image still permits Docker to autostart before its cache is mounted")
	}
	if strings.Contains(text, "operation=commit") ||
		!strings.Contains(text, `"$cache_endpoint/v1/volumes/0/discard"`) {
		t.Fatal("the guest can publish Docker state without an authoritative job result")
	}
	if !strings.Contains(text, "BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES") ||
		!strings.Contains(text, "fetch buildkit-cache-mount-limit-bytes") {
		t.Fatal("the tier's BuildKit cache-mount ceiling never reaches workflow actions")
	}
	configureAt := strings.Index(text, "registry-mirrors")
	if configureAt < 0 || configureAt >= startAt {
		t.Fatal("the guest does not configure its Docker Hub mirror before Docker starts")
	}
	if !strings.Contains(text, "fetch registry-mirrors") ||
		!strings.Contains(text, "BILLET_REGISTRY_MIRRORS_JSON") {
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

	const begin = "runner_env=(\n"
	const end = ")\nif [ -n \"$cache_endpoint\" ]"
	if strings.Count(text, begin) != 1 || strings.Count(text, end) != 1 {
		t.Fatal("the guest agent's base runner environment is no longer uniquely extractable")
	}
	_, after, _ := strings.Cut(text, begin)
	body, _, _ := strings.Cut(after, end)
	block := begin + body + ")\n"

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
cmd=(bash -c 'printf "%s\0" "${HOME-}" "${USER-}" "${LOGNAME-}" "${RUNNER_TOOL_CACHE-}" "${AGENT_TOOLSDIRECTORY-}" "$(id -u)" "$(id -g)"')
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
	}
	out, err := run.Output()
	if err != nil {
		t.Fatalf("run the extracted privilege-drop launch: %v", err)
	}
	fields := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	want := []string{
		"/home/runner", "runner", "runner", "/opt/hostedtoolcache", "/opt/hostedtoolcache",
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

func TestTheGuestImageIncludesBuildxForThePersistentBuilder(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read guest image builder: %v", err)
	}

	if !strings.Contains(string(source), "docker.io docker-buildx") {
		t.Fatal("the guest installs Docker without the Buildx CLI required by setup-docker-builder")
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
