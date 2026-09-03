package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// --kms-key-arn REACHES THE CODEBUILD POLICY, and it used not to.
//
// The flag was parsed and the codebuild branch returned without it, while
// codeBuildKeyARN's own comment named the flag as the remedy for a bare id or alias. So
// an operator who followed billet's advice got the identical policy back and a build
// that could not decrypt its registration — a diagnostic prescribing a command that
// does nothing, which is the failure ADR-005 names.
func TestTheKMSKeyARNFlagReachesTheCodeBuildPolicy(t *testing.T) {
	const keyARN = "arn:aws:kms:us-west-2:000000000000:key/" +
		"11111111-2222-3333-4444-555555555555"

	cfg := codeBuildIAMConfig(t, "alias/billet-jit")

	out := capture(t, func() {
		if err := printCodeBuildIAM(cfg, false, keyARN, testAccount); err != nil {
			t.Fatalf("printCodeBuildIAM: %v", err)
		}
	})

	if !strings.Contains(out, keyARN) {
		t.Errorf("the policy does not name the key the flag supplied, so a build cannot "+
			"decrypt its own registration:\n%s", out)
	}

	if !strings.Contains(out, "kms:") {
		t.Errorf("the policy carries no KMS statement at all:\n%s", out)
	}
}

// AND A BARE ID OR ALIAS WITH NO FLAG IS REFUSED RATHER THAN SILENTLY DROPPED.
//
// Emitting no KMS statement was the worse half: the policy looked complete, applied
// cleanly, and every build then failed to decrypt. The refusal names the exact command
// that produces the ARN.
func TestABareKMSKeyWithoutTheFlagIsRefused(t *testing.T) {
	for _, key := range []string{"alias/billet-jit", "11111111-2222-3333-4444-555555555555"} {
		cfg := codeBuildIAMConfig(t, key)

		err := printCodeBuildIAM(cfg, false, "", testAccount)
		if err == nil {
			t.Errorf("jit_kms_key_id %q produced a policy with no KMS grant, which applies "+
				"cleanly and then fails every build's decryption", key)

			continue
		}

		for _, want := range []string{"--kms-key-arn", "aws kms describe-key"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal for %q does not name %q: %v", key, want, err)
			}
		}

		// THE REMEDY MUST NOT REBUILD THIS COMMAND'S OWN INVOCATION, and two attempts
		// at rebuilding it were both wrong.
		//
		// The first omitted --account, which the command requires, so copying it met a
		// second refusal. Adding --account fixed one flag and left the rest: the
		// reconstruction still dropped --build-role and a non-default --config, so
		// following it after `--build-role` printed THE NODE'S POLICY — StartBuild,
		// StopBuild and DeleteParameter, the three things that must never reach a role
		// a workflow runs as. A diagnostic that hands somebody a privilege escalation
		// is worse than one that hands them a failing command.
		if strings.Contains(err.Error(), "billet init iam --") {
			t.Errorf("the remedy reconstructs a `billet init iam` invocation, which cannot "+
				"carry the flags of the one that was actually run: %v", err)
		}

		if !strings.Contains(err.Error(), "you just ran") {
			t.Errorf("the remedy does not tell the operator to amend their own command: %v", err)
		}
	}
}

// THE REMEDY IS THE SAME FOR BOTH PRINCIPALS, which is what makes it safe: it names no
// mode, so there is no mode for it to get wrong.
//
// A remedy that rebuilt the invocation could only guess, and guessing wrong for
// --build-role prints the NODE's policy — a document carrying exactly the capabilities
// that must never reach the role a workflow runs as.
func TestTheKMSRemedyIsSafeForBothPrincipals(t *testing.T) {
	for _, buildRole := range []bool{false, true} {
		cfg := codeBuildIAMConfig(t, "alias/billet-jit")

		err := printCodeBuildIAM(cfg, buildRole, "", testAccount)
		if err == nil {
			t.Fatalf("buildRole=%v: a bare alias produced a policy with no KMS grant", buildRole)
		}

		if strings.Contains(err.Error(), "billet init iam --") {
			t.Errorf("buildRole=%v: the remedy reconstructs an invocation and so must pick a "+
				"mode; picking wrong for --build-role prints the node's policy: %v",
				buildRole, err)
		}
	}
}

