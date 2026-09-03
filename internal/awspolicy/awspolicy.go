// Package awspolicy assembles the least-privilege IAM policy an ec2 billet node
// (or the AMI builder) needs, from the action constants each owning package
// declares.
//
// ONE GENERATOR, THREE CONSUMERS. `billet init iam` prints the policy for a
// deployment, a drift test pins the generator's output to a committed rendering,
// and the Terraform module consumes that same committed rendering. If the actions
// billet's code performs and the permissions its policy grants ever disagreed, a
// node would fail at runtime with a permission error that no config check could
// have caught — so the actions live beside the code that performs them
// (internal/provider/ec2, internal/store/ebss3) and this package is the only place
// that unions them into statements.
//
// THE OWNERSHIP BOUNDARY IS PER DEPLOYMENT WHEN THE OWNER IS KNOWN. Every
// destructive action is conditioned on billet's owner tag: on its exact VALUE
// (the deployment identity) when Inputs.Owner is set, or on mere PRESENCE when it
// is not. `billet init iam` reads the deployment id from the state directory and
// always sets it, so the policy it generates isolates ONE deployment from any
// other billet deployment sharing the AWS account — deployment A's role cannot
// terminate or delete deployment B's resources, because B's carry a different
// value. Presence remains the fallback for a caller that has no id yet, and is a
// sound boundary only when a single billet deployment runs in the account.
//
// The static value is safe because the id is stable and knowable at generation
// time; it needs no principal-tag on the role and so has no forgotten-tag footgun.
// Value-conditioning holds because a runtime role's CreateTags is restricted to
// create-time tagging (ec2:CreateAction) and a node never calls CreateTags on its
// own, so it can stamp an owner tag only on resources it is creating — and
// RunInstances also requires the request to carry the owner value where it can.
// The BUILDER's extra standalone CreateTags does not weaken that: it is scoped to
// image resources that already carry the per-build owner tag, so it can add a tag
// only to an AMI this builder made. Creates require the
// owner (by value) in the request; deletes, attach/detach and terminate require it
// on the resource; CreateSnapshot additionally requires the SOURCE volume to carry
// it, so a foreign volume cannot be copied into a billet-tagged snapshot.
// RunInstances and the describes cannot be scoped this way (RunInstances makes
// several resource types at once; describes are not resource-scopable) and stay on
// "*".
//
// THE TAG BOUNDARY IS DESTRUCTIVE INTEGRITY; CONFIDENTIALITY NEEDS THE KEY. Tag
// conditions stop one deployment terminating, deleting or wedging another's
// resources. ec2:CreateVolume from a snapshot authorizes the PARENT SNAPSHOT as
// well as the volume it creates — this file used to say the opposite, and the
// first real clone under the policy was refused for it (see
// BilletCacheCloneSource) — so the source is scoped by its owner tag in a
// statement of its own, which stops a role cloning a snapshot that carries no
// billet owner tag. It does not stop a role cloning ANOTHER billet deployment's
// snapshot in account-wide mode, where the condition is tag presence rather than
// value, and a tag is not a secret in any mode. A PER-DEPLOYMENT KMS KEY is
// what closes that read boundary: with Cache.KMSKeyARN set, every volume and
// snapshot is encrypted under the deployment's own key, and the policy's KMS
// statements are scoped to exactly that key — creating a volume from a FOREIGN
// deployment's snapshot then fails at the KMS grant EBS needs, before any data
// moves. Measured with iam:SimulateCustomPolicy against the generated policy:
// every KMS action on the deployment's own key through EBS (kms:ViaService) is
// allowed; every KMS action on another deployment's key is implicitly denied; a
// DIRECT KMS call (no ViaService) is denied even on the deployment's own key;
// and a grant not destined for an AWS service is denied. The boundary therefore
// holds exactly when each deployment has its OWN key — the terraform module's
// enable_kms mints one per module instance — and silently reopens if two
// deployments share a key, which nothing at policy level can detect. The
// protection is also STRICTLY OPT-IN PER DEPLOYMENT: a deployment that sets no
// key encrypts under the ACCOUNT's default EBS key — the AWS-managed aws/ebs
// unless the account configured another — and aws/ebs authorizes ANY principal
// in the account through EC2 without a kms: statement, so its snapshots stay
// readable no matter what keys the other deployments use (a shared customer
// default key is no better: shared is not per-deployment). Setting a key
// protects the deployment that sets it, not its neighbours — and only the
// snapshots created AFTER it was set: earlier ones remain under the old key
// and stay readable until re-snapshotted or evicted. The simulation proves the
// IDENTITY-policy decisions; that is decisive for keys whose own key policy
// delegates to IAM (the terraform module's default-policy keys do), while a
// key whose policy or grants admit foreign roles reopens the boundary on the
// key side, which no identity policy can see.
//
// THE BUILDER IS SCOPED SEPARATELY. `billet ami build` tags its builder instance
// with a per-build owner (BuilderOwnerPrefix + name), NOT a deployment id, so the
// --builder statements match that prefix by StringLike and carry their own
// Terminate — the deployment-scoped runtime Terminate would not reach a builder.
//
// VALIDATED AGAINST A LIVE AWS ACCOUNT with iam:SimulateCustomPolicy in both
// modes: every action billet performs is allowed for its own tagged resources with
// the right context; every boundary denies the foreign or wrong-context case; and
// with a value condition a DIFFERENT deployment's owner-tagged instance is denied,
// where under presence it was allowed. Access Analyzer reports the document clean.
// Re-run the simulation after changing a condition; see the drift goldens.
//
// THE THREE GRANTS THE VERIFICATION ADDED ARE SIMULATED TOO, on 2026-08-29, both
// directions each. Tagging on CreateImage: allowed with the grant, implicitDeny
// without it, and RunInstances tagging still allowed either way, so the
// difference is that one CreateAction rather than a broken policy.
// ec2:GetConsoleOutput on an instance carrying the builder prefix: allowed, and
// implicitDeny for a foreign owner. The standalone ec2:CreateTags on an image:
// the same pair. Re-run all of them after changing a condition.
//
// The policy is CAPABILITY-SCOPED: a compute-only node gets the runtime
// statements alone, and each of cache, spot, a builder, an instance profile and a
// KMS key adds exactly the statements it needs, scoped to the resource it names.
package awspolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/archivestore"
	"github.com/junioryono/billet/internal/provider/codebuild"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/store/ebss3"
)

// Policy is an AWS IAM policy document.
type Policy struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

// Statement is one Allow rule. Condition is omitted when empty so an
// unconditioned statement does not render an empty object.
type Statement struct {
	Sid       string         `json:"Sid"`
	Effect    string         `json:"Effect"`
	Action    []string       `json:"Action"`
	Resource  []string       `json:"Resource"`
	Condition map[string]any `json:"Condition,omitempty"`
}

// Cache describes the cache storage a node's policy must permit.
type Cache struct {
	// Bucket holds the S3 pointer/lease/fencing state; Prefix isolates one
	// deployment inside it. Both name the S3 resources the statement scopes to,
	// and the prefix must be a literal — a `*` or `?` in it would widen the IAM
	// grant to sibling prefixes.
	Bucket string
	Prefix string
	// KMSKeyARN, when set, is the customer-managed key the cache's EBS volumes are
	// encrypted with. It must be a full key ARN, not an alias or bare id: IAM
	// resource scoping needs an ARN, and a bare `*` would grant every key.
	KMSKeyARN string
}

