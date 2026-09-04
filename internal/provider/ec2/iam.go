package ec2

// IAM action names for the operations this backend performs, kept beside the code
// that performs them so the node's IAM policy grants exactly what this package
// calls. internal/awspolicy imports these and assembles the policy; a drift test
// pins the assembled document. A permission with no direct API call site is
// commented as such — ec2:CreateTags is needed because create-time tagging (a
// TagSpecification on RunInstances, CreateVolume, CreateSnapshot and CreateImage)
// requires it. There is now ONE standalone CreateTags call, and only on the
// builder path: a verified AMI's contract tag is written after the image has been
// booted and proved, so it cannot be a create-time tag.
const (
	IAMRunInstances           = "ec2:RunInstances"
	IAMTerminateInstances     = "ec2:TerminateInstances"
	IAMDescribeInstances      = "ec2:DescribeInstances"
	IAMDescribeImages         = "ec2:DescribeImages"
	IAMCreateTags             = "ec2:CreateTags"
	IAMDescribeSubnets        = "ec2:DescribeSubnets"
	IAMDescribeSecurityGroups = "ec2:DescribeSecurityGroups"
	IAMAttachVolume           = "ec2:AttachVolume"
	IAMDetachVolume           = "ec2:DetachVolume"
	IAMDescribeVolumes        = "ec2:DescribeVolumes"
	IAMCreateImage            = "ec2:CreateImage"
	IAMGetConsoleOutput       = "ec2:GetConsoleOutput"

	IAMSQSReceiveMessage     = "sqs:ReceiveMessage"
	IAMSQSDeleteMessage      = "sqs:DeleteMessage"
	IAMSQSGetQueueAttributes = "sqs:GetQueueAttributes"

	IAMS3PutObject    = "s3:PutObject"
	IAMS3GetObject    = "s3:GetObject"
	IAMS3DeleteObject = "s3:DeleteObject"
)

// BuilderPayloadKeyPrefix is the one object-name prefix `billet ami build`
// stages under, and it is what scopes the payload grant.
//
// THE KEYS SIT AT THE BUCKET ROOT. stagePayload builds
// `billet-payload-<digest>-<nonce>.tar.gz` and payloadStager refuses a key
// containing a slash, so there is no configurable prefix to grant on — which is
// better than one: the grant reaches exactly the objects billet writes and
// nothing else an operator keeps in the same bucket.
const BuilderPayloadKeyPrefix = "billet-payload-"

// BuilderOwnerPrefix begins the owner-tag value `billet ami build` stamps on the
// builder instance it launches — a per-build identity distinct from a deployment
// id (which is 32 hex characters). A bundled IAM policy scopes the builder's
// permissions to this prefix so they isolate the builder without being confused
// with a deployment's job instances.
const BuilderOwnerPrefix = "billet-ami-build-"

// OwnerTagKey is the tag billet stamps on every instance and cache volume/snapshot
// it creates, carrying the deployment identity. A bundled IAM policy conditions the
// destructive actions on this tag: on its exact VALUE (the deployment id) when
// that is known, isolating one deployment from another in a shared account, or on
// PRESENCE otherwise. Either holds because the RUNTIME role only ever tags at
// create time (a TagSpecification, which the policy restricts to
// ec2:CreateAction, and in value mode to this deployment's own owner), so it can
// carry the tag only onto resources it is itself creating. The BUILDER role gets
// one standalone CreateTags besides — the contract promotion — and that one is
// scoped to image resources ALREADY carrying the per-build owner tag, so it can
// still only speak about what it made.
const OwnerTagKey = ownerTag

// The runtime permissions are split into groups a bundled policy conditions
// differently: describes cannot be resource-scoped and act on "*"; RunInstances
// acts on many resource types at once and stays on "*"; TerminateInstances is
// conditioned on the owner tag (by value or presence) so the role can tear down
// only its own deployment's instances; CreateTags is restricted to create-time
// tagging and, in value mode, to this deployment's own owner value. A node's
// runtime role never gets the builder's standalone CreateTags.