// AND THE KEY IS SHELL-QUOTED IN THE COMMAND IT DOES PRINT — PROVED BY RUNNING IT.
//
// The remedy still prints one command — the `aws kms describe-key` that resolves the
// ARN — and the configured key lands inside it. config refuses a wildcard and whitespace
// in that value but not `$(`, a backtick, `;` or an apostrophe, so an unquoted rendering
// turns a config file into executable syntax on the terminal of somebody following
// billet's advice. That the config is the operator's own file makes this a thin threat
// and not a reason to interpolate unquoted.
//
// THE APOSTROPHE CASE IS THE ONLY DIFFICULT ONE, and asserting a substring missed it
// entirely: with a value containing no quote, `"'" + v + "'"` — the broken
// implementation — satisfies the assertion, so the `'\”` branch that exists for
// exactly this was untested. config permits apostrophes here, so that branch is
// reachable.
//
// SO THE COMMAND IS EXECUTED rather than pattern-matched, which is the rule this
// repository already applies to a generated boot script and a generated buildspec: `aws`
// is replaced by a stand-in that prints its argv one field per line, and the assertion
// is that the key arrives as ONE argument with its bytes intact. A substring check
// agrees with a quoting bug; argv cannot.
func TestTheKMSRemedyQuotesTheConfiguredKey(t *testing.T) {
	for name, key := range map[string]string{
		"a command substitution": "alias/$(touch${IFS}/tmp/pwn)",
		"a backtick":             "alias/`id`",
		"a separator":            "alias/x;id",
		"an apostrophe":          "alias/it's",
		"both":                   "alias/it's$(id)",
		"an ordinary alias":      "alias/billet-jit",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := codeBuildIAMConfig(t, key)

			err := printCodeBuildIAM(cfg, false, "", testAccount)
			if err == nil {
				t.Fatal("a bare alias produced a policy with no KMS grant")
			}

			command := commandLineIn(t, err.Error(), "aws ")
			argv := runWithStandInAWS(t, command)

			// THE KEY ARRIVES AS ITS OWN ARGUMENT, BYTES UNCHANGED. Any expansion,
			// word split or dropped quote shows up here and nowhere else.
			if !slices.Contains(argv, key) {
				t.Errorf("the key did not survive the shell as one argument.\n command: %s\n"+
					" argv:    %q\n want an element equal to %q", command, argv, key)
			}

			// AND THE REGION TOO, because an alias is regional: resolving one against
			// the CLI's default region can hand back a DIFFERENT key, after which the
			// policy applies cleanly and no build can decrypt its registration.
			if !slices.Contains(argv, cfg.Node.CodeBuild.Region) {
				t.Errorf("the resolve command does not name the codebuild region, so an alias "+
					"may resolve against the CLI default: %s", command)
			}
		})
	}
}

// runWithStandInAWS runs one printed command with `aws` replaced by a stand-in that
// prints its argv, and returns the arguments the shell actually produced.
//
// ONE ARGUMENT PER LINE, because a joined string collapses the boundaries this test is
// about: `--key-id alias/a b` and `--key-id 'alias/a b'` differ only in how many
// arguments they are.
func runWithStandInAWS(t *testing.T, command string) []string {
	t.Helper()

	dir := t.TempDir()
	stand := filepath.Join(dir, "aws")

	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"
	if err := os.WriteFile(stand, []byte(script), 0o700); err != nil {
		t.Fatalf("write the stand-in: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+dir)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the printed command did not run: %v\ncommand: %s\noutput: %s",
			err, command, out)
	}

	// A trailing newline leaves an empty final field.
	fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")

	if len(fields) < 2 {
		t.Fatalf("the stand-in saw %d argument(s), so the command reached it malformed: %q",
			len(fields), out)
	}

	return fields
}