// Backup describes the archive store a control plane's policy must permit.
//
// THERE IS NO DELETE IN IT, AND THAT IS THE DESIGN RATHER THAN AN OVERSIGHT.
// billet never issues one — internal/archivestore has no delete at all — so
// granting the permission would leave the one host that holds the GitHub App
// private key and the node-wire CA able to destroy the off-site copies whose
// whole purpose is surviving the loss of that host. Retention belongs to the
// bucket: versioning and a lifecycle rule, which the Terraform module sets.
type Backup struct {
	// Bucket holds this deployment's archives; Prefix isolates it inside one.
	// Both land in an IAM Resource ARN, so both must be literal — a `*` widens
	// the grant to every sibling prefix, and every sibling prefix is another
	// deployment's App key.
	Bucket string
	Prefix string
	// KMSKeyARN, when set, is the customer-managed key the bucket encrypts with.
	// A full key ARN, not an alias or a bare id: IAM resource scoping needs one,
	// and a bare `*` would grant every key in the account.
	KMSKeyARN string
}

// Identity describes what a CONTROL PLANE needs to reach this deployment's
// identity material in Parameter Store.
//
// THE MOST SENSITIVE GRANT BILLET GENERATES, and the prefix is the whole of the
// boundary. What lives under it is the GitHub App private key — which can mint
// tokens for an entire organization and which GitHub issues exactly once — and
// the node-wire certificate authority, which decides who may connect to this
// control plane. A `*` in the prefix reaches every sibling, and every sibling is
// another deployment's.
//
// THERE IS NO DELETE, deliberately, and for the reason `backup` states one level
// over: the credential on a host that also holds these must not be able to
// destroy them. Removing a parameter is a console or CLI action an operator takes
// knowing what it is.
type Identity struct {
	// Prefix is the Parameter Store path this deployment's identity lives under.
	// It lands in an IAM Resource ARN, so it must be literal.
	Prefix string
	// KMSKeyARN, when set, is the customer-managed key the SecureStrings are
	// encrypted with. A full key ARN, not an alias or a bare id.
	KMSKeyARN string
}

// CodeBuild describes what a codebuild NODE's role must permit.
//
// THE PROJECT IS THE OWNERSHIP BOUNDARY HERE, and that is forced rather than
// chosen: a CodeBuild build cannot be tagged — tags exist on projects and report
// groups, and StartBuild has no field that becomes one — so the
// `aws:ResourceTag/sh.billet.owner` condition every ec2 statement carries has no
// equivalent. What replaces it is scoping the build actions to exactly one project
// ARN, which is why that project must be billet's alone: StartBuild and StopBuild
// on a shared project is a way for billet to stop somebody else's build.
type CodeBuild struct {
	// ProjectARN is the one project this node may start and stop builds in. A full
	// ARN and a literal — a `*` in it would widen the grant to every project in the
	// account, which is the boundary this is.
	ProjectARN string
	// FleetARN, when set, is the reserved-capacity fleet the node describes for
	// `billet check`. Read-only; nothing here creates or changes a fleet.
	FleetARN string
	// ParameterPath is the Parameter Store path prefix the single-use runner
	// registration is written under, without a trailing slash. A literal, for the
	// reason a cache prefix is: it lands in an IAM Resource ARN, and on a shared
	// account the sibling paths a wildcard admits are other deployments'
	// registrations.
	ParameterPath string
	// KMSKeyARN, when set, is the customer-managed key those SecureString
	// parameters are encrypted with. A full key ARN, not an alias or bare id.
	KMSKeyARN string
}

// CodeBuildRole describes what the BUILD's own service role must permit.
//
// IT STARTS NOTHING AND DELETES NOTHING. This role runs inside the compute that
// executes a workflow, so every permission it holds is a permission that workflow
// holds: it reads the one parameter carrying its own registration, and writes its
// own logs. `ssm:DeleteParameter` is deliberately absent — cleanup is the node's job
// — and so is every codebuild action, because a role that could start a build, from
// inside a build, is how a job launches runners billet never escrowed capacity for.
type CodeBuildRole struct {
	// ParameterPath is the same prefix the node writes under, so the build can read
	// the registration staged for it and nothing else.
	ParameterPath string
	// KMSKeyARN, when set, is the key it needs kms:Decrypt on.
	KMSKeyARN string
	// LogGroupARN, when set, scopes the log grant to one group. Empty leaves logs
	// on "*", which is what CodeBuild's own default log group requires.
	LogGroupARN string
}

// CodeBuildSweep describes what the CONTROL PLANE must permit in order to remove
// the staged runner registrations a dead codebuild node never reaped.
//
// A THIRD PRINCIPAL BESIDE THE NODE AND THE BUILD, and the narrowest. It lists the
// names under one path and deletes the ones the LEDGER has proved dead: no
// GetParameter (it never reads a registration), no PutParameter (it stages none),
// no KMS (a listing that asks for no decryption calls no key), and nothing from
// codebuild at all. It is rendered separately so that reads at a glance.
//
// THE MISSING KMS ACTION DOES NOT, ON ITS OWN, KEEP A REGISTRATION OUT OF THIS
// PRINCIPAL. Measured under a real role holding exactly this statement: a listing
// that asked for decryption received plaintext, because the account's aws/ssm key
// authorises any principal reaching it through Parameter Store. What keeps the
// value out is billet's request never asking (codebuild.RegistrationSweeper), and a
// customer-managed key is what makes this grant decisive as well — measured: under
// one, the same role's decrypting listing was refused kms:Decrypt while its delete
// still succeeded.
type CodeBuildSweep struct {
	// ParameterPath is the prefix the node stages registrations under, without a
	// trailing slash. A literal, because it lands in an IAM Resource ARN.
	ParameterPath string
	// Account is the AWS account the parameters live in. Required because a
	// parameter ARN names one and this rendering has no other ARN to take it from.
	Account string
}

