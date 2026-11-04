package ec2

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/runnerrelease"
)

const windowsImageReply = `<DescribeImagesResponse><imagesSet><item>` +
	`<imageId>ami-0abc</imageId><rootDeviceName>/dev/sda1</rootDeviceName>` +
	`<rootDeviceType>ebs</rootDeviceType><architecture>x86_64</architecture>` +
	`<platform>windows</platform>` +
	`<blockDeviceMapping><item><deviceName>/dev/sda1</deviceName><ebs>` +
	`<deleteOnTermination>true</deleteOnTermination><volumeSize>30</volumeSize></ebs></item>` +
	`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`

func windowsSpec() provider.Spec {
	spec := validSpec()
	spec.GuestOS = config.GuestWindows
	spec.Command = []string{config.WindowsRunnerCommand}

	return spec
}

// answerWindowsImage has the fake describe every image as a Windows AMI billet built, and
// answers the rest as usual.
func answerWindowsImage(action string, _ url.Values) (int, string) {
	if action == "DescribeImages" {
		return http.StatusOK, strings.Replace(windowsImageReply, `</blockDeviceMapping>`,
			`</blockDeviceMapping><tagSet><item><key>`+amiBuiltByTag+`</key><value>v1</value></item></tagSet>`, 1)
	}

	return http.StatusOK, defaultReply(action)
}

// AMAZON'S OWN AMI IS A WINDOWS AMI WITH NO ENTRY POINT, one paste from a tier's image, so
// the agreement of the two is not enough under the command only billet's image carries;
// a tier naming its own has said its image has it.
func TestAWindowsLeaseOnAStockAMIIsRefusedUnderBilletsCommand(t *testing.T) {
	t.Parallel()

	stock := func(action string, _ url.Values) (int, string) {
		if action == "DescribeImages" {
			return http.StatusOK, windowsImageReply
		}

		return http.StatusOK, defaultReply(action)
	}

	f := newFakeEC2(t)
	f.respond = stock

	_, err := newTestProvider(t, f, nil).Launch(t.Context(), windowsSpec())
	if err == nil || !strings.Contains(err.Error(), "no billet built") ||
		!strings.Contains(err.Error(), "billet ami verify") {
		t.Fatalf("Launch answered %v, want a refusal naming the image and what a copy needs", err)
	}
	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("%d instances were asked for", n)
	}

	own := newFakeEC2(t)
	own.respond = stock
	spec := windowsSpec()
	spec.Command = []string{`C:\mine\start.ps1`}

	if _, err := newTestProvider(t, own, nil).Launch(t.Context(), spec); err != nil {
		t.Fatalf("a tier naming its own command was refused: %v", err)
	}
}

