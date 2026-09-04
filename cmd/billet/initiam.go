package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/junioryono/billet/internal/awspolicy"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deploymentid"
	"github.com/junioryono/billet/internal/state"
)

// cmdInitIAM prints the IAM policy an ec2 node's role needs, derived from the
// config so the grant matches the deployment exactly.
//
// WHY DERIVE IT RATHER THAN ASK. The permissions a node needs are decided by what
// its config declares — a compute-only node needs the runtime set, a cache node
// adds EBS and S3, a spot node adds its queue, an instance profile adds PassRole.
// A hand-written policy drifts from the config the day either changes; this reads
// the config and emits precisely what that config will exercise, scoped to the
// resources it names. `--builder` adds what `billet ami build` needs beyond a
// node's runtime — snapshotting an image, tagging it at create, reading the
// verifier's console, and stamping the contract a verified image proved.
//
// TWO IDENTIFIERS THE CONFIG HOLDS ARE NOT ARNS, and IAM scoping needs ARNs:
// node.ec2.instance_profile is an instance-profile NAME (the role it contains may
// be named differently), and node.ebs_s3.kms_key_id may be a bare key id or an
// alias. So --role-arn and --kms-key-arn supply those exact ARNs; without them a
// policy would be scoped to the wrong thing or to everything.
func cmdInitIAM(_ context.Context, args []string) error {
	fs := newFlagSet("billet init iam")
	cfgPath := addConfigFlag(fs)
	builder := fs.Bool("builder", false,
		"also grant what `billet ami build` needs: ec2:CreateImage, tagging the image it "+
			"creates, ec2:GetConsoleOutput to read the verification's report, and the "+
			"ec2:CreateTags that stamps a verified image's contract")
	roleARN := fs.String("role-arn", "",
		"the IAM role ARN the instance_profile contains, for iam:PassRole scoping")
	kmsKeyARN := fs.String("kms-key-arn", "",
		"the full KMS key ARN, when the configured key is a bare id or alias — "+
			"node.ebs_s3.kms_key_id for an ec2 node, node.codebuild.jit_kms_key_id for a "+
			"codebuild one")
	deployment := fs.String("deployment", "",
		"this deployment's identity, scoping the policy to only its own resources "+
			"(defaults to the id in the config's state directory)")
	accountWide := fs.Bool("account-wide", false,
		"scope by tag presence instead of the deployment id — only safe when this is "+
			"the single billet deployment in the account")
	account := fs.String("account", "",
		"the 12-digit AWS account id the project and its log group live in (codebuild "+
			"only) — `aws sts get-caller-identity --query Account --output text`")
	buildRole := fs.Bool("build-role", false,
		"print the policy for the BUILD's own service role instead of the node's "+
			"(codebuild only) — the role a build runs AS, so every permission in it is "+
			"one the workflow holds")
	controllerSweep := fs.Bool("controller-sweep", false,
		"print the policy the CONTROL PLANE's role needs to sweep staged runner "+
			"registrations a dead node left under this node's parameter path (codebuild "+
			"only) — list and delete under the path, nothing else")

	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", *cfgPath, err)
	}

	if cfg.Node == nil {
		return errors.New("`billet init iam` describes a node's AWS role and this config " +
			"declares no `node:` block at all, so there is no backend to describe one for")
	}

	// THE REFUSAL NAMES WHAT DOES APPLY, because the gate is no longer about one
	// backend: it asks whether the node's compute runs on this machine, and an
	// operator who reaches it needs to know which providers it is asking about rather
	// than being told what theirs is not.
	if cfg.Node.Provider.RunsOnHost() {
		return fmt.Errorf("`billet init iam` describes a REMOTE node's role, but node.provider "+
			"is %s, which runs work on this host — an IAM policy is only meaningful for a "+
			"backend that calls an AWS API, which means %s or %s",
			cfg.Node.Provider, config.ProviderEC2, config.ProviderCodeBuild)
	}

	// A CODEBUILD NODE IS A DIFFERENT PRINCIPAL WITH A DIFFERENT BOUNDARY, so it gets
	// its own path rather than a flag on the ec2 one. The ec2 statements are scoped by
	// an owner TAG on each instance; a CodeBuild build cannot be tagged at all, so the
	// boundary is the project ARN plus the parameter path — and the deployment
	// identity, which every ec2 condition turns on, appears in none of it.
	if cfg.Node.Provider == config.ProviderCodeBuild {
		if *controllerSweep {
			return printCodeBuildSweepIAM(cfg, *buildRole, *kmsKeyARN, *account)
		}

		return printCodeBuildIAM(cfg, *buildRole, *kmsKeyARN, *account)
	}

	if *controllerSweep {
		return fmt.Errorf("--controller-sweep describes the control plane's grant over a codebuild "+
			"node's parameter path, and node.provider is %s", cfg.Node.Provider)
	}

	if cfg.Node.EC2 == nil {
		return errors.New("`billet init iam` needs node.ec2 for an ec2 node")
	}

	owner, err := resolveDeploymentOwner(cfg, *deployment, *accountWide)
	if err != nil {
		return err
	}

	in, err := iamInputsFromConfig(cfg, *builder, *roleARN, *kmsKeyARN)
	if err != nil {
		return err
	}

	in.Owner = owner

	policy, err := in.Build()
	if err != nil {
		return err
	}

	body, err := policy.JSON()
	if err != nil {
		return fmt.Errorf("render the policy: %w", err)
	}

	fmt.Println(string(body))

	return nil
}