// Inputs is the deployment a policy is built for. The zero value yields the
// runtime-only policy a compute-only node needs.
type Inputs struct {
	// Owner is the deployment identity (`billet init iam` reads it from the state
	// directory). When set, every ownership condition matches this exact value, so
	// the policy isolates ONE deployment from any other billet deployment sharing
	// the account. When empty the conditions fall back to tag PRESENCE, a boundary
	// sound only for a single billet deployment per account.
	Owner string
	// Partition is the AWS partition ("aws", "aws-cn", "aws-us-gov") the ARNs are
	// built in. Empty defaults to "aws".
	Partition string
	// DNSSuffix overrides the endpoint DNS suffix used in the kms:ViaService and
	// iam:PassRole service conditions. Empty derives it from Partition (amazonaws.com,
	// or amazonaws.com.cn for aws-cn). It exists so the Terraform module's committed
	// rendering can carry a substitutable sentinel independent of the region sentinel
	// — a real deployment never sets it and takes the partition's own suffix.
	DNSSuffix string
	// Region scopes the KMS kms:ViaService condition to this region's EC2 service.
	// Required when Cache.KMSKeyARN is set.
	Region string
	// Cache, when non-nil, adds the sticky-disk cache statements.
	Cache *Cache
	// CodeBuild, when non-nil, adds the statements a codebuild NODE needs.
	//
	// A DIFFERENT PRINCIPAL FROM THE BUILD ITSELF, and that split is the whole
	// reason there are two structs. This one starts and stops builds and stages the
	// single-use runner registration; CodeBuildRole below is what runs INSIDE the
	// compute that executes somebody's job, and it may only read one parameter and
	// write logs. One role carrying both is the NoCompute mistake wearing different
	// clothes — a role that could start builds, running inside a build, is a way for
	// a job to launch runners billet never escrowed capacity for.
	CodeBuild *CodeBuild
	// CodeBuildRole, when non-nil, renders the BUILD's own service role. It
	// describes a different principal from everything else here.
	CodeBuildRole *CodeBuildRole
	// CodeBuildSweep, when non-nil, adds the statement the CONTROL PLANE needs to
	// sweep staged registrations under one path. A third principal again: it is
	// what `billet server` runs as, never the node and never the build.
	CodeBuildSweep *CodeBuildSweep
	// Backup, when non-nil, adds the archive-store statements the CONTROL PLANE
	// needs. On the recommended AWS topology the root module passes fleet-ec2's
	// instance profile to the co-located controller, so one role carries both;
	// a standalone controller gets a policy with this and nothing else.
	Backup *Backup
	// Identity, when non-nil, adds the statements a CONTROL PLANE needs to read
	// and publish this deployment's identity material. Only a controller ever gets
	// these: a node holds no App key and no certificate authority.
	Identity *Identity
	// SpotQueueARN, when set, adds the interruption-queue statement scoped to it.
	SpotQueueARN string
	// InstanceProfileRoleARN, when set, adds an iam:PassRole scoped to that role.
	// It must be the ROLE ARN, not an instance-profile name.
	InstanceProfileRoleARN string
	// Builder adds the ec2:CreateImage the AMI builder needs.
	Builder bool
	// NoCompute omits the runtime statements entirely, for a principal that
	// launches nothing.
	//
	// A CONTROL PLANE IS SUCH A PRINCIPAL, and this exists because the first
	// version of the backup policy did not have it: a standalone controller that
	// only puts archives in a bucket came out holding ec2:RunInstances and
	// ec2:TerminateInstances, on the one host in the deployment that also holds
	// the GitHub App private key. The zero value keeps meaning "the runtime-only
	// node policy", so nothing that does not ask for this changes.
	NoCompute bool
}

const policyVersion = "2012-10-17"

// PartitionForRegion returns the AWS partition an ARN in region belongs to. The
// commercial partition is "aws"; China and GovCloud are separate partitions whose
// ARNs would be malformed under "aws".
func PartitionForRegion(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	default:
		return "aws"
	}
}

// dnsSuffix is the endpoint DNS suffix for a partition, which the kms:ViaService
// and iam:PassedToService service names are built from. China's is
// amazonaws.com.cn, and a policy that hard-coded amazonaws.com would DENY a China
// node's KMS and PassRole operations.
func dnsSuffix(partition string) string {
	if partition == "aws-cn" {
		return "amazonaws.com.cn"
	}

	return "amazonaws.com"
}

// Build assembles the policy for these inputs, or reports why it cannot.
func (in Inputs) Build() (Policy, error) {
	partition := in.Partition
	if partition == "" {
		partition = "aws"
	}

	suffix := in.DNSSuffix
	if suffix == "" {
		suffix = dnsSuffix(partition)
	}

	if in.InstanceProfileRoleARN != "" {
		if err := requireARN(in.InstanceProfileRoleARN, "iam", "role/", partition, ""); err != nil {
			return Policy{}, fmt.Errorf("instance profile role: %w", err)
		}
	}
	if in.SpotQueueARN != "" {
		// in.Region, when set, must match: a queue ARN in the wrong region grants a
		// permission the node's ReceiveMessage never uses, and denies the one it does.
		if err := requireARN(in.SpotQueueARN, "sqs", "", partition, in.Region); err != nil {
			return Policy{}, fmt.Errorf("spot queue: %w", err)
		}
	}

	p := Policy{Version: policyVersion}

	ownerResource := ownerTagCondition("aws:ResourceTag/"+ec2.OwnerTagKey, in.Owner)

	if !in.NoCompute {
		p.Statement = append(p.Statement,
			// The describes cannot be resource-scoped.
			Statement{
				Sid: "BilletRuntimeRead", Effect: "Allow",
				Action: ec2.RuntimeDescribeIAMActions(), Resource: []string{"*"},
			},
			// RunInstances stays on "*" — it creates several resource types at once, and
			// the instances it makes are tagged, which the teardown below relies on.
			Statement{
				Sid: "BilletRuntimeLaunch", Effect: "Allow",
				Action: ec2.RuntimeLaunchIAMActions(), Resource: []string{"*"},
			},
			// Tagging only as part of a create, AND — in per-deployment mode — only with
			// this deployment's own owner value. Without the value condition a role could
			// RunInstances tagging the instance with ANOTHER deployment's owner id, which
			// lands forged compute in that deployment's owner-filtered inventory and
			// wedges its reconciliation. Requiring the request tag to equal this
			// deployment (or the builder prefix when --builder, since the builder tags its
			// own instances) fails such a launch at CreateTags, which fails the whole
			// RunInstances. billet always sets the owner tag on the instance and volume it
			// creates, so its own launches keep working.
			Statement{
				Sid: "BilletRuntimeTag", Effect: "Allow",
				Action: ec2.RuntimeTagIAMActions(), Resource: []string{"*"},
				Condition: createTagCondition(in.Owner, in.Builder),
			},
			// Terminate only what carries billet's owner tag.
			Statement{
				Sid: "BilletRuntimeTerminate", Effect: "Allow",
				Action: ec2.RuntimeTerminateIAMActions(), Resource: []string{"*"},
				Condition: ownerResource,
			},
		)
	}

	if in.Cache != nil {
		stmts, err := cacheStatements(partition, suffix, in.Region, in.Owner, *in.Cache)
		if err != nil {
			return Policy{}, err
		}

		p.Statement = append(p.Statement, stmts...)
	}

	if in.CodeBuild != nil {
		stmts, err := codeBuildStatements(partition, suffix, in.Region, *in.CodeBuild)
		if err != nil {
			return Policy{}, err
		}

		p.Statement = append(p.Statement, stmts...)
	}

	if in.CodeBuildRole != nil {
		stmts, err := codeBuildRoleStatements(partition, suffix, in.Region, *in.CodeBuildRole)
		if err != nil {
			return Policy{}, err
		}

		p.Statement = append(p.Statement, stmts...)
	}

	if in.CodeBuildSweep != nil {
		stmt, err := codeBuildSweepStatement(partition, in.Region, *in.CodeBuildSweep)
		if err != nil {
			return Policy{}, err
		}

		p.Statement = append(p.Statement, stmt)
	}

	if in.Identity != nil {
		stmts, err := identityStatements(partition, suffix, in.Region, *in.Identity)
		if err != nil {
			return Policy{}, err
		}

		p.Statement = append(p.Statement, stmts...)
	}

	if in.Backup != nil {
		stmts, err := backupStatements(partition, suffix, in.Region, *in.Backup)
		if err != nil {
			return Policy{}, err
		}

		p.Statement = append(p.Statement, stmts...)
	}

	if in.SpotQueueARN != "" {
		p.Statement = append(p.Statement, Statement{
			Sid: "BilletSpotInterruptions", Effect: "Allow",
			Action: ec2.SpotIAMActions(), Resource: []string{in.SpotQueueARN},
		})
	}

	if in.Builder {
		// The builder is scoped to its OWN per-build owner prefix, not the
		// deployment id — `billet ami build` tags the builder with billet-ami-build-*,
		// a distinct identity. It gets its own Terminate for cleanup, because the
		// runtime Terminate above is scoped to the deployment and would not match the
		// builder once the policy is per-deployment.
		//
		// CreateImage authorizes MULTIPLE resource types — the source instance, the
		// new image, and (for an EBS-backed builder) the snapshots it creates — so it
		// needs the same split as CreateSnapshot: a single conditioned statement on
		// "*" would apply the condition to the not-yet-existing image, which has no
		// tags, and deny the whole call. The source instance carries the builder tag
		// (the gate); the new image and snapshots are untagged at create.
		p.Statement = append(p.Statement,
			Statement{
				Sid: "BilletAMIBuilderSource", Effect: "Allow",
				Action:    ec2.BuilderIAMActions(),
				Resource:  []string{"arn:" + partition + ":ec2:*:*:instance/*"},
				Condition: builderOwnerCondition(),
			},
			Statement{
				Sid: "BilletAMIBuilderImage", Effect: "Allow",
				Action: ec2.BuilderIAMActions(),
				// An AMI ARN has an empty account field; the backing snapshots do not.
				Resource: []string{
					"arn:" + partition + ":ec2:*::image/*",
					"arn:" + partition + ":ec2:*:*:snapshot/*",
				},
			},
			Statement{
				Sid: "BilletAMIBuilderTerminate", Effect: "Allow",
				Action:    ec2.RuntimeTerminateIAMActions(),
				Resource:  []string{"arn:" + partition + ":ec2:*:*:instance/*"},
				Condition: builderOwnerCondition(),
			},
			// A build boots the image it produced and reads the verifier's report off
			// the serial console. Scoped to instances carrying the builder tag: console
			// output is whatever the machine printed, and on a job instance that is a
			// runner's own log rather than anything this role has business reading.
			//
			// AND THAT SCOPING WORKS, WHICH IS WORTH RECORDING BECAUSE A REVIEW SAID IT
			// DOES NOT. The claim was that GetConsoleOutput supports no resource type,
			// so an instance ARN with a tag condition grants nothing and every build
			// under this policy would be denied. It is wrong, and IAM says so:
			// simulated 2026-08-29, this statement answers ALLOWED for an instance
			// tagged billet-ami-build-* and implicitDeny for one owned by anybody else.
			// An unscopable action would have denied both.
			//
			// It also matches AWS's own practice — the service authorization data
			// lists GetConsoleOutput against an instance resource with the
			// ec2:ResourceTag key, and AWSApplicationMigrationService's service-linked
			// role grants it on `arn:aws:ec2:*:*:instance/*` under an aws:ResourceTag
			// condition, as does Elastic Disaster Recovery's console policy.
			//
			// The suggested fix was to widen this to "*", which would hand the role
			// every instance's console in the account. Worth the measurement.
			Statement{
				Sid: "BilletAMIBuilderConsole", Effect: "Allow",
				Action:    ec2.BuilderVerifyIAMActions(),
				Resource:  []string{"arn:" + partition + ":ec2:*:*:instance/*"},
				Condition: builderOwnerCondition(),
			},
			// And the contract promotion, which is the one standalone CreateTags billet
			// makes. It is conditioned on the RESOURCE tag rather than the request tag —
			// the AMI already carries the per-build owner from CreateImage, so this
			// grant reaches only images this builder made, and nothing it did not.
			Statement{
				Sid: "BilletAMIBuilderPromote", Effect: "Allow",
				Action: ec2.BuilderPromoteIAMActions(),
				// An AMI ARN has an empty account field, as above.
				Resource:  []string{"arn:" + partition + ":ec2:*::image/*"},
				Condition: builderOwnerCondition(),
			},
		)
	}

	if in.InstanceProfileRoleARN != "" {
		p.Statement = append(p.Statement, Statement{
			Sid: "BilletPassRole", Effect: "Allow",
			Action: []string{"iam:PassRole"}, Resource: []string{in.InstanceProfileRoleARN},
			// Pass the role only to EC2, so a leaked policy cannot hand it elsewhere.
			Condition: map[string]any{
				"StringEquals": map[string]any{"iam:PassedToService": "ec2." + suffix},
			},
		})
	}

	return p, nil
}