// commandLineIn extracts the line a diagnostic prescribes, by its leading word.
//
// THE LINE, NOT THE WHOLE MESSAGE. Asserting that a flag appears somewhere in the prose
// is satisfied by the sentence that explains the flag, which is exactly how a remedy
// missing a required argument passed its own test.
func commandLineIn(t *testing.T, message, prefix string) string {
	t.Helper()

	for line := range strings.SplitSeq(message, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return trimmed
		}
	}

	t.Fatalf("the diagnostic prescribes no %q command at all: %s", prefix, message)

	return ""
}

// AND THE FLAG WITHOUT A CONFIGURED KEY IS REFUSED TOO, mirroring the ec2 path: a
// deployment whose registrations use the account's aws/ssm key needs no grant, so a key
// named here would widen the policy past what the config asks for.
func TestTheKMSKeyARNFlagWithoutAConfiguredKeyIsRefused(t *testing.T) {
	cfg := codeBuildIAMConfig(t, "")

	err := printCodeBuildIAM(cfg, false,
		"arn:aws:kms:us-west-2:000000000000:key/11111111-2222-3333-4444-555555555555",
		testAccount)
	if err == nil {
		t.Fatal("--kms-key-arn was accepted against a config that names no key, widening the " +
			"policy past what the config asks for")
	}

	if !strings.Contains(err.Error(), "jit_kms_key_id") {
		t.Errorf("the refusal does not say which config key is missing: %v", err)
	}
}

// AND A FULL ARN IN THE CONFIG NEEDS NO FLAG, or this would be refusing correct
// configuration — the failure that gets a check deleted.
func TestAFullKMSARNInTheConfigNeedsNoFlag(t *testing.T) {
	const keyARN = "arn:aws:kms:us-west-2:000000000000:key/" +
		"66666666-7777-8888-9999-000000000000"

	cfg := codeBuildIAMConfig(t, keyARN)

	out := capture(t, func() {
		if err := printCodeBuildIAM(cfg, false, "", testAccount); err != nil {
			t.Fatalf("printCodeBuildIAM: %v", err)
		}
	})

	if !strings.Contains(out, keyARN) {
		t.Errorf("the configured key ARN did not reach the policy:\n%s", out)
	}
}

// AND THE BUILD ROLE ALWAYS NAMES A LOG GROUP, derived from CodeBuild's own default
// when the operator names none.
//
// The fallback was "*" — logs:CreateLogGroup on every group in the account, held by a
// role that RUNS THE WORKFLOW. There is a real default to derive
// (/aws/codebuild/<project>), which is why awspolicy can refuse an empty one outright
// rather than falling back to everything.
func TestTheBuildRolePolicyNamesADerivedLogGroup(t *testing.T) {
	cfg := codeBuildIAMConfig(t, "")

	out := capture(t, func() {
		if err := printCodeBuildIAM(cfg, true, "", testAccount); err != nil {
			t.Fatalf("printCodeBuildIAM: %v", err)
		}
	})

	if strings.Contains(out, `"*"`) {
		t.Errorf("the build role is scoped to \"*\" somewhere, and it runs inside somebody's "+
			"workflow:\n%s", out)
	}

	if !strings.Contains(out, "log-group:/aws/codebuild/"+cfg.Node.CodeBuild.Project) {
		t.Errorf("the build role does not name CodeBuild's own default log group for this "+
			"project:\n%s", out)
	}
}

// codeBuildIAMConfig is a minimal codebuild node config for the policy printer.
func codeBuildIAMConfig(t *testing.T, kmsKeyID string) *config.Config {
	t.Helper()

	return &config.Config{Node: &config.NodeConfig{
		Provider: config.ProviderCodeBuild,
		CodeBuild: &config.CodeBuildConfig{
			Region:           "us-west-2",
			Project:          "billet-linux",
			EnvironmentType:  config.CodeBuildLinuxContainer,
			JITParameterPath: "/billet/jit",
			JITKMSKeyID:      kmsKeyID,
		},
	}}
}