// resolveDeploymentOwner determines the deployment identity the policy is scoped
// to, so it isolates this deployment from any other billet deployment in the
// account. Precedence: --deployment with --account-wide is refused as
// contradictory; --account-wide then wins (explicit opt-out to the per-account
// boundary, so it overrides an id that happens to be on disk); then an explicit
// --deployment; then the id already in the config's state directory (a control
// plane mints it on first run, a node adopts it at enrollment). With none of
// those the command refuses rather than silently generating a per-account policy.
func resolveDeploymentOwner(cfg *config.Config, flag string, accountWide bool) (string, error) {
	// The two explicit choices conflict: --deployment scopes to a value, --account-wide
	// to tag presence. Refuse rather than silently pick one.
	if flag != "" && accountWide {
		return "", errors.New("--deployment and --account-wide are contradictory: one scopes the " +
			"policy to a deployment value, the other to tag presence — pass only one")
	}

	// --account-wide is an explicit opt-out to the per-account boundary, so it wins
	// over an id that happens to be on disk; otherwise the flag would do nothing on
	// an enrolled host.
	if accountWide {
		return "", nil
	}

	if flag != "" {
		if err := deploymentid.Validate(flag); err != nil {
			return "", fmt.Errorf("--deployment: %w", err)
		}

		return flag, nil
	}

	for _, dir := range deploymentStateDirs(cfg) {
		id, found, err := state.PeekDeploymentID(dir)
		if err != nil {
			return "", err
		}
		if found {
			return id, nil
		}
	}

	return "", errors.New("could not determine this deployment's identity: it is not in the " +
		"config's state directory yet and no --deployment was given. Run `billet server` once to " +
		"mint it (or `billet ca issue`/enroll a node), then re-run; or pass --deployment <id>. To " +
		"scope the policy by tag presence instead — only safe if this is the single billet " +
		"deployment in the account — pass --account-wide")
}

// deploymentStateDirs are the directories that may hold this deployment's id,
// most authoritative first: the control plane mints it under server.state_dir, a
// node adopts it under node.state_dir.
func deploymentStateDirs(cfg *config.Config) []string {
	var dirs []string
	if cfg.Server != nil && cfg.Server.IdentityDir != "" {
		dirs = append(dirs, cfg.Server.IdentityDir)
	}
	if cfg.Node != nil && cfg.Node.StateDir != "" {
		dirs = append(dirs, cfg.Node.StateDir)
	}

	return dirs
}