// cacheStatements are the EBS + S3 (+ optional KMS) rules the sticky-disk cache
// needs. Creates require the owner tag in the request; describes act on "*";
// attach/detach and deletes require the owner tag on the resource; S3 is scoped by
// bucket and prefix; KMS is scoped to the key and to EBS use.
func cacheStatements(partition, suffix, region, owner string, c Cache) ([]Statement, error) {
	if c.Bucket == "" {
		return nil, fmt.Errorf("awspolicy: a cache policy needs the S3 bucket name")
	}
	if c.Prefix == "" {
		return nil, fmt.Errorf("awspolicy: a cache policy needs the S3 prefix")
	}
	// The bucket and prefix both land in an IAM Resource ARN, so both are checked
	// here — Build is an exported entry point that may not have gone through
	// config.Load. A `*` or `?` widens the grant to sibling buckets/prefixes, and a
	// `${` would be interpolated by IAM as a policy variable rather than matched
	// literally.
	if err := literalARNComponent("bucket", c.Bucket); err != nil {
		return nil, err
	}
	if err := literalARNComponent("prefix", c.Prefix); err != nil {
		return nil, err
	}

	prefix := strings.TrimSuffix(c.Prefix, "/")
	bucketARN := fmt.Sprintf("arn:%s:s3:::%s", partition, c.Bucket)
	objectARN := bucketARN + "/" + prefix + "/*"

	ownerResource := ownerTagCondition("aws:ResourceTag/"+ec2.OwnerTagKey, owner)
	ownerRequest := ownerTagCondition("aws:RequestTag/"+ec2.OwnerTagKey, owner)

	stmts := []Statement{
		{
			Sid: "BilletCacheCreateVolume", Effect: "Allow",
			Action:   ebss3.VolumeCreateIAMActions(),
			Resource: []string{"*"},
			// Tag the new volume as billet's. The source snapshot (when cloning) is
			// not scoped HERE, because billet also creates FRESH volumes with no
			// source and a source condition would deny those; the clone's source is
			// the next statement.
			Condition: ownerRequest,
		},
		// CREATEVOLUME FROM A SNAPSHOT AUTHORIZES THE SNAPSHOT TOO, and the
		// statement above cannot grant it: aws:RequestTag is not in the context of
		// the snapshot's authorization, so its condition fails there and the whole
		// clone is refused. MEASURED on the first real warm reuse of an ebs-s3 cache
		// (2026-09-03): the node's clone answered UnauthorizedOperation and the job
		// ran cold, after every fake had accepted the request; with this statement
		// attached to the same role the next clone mounted the generation warm. It
		// also scopes the source by its owner tag, which is a boundary the package
		// comment above used to say tags could not draw. The ARN carries no
		// account, because EBS snapshot ARNs do not.
		{
			Sid: "BilletCacheCloneSource", Effect: "Allow",
			Action:    ebss3.VolumeCreateIAMActions(),
			Resource:  []string{"arn:" + partition + ":ec2:*::snapshot/*"},
			Condition: ownerResource,
		},
		// CreateSnapshot authorizes TWO resource types, and each needs its own
		// statement: a single statement combining aws:ResourceTag (the source volume)
		// and aws:RequestTag (the new snapshot) is ANDed against BOTH authorizations,
		// and the new snapshot — which has no resource tags yet — cannot satisfy
		// aws:ResourceTag, so billet's own snapshot would be denied. Split per AWS's
		// documented EBS pattern: the source volume must carry the owner tag, the new
		// snapshot must be created with it. Together they stop the role from copying a
		// foreign volume into a billet-tagged snapshot.
		{
			Sid: "BilletCacheSnapshotSource", Effect: "Allow",
			Action:    ebss3.SnapshotCreateIAMActions(),
			Resource:  []string{"arn:" + partition + ":ec2:*:*:volume/*"},
			Condition: ownerResource,
		},
		{
			Sid: "BilletCacheSnapshotCreate", Effect: "Allow",
			Action:    ebss3.SnapshotCreateIAMActions(),
			Resource:  []string{"arn:" + partition + ":ec2:*:*:snapshot/*"},
			Condition: ownerRequest,
		},
		{
			Sid: "BilletCacheDescribe", Effect: "Allow",
			Action:   union(ec2.CacheDescribeIAMActions(), ebss3.StorageDescribeIAMActions()),
			Resource: []string{"*"},
		},
		{
			Sid: "BilletCacheAttach", Effect: "Allow",
			Action: ec2.CacheAttachIAMActions(), Resource: []string{"*"},
			Condition: ownerResource,
		},
		{
			Sid: "BilletCacheDelete", Effect: "Allow",
			Action:   ebss3.StorageDeleteIAMActions(),
			Resource: []string{"*"},
			// Delete only what billet owns AND tagged as a cache resource: the owner
			// tag by value (or presence) plus the cache-owner tag present.
			Condition: withCacheOwnerPresent(ownerResource),
		},
		{
			Sid: "BilletCacheObjects", Effect: "Allow",
			Action: ebss3.S3ObjectIAMActions(), Resource: []string{objectARN},
		},
		{
			Sid: "BilletCacheList", Effect: "Allow",
			Action: ebss3.S3ListIAMActions(), Resource: []string{bucketARN},
			Condition: map[string]any{
				"StringLike": map[string]any{"s3:prefix": []string{prefix + "/*"}},
			},
		},
	}

	if c.KMSKeyARN != "" {
		if region == "" {
			return nil, fmt.Errorf("awspolicy: a KMS-encrypted cache needs the region for the " +
				"kms:ViaService condition")
		}
		if err := requireARN(c.KMSKeyARN, "kms", "key/", partition, region); err != nil {
			return nil, fmt.Errorf("cache KMS key: %w", err)
		}

		viaService := fmt.Sprintf("ec2.%s.%s", region, suffix)
		stmts = append(stmts,
			Statement{
				Sid: "BilletCacheKMSUse", Effect: "Allow",
				Action: ebss3.KMSCryptoIAMActions(), Resource: []string{c.KMSKeyARN},
				// Only through EBS, never a direct call.
				Condition: map[string]any{
					"StringEquals": map[string]any{"kms:ViaService": viaService},
				},
			},
			Statement{
				Sid: "BilletCacheKMSGrant", Effect: "Allow",
				Action: ebss3.KMSGrantIAMActions(), Resource: []string{c.KMSKeyARN},
				// Grants for AWS services only, so the key cannot be delegated away.
				Condition: map[string]any{
					"Bool": map[string]any{"kms:GrantIsForAWSResource": "true"},
				},
			},
		)
	}

	return stmts, nil
}