// A WINDOWS LAUNCH BOOTS POWERSHELL: EC2Launch runs what is between the tags, and the
// registration is an assignment in that script and nowhere else in the request.
func TestAWindowsLaunchBootsPowerShellWithTheRegistrationInItsEnvironment(t *testing.T) {
	t.Parallel()

	f := newFakeEC2(t)
	f.respond = answerWindowsImage
	p := newTestProvider(t, f, nil)

	if _, err := p.Launch(t.Context(), windowsSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	params := f.paramsFor(t, "RunInstances")
	script := decodeUserData(t, params.Get("UserData"))

	if !strings.HasPrefix(script, "<powershell>\n") || !strings.HasSuffix(script, "</powershell>\n") {
		t.Fatalf("the boot script is not one EC2Launch runs:\n%s", script)
	}

	jit := validSpec().JITConfig

	if !strings.Contains(script, "$env:"+jitEnvVar+" = '"+jit+"'\n") {
		t.Errorf("the registration is not in the runner's variable:\n%s", script)
	}
	if n := strings.Count(script, jit); n != 1 {
		t.Errorf("the registration appears %d times in the boot script", n)
	}
	if !strings.Contains(script, "& '"+config.WindowsRunnerCommand+"'\n") {
		t.Errorf("the boot script does not run the tier's command:\n%s", script)
	}
	// A BOOT THAT CANNOT START A RUNNER ENDS THE MACHINE, rather than billing a licence by
	// the hour for an instance that holds a lease and registers nothing.
	if !strings.Contains(script, "} catch {\n\tStop-Computer -Force") {
		t.Errorf("a failed start does not end the machine:\n%s", script)
	}
	if strings.Contains(script, "#!/bin/sh") {
		t.Errorf("a Windows boot script carries Linux's shebang:\n%s", script)
	}

	// THE SAME INSTANCE EVERY OTHER LAUNCH GETS, for the rules the launch path states.
	for key, want := range map[string]string{
		"MetadataOptions.HttpTokens":              "required",
		"MetadataOptions.HttpPutResponseHopLimit": "1",
		"BlockDeviceMapping.1.DeviceName":         "/dev/sda1",
		// READ FAST ENOUGH TO TAKE ITS ASSIGNMENT: windowsVolumeInitRate says what it costs.
		"BlockDeviceMapping.1.Ebs.VolumeInitializationRate": "200",
	} {
		if got := params.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// AND A LINUX LAUNCH, WHICH NEEDS NO SUCH THING, PAYS NOTHING FOR IT.
	linux := newFakeEC2(t)
	if _, err := newTestProvider(t, linux, nil).Launch(t.Context(), validSpec()); err != nil {
		t.Fatalf("Linux Launch: %v", err)
	}
	if got := linux.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.1.Ebs.VolumeInitializationRate"); got != "" {
		t.Errorf("a Linux launch asks for an initialization rate of %s", got)
	}
}

// A LAUNCH WHOSE GUEST OS AND AMI DISAGREE SENDS NOTHING, either way: the boot script
// would be text nothing on that image runs, and the lease would sit charged until
// something reaped an instance that looks healthy.
func TestALaunchWhoseGuestOSAndImageDisagreeSendsNothing(t *testing.T) {
	t.Parallel()

	t.Run("a Linux lease on a Windows AMI", func(t *testing.T) {
		t.Parallel()

		f := newFakeEC2(t)
		f.respond = answerWindowsImage

		_, err := newTestProvider(t, f, nil).Launch(t.Context(), validSpec())
		if err == nil || !strings.Contains(err.Error(), "is a Windows AMI") {
			t.Fatalf("Launch answered %v, want a refusal naming the image", err)
		}
		if n := f.countOf("RunInstances"); n != 0 {
			t.Errorf("%d instances were asked for", n)
		}
	})

	t.Run("a Windows lease on a Linux AMI", func(t *testing.T) {
		t.Parallel()

		f := newFakeEC2(t)

		_, err := newTestProvider(t, f, nil).Launch(t.Context(), windowsSpec())
		if err == nil || !strings.Contains(err.Error(), "--os windows") {
			t.Fatalf("Launch answered %v, want a refusal naming the build that makes one", err)
		}
		if n := f.countOf("RunInstances"); n != 0 {
			t.Errorf("%d instances were asked for", n)
		}
	})
}

func TestPsQuoteQuotesWhatItCanAndRefusesWhatItCannot(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]string{
		"":                  "''",
		"plain":             "'plain'",
		"it's":              "'it''s'",
		`C:\billet\x.ps1`:   `'C:\billet\x.ps1'`,
		"$(Stop-Computer)":  "'$(Stop-Computer)'",
		"`n a; b | c & d":   "'`n a; b | c & d'",
		"a''b":              "'a''''b'",
		`"$env:PATH" %TMP%`: `'"$env:PATH" %TMP%'`,
		"~ at the top":      "'~ at the top'",
	} {
		got, err := psQuote(value)
		if err != nil || got != want {
			t.Errorf("psQuote(%q) = %q, %v; want %q", value, got, err, want)
		}
	}

	for name, value := range map[string]string{
		"a NUL":            "a\x00b",
		"a newline":        "a\nb",
		"a carriage":       "a\rb",
		"a tab":            "a\tb",
		"a DEL":            "a\x7fb",
		"a closing tag":    "x</powershell>",
		"an opening tag":   "<persist>true</persist>",
		"a greater-than":   "a>b",
		"a less-than":      "a<b",
		"a typographic ’":  "it’s",
		"a typographic ‘":  "‘quoted",
		"any UTF-8 at all": "café",
	} {
		if got, err := psQuote(value); err == nil {
			t.Errorf("%s: psQuote(%q) = %q, want a refusal", name, value, got)
		}
	}
}

// WHAT psQuote WROTE IS ONE VALUE TO POWERSHELL, read by PowerShell, whatever it held.
func TestPsQuoteIsOneValueToPowerShell(t *testing.T) {
	t.Parallel()

	pwsh := lookPwsh(t)

	for _, value := range []string{
		"it's", "''", "'; Stop-Computer; '", "$(Write-Output no)", "`$x", "a b  c", `"quoted"`,
		"@(1,2)", "%PATH%", "-Force", `C:\x\y`, "#comment",
	} {
		quoted, err := psQuote(value)
		if err != nil {
			t.Fatalf("psQuote(%q): %v", value, err)
		}

		out, err := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-Command",
			"[Console]::Out.Write("+quoted+")").CombinedOutput()
		if err != nil {
			t.Fatalf("%q: %v\n%s", value, err, out)
		}
		if string(out) != value {
			t.Errorf("%q came back as %q", value, out)
		}
	}
}

func lookPwsh(t *testing.T) string {
	t.Helper()

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh on this machine, so nothing here can read PowerShell")
	}

	return pwsh
}

