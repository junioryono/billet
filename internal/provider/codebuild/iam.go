package codebuild

// The IAM actions this backend performs, declared BESIDE THE CODE THAT PERFORMS
// THEM.
//
// internal/awspolicy unions these into statements, and that is the only place the
// union happens. If the actions billet's code performs and the permissions its
// policy grants ever disagreed, a node would fail at runtime with a permission
// error that no config check could have caught — so the constants live here and the
// generator imports them, exactly as internal/provider/ec2/iam.go does.
//
// TWO PRINCIPALS, AND THEY ARE DELIBERATELY DIFFERENT. The NODE role starts, stops,
// describes and lists builds on its own project and writes and deletes parameters
// under its own path. The BUILD's service role reads one parameter and writes logs.
// A single role carrying both is the mistake the backup policy made in the other
// direction — a principal that only puts archives in a bucket came out holding
// ec2:RunInstances — so the split is stated in types rather than in a comment.

// OwnerMarkerKey is the environment-variable name carrying the deployment identity
// on every build billet starts.
//
// EXPORTED BECAUSE A BUILD CANNOT BE TAGGED. Every other backend's ownership
// boundary is a tag, and awspolicy conditions its destructive statements on one —
// `aws:ResourceTag/sh.billet.owner`. There is no such condition available for a
// build, so the boundary here is the PROJECT (which can be tagged, and which
// awspolicy scopes the build actions to by ARN) plus this marker, which billet
// checks itself in List and Find. Exported so a policy generator and a diagnostic
// can name the same string billet writes.
const OwnerMarkerKey = ownerEnvVar

// NameMarkerKey is the environment-variable name carrying the lease name.
const NameMarkerKey = nameEnvVar

// BuildIAMActions are what a node needs to start and stop its own builds.
//
// SCOPED TO THE PROJECT ARN by the generator. StartBuild and StopBuild are the two
// that act, and neither can be conditioned on an ownership tag because a build
// carries none — so the project is the boundary, which is why the project must be
// billet's alone.
//
// THE PROJECT ARN IS THE RIGHT RESOURCE FOR THE BUILD ACTIONS TOO, and this was
// MEASURED because it reads like a bug. A review argued that StopBuild and
// BatchGetBuilds act on BUILD resources, so a project-scoped grant would let the
// first launch succeed and then refuse every teardown and inventory. Tested against
// real AWS in us-west-2 on 2026-08-31 with a role holding exactly these five actions
// on one project ARN and nothing else: ListBuildsForProject, BatchGetProjects,
// BatchGetBuilds and StopBuild all SUCCEEDED, and the same role was correctly
// refused ListBuildsForProject on a project the policy does not name
// (AccessDeniedException). IAM's own simulate-custom-policy agrees, and additionally
// refuses a build-ARN-scoped statement against a build ARN — `build/...` is not a
// resource type these actions accept. Do not "fix" this by splitting the statement;
// it would grant nothing.
func BuildIAMActions() []string {
	return []string{"codebuild:StartBuild", "codebuild:StopBuild"}
}

// BuildDescribeIAMActions are the read-only lookups reconciliation and teardown
// depend on.
//
// ListBuildsForProject IS NOT OPTIONAL, and it is worth saying why a read looks
// load-bearing: it is the only route to an inventory, because CodeBuild offers no
// tag filter and no status filter. A node that cannot list cannot reconcile, and a
// node that cannot reconcile frees capacity for builds it can no longer see.
func BuildDescribeIAMActions() []string {
	return []string{"codebuild:BatchGetBuilds", "codebuild:ListBuildsForProject"}
}

// ProjectDescribeIAMActions are what `billet check` reads to report what a tier
// will actually get.
//
// BatchGetProjects is also how billet answers the one question it must not get
// wrong about a project it did not create: whether a WORKFLOW_JOB_QUEUED webhook is
// attached, which would mean CodeBuild is acquiring jobs too.
func ProjectDescribeIAMActions() []string {
	return []string{"codebuild:BatchGetProjects"}
}

// FleetDescribeIAMActions are what `billet check` reads to report a reserved
// fleet's capacity and environment.
func FleetDescribeIAMActions() []string {
	return []string{"codebuild:BatchGetFleets"}
}