// backupStatements is what a control plane needs to put its archives somewhere
// other than the disk it protects, and nothing else.
//
// READ AS WELL AS WRITE, because the fetch is the half that matters: on the day
// this is used the machine is new and holds the billet binary alone, and a
// write-only grant would leave an operator installing aws-cli during an outage.
// NEVER DELETE — see Backup.
// codeBuildStatements are what a codebuild NODE needs.
func codeBuildStatements(partition, suffix, region string, c CodeBuild) ([]Statement, error) {
	if c.ProjectARN == "" {
		return nil, errors.New("awspolicy: a codebuild policy needs the project ARN; the " +
			"project is the ownership boundary, because a build cannot be tagged")
	}

	if err := requireARN(c.ProjectARN, "codebuild", "project/", partition, region); err != nil {
		return nil, fmt.Errorf("codebuild project: %w", err)
	}

	if c.ParameterPath == "" {
		return nil, errors.New("awspolicy: a codebuild policy needs the parameter path the " +
			"single-use runner registration is staged under")
	}

	if err := literalARNComponent("codebuild parameter path", c.ParameterPath); err != nil {
		return nil, err
	}

	path := strings.TrimSuffix(c.ParameterPath, "/")

	// THE ACCOUNT COMES OUT OF THE PROJECT ARN rather than being a second input.
	//
	// The parameter ARN used a `*` for the account while the project beside it named
	// one, which is the inconsistency a reader has to stop and think about: it is
	// harmless in practice (a role acts only in the account it lives in) and it is
	// exactly the scoped-looking-but-not shape requireARN refuses everywhere else. One
	// value, taken from the ARN this statement is already scoped by, cannot disagree
	// with it — a separate Account field could.
	account := arnAccount(c.ProjectARN)

	stmts := []Statement{
		// SCOPED TO ONE PROJECT. There is no tag condition available for a build, so
		// this ARN is the entire boundary between billet's builds and anybody else's
		// — and List feeds a loop that STOPS builds.
		{
			Sid: "BilletCodeBuildRun", Effect: "Allow",
			Action:   codebuild.BuildIAMActions(),
			Resource: []string{c.ProjectARN},
		},
		{
			Sid: "BilletCodeBuildRead", Effect: "Allow",
			Action: union(codebuild.BuildDescribeIAMActions(),
				codebuild.ProjectDescribeIAMActions()),
			Resource: []string{c.ProjectARN},
		},
		// SCOPED TO ONE PATH, and the path is why node.codebuild.jit_parameter_path
		// refuses a wildcard: this Resource is where the value lands, and on a shared
		// account the sibling paths a `*` admits are other deployments' registrations.
		//
		// NO GetParameter. The node writes the registration and deletes it; it never
		// reads one back, and granting a read would let a compromised node recover a
		// credential it had already handed over for no operational benefit. The
		// BUILD's role is what reads it, and that is a different principal.
		{
			Sid: "BilletCodeBuildStageRegistration", Effect: "Allow",
			Action: codebuild.JITParameterIAMActions(),
			Resource: []string{
				fmt.Sprintf("arn:%s:ssm:%s:%s:parameter%s/*",
					partition, region, account, path),
			},
		},
	}

	if c.FleetARN != "" {
		if err := requireARN(c.FleetARN, "codebuild", "fleet/", partition, region); err != nil {
			return nil, fmt.Errorf("codebuild fleet: %w", err)
		}

		// READ ONLY. `billet check` reports a fleet's capacity and environment so an
		// operator can see what a macOS tier's macos_vm_limit should be; nothing in
		// billet creates, resizes or deletes a fleet, and granting that would put
		// standing cost under the control of a node process.
		stmts = append(stmts, Statement{
			Sid: "BilletCodeBuildFleetRead", Effect: "Allow",
			Action: codebuild.FleetDescribeIAMActions(), Resource: []string{c.FleetARN},
		})
	}

	if c.KMSKeyARN != "" {
		if region == "" {
			return nil, errors.New("awspolicy: a KMS-encrypted parameter path needs the region " +
				"for the kms:ViaService condition")
		}

		if err := requireARN(c.KMSKeyARN, "kms", "key/", partition, region); err != nil {
			return nil, fmt.Errorf("codebuild KMS key: %w", err)
		}

		// VIA PARAMETER STORE ONLY, the same shape the cache's EBS grant uses: the
		// grant cannot be used to decrypt anything else the key protects, which
		// matters because a per-deployment key is what keeps one deployment from
		// reading another's staged registrations.
		stmts = append(stmts, Statement{
			Sid: "BilletCodeBuildRegistrationKMS", Effect: "Allow",
			Action: codebuild.JITKMSIAMActions(), Resource: []string{c.KMSKeyARN},
			Condition: map[string]any{
				"StringEquals": map[string]any{
					"kms:ViaService": fmt.Sprintf("ssm.%s.%s", region, suffix),
				},
			},
		})
	}

	return stmts, nil
}