// assertParses holds a script to PowerShell's own parser, tags stripped.
func assertParses(t *testing.T, name, script string) {
	t.Helper()

	pwsh := lookPwsh(t)
	body := strings.TrimSuffix(strings.TrimPrefix(script, "<powershell>\n"), "</powershell>\n")

	if strings.TrimSpace(body) == "" {
		t.Fatalf("%s is empty, which parses", name)
	}

	file := filepath.Join(t.TempDir(), "script.ps1")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-Command", `
$errors = $null
$tokens = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($env:BILLET_PARSE, [ref]$tokens, [ref]$errors)
if ($errors.Count -gt 0) { $errors | ForEach-Object { Write-Output $_.ToString() }; exit 1 }
if ($ast.EndBlock.Statements.Count -eq 0) { Write-Output 'no statements'; exit 1 }
`)
	cmd.Env = append(os.Environ(), "BILLET_PARSE="+file)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("%s does not parse: %v\n%s\n--- script ---\n%s", name, err, out, script)
	}
}

func TestEveryWindowsScriptBilletRendersParses(t *testing.T) {
	t.Parallel()

	boot, err := windowsUserData(windowsSpec(), 1)
	if err != nil {
		t.Fatal(err)
	}
	assertParses(t, "the boot script", boot)

	bootstrap, err := windowsBootstrapScript(testWindowsBuild())
	if err != nil {
		t.Fatal(err)
	}
	assertParses(t, "the first boot", bootstrap)

	second, _, ok := strings.Cut(strings.SplitN(bootstrap, "@'\n", 2)[1], "\n'@\n")
	if !ok {
		t.Fatalf("no second boot in the bootstrap:\n%s", bootstrap)
	}
	assertParses(t, "the second boot", second)

	verifier, _, err := windowsVerifyScript("x64", verifyProbeNonce)
	if err != nil {
		t.Fatal(err)
	}
	assertParses(t, "the verifier", verifier)

	check, _, ok := strings.Cut(strings.SplitN(verifier, "@'\n", 2)[1], "\n'@\n")
	if !ok {
		t.Fatalf("no check in the verifier:\n%s", verifier)
	}
	assertParses(t, "the verifier's check", check)
}

// A CACHE FIELD ON A WINDOWS LAUNCH IS A CALLER WHO BELIEVES A CACHE EXISTS, and the
// bearer would be in a boot script with no client to spend it.
func TestAWindowsBootScriptRefusesWhatOnlyALinuxGuestReads(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*provider.Spec){
		"a cache endpoint": func(s *provider.Spec) { s.CacheEndpoint = "https://cache" },
		"a cache token":    func(s *provider.Spec) { s.CacheToken = "bearer" },
		"an actions proxy": func(s *provider.Spec) { s.ActionsProxy = "http://proxy" },
		"an npm proxy":     func(s *provider.Spec) { s.NpmProxy = "http://proxy" },
		"an actions CA":    func(s *provider.Spec) { s.ActionsCAPEM = "pem" },
		"the guest agent":  func(s *provider.Spec) { s.GuestMonitor = true },
		"guest caches":     func(s *provider.Spec) { s.GuestCaches = []provider.GuestCache{provider.GuestCacheGo} },
	} {
		spec := windowsSpec()
		mutate(&spec)

		if _, err := windowsUserData(spec, 1); err == nil {
			t.Errorf("%s reached a Windows boot script", name)
		}
	}

	spec := windowsSpec()
	spec.JITConfig = "a</powershell>b"

	_, err := windowsUserData(spec, 1)
	if err == nil {
		t.Fatal("a registration that could end the script's tag was carried")
	}
	if strings.Contains(err.Error(), "a</powershell>b") {
		t.Errorf("the refusal quotes the registration: %v", err)
	}

	spec = windowsSpec()
	spec.Command = []string{"C:\\x.ps1", "$(a)</powershell>"}

	if _, err := windowsUserData(spec, 1); err == nil {
		t.Fatal("a command argument that could end the script's tag was carried")
	}
}

func testWindowsBuild() BuildSpec {
	return BuildSpec{
		Windows: true, PayloadBucket: testPayloadBucket, BaseImage: "ami-base", InstanceType: "c7i.xlarge",
		Arch: "x64", RunnerSHA256: testRunnerDigest, RunnerVersion: "2.337.0", Name: "win",
		payloadURL: testPayloadURL, payloadSHA256: testRunnerDigest, nonce: verifyProbeNonce,
	}
}

// THE PAYLOAD IS WHAT THE DRIVER READS, at the paths it reads them from, and the
// settings name this build and the runner it checks.
func TestTheWindowsPayloadLaysOutWhatTheDriverReads(t *testing.T) {
	t.Parallel()

	archive, digest, err := windowsPayloadArchive(testWindowsBuild())
	if err != nil {
		t.Fatal(err)
	}
	if digest != sha256Hex(archive) {
		t.Errorf("the digest %s is not the archive's", digest)
	}

	headers, bodies := payloadMembers(t, archive)

	for _, member := range []string{
		"billet-ami/repo/scripts/windows/build-ami.ps1",
		"billet-ami/repo/scripts/windows/ec2-layer.ps1",
		"billet-ami/repo/scripts/windows/billet-runner.ps1",
		"billet-ami/repo/scripts/windows/billet-runner-service.ps1",
		"billet-ami/repo/scripts/windows/billet-job-started.ps1",
		"billet-ami/repo/scripts/windows/prepare-aws-tools.ps1",
		"billet-ami/repo/scripts/runner-images/plan-windows-2025.json",
		"billet-ami/repo/scripts/runner-images/differences-windows.tsv",
		"billet-ami/repo/scripts/runner-images/upstream/images/windows/scripts/build/Install-Toolset.ps1",
		"billet-ami/repo/scripts/runner-images/upstream/images/windows/scripts/helpers/ImageHelpers.psm1",
		"billet-ami/repo/scripts/runner-images/upstream/images/windows/scripts/tests/Helpers.psm1",
		"billet-ami/repo/scripts/runner-images/upstream/images/windows/assets/post-gen/GenerateIISExpressCertificate.ps1",
		"billet-ami/repo/internal/runnerimages/toolset-2025.json",
		"billet-ami/repo/internal/runnerimages/pinned.txt",
	} {
		if _, ok := headers[member]; !ok {
			t.Errorf("the payload does not carry %s", member)
		}
	}

	for name := range headers {
		if strings.Contains(name, "images/ubuntu") || strings.HasSuffix(name, "toolset-2404.json") {
			t.Errorf("a Windows payload carries Linux's %s", name)
		}
	}

	var settings struct {
		Nonce, RunnerVersion, RunnerSHA256 string
	}

	if err := json.Unmarshal([]byte(strings.NewReplacer(`"runner_version"`, `"RunnerVersion"`,
		`"runner_sha256"`, `"RunnerSHA256"`).Replace(string(bodies["billet-ami/build.json"]))), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Nonce != verifyProbeNonce || settings.RunnerVersion != "2.337.0" ||
		settings.RunnerSHA256 != testRunnerDigest {
		t.Errorf("the settings are %+v", settings)
	}
}

// EVERY FILE THE DRIVER RUNS IN THE PLAN IS IN THE PAYLOAD: a step that is in the plan
// and not skipped and not on the builder fails an hour into a build, not here.
func TestTheWindowsPayloadCarriesEveryFileThePlanRuns(t *testing.T) {
	t.Parallel()

	archive, _, err := windowsPayloadArchive(testWindowsBuild())
	if err != nil {
		t.Fatal(err)
	}

	headers, bodies := payloadMembers(t, archive)

	const scripts = "billet-ami/repo/scripts/runner-images/"

	var plan []struct {
		ID, Kind, Script string
		Sources          []string
	}
	if err := json.Unmarshal(bodies[scripts+"plan-windows-2025.json"], &plan); err != nil {
		t.Fatal(err)
	}

	skipped := map[string]bool{}
	for _, line := range strings.Split(string(bodies[scripts+"differences-windows.tsv"]), "\n") {
		if strings.HasPrefix(line, "skip\t") {
			skipped[strings.Split(line, "\t")[1]] = true
		}
	}

	ran := 0

	for _, step := range plan {
		if skipped[step.ID] {
			continue
		}

		ran++

		if step.Script != "" {
			if _, ok := headers[scripts+"upstream/"+step.Script]; !ok {
				t.Errorf("the step %s runs %s, which the payload does not carry", step.ID, step.Script)
			}
		}

		for _, source := range step.Sources {
			if source == "images/windows/toolsets" {
				continue
			}

			prefix := scripts + "upstream/" + source + "/"
			if !slices.ContainsFunc(keysOf(headers), func(name string) bool { return strings.HasPrefix(name, prefix) }) {
				t.Errorf("the step %s copies %s, which the payload does not carry", step.ID, source)
			}
		}
	}

	if ran < 10 {
		t.Fatalf("only %d steps run; the list reads wrongly", ran)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

// THE BOOTSTRAP CHECKS WHAT IT FETCHED, carries the URL in no shell of its own making,
// fits what EC2 takes as it is, and asks for the reboot COM1 needs.
func TestTheWindowsBootstrapIsWhatEC2Takes(t *testing.T) {
	t.Parallel()

	spec := testWindowsBuild()

	script, err := windowsBootstrapScript(spec)
	if err != nil {
		t.Fatal(err)
	}

	if len(script) > maxUserData {
		t.Fatalf("the bootstrap is %d bytes and EC2 takes %d, and EC2Launch documents no gzip",
			len(script), maxUserData)
	}

	for _, want := range []string{
		"<powershell>", "'" + testPayloadURL + "'", spec.payloadSHA256, verifyProbeNonce,
		"bcdedit /ems", "bcdedit /bootems", "Restart-Computer -Force", "-AtStartup", "Get-FileHash",
		"tar.exe -xzf", windowsImageMark, windowsDriver,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the bootstrap does not say %q:\n%s", want, script)
		}
	}

	if at := strings.Index(script, "Get-FileHash"); at < 0 || at > strings.Index(script, "tar.exe") {
		t.Errorf("the bootstrap does not check the payload before it unpacks it:\n%s", script)
	}

	// A PLACEHOLDER LEFT IN IS A STRING POWERSHELL WOULD COMPARE AGAINST.
	if m := regexp.MustCompile(`@[A-Z_]+@`).FindString(script); m != "" {
		t.Errorf("the bootstrap still holds the placeholder %s:\n%s", m, script)
	}

	for _, bad := range []func(*BuildSpec){
		func(s *BuildSpec) { s.payloadURL = "" },
		func(s *BuildSpec) { s.payloadSHA256 = "not-a-digest" },
		func(s *BuildSpec) { s.nonce = "$(no)" },
		func(s *BuildSpec) { s.payloadURL = "https://x/'; Stop-Computer; '" + "\n" },
	} {
		spec := testWindowsBuild()
		bad(&spec)

		if _, err := windowsBootstrapScript(spec); err == nil {
			t.Errorf("a bootstrap was rendered for %+v", spec)
		}
	}
}

// A DRIVER THAT RETURNS HAVING REMOVED ITSELF IS A BUILD THAT FINISHED, ended by its own
// shutdown: reporting a failure then would read as billet's last word on a good image.
func TestTheWindowsBootstrapReportsADriverThatReturnedOnlyIfItIsStillThere(t *testing.T) {
	t.Parallel()

	script, err := windowsBootstrapScript(testWindowsBuild())
	if err != nil {
		t.Fatal(err)
	}

	if want := "if (Test-Path -LiteralPath '" + windowsDriver + "') { Stop-WithFailure 'start' }"; !strings.Contains(script, want) {
		t.Errorf("the bootstrap does not hold its report to the driver being there: want %q\n%s", want, script)
	}
}

// A BUILDER'S USER DATA TRAVELS AS IT IS ON WINDOWS, where nothing would unzip it, and
// the too-large bootstrap is refused before a machine that would do nothing is bought.
func TestAWindowsBootstrapIsNeverCompressed(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", maxUserData+1)

	if _, err := builderUserData(true, large); err == nil {
		t.Error("a Windows bootstrap past EC2's limit was accepted")
	}

	compressed, err := builderUserData(false, large)
	if err != nil {
		t.Fatalf("a Linux script past the limit that compresses was refused: %v", err)
	}
	if string(compressed) == large {
		t.Error("the Linux path stopped compressing")
	}

	plain, err := builderUserData(true, "short")
	if err != nil || string(plain) != "short" {
		t.Errorf("a Windows bootstrap that fits became %q, %v", plain, err)
	}
}

// windowsBuildFake is a whole Windows build as EC2 answers one.
type windowsBuildFake struct {
	server *fakeEC2

	baseLinux      bool
	verdict        string
	templateStep   string
	verifierAnswer func(nonce string) string
	nonce          string
	builderRead    bool
	promoted       string
	verifyScript   string
	builderScript  string
	builderDisk    string
}

func (b *windowsBuildFake) buildNonce() string {
	b.server.mu.Lock()
	defer b.server.mu.Unlock()

	if len(b.server.staged) == 0 {
		return ""
	}

	var settings struct {
		Nonce string `json:"nonce"`
	}

	if err := json.Unmarshal(payloadMember(b.server.staged[len(b.server.staged)-1], "billet-ami/build.json"),
		&settings); err != nil {
		return ""
	}

	return settings.Nonce
}

func (b *windowsBuildFake) reply(action string, params url.Values) (int, string) {
	switch action {
	case "DescribeImages":
		reply := windowsImageReply
		if b.baseLinux {
			reply = strings.Replace(reply, `<platform>windows</platform>`, "", 1)
		}

		tags := ""
		if b.promoted != "" {
			tags = `<tagSet><item><key>` + amiContractTag + `</key><value>` + b.promoted + `</value></item></tagSet>`
		}

		return http.StatusOK, strings.Replace(reply, `</blockDeviceMapping>`,
			`</blockDeviceMapping><imageState>available</imageState>`+tags, 1)

	case "RunInstances":
		raw, _ := base64.StdEncoding.DecodeString(params.Get("UserData"))
		id := "i-builder"

		if params.Get("ImageId") != "ami-base" {
			id = "i-verify"
			b.verifyScript = string(raw)
			b.nonce = nonceFromUserData(params.Get("UserData"))
		} else {
			b.builderScript = string(raw)
			b.builderDisk = params.Get("BlockDeviceMapping.1.Ebs.VolumeSize")
		}

		return http.StatusOK, `<RunInstancesResponse><instancesSet><item><instanceId>` + id +
			`</instanceId><instanceState><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`

	case "GetConsoleOutput":
		var text string

		if params.Get("InstanceId") == "i-builder" {
			nonce := b.buildNonce()
			text = buildBegin + " " + nonce + "\nverdict=" + b.verdict + "\nstep=" + b.step() +
				"\ntemplate_step=" + b.templateStep + "\ncomponents=\n" + buildEnd + " " + nonce + "\n"
			b.builderRead = true
		} else {
			text = b.verifierAnswer(b.nonce)
		}

		return http.StatusOK, `<GetConsoleOutputResponse><output>` +
			base64.StdEncoding.EncodeToString([]byte(text)) + `</output></GetConsoleOutputResponse>`

	case "DescribeInstances":
		id, state, reason := params.Get("InstanceId.1"), "running", ""
		if id == "i-builder" && b.builderRead && b.verdict == verdictOK {
			state, reason = "stopped", "Client.InstanceInitiatedShutdown"
		}

		return http.StatusOK, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>` + id +
			`</instanceId><instanceState><name>` + state + `</name></instanceState><stateReason><code>` + reason +
			`</code></stateReason></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`

	case "CreateImage":
		return http.StatusOK, `<CreateImageResponse><imageId>ami-new</imageId></CreateImageResponse>`

	case "CreateTags":
		b.promoted = params.Get("Tag.1.Value")

		return http.StatusOK, `<CreateTagsResponse><return>true</return></CreateTagsResponse>`

	default:
		return http.StatusOK, defaultReply(action)
	}
}