// JITParameterIAMActions are what a node needs to stage and remove one build's
// single-use runner registration.
//
// SCOPED TO THE CONFIGURED PATH by the generator, which is why
// node.codebuild.jit_parameter_path refuses a wildcard: the value lands in an IAM
// Resource ARN, and on a shared account the sibling paths a `*` admits are other
// deployments' runner registrations.
//
// THERE IS NO GetParameter HERE. The node writes the registration and deletes it; it
// never reads one back. Granting a read would let a compromised node recover a
// credential it had already handed over, for no operational benefit — the BUILD's
// role is what reads it, and that is a different principal.
func JITParameterIAMActions() []string {
	return []string{"ssm:PutParameter", "ssm:DeleteParameter"}
}

// SweepIAMActions are what the CONTROL PLANE needs to remove the registrations a
// dead node never reaped.
//
// A THIRD PRINCIPAL, and the narrowest of the three. It LISTS the names under the
// path and DELETES the ones the ledger has proved dead — and that is all: there is
// no GetParameter, because the sweep never reads a registration; no PutParameter,
// because it stages nothing; and no KMS action, because a listing that asks for no
// decryption calls no key. The listing is GetParametersByPath rather than
// DescribeParameters because IAM can scope the former to exactly this path and
// cannot scope the latter at all, and a controller that could enumerate every
// parameter name in the account is wider than a sweep of one path needs.
//
// THE MISSING KMS ACTION IS NOT WHAT KEEPS A REGISTRATION OUT OF THE CONTROLLER,
// and that was MEASURED rather than assumed (2026-09-02): a real role holding
// exactly this grant and nothing else, asked for the listing WITH decryption,
// received plaintext — the account's aws/ssm key authorises any principal that
// reaches it through Parameter Store, so an identity policy's silence about KMS
// bounds nothing there. What keeps the value out is the sweep's request stating
// WithDecryption=false and its response type having no Value field, both pinned by
// tests; a customer-managed key is what makes this grant decisive again, because
// decryption then needs a kms:Decrypt it does not carry — measured under a fresh
// key the same day: the decrypting listing was refused kms:Decrypt, the delete
// still succeeded, and a page mixing one customer-key parameter with one
// default-key parameter failed whole rather than handing back the default one.
//
// GetParametersByPath IS AUTHORISED AGAINST THE PATH ITSELF, so the generator
// renders the grant on both the path and the path's children — the first for the
// listing, the second for each delete. MEASURED on 2026-09-02, twice: with
// iam:SimulateCustomPolicy, and then under that real role, whose listing succeeded
// with both resources and was refused `ssm:GetParametersByPath on resource:
// …:parameter<path>` once the grant named only `<path>/*` — the service naming the
// PATH as what it authorised against. A grant naming only the children looks
// scoped and lists nothing.
func SweepIAMActions() []string {
	return []string{"ssm:GetParametersByPath", "ssm:DeleteParameter"}
}

// JITKMSIAMActions are what encrypting a SecureString under a customer-managed key
// needs.
//
// SCOPED TO THE KEY, and conditioned by the generator on kms:ViaService for Parameter
// Store, so the grant cannot be used to decrypt anything else the key protects. The
// same shape the cache's EBS grant uses.
func JITKMSIAMActions() []string {
	return []string{"kms:Encrypt", "kms:GenerateDataKey", "kms:Decrypt"}
}

// BuildRoleParameterIAMActions are what the BUILD's own service role needs.
//
// A DIFFERENT PRINCIPAL FROM THE NODE, and the reason it has its own function
// rather than being folded into the set above: this role runs inside the compute
// that executes somebody's job. It reads one parameter path and writes logs. It may
// NOT start a build, may not delete a parameter, and may not reach any other
// deployment's path — a role that could start builds, running inside a build, is a
// way for a job to launch runners billet never escrowed capacity for.
//
// GetParameters ALONE, MEASURED. `ssm:GetParameter` (singular) was in this list on
// the reasoning that either spelling might be the one CodeBuild calls, which is a
// guess about somebody else's API in a grant handed to job code. A real build in
// us-west-2 on 2026-08-31, whose service role held ONLY ssm:GetParameters, resolved
// its PARAMETER_STORE SecureString correctly — the buildspec reported the exact
// 48-byte length of the staged value. The singular is not called, so it is not
// granted.
func BuildRoleParameterIAMActions() []string {
	return []string{"ssm:GetParameters"}
}

// BuildRoleKMSIAMActions are what the build needs in order to decrypt its own
// registration under a customer-managed key.
func BuildRoleKMSIAMActions() []string {
	return []string{"kms:Decrypt"}
}

// BuildRoleLogIAMActions are what a build needs to write its own logs.
func BuildRoleLogIAMActions() []string {
	return []string{
		"logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents",
	}
}