// codeBuildRoleStatements are what the BUILD's own service role needs.
//
// EVERY PERMISSION HERE IS ONE THE WORKFLOW HOLDS, because this role runs inside the
// compute that executes it. That is the whole reason it is a separate rendering with
// a separate drift test: it must read at a glance as "reads one parameter, writes
// logs", and anything that creeps into it is a capability handed to arbitrary job
// code.
func codeBuildRoleStatements(
	partition, suffix, region string, r CodeBuildRole,
) ([]Statement, error) {
	if r.ParameterPath == "" {
		return nil, errors.New("awspolicy: a codebuild build role needs the parameter path its " +
			"registration is staged under")
	}

	if err := literalARNComponent("codebuild parameter path", r.ParameterPath); err != nil {
		return nil, err
	}

	path := strings.TrimSuffix(r.ParameterPath, "/")

	// THE LOG GROUP IS REQUIRED HERE, and falling back to "*" was a real widening.
	//
	// This role RUNS THE WORKFLOW, so `logs:CreateLogGroup` on "*" is arbitrary job
	// code able to create and write any log group in the account — which contradicts
	// this function's own stated contract two comments up, and is the kind of creep it
	// exists to make visible. There is no honest default to fall back to either: the
	// group a build writes to is the one billet pins in logsConfigOverride, so a policy
	// that did not name it would be describing a different deployment.
	//
	// THE CALLER CAN ALWAYS SUPPLY IT, because CodeBuild's own default group for a
	// project is /aws/codebuild/<project> — cmd derives that when node.codebuild
	// names none, which is why this is a refusal rather than a fallback.
	if r.LogGroupARN == "" {
		return nil, errors.New("awspolicy: a codebuild build role needs the ARN of the log " +
			"group its builds write to; this role runs the workflow, so granting " +
			"logs:CreateLogGroup on every group would hand arbitrary job code the account's " +
			"logs")
	}

	if err := requireARN(r.LogGroupARN, "logs", "log-group:", partition, region); err != nil {
		return nil, fmt.Errorf("codebuild log group: %w", err)
	}

	logResource := r.LogGroupARN + ":*"

	// THE ACCOUNT COMES OUT OF THE LOG GROUP ARN, for the reason codeBuildStatements
	// gives: one value taken from an ARN this rendering is already scoped by cannot
	// disagree with it.
	account := arnAccount(r.LogGroupARN)

	stmts := []Statement{
		{
			// THE READ IS PREFIX-WIDE, AND THAT IS STRUCTURAL RATHER THAN AN
			// OVERSIGHT. CodeBuild resolves a PARAMETER_STORE environment variable
			// using the PROJECT's service role, before the build exists — so there is
			// no per-build identity to scope to, and one project's role must be able
			// to read any registration that project's builds may reference.
			//
			// WHAT IT COSTS, said plainly: a job in this deployment that learns
			// another concurrent job's parameter name could read that registration and
			// register a runner as that lease. It is bounded by the boundary this
			// backend already declares — untrusted work is REFUSED on codebuild
			// outright (ADR-007 finding 6), so every job here is one the operator
			// trusts, and those same jobs already share a reserved fleet's cache by
			// AWS's own design — and by the registration being single-use and deleted
			// as soon as consumption is established.
			//
			// CLOSING IT NEEDS A PER-BUILD CREDENTIAL, not a narrower prefix: a
			// per-build role, or an intermediary that hands back only the caller's own
			// parameter. Neither is in this change, and pretending the prefix is
			// narrow would be worse than recording that it is not.
			Sid: "BilletBuildReadRegistration", Effect: "Allow",
			Action: codebuild.BuildRoleParameterIAMActions(),
			Resource: []string{
				fmt.Sprintf("arn:%s:ssm:%s:%s:parameter%s/*",
					partition, region, account, path),
			},
		},
		{
			Sid: "BilletBuildLogs", Effect: "Allow",
			Action: codebuild.BuildRoleLogIAMActions(), Resource: []string{logResource},
		},
	}

	if r.KMSKeyARN != "" {
		if region == "" {
			return nil, errors.New("awspolicy: a KMS-encrypted parameter path needs the region " +
				"for the kms:ViaService condition")
		}

		if err := requireARN(r.KMSKeyARN, "kms", "key/", partition, region); err != nil {
			return nil, fmt.Errorf("codebuild build role KMS key: %w", err)
		}

		stmts = append(stmts, Statement{
			Sid: "BilletBuildRegistrationKMS", Effect: "Allow",
			Action: codebuild.BuildRoleKMSIAMActions(), Resource: []string{r.KMSKeyARN},
			Condition: map[string]any{
				"StringEquals": map[string]any{
					"kms:ViaService": fmt.Sprintf("ssm.%s.%s", region, suffix),
				},
			},
		})
	}

	return stmts, nil
}

// codeBuildSweepStatement is what the CONTROL PLANE needs to sweep one path.
//
// TWO RESOURCES FOR TWO ACTIONS. GetParametersByPath is authorised against the
// hierarchy it is asked about, so the path itself is named; DeleteParameter is
// authorised against each parameter, so the path's children are named. Naming only
// the children is the shape that looks scoped and lists nothing — MEASURED with
// iam:SimulateCustomPolicy on 2026-09-02: the children-only rendering is implicitly
// denied ssm:GetParametersByPath against the path ARN, and this one is allowed.
func codeBuildSweepStatement(partition, region string, c CodeBuildSweep) (Statement, error) {
	if c.ParameterPath == "" {
		return Statement{}, errors.New("awspolicy: a codebuild sweep policy needs the parameter " +
			"path the registrations are staged under")
	}

	if err := literalARNComponent("codebuild parameter path", c.ParameterPath); err != nil {
		return Statement{}, err
	}

	if c.Account == "" {
		return Statement{}, errors.New("awspolicy: a codebuild sweep policy needs the account the " +
			"parameters live in; a parameter ARN names one and a wildcard there is a grant " +
			"that looks scoped and is not")
	}

	if err := literalARNComponent("codebuild account", c.Account); err != nil {
		return Statement{}, err
	}

	if region == "" {
		return Statement{}, errors.New("awspolicy: a codebuild sweep policy needs the region the " +
			"parameters live in")
	}

	path := strings.TrimSuffix(c.ParameterPath, "/")

	return Statement{
		Sid: "BilletControllerSweepRegistrations", Effect: "Allow",
		Action: codebuild.SweepIAMActions(),
		Resource: []string{
			fmt.Sprintf("arn:%s:ssm:%s:%s:parameter%s", partition, region, c.Account, path),
			fmt.Sprintf("arn:%s:ssm:%s:%s:parameter%s/*", partition, region, c.Account, path),
		},
	}, nil
}