func (b *windowsBuildFake) step() string {
	if b.verdict == verdictOK {
		return "done"
	}

	return "template"
}

func provedVerifier(nonce string) string {
	return reportBegin + " " + nonce + "\nverdict=ok\nstep=done\nrunner=2.337.0\n" + reportEnd + " " + nonce + "\n"
}

// A WHOLE BUILD: the stock AMI, the framed verdict, the image, a boot of the image, and
// the Windows contract stamped on it only then.
func TestAWindowsBuildImagesVerifiesAndStampsItsOwnContract(t *testing.T) {
	t.Parallel()

	b := &windowsBuildFake{verdict: verdictOK, verifierAnswer: provedVerifier}
	f := newFakeEC2(t)
	b.server = f
	f.respond = b.reply

	spec := testWindowsBuild()
	spec.Verify = true

	image, err := newTestProvider(t, f, nil).BuildImage(t.Context(), spec)
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if image != "ami-new" {
		t.Errorf("the build returned %q", image)
	}

	if !strings.HasPrefix(b.builderScript, "<powershell>") {
		t.Errorf("the builder booted something EC2Launch does not run:\n%s", b.builderScript)
	}
	if !strings.HasPrefix(b.verifyScript, "<powershell>") || !strings.Contains(b.verifyScript, reportBegin) {
		t.Errorf("the verifier booted something EC2Launch does not run:\n%s", b.verifyScript)
	}
	if b.promoted != "1" {
		t.Errorf("the image was stamped %q, want the Windows contract", b.promoted)
	}
	if b.builderDisk != strconv.Itoa(DefaultWindowsBuilderDiskGiB) {
		t.Errorf("the builder's root is %q GiB, want the Windows default", b.builderDisk)
	}
	if got := f.countOf("CreateImage"); got != 1 {
		t.Errorf("%d images were registered", got)
	}

	// THE PAYLOAD WENT, whatever became of the build.
	f.mu.Lock()
	removed := f.removed
	f.mu.Unlock()

	if removed != 1 {
		t.Errorf("%d payloads were removed", removed)
	}
}