// iamInputsFromConfig reads the deployment's shape out of the loaded config,
// resolving the two identifiers that must be ARNs from their flags.
func iamInputsFromConfig(cfg *config.Config, builder bool, roleARN, kmsKeyARN string) (awspolicy.Inputs, error) {
	ec2cfg := cfg.Node.EC2

	in := awspolicy.Inputs{
		Partition: awspolicy.PartitionForRegion(ec2cfg.Region),
		Region:    ec2cfg.Region,
		Builder:   builder,
	}

	// PassRole is needed only when the config names an instance profile, and it
	// must be scoped to the ROLE that profile contains — which the config does not
	// hold. The name is not the ARN, so --role-arn is required rather than guessed.
	switch {
	case ec2cfg.InstanceProfile != "":
		if roleARN == "" {
			return awspolicy.Inputs{}, fmt.Errorf("this config names node.ec2.instance_profile %q, "+
				"which is an instance-profile name, not the IAM role ARN iam:PassRole must be scoped "+
				"to; pass --role-arn arn:aws:iam::<account>:role/<role> (the role that instance "+
				"profile contains)", ec2cfg.InstanceProfile)
		}

		in.InstanceProfileRoleARN = roleARN
	case roleARN != "":
		return awspolicy.Inputs{}, errors.New("--role-arn is only meaningful when the config names " +
			"node.ec2.instance_profile; this config does not, so no iam:PassRole is needed")
	}

	if cfg.Node.EBSS3 == nil && kmsKeyARN != "" {
		return awspolicy.Inputs{}, errors.New("--kms-key-arn is only meaningful when the config " +
			"names node.ebs_s3 (the cache that uses a KMS-encrypted volume); this config has no " +
			"cache, so no KMS statement is needed")
	}

	if cfg.Node.EBSS3 != nil {
		key := kmsKeyARN
		if key == "" {
			key = cfg.Node.EBSS3.KMSKeyID
		}

		if key != "" && !strings.HasPrefix(key, "arn:") {
			return awspolicy.Inputs{}, fmt.Errorf("node.ebs_s3.kms_key_id is %q, which is not a key "+
				"ARN; IAM resource scoping needs the full ARN, so pass --kms-key-arn "+
				"arn:aws:kms:<region>:<account>:key/<id> (the EBS config may keep the short form)", key)
		}

		in.Cache = &awspolicy.Cache{
			Bucket:    cfg.Node.EBSS3.Bucket,
			Prefix:    cfg.Node.EBSS3.Prefix,
			KMSKeyARN: key,
		}
	}

	if ec2cfg.Spot {
		arn, err := sqsQueueARN(ec2cfg.InterruptionQueueURL, in.Partition, ec2cfg.Region)
		if err != nil {
			return awspolicy.Inputs{}, err
		}

		in.SpotQueueARN = arn
	}

	// THE IDENTITY STORE, WHEN THIS DEPLOYMENT USES ONE.
	//
	// A CONTROL-PLANE GRANT, and it appears here because the recommended AWS
	// topology gives the controller the fleet's instance profile — so one role
	// carries both. A deployment whose controller has a role of its own gets a
	// policy with this and nothing else, exactly as `backup` already does.
	if ssm := cfg.Server.IdentitySSM(); ssm != nil {
		key := ssm.KMSKeyID
		if key != "" && !strings.HasPrefix(key, "arn:") {
			return awspolicy.Inputs{}, fmt.Errorf(
				"server.identity.aws_ssm.kms_key_id is %q, which is not a key ARN; IAM resource "+
					"scoping needs the full ARN, so pass --kms-key-arn "+
					"arn:aws:kms:<region>:<account>:key/<id> (the identity config may keep the "+
					"short form)", key)
		}

		in.Identity = &awspolicy.Identity{Prefix: ssm.Prefix, KMSKeyARN: key}

		// THE REGION FOR THE kms:ViaService CONDITION comes from the identity block
		// rather than from the compute one: a deployment whose controller and fleet
		// are in different regions would otherwise confine the key to a service
		// endpoint nothing calls.
		if key != "" && in.Region == "" {
			in.Region = ssm.Region
		}
	}

	return in, nil
}

// awsAccountID matches a 12-digit AWS account id; sqsQueueName matches the SQS
// queue-name grammar (alphanumerics, hyphen and underscore, up to 80 chars, with
// an optional .fifo suffix). Both guard against a malformed URL producing a
// wrong-but-plausible ARN.
var (
	awsAccountID = regexp.MustCompile(`^\d{12}$`)
	sqsQueueName = regexp.MustCompile(`^(?:[A-Za-z0-9_-]{1,80}|[A-Za-z0-9_-]{1,75}\.fifo)$`)
)

// sqsQueueARN turns a validated SQS queue URL into its ARN, scoped to that one
// queue rather than every queue in the account.
//
// THE REGION COMES FROM THE CONFIG, NOT THE HOST. config already proved the queue
// is in node.ec2.region AND in that region's partition, and within that the host
// still varies — sqs.<region>.<suffix>, a legacy <region>.queue.<suffix>, or a VPC
// endpoint under .sqs.<region>.vpce.<suffix> — so only the ACCOUNT and NAME are read
// from the URL path, which is the same across all of them, and both are checked
// against their grammars.
//
// THE PARTITION HERE AND THE SUFFIX THERE COME FROM THE SAME REGION, which is the
// point rather than a coincidence: this renders arn:aws-cn:sqs:... for a cn- region,
// and a validator that admitted the commercial host for one would authorise a queue
// the node then failed to resolve.
func sqsQueueARN(queueURL, partition, region string) (string, error) {
	u, err := url.Parse(queueURL)
	if err != nil {
		return "", fmt.Errorf("parse the interruption queue URL: %w", err)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("interruption queue URL path %q is not /<account>/<name>", u.Path)
	}

	account, name := parts[0], parts[1]
	if !awsAccountID.MatchString(account) {
		return "", fmt.Errorf("interruption queue URL account %q is not a 12-digit AWS account id", account)
	}
	if !sqsQueueName.MatchString(name) {
		return "", fmt.Errorf("interruption queue URL name %q is not a valid SQS queue name", name)
	}

	return fmt.Sprintf("arn:%s:sqs:%s:%s:%s", partition, region, account, name), nil
}