// identityStatements grants a control plane the two parameters that ARE this
// deployment.
//
// READ AND WRITE, AND NO DELETE. A controller reads the App key on every token
// mint and the authority at every start; it writes the authority when it rotates
// one and the App key exactly once, at onboarding. Deleting either is an
// operator's act taken knowing what it is — the same reasoning that keeps
// `s3:DeleteObject` off the backup grant, and sharper here, because the value is
// a credential GitHub will not re-issue.
//
// SCOPED TO THE PREFIX, WHICH IS THE WHOLE BOUNDARY. Every sibling path is
// another deployment's App key and another deployment's certificate authority, so
// a `*` reaching one of them is not a widened grant but a different deployment's
// identity.
func identityStatements(partition, suffix, region string, id Identity) ([]Statement, error) {
	if id.Prefix == "" {
		return nil, errors.New("awspolicy: an identity policy needs the Parameter Store prefix")
	}

	if !strings.HasPrefix(id.Prefix, "/") {
		return nil, fmt.Errorf(
			"awspolicy: the identity prefix must be absolute (got %q); a relative Parameter "+
				"Store name is a different parameter, so a policy built from one grants "+
				"access to something billet does not use", id.Prefix)
	}

	if err := literalARNComponent("identity prefix", id.Prefix); err != nil {
		return nil, err
	}

	// THE LEADING SLASH IS DROPPED FROM THE ARN. A Parameter Store name is
	// `/billet/prod/ca`, and its ARN is `…:parameter/billet/prod/ca` — the
	// resource part carries exactly one separator, so keeping both produces an ARN
	// that matches nothing and a policy that silently grants no access.
	path := strings.TrimSuffix(strings.TrimPrefix(id.Prefix, "/"), "/")

	stmts := []Statement{{
		Sid: "BilletIdentityParameters", Effect: "Allow",
		Action: []string{"ssm:GetParameter", "ssm:PutParameter"},
		Resource: []string{
			fmt.Sprintf("arn:%s:ssm:*:*:parameter/%s/*", partition, path),
		},
	}}

	if id.KMSKeyARN != "" {
		if region == "" {
			return nil, errors.New("awspolicy: a KMS-encrypted identity store needs the " +
				"region for the kms:ViaService condition")
		}

		if err := requireARN(id.KMSKeyARN, "kms", "key/", partition, region); err != nil {
			return nil, fmt.Errorf("identity KMS key: %w", err)
		}

		stmts = append(stmts, Statement{
			Sid: "BilletIdentityKMSUse", Effect: "Allow",
			Action:   []string{"kms:Decrypt", "kms:GenerateDataKey"},
			Resource: []string{id.KMSKeyARN},
			// ONLY THROUGH SSM, never a direct call: a compromised control-plane
			// role must not be able to use this key on anything else.
			Condition: map[string]any{
				"StringEquals": map[string]any{
					"kms:ViaService": fmt.Sprintf("ssm.%s.%s", region, suffix),
				},
			},
		})
	}

	return stmts, nil
}

func backupStatements(partition, suffix, region string, b Backup) ([]Statement, error) {
	if b.Bucket == "" {
		return nil, errors.New("awspolicy: a backup policy needs the S3 bucket name")
	}

	if b.Prefix == "" {
		return nil, errors.New("awspolicy: a backup policy needs the S3 prefix")
	}

	if err := literalARNComponent("backup bucket", b.Bucket); err != nil {
		return nil, err
	}

	if err := literalARNComponent("backup prefix", b.Prefix); err != nil {
		return nil, err
	}

	prefix := strings.TrimSuffix(b.Prefix, "/")
	bucketARN := fmt.Sprintf("arn:%s:s3:::%s", partition, b.Bucket)

	stmts := []Statement{
		{
			Sid: "BilletBackupObjects", Effect: "Allow",
			Action:   archivestore.ObjectIAMActions(),
			Resource: []string{bucketARN + "/" + prefix + "/*"},
		},
		{
			Sid: "BilletBackupList", Effect: "Allow",
			Action: archivestore.ListIAMActions(), Resource: []string{bucketARN},
			// SCOPED BY PREFIX, because a listing is how a restore finds an
			// archive when the machine has no deployment identity of its own —
			// and an unscoped one would enumerate the whole bucket.
			Condition: map[string]any{
				"StringLike": map[string]any{"s3:prefix": []string{prefix + "/*"}},
			},
		},
	}

	if b.KMSKeyARN != "" {
		if region == "" {
			return nil, errors.New("awspolicy: a KMS-encrypted backup bucket needs the region " +
				"for the kms:ViaService condition")
		}

		if err := requireARN(b.KMSKeyARN, "kms", "key/", partition, region); err != nil {
			return nil, fmt.Errorf("backup KMS key: %w", err)
		}

		stmts = append(stmts, Statement{
			Sid: "BilletBackupKMSUse", Effect: "Allow",
			Action: archivestore.KMSCryptoIAMActions(), Resource: []string{b.KMSKeyARN},
			// ONLY THROUGH S3, never a direct call: a compromised control-plane
			// role must not be able to use the key on anything else.
			Condition: map[string]any{
				"StringEquals": map[string]any{
					"kms:ViaService": fmt.Sprintf("s3.%s.%s", region, suffix),
				},
			},
		})
	}

	return stmts, nil
}

// arnAccount is the account field of an ARN, or "*" when there is none.
//
// THE FALLBACK IS ONLY REACHABLE FOR AN ARN THAT HAS ALREADY BEEN VALIDATED without
// one, which for these callers means never: both are checked by requireARN first, and
// requireARN refuses an empty account for every service but s3. It is a `*` rather
// than an error because this is a formatting helper and the refusal belongs where the
// ARN is checked — two places refusing the same thing is how one of them drifts.
func arnAccount(arn string) string {
	fields := strings.SplitN(arn, ":", 6)
	if len(fields) != 6 || fields[4] == "" {
		return "*"
	}

	return fields[4]
}

// requireARN checks that value is an ARN for the expected service
// resource kind: an alias, a bare id, a wildcard, or an ARN for the wrong service,
// partition or region would either fail to match at runtime or widen the grant to
// every resource of that kind. It parses the six ARN fields rather than substring
// matching, so a value that merely contains ":kms:" does not pass.
//
// wantPartition and wantRegion, when non-empty, must match the ARN's fields —
// enforced for a KMS key, whose grant is meaningless in the wrong region or
// partition. resourcePrefix, when set, must begin the resource field. An empty
// region field is allowed (a global service such as IAM writes no region).
func requireARN(value, service, resourcePrefix, wantPartition, wantRegion string) error {
	if strings.ContainsAny(value, "*?") {
		return fmt.Errorf("%q contains a wildcard; it must name one resource exactly", value)
	}

	fields := strings.SplitN(value, ":", 6)
	if len(fields) != 6 || fields[0] != "arn" {
		return fmt.Errorf("%q is not an ARN (arn:partition:service:region:account:resource)", value)
	}

	partition, svc, region, account, resource := fields[1], fields[2], fields[3], fields[4], fields[5]
	if svc != service {
		return fmt.Errorf("%q is a %q ARN, not %s", value, svc, service)
	}
	if partition == "" {
		return fmt.Errorf("%q has an empty partition", value)
	}
	if wantPartition != "" && partition != wantPartition {
		return fmt.Errorf("%q is in partition %q, not %q", value, partition, wantPartition)
	}
	if wantRegion != "" && region != wantRegion {
		return fmt.Errorf("%q is in region %q, not %q", value, region, wantRegion)
	}
	if account == "" && service != "s3" {
		return fmt.Errorf("%q has an empty account id", value)
	}
	if resource == "" || (resourcePrefix != "" && !strings.HasPrefix(resource, resourcePrefix)) {
		return fmt.Errorf("%q does not name a %s%s resource", value, service, resourcePrefix)
	}

	return nil
}