// AN IMAGE THAT FAILS ITS BOOT KEEPS NO CONTRACT.
func TestAWindowsImageThatFailsItsBootIsNotStamped(t *testing.T) {
	t.Parallel()

	b := &windowsBuildFake{verdict: verdictOK, verifierAnswer: func(nonce string) string {
		return reportBegin + " " + nonce + "\nverdict=fail\nstep=toolcache\n" + reportEnd + " " + nonce + "\n"
	}}
	f := newFakeEC2(t)
	b.server = f
	f.respond = b.reply

	spec := testWindowsBuild()
	spec.Verify = true

	_, err := newTestProvider(t, f, nil).BuildImage(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), "toolcache") {
		t.Fatalf("BuildImage answered %v, want the step the image failed at", err)
	}
	if b.promoted != "" {
		t.Errorf("the image was stamped %q", b.promoted)
	}
}

// A VERDICT OF FAILURE ENDS THE WAIT, naming the step from billet's own plan, and no
// image is made.
func TestAWindowsBuildThatFailsIsNamedFromThePlanAndNotImaged(t *testing.T) {
	t.Parallel()

	b := &windowsBuildFake{verdict: verdictFail, templateStep: "1"}
	f := newFakeEC2(t)
	b.server = f
	f.respond = b.reply

	_, err := newTestProvider(t, f, nil).BuildImage(t.Context(), testWindowsBuild())
	if err == nil || !strings.Contains(err.Error(), "of GitHub's template (inline:") {
		t.Fatalf("BuildImage answered %v, want the failed step named from the plan", err)
	}
	if strings.Contains(err.Error(), "journalctl") {
		t.Errorf("a Windows failure sends the operator to systemd: %v", err)
	}
	if n := f.countOf("CreateImage"); n != 0 {
		t.Errorf("%d images were registered", n)
	}

	// THE PAYLOAD GOES WHEN THE BUILD DOES NOT FINISH, TOO: it is costing money.
	f.mu.Lock()
	removed := f.removed
	f.mu.Unlock()

	if removed != 1 {
		t.Errorf("%d payloads were removed", removed)
	}
}

