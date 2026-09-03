package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/junioryono/billet/internal/awspolicy"
	"github.com/junioryono/billet/internal/config"
)

// printCodeBuildIAM renders the least-privilege policy for one of a codebuild
// deployment's TWO roles.
//
// TWO ROLES, AND THE FLAG IS WHICH ONE. They are not two views of one thing:
//
// The NODE role is what billet assumes. It starts and stops builds in one project,
// reads them back, and stages and removes the single-use runner registration.
//
// The BUILD role is what CodeBuild runs the build AS, which means every permission it
// holds is a permission the workflow holds. It reads the one parameter carrying its
// own registration and writes its own logs — and nothing else. A role that could
// start a build, running inside a build, is how a job launches runners billet never
// escrowed capacity for.
//
// THERE IS NO --deployment SCOPING HERE, and that absence is the interesting part.
// Every ec2 statement is conditioned on an owner TAG carrying the deployment
// identity, which is what isolates one deployment from another sharing an account. A
// CodeBuild BUILD CANNOT BE TAGGED — tags exist on projects and report groups, and
// StartBuild has no field that becomes one — so there is no such condition to write.
// What replaces it is the PROJECT ARN and the PARAMETER PATH, both of which are
// per-deployment resources rather than per-deployment conditions. The boundary is
// just as real; it is enforced by a different mechanism, and it is why the project
// must be billet's alone.
func printCodeBuildIAM(cfg *config.Config, buildRole bool, kmsKeyARN, account string) error {
	if cfg.Node.CodeBuild == nil {
		return fmt.Errorf("`billet init iam` needs node.codebuild for a codebuild node")
	}

	cb := cfg.Node.CodeBuild

	if err := checkAccountID(account); err != nil {
		return err
	}

	keyARN, err := codeBuildKeyARN(cb, kmsKeyARN)
	if err != nil {
		return err
	}

	// THE ACCOUNT IS NOT IN THE CONFIG, so it is a flag rather than a wildcard.
	//
	// A WILDCARD WAS THE FIRST ANSWER AND IT DID NOT WORK AT ALL. The reasoning read
	// well — a project ARN and a parameter path are names inside ONE account, and a
	// role can only ever act in the account it lives in, so the `*` costs only a
	// refusal IAM already makes. What it missed is that awspolicy VALIDATES the ARNs
	// it is handed (requireARN refuses a wildcard anywhere, deliberately, because a
	// wildcard in a Resource is how a scoped-looking grant turns out not to be), so
	// every codebuild invocation of `billet init iam` failed with "contains a
	// wildcard" and the command could not emit a policy at all.
	//
	// NOTHING CAUGHT IT because nothing tested this printer — the drift test builds
	// its renderings from awspolicy directly with an account sentinel, so it exercised
	// the generator and never this path. The command is now covered.
	in := awspolicy.Inputs{
		Region: cb.Region,
		// A CODEBUILD NODE LAUNCHES NOTHING. Without this it would come out holding
		// ec2:RunInstances and ec2:TerminateInstances — which is exactly what the
		// first version of the backup policy did to a control plane, and the reason
		// NoCompute exists at all.
		NoCompute: true,
	}

	parameterPath := strings.TrimSuffix(cb.JITParameterPath, "/")

	if buildRole {
		in.CodeBuildRole = &awspolicy.CodeBuildRole{
			ParameterPath: parameterPath,
			KMSKeyARN:     keyARN,
			LogGroupARN:   codeBuildLogGroupARN(cb, account),
		}
	} else {
		in.CodeBuild = &awspolicy.CodeBuild{
			ProjectARN:    codeBuildProjectARN(cb, account),
			FleetARN:      cb.FleetARN,
			ParameterPath: parameterPath,
			KMSKeyARN:     keyARN,
		}
	}

	policy, err := in.Build()
	if err != nil {
		return err
	}

	body, err := policy.JSON()
	if err != nil {
		return fmt.Errorf("render the policy: %w", err)
	}

	fmt.Println(string(body))

	// SAID OUT LOUD, because the document alone does not say which of the two roles
	// it belongs on — and attaching the build role's policy to the node (or the
	// reverse) produces a deployment that validates and cannot work.
	if buildRole {
		fmt.Fprintln(os.Stderr, "\n# Attach this to the CodeBuild project's SERVICE ROLE — the role "+
			"a build runs AS.\n# Every permission in it is one the workflow holds.")
	} else {
		fmt.Fprintln(os.Stderr, "\n# Attach this to the role the machine running `billet node` "+
			"assumes.\n# Pass --build-role for the project's service role, which is a "+
			"different principal.")
	}

	return nil
}