// RuntimeDescribeIAMActions are the read-only describes, including the two the
// cloud preflight makes on the subnet and security groups — a node whose role
// cannot describe its own subnet would fail that check.
func RuntimeDescribeIAMActions() []string {
	return []string{
		IAMDescribeInstances, IAMDescribeImages,
		IAMDescribeSubnets, IAMDescribeSecurityGroups,
	}
}

// RuntimeLaunchIAMActions is RunInstances, which a bundled policy leaves on "*":
// it creates an instance, volumes and a network interface at once, and scoping it
// tightly enough to be safe is exactly the kind of multi-resource condition that
// denies a legitimate launch. The instances it creates are tagged, which is what
// the tag-conditioned teardown below relies on.
//
// THE ONE BOUND ON IT IS A DENY, not a narrower Allow: RunInstances also
// authorizes every snapshot a block-device mapping names, so the bundled policy
// denies this action outright for any snapshot resource the deployment does not
// own. billet's own launches name no snapshot, so nothing this package sends is
// affected.
func RuntimeLaunchIAMActions() []string {
	return []string{IAMRunInstances}
}

// RuntimeTerminateIAMActions is TerminateInstances, conditioned by a bundled
// policy on the owner tag's presence.
func RuntimeTerminateIAMActions() []string {
	return []string{IAMTerminateInstances}
}

// RuntimeTagIAMActions is CreateTags, restricted by a bundled policy to
// create-time tagging (ec2:CreateAction) — a node's runtime never calls CreateTags
// on its own, so this both matches its usage and stops the role from stamping
// billet's tags onto a resource it did not create.
func RuntimeTagIAMActions() []string {
	return []string{IAMCreateTags}
}

// CacheAttachIAMActions are attach/detach of the fenced EBS volume, conditioned
// by a bundled policy on the owner tag so the role touches only billet's volumes.
func CacheAttachIAMActions() []string {
	return []string{IAMAttachVolume, IAMDetachVolume}
}

// CacheDescribeIAMActions is the volume describe, which acts on "*".
func CacheDescribeIAMActions() []string {
	return []string{IAMDescribeVolumes}
}

// BuilderIAMActions are what `billet ami build` needs beyond the runtime set — it
// snapshots a stopped builder into an AMI with CreateImage.
func BuilderIAMActions() []string {
	return []string{IAMCreateImage}
}

// BuilderVerifyIAMActions is how a build reads the verdict off the image it just
// made: the verifier instance prints its report to the serial console and billet
// reads it back. There is no other channel that needs no key pair, no agent and
// no inbound access.
func BuilderVerifyIAMActions() []string {
	return []string{IAMGetConsoleOutput}
}

// BuilderPromoteIAMActions is the one standalone CreateTags billet makes, and it
// is standalone for a reason that cannot be worked around: the AMI contract tag
// records that billet BOOTED the image and proved its properties, which is not
// knowable at the instant CreateImage creates it. A bundled policy scopes it to
// image resources carrying the per-build owner tag, which CreateImage stamped.
func BuilderPromoteIAMActions() []string {
	return []string{IAMCreateTags}
}

// BuilderPayloadIAMActions is what staging the shared installers costs, and all
// three are performed on the same object by one build.
//
// PUT uploads the archive, GET is what the PRESIGNED url the builder hands the
// guest resolves to — a presigned URL grants no more than its signer holds, so
// without GetObject the builder signs a URL that answers 403 and the build fails
// downloading its own payload — and DELETE is the cleanup that runs as each
// build ends. Declared here, beside the code that performs them, so the
// generator cannot grant something billet does not do.
func BuilderPayloadIAMActions() []string {
	return []string{IAMS3PutObject, IAMS3GetObject, IAMS3DeleteObject}
}

// SpotIAMActions are what a spot node needs to consume its interruption queue,
// which is how billet learns a spot instance is about to be reclaimed —
// plus GetQueueAttributes, which is how `billet check` proves the queue
// answers WITHOUT consuming a warning. A role provisioned before the probe
// existed lacks it; the probe classifies that refusal as advisory rather
// than failing a working node.
func SpotIAMActions() []string {
	return []string{IAMSQSReceiveMessage, IAMSQSDeleteMessage, IAMSQSGetQueueAttributes}
}