// THE OPERATOR SAYS WHICH TEMPLATE AND THE IMAGE SAYS WHAT IT IS, and a disagreement buys
// no builder: the bootstrap would be text nothing there runs, for hours.
func TestABuildWhoseOSAndBaseImageDisagreeBuysNothing(t *testing.T) {
	t.Parallel()

	t.Run("windows from a Linux base", func(t *testing.T) {
		t.Parallel()

		b := &windowsBuildFake{baseLinux: true, verdict: verdictOK}
		f := newFakeEC2(t)
		b.server = f
		f.respond = b.reply

		_, err := newTestProvider(t, f, nil).BuildImage(t.Context(), testWindowsBuild())
		if err == nil || !strings.Contains(err.Error(), "is not a Windows AMI") {
			t.Fatalf("BuildImage answered %v", err)
		}
		if n := f.countOf("RunInstances"); n != 0 {
			t.Errorf("%d builders were bought", n)
		}
	})

	t.Run("linux from a Windows base", func(t *testing.T) {
		t.Parallel()

		b := &windowsBuildFake{verdict: verdictOK}
		f := newFakeEC2(t)
		b.server = f
		f.respond = b.reply

		_, err := newTestProvider(t, f, nil).BuildImage(t.Context(), verifyingBuild())
		if err == nil || !strings.Contains(err.Error(), "--os windows") {
			t.Fatalf("BuildImage answered %v", err)
		}
		if n := f.countOf("RunInstances"); n != 0 {
			t.Errorf("%d builders were bought", n)
		}
	})
}