// printCodeBuildSweepIAM renders the policy the CONTROL PLANE's role needs to sweep
// the staged registrations a dead node left under this node's parameter path.
//
// A THIRD PRINCIPAL, and the printer says so twice: in the refusals, because a flag
// that also selected the build role or a KMS key would be describing two roles at
// once, and on stderr, because the document alone does not say whose it is — and
// this one belongs on the machine running `billet server`, which may be neither the
// node nor anything terraform knows about.
//
// IT IS RUN AGAINST THE NODE'S CONFIG, because that is where the path lives; the
// control plane's own file usually carries no node.codebuild block at all.
func printCodeBuildSweepIAM(cfg *config.Config, buildRole bool, kmsKeyARN, account string) error {
	if cfg.Node.CodeBuild == nil {
		return fmt.Errorf("`billet init iam --controller-sweep` needs node.codebuild, because the " +
			"path it grants over is node.codebuild.jit_parameter_path")
	}

	if buildRole {
		return errors.New("--controller-sweep and --build-role name two different principals: the " +
			"control plane sweeps the path, the build reads one parameter under it. Pass one")
	}

	if kmsKeyARN != "" {
		return errors.New("--kms-key-arn is not meaningful with --controller-sweep: the sweep lists " +
			"names without decrypting anything and deletes by name, so it needs no KMS grant " +
			"whatever key the registrations are encrypted under")
	}

	if err := checkAccountID(account); err != nil {
		return err
	}

	cb := cfg.Node.CodeBuild

	in := awspolicy.Inputs{
		Partition: awsPartitionFor(cb.Region),
		Region:    cb.Region,
		// THE CONTROL PLANE LAUNCHES NOTHING. Same rule as the node's rendering, and
		// the same defect it guards against.
		NoCompute: true,
		CodeBuildSweep: &awspolicy.CodeBuildSweep{
			ParameterPath: strings.TrimSuffix(cb.JITParameterPath, "/"),
			Account:       account,
		},
	}

	policy, err := in.Build()
	if err != nil {
		return err
	}

	body, err := policy.JSON()
	if err != nil {
		return fmt.Errorf("render the policy: %w", err)
	}

	fmt.Println(string(body))

	fmt.Fprintln(os.Stderr, "\n# Attach this to the role the machine running `billet server` "+
		"assumes — the CONTROL PLANE.\n# Not the node's role and never the build's: it lists and "+
		"deletes under the path, and nothing else.")

	return nil
}

// checkAccountID refuses anything but twelve digits.
//
// EXACTLY TWELVE DIGITS, because the value lands in an IAM Resource. Anything else
// either fails awspolicy's own ARN validation with a message about a shape rather than
// about the flag, or — for a wildcard — produces a grant that looks scoped and is not.
func checkAccountID(account string) error {
	if account == "" {
		return errors.New("`billet init iam` needs --account for a codebuild node: the " +
			"project and its log group are addressed by ARN and node.codebuild carries no " +
			"account id.\n\n  billet init iam --account " +
			"$(aws sts get-caller-identity --query Account --output text)")
	}

	if len(account) != 12 {
		return fmt.Errorf("--account %q is not a 12-digit AWS account id", account)
	}

	for _, r := range account {
		if r < '0' || r > '9' {
			return fmt.Errorf("--account %q is not a 12-digit AWS account id; it lands in an "+
				"IAM Resource, so a wildcard or an alias there would produce a grant that "+
				"looks scoped and is not", account)
		}
	}

	return nil
}

// codeBuildProjectARN builds the project ARN from what the config names.
func codeBuildProjectARN(cb *config.CodeBuildConfig, account string) string {
	return fmt.Sprintf("arn:%s:codebuild:%s:%s:project/%s",
		awsPartitionFor(cb.Region), cb.Region, account, cb.Project)
}

// codeBuildLogGroupARN builds the log group ARN, or empty when none is configured —
// in which case the log grant stays on "*", which is what CodeBuild's own default
// group requires.
func codeBuildLogGroupARN(cb *config.CodeBuildConfig, account string) string {
	// THE GROUP COMES FROM config.LogGroupName, WHICH THE LAUNCH PATH ALSO READS.
	//
	// Two things went wrong here and the shared derivation fixes both. An empty answer
	// used to widen the BUILD role's logs grant to "*" — arbitrary job code able to
	// create and write any group in the account. And deriving the default HERE while
	// the launch sent no override left the two disagreeing for an adopted project with
	// a custom group: the policy named /aws/codebuild/<project> and the build wrote
	// somewhere else, so the role could not write its own logs. billet now pins the
	// same derived group on every launch, so this ARN describes where the build
	// actually writes.
	return fmt.Sprintf("arn:%s:logs:%s:%s:log-group:%s",
		awsPartitionFor(cb.Region), cb.Region, account, cb.LogGroupName())
}