// literalARNComponent refuses a value that would not be matched literally in an
// IAM Resource ARN: `*`/`?` are IAM wildcards, and `${...}` is a policy variable.
func literalARNComponent(field, value string) error {
	if strings.ContainsAny(value, "*?") {
		return fmt.Errorf("awspolicy: the S3 %s %q contains a wildcard, which would widen the "+
			"IAM grant beyond it; it must be a literal", field, value)
	}
	if strings.Contains(value, "${") {
		return fmt.Errorf("awspolicy: the S3 %s %q contains ${, which IAM would expand as a "+
			"policy variable rather than match literally", field, value)
	}

	return nil
}

// nullPresent renders the IAM condition "tag key is present" — Null:false means
// the key is NOT null, i.e. it exists.
func nullPresent(key string) map[string]any {
	return map[string]any{"Null": map[string]any{key: "false"}}
}

// ownerTagCondition matches billet's owner tag on the tag key given (a ResourceTag
// or a RequestTag). With a deployment id it demands that exact VALUE — the
// per-deployment boundary; without one it demands mere PRESENCE — the per-account
// boundary. This is the one place the two modes diverge.
func ownerTagCondition(key, owner string) map[string]any {
	if owner == "" {
		return nullPresent(key)
	}

	return map[string]any{"StringEquals": map[string]any{key: owner}}
}

// withCacheOwnerPresent adds "the cache-owner tag is present" to an owner
// condition, so a delete requires BOTH the owner match and the cache-owner tag —
// the owner scopes to the deployment, the cache-owner marks it a cache resource.
func withCacheOwnerPresent(owner map[string]any) map[string]any {
	out := map[string]any{}
	for op, kv := range owner {
		out[op] = kv
	}

	cacheKey := "aws:ResourceTag/" + ebss3.CacheOwnerTagKey
	if null, ok := out["Null"].(map[string]any); ok {
		merged := map[string]any{cacheKey: "false"}
		for k, v := range null {
			merged[k] = v
		}

		out["Null"] = merged
	} else {
		out["Null"] = map[string]any{cacheKey: "false"}
	}

	return out
}

// createTagCondition is the condition on CreateTags: always restricted to
// create-time tagging, and in per-deployment mode also to this deployment's owner
// value in the request tag — so a role cannot stamp another deployment's owner tag
// onto a resource it launches. When a builder shares the policy, the per-build
// builder prefix is allowed too, since the builder tags its own instances with it.
func createTagCondition(owner string, builder bool) map[string]any {
	actions := []string{"RunInstances", "CreateVolume", "CreateSnapshot"}
	if builder {
		// CREATEIMAGE TAGS, AND WITHOUT THIS A BUILD UNDER THIS POLICY SHOULD BE
		// REFUSED OUTRIGHT.
		//
		// `billet ami build` sends a TagSpecification on CreateImage (the owner and
		// the billet that built it), and AWS authorizes create-time tags as a
		// SEPARATE ec2:CreateTags check keyed on ec2:CreateAction — "if the user
		// attempts to create a resource with tags, the request fails if the user
		// does not have permissions to use the ec2:CreateTags action". Under that
		// rule a policy granting ec2:CreateImage while leaving CreateImage out of
		// this list denies the whole call, which would mean the AMI stamp has been
		// unreachable since it landed.
		//
		// MEASURED, 2026-08-29, AND BY BOTH HALVES SEPARATELY. The policy half:
		// iam:SimulateCustomPolicy answers implicitDeny for ec2:CreateTags with
		// ec2:CreateAction=CreateImage without this entry and allowed with it, while
		// CreateAction=RunInstances stays allowed either way. The service half, which
		// a simulation cannot reach — EC2 asked with --dry-run under a role holding
		// this policy, against a real instance carrying the builder owner tag:
		//
		//	without a TagSpecification  DryRunOperation      "would have succeeded"
		//	with one, this entry absent UnauthorizedOperation "not authorized to
		//	                                                   perform: ec2:CreateTags"
		//	with one, this entry present DryRunOperation
		//
		// Same policy, same instance, the only difference being whether tags were
		// sent — and EC2 names ec2:CreateTags in the refusal. So the separate check
		// is real and this entry is what satisfies it.
		//
		// ONLY FOR THE BUILDER, so the three committed terraform renderings do not
		// move. A runtime role holds no ec2:CreateImage at all, so allowing the tag
		// action for a create it cannot perform would grant it nothing.
		actions = append(actions, "CreateImage")
	}

	stringEquals := map[string]any{"ec2:CreateAction": actions}
	cond := map[string]any{"StringEquals": stringEquals}
	if owner == "" {
		return cond
	}

	key := "aws:RequestTag/" + ec2.OwnerTagKey
	if builder {
		// StringLike, so the exact owner AND the builder prefix both match.
		cond["StringLike"] = map[string]any{key: []string{owner, ec2.BuilderOwnerPrefix + "*"}}
	} else {
		stringEquals[key] = owner
	}

	// The cache-owner tag (value "<owner>/<site>") must also stay within this
	// deployment when present, or a role could create a cache volume/snapshot tagged
	// with ANOTHER deployment's cache-owner — which lands in that deployment's
	// cache-owner-filtered listing and wedges its eviction sweep on a resource it
	// cannot delete. IfExists, because RunInstances and the builder set no
	// cache-owner tag and a plain condition on the missing key would deny them.
	cond["StringLikeIfExists"] = map[string]any{
		"aws:RequestTag/" + ebss3.CacheOwnerTagKey: []string{owner + "/*"},
	}

	return cond
}

// builderOwnerCondition matches the builder's per-build owner value by prefix, so
// the builder's permissions reach only builder instances — never a deployment's
// job instances, whatever mode the rest of the policy is in.
func builderOwnerCondition() map[string]any {
	return map[string]any{
		"StringLike": map[string]any{
			"aws:ResourceTag/" + ec2.OwnerTagKey: []string{ec2.BuilderOwnerPrefix + "*"},
		},
	}
}

// union concatenates action lists preserving first-occurrence order and dropping
// duplicates — ec2 and ebss3 both list ec2:DescribeVolumes, and a policy must not
// name an action twice.
func union(lists ...[]string) []string {
	seen := make(map[string]struct{})

	var out []string

	for _, list := range lists {
		for _, a := range list {
			if _, dup := seen[a]; dup {
				continue
			}

			seen[a] = struct{}{}
			out = append(out, a)
		}
	}

	return out
}

// JSON renders the policy as indented, deterministic JSON. Struct fields marshal
// in declaration order and Go sorts map keys, so the same inputs always produce
// the same bytes — which is what the drift test and the Terraform rendering rely
// on.
func (p Policy) JSON() ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}