// WHAT A WINDOWS BUILD CANNOT DO IS REFUSED BEFORE ANYTHING IS STAGED.
func TestAWindowsBuildRefusesWhatItCannotCarry(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*BuildSpec){
		"arm64":          func(s *BuildSpec) { s.Arch = "arm64" },
		"a cache CA":     func(s *BuildSpec) { s.CACertPEM = "pem" },
		"a component":    func(s *BuildSpec) { s.Accept = []string{"firefox"} },
		"a bad runner":   func(s *BuildSpec) { s.RunnerVersion = "$(no)" },
		"a bad digest":   func(s *BuildSpec) { s.RunnerSHA256 = "nope" },
		"no bucket here": func(s *BuildSpec) { s.PayloadBucket = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newFakeEC2(t)
			f.respond = answerWindowsImage

			spec := testWindowsBuild()
			mutate(&spec)

			if _, err := newTestProvider(t, f, nil).BuildImage(t.Context(), spec); err == nil {
				t.Fatal("the build went ahead")
			}

			f.mu.Lock()
			staged := len(f.staged)
			f.mu.Unlock()

			if staged != 0 || f.countOf("RunInstances") != 0 {
				t.Errorf("staged %d payloads and bought %d builders", staged, f.countOf("RunInstances"))
			}
		})
	}
}