// codeBuildKeyARN resolves the KMS key ARN the policy is scoped to.
//
// AN ALIAS OR A BARE ID IS NEVER GUESSED AT. IAM resource scoping needs an ARN, and
// assembling one from an alias would produce a grant that names a key AWS resolves
// differently — a policy that looks scoped and is not. --kms-key-arn is how an
// operator supplies the exact ARN, the same shape the ec2 path takes.
//
// A BARE ID WITH NO FLAG IS NOW REFUSED, and the first version silently emitted no
// KMS statement at all. That was the worse half of the bug: this function's own
// comment named --kms-key-arn as the remedy while nothing passed the flag through,
// so an operator who followed the advice got the identical policy back and a build
// that could not decrypt its registration — a diagnostic prescribing a command that
// does nothing, which is the failure ADR-005 names.
//
// AND THE FLAG WITHOUT A CONFIGURED KEY IS REFUSED TOO, mirroring the ec2 path: a
// deployment whose registrations use the account's aws/ssm key needs no grant, so a
// key named here would widen the policy past anything the config asks for.
func codeBuildKeyARN(cb *config.CodeBuildConfig, flag string) (string, error) {
	switch {
	case cb.JITKMSKeyID == "" && flag != "":
		return "", fmt.Errorf("--kms-key-arn was given but node.codebuild.jit_kms_key_id is " +
			"not set, so this deployment stages its registrations under the account's aws/ssm " +
			"key and needs no KMS grant; naming a key here would widen the policy past what " +
			"the config asks for")

	case cb.JITKMSKeyID == "":
		return "", nil

	case flag != "":
		if !strings.HasPrefix(flag, "arn:") {
			return "", fmt.Errorf("--kms-key-arn %q is not a full ARN; IAM scopes on ARNs, and "+
				"an id or alias here would produce a policy that looks scoped and is not", flag)
		}

		return flag, nil

	case strings.HasPrefix(cb.JITKMSKeyID, "arn:"):
		return cb.JITKMSKeyID, nil
	}

	// THE REMEDY DOES NOT REBUILD THIS COMMAND'S OWN INVOCATION, and two rounds of
	// trying to were both wrong in the same way.
	//
	// The first version printed `billet init iam --kms-key-arn …` and omitted the
	// --account this command requires, so an operator who copied it met a second
	// refusal. Adding --account fixed that one flag and left the rest: the
	// reconstruction still dropped --build-role and a non-default --config, so
	// following it after `billet init iam --build-role` printed THE NODE'S POLICY —
	// which carries StartBuild, StopBuild and DeleteParameter, the three things that
	// must never reach a role a workflow runs as. A diagnostic that hands somebody a
	// privilege escalation is worse than one that hands them a failing command.
	//
	// So it prints the one thing billet actually knows how to derive — the command
	// that resolves the key ARN — and tells the operator to add the flag to the
	// invocation they already have. billet cannot see their argv; they can.
	//
	// AND THE KEY IS SHELL-QUOTED, because it lands inside a command a person will
	// paste. config refuses a wildcard and whitespace in this value but not `$(`,
	// backticks, `;` or an apostrophe, so an unquoted rendering turns a config file into
	// executable syntax on somebody's terminal. That the config is the operator's own
	// file makes it a thin threat and not a reason to interpolate unquoted: the quoting
	// costs nothing, and "the input was trusted" is the sentence in front of most of
	// these.
	//
	// THE REGION IS NAMED, and leaving it out was a way to hand back the WRONG KEY. A
	// KMS alias is regional: an operator whose CLI default region differs from
	// node.codebuild.region either gets a command that fails, or — worse — resolves a
	// same-named alias in another region to a DIFFERENT key, after which the policy
	// applies cleanly and no build can decrypt its registration. A supplied ARN in the
	// wrong region is already refused, by awspolicy's requireARN, which compares the
	// ARN's region against the CodeBuild one; nothing was checking the command that
	// produces it.
	return "", fmt.Errorf("node.codebuild.jit_kms_key_id is %q, which is an id or alias rather "+
		"than an ARN, and IAM scopes on ARNs — an alias resolves to whichever key it points at "+
		"today and in whichever region it is asked, so a policy assembled from one looks "+
		"scoped and is not. Resolve the exact ARN:\n\n  aws kms describe-key --region %s "+
		"--key-id %s --query KeyMetadata.Arn --output text\n\nthen add --kms-key-arn <that "+
		"arn> to the `billet init iam` command you just ran",
		cb.JITKMSKeyID, shellSingleQuote(cb.Region), shellSingleQuote(cb.JITKMSKeyID))
}

// shellSingleQuote renders a value for a command a person will paste.
//
// SINGLE QUOTES, WITH THE ONE ESCAPE THAT EXISTS. Inside single quotes a POSIX shell
// expands nothing at all — no `$(`, no backtick, no `;` — and the only character that
// cannot appear is the quote itself, which is closed, escaped and reopened. billet has
// this rule already in the codebuild buildspec builder; a diagnostic is not a lesser
// place for it, because the buildspec runs on a machine that is about to be destroyed
// and this runs on an operator's own terminal.
func shellSingleQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// awsPartitionFor derives the partition from a region, the same rule the provider's
// endpoint derivation follows: the commercial suffix is wrong for China, and a
// GovCloud region uses the commercial partition's own naming.
func awsPartitionFor(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	default:
		return "aws"
	}
}