// THE CONTROLLER'S SWEEP POLICY LISTS AND DELETES UNDER THE PATH, AND NOTHING ELSE.
//
// It is the one grant that lands on the principal holding the ledger and the App
// key, so what it must never carry is asserted here as well as in the generator:
// no read of a registration, no staging, no build action, no key.
func TestTheControllerSweepPolicyListsAndDeletesUnderThePath(t *testing.T) {
	cfg := codeBuildIAMConfig(t, "")

	out := capture(t, func() {
		if err := printCodeBuildSweepIAM(cfg, false, "", testAccount); err != nil {
			t.Fatalf("printCodeBuildSweepIAM: %v", err)
		}
	})

	for _, want := range []string{
		`"ssm:GetParametersByPath"`,
		`"ssm:DeleteParameter"`,
		`"arn:aws:ssm:us-west-2:` + testAccount + `:parameter/billet/jit"`,
		`"arn:aws:ssm:us-west-2:` + testAccount + `:parameter/billet/jit/*"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the sweep policy lacks %s:\n%s", want, out)
		}
	}

	for _, never := range []string{"codebuild:", "ssm:PutParameter", `"ssm:GetParameter"`, "kms:", "ec2:", `"*"`} {
		if strings.Contains(out, never) {
			t.Errorf("the sweep policy carries %s, which the control plane must not hold:\n%s", never, out)
		}
	}

	// AND IT REFUSES TO DESCRIBE TWO PRINCIPALS AT ONCE, or a key it has no use for.
	if err := printCodeBuildSweepIAM(cfg, true, "", testAccount); err == nil {
		t.Error("--controller-sweep with --build-role printed a policy for two principals")
	}

	if err := printCodeBuildSweepIAM(cfg, false, "arn:aws:kms:us-west-2:"+testAccount+":key/k", testAccount); err == nil {
		t.Error("--controller-sweep with --kms-key-arn printed a grant the sweep cannot use")
	}

	if err := printCodeBuildSweepIAM(cfg, false, "", ""); err == nil {
		t.Error("--controller-sweep without --account printed a policy with no account to scope to")
	}
}

// AND THE COMMAND REFUSES WITHOUT AN ACCOUNT, rather than emitting a wildcard.
//
// THIS IS WHAT THE FIRST VERSION DID AND IT COULD NOT WORK AT ALL. The project and log
// group are addressed by ARN and node.codebuild carries no account, so the printer used
// `*` — and awspolicy refuses a wildcard in a Resource, deliberately, because that is
// how a scoped-looking grant turns out not to be. Every codebuild invocation of `billet
// init iam` therefore failed with "contains a wildcard", and nothing caught it because
// nothing tested this printer: the drift test builds its renderings from awspolicy
// directly with an account sentinel.
func TestTheCodeBuildIAMCommandRefusesWithoutAnAccount(t *testing.T) {
	for name, account := range map[string]string{
		"absent":      "",
		"a wildcard":  "*",
		"too short":   "12345",
		"not digits":  "my-account0",
		"with spaces": "  0123456789",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := codeBuildIAMConfig(t, "")

			err := printCodeBuildIAM(cfg, false, "", account)
			if err == nil {
				t.Fatalf("--account %q was accepted; it lands in an IAM Resource", account)
			}

			if !strings.Contains(err.Error(), "--account") {
				t.Errorf("the refusal does not name the flag: %v", err)
			}
		})
	}

	// AND THE COMMAND ACTUALLY EMITS A POLICY with a real account, or the cases above
	// would be passing against a printer that is broken for another reason entirely —
	// which is exactly the state this test was written to find.
	cfg := codeBuildIAMConfig(t, "")

	out := capture(t, func() {
		if err := printCodeBuildIAM(cfg, false, "", testAccount); err != nil {
			t.Fatalf("printCodeBuildIAM: %v", err)
		}
	})

	if !strings.Contains(out, ":"+testAccount+":project/billet-linux") {
		t.Errorf("the policy does not name the project in the given account:\n%s", out)
	}

	if strings.Contains(out, "*:project/") {
		t.Errorf("the policy still carries a wildcard account:\n%s", out)
	}
}

// testAccount is a syntactically valid account id that belongs to nobody.
const testAccount = "000000000000"