// THE VERIFIER'S SCRIPT ASKS WHAT THE CONTRACT SAYS, through the job's path, and reports
// nothing the schema would not read back.
func TestTheWindowsVerifierAsksWhatTheContractSays(t *testing.T) {
	t.Parallel()

	script, schema, err := windowsVerifyScript("x64", verifyProbeNonce)
	if err != nil {
		t.Fatal(err)
	}

	if len(script) > maxUserData {
		t.Fatalf("the verifier is %d bytes and EC2 takes %d", len(script), maxUserData)
	}

	for _, want := range []string{
		reportBegin + " " + verifyProbeNonce, reportEnd + " " + verifyProbeNonce,
		"-User 'runner'", "-RunLevel Highest", "Stop-Computer -Force",
		`C:\billet\billet-runner.ps1`, `C:\billet\billet-runner-service.ps1`, `C:\billet\billet-job-started.ps1`,
		"Ask-Tool 'pwsh' 'pwsh' '--version'", "Ask-Tool 'git' 'git' '--version'",
		"-cnotlike 'PowerShell 7.6.*'",
		"Ask-Toolcache 'tc_python' 'Python' 'x64' @('3.10.', '3.11.', '3.12.', '3.13.', '3.14.')",
		"Ask-Toolcache 'tc_ruby' 'Ruby' 'x64' @('3.2.', '3.3.', '3.4.', '4.0.')",
		"Ask-Toolcache 'tc_pypy' 'PyPy' 'x86' @('3.9.', '3.10.')",
		"Ask-Toolcache 'tc_node' 'node' 'x64' @('22.', '24.')",
		"Ask-Toolcache 'tc_go' 'go' 'x64' @('1.24.', '1.25.', '1.26.')",
		"win25", `C:\hostedtoolcache\windows`,
		`result-` + verifyProbeNonce + `.txt`, "LastTaskResult -eq 0",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the verifier does not say %q", want)
		}
	}

	// THE MACHINE'S OWN END IS SET BEFORE THE CONSOLE IS OPENED, or a console that cannot be
	// opened leaves a verifier nothing ends.
	if at := strings.Index(script, "shutdown.exe /s"); at < 0 || at > strings.Index(script, "$port.Open()") {
		t.Errorf("the verifier opens the console before it sets its own end:\n%s", script)
	}

	if strings.Contains(script, `result.txt`) {
		t.Errorf("the verifier reads a result file an image could already hold:\n%s", script)
	}

	if m := regexp.MustCompile(`@[A-Z_]+@`).FindString(script); m != "" {
		t.Errorf("the verifier still holds the placeholder %s", m)
	}

	for key, value := range map[string]string{
		reportVerdictKey: verdictOK, reportStepKey: "toolcache", "runner": "2.337.0", "tc_pypy": "3.9.19",
		"root_free_gib": "100.1", "image_os": "win25",
	} {
		if !schema.allows(key, value) {
			t.Errorf("the schema refuses %s=%s, which the script writes", key, value)
		}
	}

	if schema.allows("secret", "x") || schema.allows(reportStepKey, "somewhere-else") {
		t.Error("the schema reads what the script never writes")
	}

	if _, _, err := windowsVerifyScript("arm64", verifyProbeNonce); err == nil {
		t.Error("a Windows verifier was rendered for arm64")
	}
	if _, _, err := windowsVerifyScript("x64", "$(no)"); err == nil {
		t.Error("a nonce that is not one reached the script")
	}
}

// THE ENTRY POINT THE VERIFIER HOLDS AN IMAGE TO IS THE ONE THE BUILD INSTALLS: both come
// from the same embedded bytes, and this reads the digest off the rendered script.
func TestTheWindowsVerifierHoldsAnImageToTheEntryPointsThePayloadCarries(t *testing.T) {
	t.Parallel()

	archive, _, err := windowsPayloadArchive(testWindowsBuild())
	if err != nil {
		t.Fatal(err)
	}

	_, bodies := payloadMembers(t, archive)

	script, _, err := windowsVerifyScript("x64", verifyProbeNonce)
	if err != nil {
		t.Fatal(err)
	}

	for path, member := range map[string]string{
		`C:\billet\billet-runner.ps1`:         "billet-ami/repo/scripts/windows/billet-runner.ps1",
		`C:\billet\billet-runner-service.ps1`: "billet-ami/repo/scripts/windows/billet-runner-service.ps1",
		`C:\billet\billet-job-started.ps1`:    "billet-ami/repo/scripts/windows/billet-job-started.ps1",
	} {
		want := "@('" + path + "', '" + sha256Hex(bodies[member]) + "')"
		if !strings.Contains(script, want) {
			t.Errorf("the verifier does not hold %s to its payload's bytes: want %s", path, want)
		}
	}
}

// THE RUNNER'S WINDOWS DIGEST IS PINNED beside the others, and is what a build checks.
func TestTheWindowsRunnerDigestIsTheOnePinned(t *testing.T) {
	t.Parallel()

	sum, ok := PinnedWindowsRunnerSHA256()
	if !ok || !IsRunnerSHA256(sum) {
		t.Fatalf("PinnedWindowsRunnerSHA256 = %q, %v", sum, ok)
	}

	want, ok := runnerrelease.PinnedSHA256For("win-x64")
	if !ok || want != sum {
		t.Errorf("the provider pins %q and the table says %q", sum, want)
	}
}
