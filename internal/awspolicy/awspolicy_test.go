package awspolicy

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
)

func sids(p Policy) []string {
	out := make([]string, len(p.Statement))
	for i, s := range p.Statement {
		out[i] = s.Sid
	}

	return out
}

func stmt(t *testing.T, p Policy, sid string) Statement {
	t.Helper()

	for _, s := range p.Statement {
		if s.Sid == sid {
			return s
		}
	}

	t.Fatalf("policy has no statement %q; has %v", sid, sids(p))

	return Statement{}
}

// nullKeys returns the tag keys a statement's Null condition requires present.
func nullKeys(t *testing.T, s Statement) []string {
	t.Helper()

	null, ok := s.Condition["Null"].(map[string]any)
	if !ok {
		t.Fatalf("statement %q has no Null condition: %v", s.Sid, s.Condition)
	}

	var keys []string
	for k, v := range null {
		if v != "false" {
			t.Errorf("statement %q Null condition on %q is %v, want \"false\" (tag must be present)", s.Sid, k, v)
		}

		keys = append(keys, k)
	}

	return keys
}

// A COMPUTE-ONLY NODE GETS THE RUNTIME STATEMENTS AND NOTHING ELSE. Every other
// statement is a capability the deployment did not ask for.
func TestRuntimeOnlyPolicy(t *testing.T) {
	p, err := Inputs{}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{"BilletRuntimeRead", "BilletRuntimeLaunch", "BilletRuntimeTag", "BilletRuntimeTerminate"}
	if got := sids(p); !slices.Equal(got, want) {
		t.Fatalf("a compute-only policy has statements %v, want %v", got, want)
	}

	// The preflight describes MUST be present, or a node's own role fails
	// billet check.
	read := stmt(t, p, "BilletRuntimeRead")
	for _, a := range []string{"ec2:DescribeSubnets", "ec2:DescribeSecurityGroups"} {
		if !slices.Contains(read.Action, a) {
			t.Errorf("the read statement is missing the preflight action %q: %v", a, read.Action)
		}
	}
}

// THE RUNTIME MUTATIONS ARE BOUNDED. Terminate is conditioned on the owner tag,
// CreateTags is restricted to create-time, and neither RunInstances nor the
// describes carry a tag condition (they cannot be scoped that way). This is the
// blast-radius boundary: with create-time-only tagging, a role can carry billet's
// owner tag only onto what it creates, so a tag-conditioned terminate reaches only
// billet's own instances.
func TestRuntimeMutationsAreBounded(t *testing.T) {
	p, err := Inputs{}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// CreateTags only as part of a create.
	tag := stmt(t, p, "BilletRuntimeTag")
	se, ok := tag.Condition["StringEquals"].(map[string]any)
	if !ok {
		t.Fatalf("CreateTags has no StringEquals condition: %v", tag.Condition)
	}
	actions, ok := se["ec2:CreateAction"].([]string)
	if !ok || !slices.Equal(actions, []string{"RunInstances", "CreateVolume", "CreateSnapshot"}) {
		t.Errorf("CreateTags ec2:CreateAction is %v, want the exact create actions", se["ec2:CreateAction"])
	}

	// Terminate only what carries the owner tag.
	if got := nullKeys(t, stmt(t, p, "BilletRuntimeTerminate")); !slices.Contains(got, "aws:ResourceTag/sh.billet.owner") {
		t.Errorf("Terminate is not conditioned on the owner tag: %v", got)
	}

	// Launch and read must NOT carry a condition — a tag condition there would deny
	// legitimate launches and describes.
	if stmt(t, p, "BilletRuntimeLaunch").Condition != nil {
		t.Error("RunInstances carries a condition; it cannot be scoped by tag without denying launches")
	}
	if stmt(t, p, "BilletRuntimeRead").Condition != nil {
		t.Error("the describes carry a condition; they are not resource-scopable")
	}
}

// EACH CAPABILITY ADDS EXACTLY ITS STATEMENTS, scoped to the resource it names.
func TestCapabilitiesAddScopedStatements(t *testing.T) {
	p, err := Inputs{
		Region: "us-west-2",
		Cache: &Cache{
			Bucket: "b", Prefix: "p",
			KMSKeyARN: "arn:aws:kms:us-west-2:1:key/k",
		},
		SpotQueueARN:           "arn:aws:sqs:us-west-2:1:q",
		InstanceProfileRoleARN: "arn:aws:iam::1:role/r",
		Builder:                true,
	}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, want := range []string{
		"BilletCacheCreateVolume", "BilletCacheCloneSource", "BilletCacheSnapshotSource",
		"BilletCacheSnapshotCreate", "BilletCacheDescribe",
		"BilletCacheAttach", "BilletCacheDelete",
		"BilletCacheObjects", "BilletCacheList", "BilletCacheKMSUse", "BilletCacheKMSGrant",
		"BilletSpotInterruptions", "BilletAMIBuilderSource", "BilletAMIBuilderImage",
		"BilletAMIBuilderTerminate", "BilletAMIBuilderConsole", "BilletAMIBuilderPromote",
		"BilletPassRole",
	} {
		stmt(t, p, want) // fails if absent
	}

	if got := stmt(t, p, "BilletCacheObjects").Resource; len(got) != 1 || got[0] != "arn:aws:s3:::b/p/*" {
		t.Errorf("cache objects resource %v, want the prefixed object ARN", got)
	}
	if got := stmt(t, p, "BilletCacheList").Resource; len(got) != 1 || got[0] != "arn:aws:s3:::b" {
		t.Errorf("cache list resource %v, want the bucket ARN", got)
	}
	if got := stmt(t, p, "BilletPassRole").Resource; len(got) != 1 || got[0] != "arn:aws:iam::1:role/r" {
		t.Errorf("PassRole resource %v, want the role ARN", got)
	}
	if got := stmt(t, p, "BilletCacheKMSUse").Resource; len(got) != 1 || got[0] != "arn:aws:kms:us-west-2:1:key/k" {
		t.Errorf("KMS resource %v, want the key ARN", got)
	}

	// Destructive cache actions are owner-tag conditioned; deletes on BOTH tags.
	if got := nullKeys(t, stmt(t, p, "BilletCacheAttach")); !slices.Contains(got, "aws:ResourceTag/sh.billet.owner") {
		t.Errorf("attach/detach is not owner-conditioned: %v", got)
	}
	del := nullKeys(t, stmt(t, p, "BilletCacheDelete"))
	for _, k := range []string{"aws:ResourceTag/sh.billet.owner", "aws:ResourceTag/sh.billet.cache-owner"} {
		if !slices.Contains(del, k) {
			t.Errorf("delete is not conditioned on %q: %v", k, del)
		}
	}
	// CreateVolume requires the tag in the REQUEST (the new volume). Its source
	// snapshot is authorized separately — EC2 evaluates the snapshot ARN too, with
	// no request tags in that context — so a clone needs a second statement scoped
	// to snapshot/* and conditioned on the snapshot's own owner tag. A statement
	// scoped to "*" with a request-tag condition cannot stand in for it: measured,
	// that refused every clone as UnauthorizedOperation.
	if got := nullKeys(t, stmt(t, p, "BilletCacheCreateVolume")); !slices.Contains(got, "aws:RequestTag/sh.billet.owner") {
		t.Errorf("CreateVolume is not request-tag conditioned: %v", got)
	}
	clone := stmt(t, p, "BilletCacheCloneSource")
	if len(clone.Resource) != 1 || !strings.HasSuffix(clone.Resource[0], ":ec2:*::snapshot/*") {
		t.Errorf("clone source is not scoped to the account-less snapshot ARN: %v", clone.Resource)
	}
	if got := nullKeys(t, clone); !slices.Contains(got, "aws:ResourceTag/sh.billet.owner") {
		t.Errorf("clone source is not resource-tag conditioned: %v", got)
	}
	if !slices.Equal(clone.Action, stmt(t, p, "BilletCacheCreateVolume").Action) {
		t.Errorf("clone source grants %v, not the CreateVolume action set", clone.Action)
	}
	// CreateSnapshot is TWO statements: the source volume (resource-scoped to
	// volume/*) must carry the owner tag, and the new snapshot (scoped to snapshot/*)
	// must be created with it. A single combined statement would deny billet's own
	// snapshot, since the new snapshot cannot satisfy a resource-tag condition.
	src := stmt(t, p, "BilletCacheSnapshotSource")
	if len(src.Resource) != 1 || !strings.Contains(src.Resource[0], ":volume/*") {
		t.Errorf("snapshot source is not scoped to volume/*: %v", src.Resource)
	}
	if got := nullKeys(t, src); !slices.Contains(got, "aws:ResourceTag/sh.billet.owner") {
		t.Errorf("snapshot source is not resource-tag conditioned: %v", got)
	}
	create := stmt(t, p, "BilletCacheSnapshotCreate")
	if len(create.Resource) != 1 || !strings.Contains(create.Resource[0], ":snapshot/*") {
		t.Errorf("snapshot create is not scoped to snapshot/*: %v", create.Resource)
	}
	if got := nullKeys(t, create); !slices.Contains(got, "aws:RequestTag/sh.billet.owner") {
		t.Errorf("snapshot create is not request-tag conditioned: %v", got)
	}

	// KMS is confined to EBS use and AWS-resource grants.
	use := stmt(t, p, "BilletCacheKMSUse").Condition
	if via, ok := use["StringEquals"].(map[string]any); !ok || via["kms:ViaService"] != "ec2.us-west-2.amazonaws.com" {
		t.Errorf("KMS use is not confined to EBS via-service: %v", use)
	}
	grant := stmt(t, p, "BilletCacheKMSGrant").Condition
	if b, ok := grant["Bool"].(map[string]any); !ok || b["kms:GrantIsForAWSResource"] != "true" {
		t.Errorf("KMS grant is not confined to AWS-resource grants: %v", grant)
	}
	// PassRole may pass the role only to EC2.
	pr := stmt(t, p, "BilletPassRole").Condition
	if se, ok := pr["StringEquals"].(map[string]any); !ok || se["iam:PassedToService"] != "ec2.amazonaws.com" {
		t.Errorf("PassRole is not confined to the ec2 service: %v", pr)
	}
	// CreateImage is split: the SOURCE instance is owner-conditioned and scoped to
	// instance/*, while the NEW image/snapshots are unconditioned (they have no tags
	// at create) and scoped to image/* + snapshot/*. A single owner-conditioned
	// statement on "*" would deny the whole call (the new image can't be tagged yet).
	bsrc := stmt(t, p, "BilletAMIBuilderSource")
	if len(bsrc.Resource) != 1 || !strings.Contains(bsrc.Resource[0], ":instance/*") {
		t.Errorf("builder source is not scoped to instance/*: %v", bsrc.Resource)
	}
	// The builder is scoped to its OWN per-build owner prefix (StringLike), NOT the
	// deployment id — its instances carry billet-ami-build-*, a distinct identity.
	like, ok := bsrc.Condition["StringLike"].(map[string]any)
	if !ok {
		t.Fatalf("builder source is not StringLike-conditioned: %v", bsrc.Condition)
	}
	if vals, ok := like["aws:ResourceTag/sh.billet.owner"].([]string); !ok || len(vals) != 1 || vals[0] != "billet-ami-build-*" {
		t.Errorf("builder source is not scoped to billet-ami-build-*: %v", like)
	}
	// The builder carries its OWN Terminate (the deployment-scoped runtime Terminate
	// would not match a builder once the policy is per-deployment).
	bterm := stmt(t, p, "BilletAMIBuilderTerminate")
	if bl, ok := bterm.Condition["StringLike"].(map[string]any); !ok || bl["aws:ResourceTag/sh.billet.owner"] == nil {
		t.Errorf("builder Terminate is not scoped to the builder prefix: %v", bterm.Condition)
	}
	img := stmt(t, p, "BilletAMIBuilderImage")
	if img.Condition != nil {
		t.Errorf("builder image statement is conditioned; the new image has no tags to match: %v", img.Condition)
	}
	joined := strings.Join(img.Resource, ",")
	if !strings.Contains(joined, ":image/*") || !strings.Contains(joined, ":snapshot/*") {
		t.Errorf("builder image statement is not scoped to image/* + snapshot/*: %v", img.Resource)
	}
}

// A NODE WITHOUT A CAPABILITY DOES NOT GET ITS STATEMENTS.
func TestOmittedCapabilitiesAreAbsent(t *testing.T) {
	p, err := Inputs{Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p"}}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, unwanted := range []string{
		"BilletCacheKMSUse", "BilletCacheKMSGrant", "BilletSpotInterruptions",
		"BilletAMIBuilderSource", "BilletAMIBuilderImage", "BilletAMIBuilderTerminate",
		"BilletAMIBuilderConsole", "BilletAMIBuilderPromote", "BilletPassRole",
	} {
		for _, s := range p.Statement {
			if s.Sid == unwanted {
				t.Errorf("a cache-only policy contains %q, which nothing asked for", unwanted)
			}
		}
	}
}

// AN ACTION IS NEVER NAMED TWICE within a statement.
func TestActionsAreNotDuplicated(t *testing.T) {
	p, err := Inputs{Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p"}}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, s := range p.Statement {
		seen := map[string]bool{}
		for _, a := range s.Action {
			if seen[a] {
				t.Errorf("statement %q names %q more than once", s.Sid, a)
			}

			seen[a] = true
		}
	}
}

// AN IDENTIFIER THAT IS NOT A SCOPING ARN IS REFUSED. A bare KMS id, an alias, a
// wildcard, an instance-profile name in place of a role ARN, or a wildcard S3
// prefix would each either fail to match at runtime or widen the grant to every
// resource of its kind.
func TestNonARNsAreRefused(t *testing.T) {
	for name, in := range map[string]Inputs{
		"role name not ARN":    {InstanceProfileRoleARN: "billet-node"},
		"role wildcard":        {InstanceProfileRoleARN: "arn:aws:iam::1:role/*"},
		"role wrong partition": {Partition: "aws-cn", InstanceProfileRoleARN: "arn:aws:iam::1:role/r"},
		"spot not ARN":         {SpotQueueARN: "billet-spot"},
		"spot wrong partition": {Partition: "aws-cn", SpotQueueARN: "arn:aws:sqs:us-west-2:1:q"},
		"kms bare id":          {Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p", KMSKeyARN: "1234abcd"}},
		"kms alias":            {Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p", KMSKeyARN: "alias/billet"}},
		"kms wildcard":         {Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p", KMSKeyARN: "*"}},
		"kms wrong region":     {Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p", KMSKeyARN: "arn:aws:kms:us-east-1:1:key/k"}},
		"kms wrong partition":  {Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p", KMSKeyARN: "arn:aws-cn:kms:us-west-2:1:key/k"}},
		"kms wrong service":    {Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p", KMSKeyARN: "arn:aws:s3:us-west-2:1:key/k"}},
		"kms without region":   {Cache: &Cache{Bucket: "b", Prefix: "p", KMSKeyARN: "arn:aws:kms:us-west-2:1:key/k"}},
		"prefix wildcard":      {Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "prod*"}},
		"prefix question mark": {Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "prod?"}},
		"bucket wildcard":      {Region: "us-west-2", Cache: &Cache{Bucket: "b*", Prefix: "p"}},
		"prefix policy var":    {Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p${aws:username}"}},
		"spot wrong region":    {Region: "us-west-2", SpotQueueARN: "arn:aws:sqs:us-east-1:1:q"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := in.Build(); err == nil {
				t.Errorf("Build accepted %s", name)
			}
		})
	}
}

// A CACHE POLICY NEEDS ITS RESOURCES.
func TestCacheNeedsBucketAndPrefix(t *testing.T) {
	if _, err := (Inputs{Cache: &Cache{Prefix: "p"}}).Build(); err == nil {
		t.Error("a cache with no bucket was accepted")
	}
	if _, err := (Inputs{Cache: &Cache{Bucket: "b"}}).Build(); err == nil {
		t.Error("a cache with no prefix was accepted")
	}
}

// THE JSON IS DETERMINISTIC.
func TestJSONIsDeterministic(t *testing.T) {
	in := Inputs{
		Region:       "us-west-2",
		Cache:        &Cache{Bucket: "b", Prefix: "p", KMSKeyARN: "arn:aws:kms:us-west-2:1:key/k"},
		SpotQueueARN: "arn:aws:sqs:us-west-2:1:q",
	}

	first, err := in.Build()
	if err != nil {
		t.Fatal(err)
	}
	second, err := in.Build()
	if err != nil {
		t.Fatal(err)
	}

	a, err := first.JSON()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("two renders of the same inputs differ:\n%s\n---\n%s", a, b)
	}
}

func TestPartitionForRegion(t *testing.T) {
	for region, want := range map[string]string{
		"us-west-2":     "aws",
		"eu-central-1":  "aws",
		"cn-north-1":    "aws-cn",
		"us-gov-west-1": "aws-us-gov",
	} {
		if got := PartitionForRegion(region); got != want {
			t.Errorf("PartitionForRegion(%q) = %q, want %q", region, got, want)
		}
	}
}

// A DEPLOYMENT ID SWITCHES THE OWNERSHIP CONDITION FROM PRESENCE TO EXACT VALUE,
// which is what isolates one billet deployment from another in a shared account.
// The same statements that used Null (present) under no owner use StringEquals
// (this value) under one.
func TestOwnerValueConditioning(t *testing.T) {
	const owner = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	p, err := Inputs{Owner: owner, Region: "us-west-2", Cache: &Cache{Bucket: "b", Prefix: "p"}}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Each destructive statement matches the exact owner value, not mere presence.
	for _, tc := range []struct{ sid, key string }{
		{"BilletRuntimeTerminate", "aws:ResourceTag/sh.billet.owner"},
		{"BilletCacheCreateVolume", "aws:RequestTag/sh.billet.owner"},
		{"BilletCacheCloneSource", "aws:ResourceTag/sh.billet.owner"},
		{"BilletCacheSnapshotSource", "aws:ResourceTag/sh.billet.owner"},
		{"BilletCacheSnapshotCreate", "aws:RequestTag/sh.billet.owner"},
		{"BilletCacheAttach", "aws:ResourceTag/sh.billet.owner"},
		{"BilletCacheDelete", "aws:ResourceTag/sh.billet.owner"},
	} {
		cond := stmt(t, p, tc.sid).Condition
		se, ok := cond["StringEquals"].(map[string]any)
		if !ok {
			t.Errorf("%s is not value-conditioned (no StringEquals): %v", tc.sid, cond)

			continue
		}
		if se[tc.key] != owner {
			t.Errorf("%s does not match the exact owner value on %s: %v", tc.sid, tc.key, se)
		}
		// A value condition must NOT also carry a Null-present on the OWNER key —
		// that would defeat the value match. (Delete legitimately keeps a Null on the
		// separate cache-owner key.)
		if null, ok := cond["Null"].(map[string]any); ok {
			if _, present := null[tc.key]; present {
				t.Errorf("%s keeps a presence check on %s alongside the value match", tc.sid, tc.key)
			}
		}
	}

	// The delete still requires the cache-owner tag present, alongside the value.
	if null, ok := stmt(t, p, "BilletCacheDelete").Condition["Null"].(map[string]any); !ok ||
		null["aws:ResourceTag/sh.billet.cache-owner"] != "false" {
		t.Error("delete lost the cache-owner presence requirement under value conditioning")
	}

	// CreateTags must ALSO require the owner VALUE in the request, or a role could
	// RunInstances tagging its instance with ANOTHER deployment's owner id.
	tag := stmt(t, p, "BilletRuntimeTag").Condition
	if se, ok := tag["StringEquals"].(map[string]any); !ok || se["ec2:CreateAction"] == nil {
		t.Errorf("CreateTags lost its create-time restriction: %v", tag)
	}
	// This build has no builder, so the owner value is an exact StringEquals match.
	if se, ok := tag["StringEquals"].(map[string]any); !ok || se["aws:RequestTag/sh.billet.owner"] != owner {
		t.Errorf("CreateTags does not require the deployment's own owner value in the request: %v", tag)
	}
	// It also confines the cache-owner tag to this deployment's namespace (IfExists,
	// so a launch without a cache-owner tag is not denied), closing the same wedge
	// one tag over.
	if like, ok := tag["StringLikeIfExists"].(map[string]any); !ok {
		t.Errorf("CreateTags does not confine the cache-owner tag: %v", tag)
	} else if vals, ok := like["aws:RequestTag/sh.billet.cache-owner"].([]string); !ok || len(vals) != 1 || vals[0] != owner+"/*" {
		t.Errorf("CreateTags cache-owner confinement is %v, want [%q]", vals, owner+"/*")
	}
}

// WITH A BUILDER, CreateTags ALLOWS BOTH the deployment value AND the builder
// prefix, so the builder's tag-on-create still passes while a foreign deployment
// id is still refused.
func TestCreateTagsAllowsBuilderPrefix(t *testing.T) {
	p, err := Inputs{Owner: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Builder: true}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	like, ok := stmt(t, p, "BilletRuntimeTag").Condition["StringLike"].(map[string]any)
	if !ok {
		t.Fatalf("builder CreateTags has no StringLike owner condition: %v", stmt(t, p, "BilletRuntimeTag").Condition)
	}
	vals, ok := like["aws:RequestTag/sh.billet.owner"].([]string)
	if !ok || len(vals) != 2 || vals[0] != "0f1e2d3c4b5a69788796a5b4c3d2e1f0" || vals[1] != "billet-ami-build-*" {
		t.Errorf("builder CreateTags owner values are %v, want [<owner>, billet-ami-build-*]", vals)
	}
}

// A BUILD'S CREATE-TIME IMAGE TAGS ARE AUTHORIZED, AND WITHOUT THIS A BUILD UNDER
// THIS POLICY IS DENIED OUTRIGHT.
//
// CreateImage carries a TagSpecification (the owner, and the billet that built
// it), and AWS authorizes create-time tags as a SEPARATE ec2:CreateTags check
// keyed on ec2:CreateAction. CreateImage missing from the list denies the whole
// call, so the AMI stamp had been unreachable since it landed — unseen because a
// build run with admin credentials never evaluates this policy.
//
// MEASURED RATHER THAN READ, 2026-08-29. EC2 was asked with --dry-run under a
// role holding this exact policy, against a real instance carrying the builder
// owner tag: without a TagSpecification the call would have succeeded, with one
// it failed UnauthorizedOperation naming ec2:CreateTags, and with this entry
// restored it would have succeeded again. iam:SimulateCustomPolicy agrees on the
// policy half — implicitDeny without, allowed with, and RunInstances tagging
// allowed either way.
//
// AND NOT FOR A NODE, which is the other half: a runtime role holds no
// ec2:CreateImage at all, so granting the tag action for a create it cannot
// perform would be a permission with no call site. Both directions, because a
// condition that listed every action would also "not deny the builder".
func TestOnlyABuilderMayTagAnImageOnCreate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		builder bool
		want    bool
	}{
		{name: "a builder", builder: true, want: true},
		{name: "a node", builder: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Inputs{Owner: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Builder: tc.builder}.Build()
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			equals, ok := stmt(t, p, "BilletRuntimeTag").Condition["StringEquals"].(map[string]any)
			if !ok {
				t.Fatalf("CreateTags carries no StringEquals condition")
			}

			actions, ok := equals["ec2:CreateAction"].([]string)
			if !ok {
				t.Fatalf("ec2:CreateAction is %T, want a list", equals["ec2:CreateAction"])
			}

			if got := slices.Contains(actions, "CreateImage"); got != tc.want {
				t.Errorf("%s allows tagging on CreateImage = %v, want %v (actions: %v)",
					tc.name, got, tc.want, actions)
			}

			// AND THE THREE THAT WERE ALWAYS THERE STAY, so a change that swapped
			// one in for another is visible.
			for _, always := range []string{"RunInstances", "CreateVolume", "CreateSnapshot"} {
				if !slices.Contains(actions, always) {
					t.Errorf("%s no longer allows tagging on %s: %v", tc.name, always, actions)
				}
			}
		})
	}
}

// THE VERIFICATION'S TWO GRANTS REACH ONLY WHAT THIS BUILD MADE.
//
// Console output is whatever a machine printed, and on a job instance that is a
// runner's own log; the contract tag is a claim `billet check` acts on. Both are
// therefore conditioned on the per-build owner tag rather than left on "*" — and
// the promotion's condition is on the RESOURCE tag, which the image carries
// because CreateImage stamped it.
func TestTheVerificationGrantsAreScopedToTheBuilder(t *testing.T) {
	p, err := Inputs{Owner: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Builder: true}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, tc := range []struct {
		sid      string
		action   string
		resource string
	}{
		{sid: "BilletAMIBuilderConsole", action: "ec2:GetConsoleOutput",
			resource: "arn:aws:ec2:*:*:instance/*"},
		// AN AMI ARN HAS AN EMPTY ACCOUNT FIELD, which is not a typo and is what
		// the sibling CreateImage statement already relies on.
		{sid: "BilletAMIBuilderPromote", action: "ec2:CreateTags",
			resource: "arn:aws:ec2:*::image/*"},
	} {
		t.Run(tc.sid, func(t *testing.T) {
			s := stmt(t, p, tc.sid)

			if len(s.Action) != 1 || s.Action[0] != tc.action {
				t.Errorf("%s grants %v, want [%s]", tc.sid, s.Action, tc.action)
			}

			if len(s.Resource) != 1 || s.Resource[0] != tc.resource {
				t.Errorf("%s acts on %v, want [%s]", tc.sid, s.Resource, tc.resource)
			}

			like, ok := s.Condition["StringLike"].(map[string]any)
			if !ok {
				t.Fatalf("%s is not conditioned on the builder's owner tag: %v",
					tc.sid, s.Condition)
			}

			vals, ok := like["aws:ResourceTag/sh.billet.owner"].([]string)
			if !ok || len(vals) != 1 || vals[0] != "billet-ami-build-*" {
				t.Errorf("%s is conditioned on %v, want [billet-ami-build-*]", tc.sid, vals)
			}
		})
	}
}

// A BACKUP POLICY GRANTS NO DELETE, EVER.
//
// billet never issues one — internal/archivestore has no delete at all — so the
// permission would be dead weight on the ONE host that also holds the GitHub App
// private key and the node-wire CA, and the archives it could destroy are the
// copies whose whole purpose is surviving the loss of that host. Retention is
// the bucket's job. This asserts on the generated ACTIONS rather than on a
// reading of the source, so a delete added to the action constants fails here.
func TestABackupPolicyCanNeverDelete(t *testing.T) {
	p, err := Inputs{
		Region: "us-west-2", NoCompute: true,
		Backup: &Backup{
			Bucket: "billet-backups-example", Prefix: "billet-backups",
			KMSKeyARN: "arn:aws:kms:us-west-2:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		},
	}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(p.Statement) == 0 {
		t.Fatal("the policy is empty, so the check below proves nothing")
	}

	for _, s := range p.Statement {
		for _, action := range s.Action {
			if strings.Contains(strings.ToLower(action), "delete") ||
				strings.Contains(strings.ToLower(action), "putobjectretention") {
				t.Errorf("%s grants %s; a backup credential that can destroy its own history "+
					"is not an off-site copy", s.Sid, action)
			}
		}
	}
}

// A CONTROL PLANE THAT LAUNCHES NOTHING GETS NO COMPUTE PERMISSIONS.
//
// The first version of the backup policy handed a standalone controller
// ec2:RunInstances and ec2:TerminateInstances because Build always emitted the
// runtime statements — on the one host in a deployment that holds the App key.
func TestAControlPlaneOnlyPolicyGrantsNoCompute(t *testing.T) {
	p, err := Inputs{
		NoCompute: true,
		Backup:    &Backup{Bucket: "billet-backups-example", Prefix: "billet-backups"},
	}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, s := range p.Statement {
		for _, action := range s.Action {
			if strings.HasPrefix(action, "ec2:") {
				t.Errorf("%s grants %s to a principal that runs no compute", s.Sid, action)
			}
		}
	}

	// AND THE ZERO VALUE STILL MEANS THE NODE POLICY, or every existing caller
	// silently loses the permissions it runs on.
	node, err := Inputs{}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var launches bool

	for _, s := range node.Statement {
		for _, action := range s.Action {
			if action == "ec2:RunInstances" {
				launches = true
			}
		}
	}

	if !launches {
		t.Error("the default policy no longer lets a node launch anything")
	}
}

// A backup prefix or bucket that is not literal is refused.
//
// Both land in an IAM Resource ARN: a `*` widens the grant to every sibling
// prefix, and every sibling prefix is another deployment's App key.
func TestABackupPolicyRefusesAWideningResource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		backup Backup
	}{
		{"a wildcard bucket", Backup{Bucket: "billet-*", Prefix: "billet-backups"}},
		{"a wildcard prefix", Backup{Bucket: "billet-backups", Prefix: "*"}},
		{"an IAM policy variable", Backup{Bucket: "billet-backups", Prefix: "${aws:username}"}},
		{"no bucket", Backup{Prefix: "billet-backups"}},
		{"no prefix", Backup{Bucket: "billet-backups"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (Inputs{NoCompute: true, Backup: &tc.backup}).Build(); err == nil {
				t.Fatal("the generator accepted a resource that widens the grant")
			}
		})
	}
}

// THE GENERATOR MATCHES ITS COMMITTED RENDERING. billet init iam, this drift test,
// and the Terraform module all consume the same policy; if the generator's output
// ever changes, this fails until the committed rendering is regenerated on purpose
// — so a permission cannot be added, removed, reordered or re-conditioned without
// a reviewer seeing the golden diff.
func TestGeneratorMatchesCommittedRendering(t *testing.T) {
	cases := map[string]Inputs{
		"policy-compute-only.json": {},
		"policy-cache.json": {
			Region: "us-west-2",
			Cache:  &Cache{Bucket: "billet-cache-example", Prefix: "production"},
		},
		"policy-full.json": {
			Region: "us-west-2",
			Cache: &Cache{
				Bucket: "billet-cache-example", Prefix: "production",
				KMSKeyARN: "arn:aws:kms:us-west-2:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			},
			SpotQueueARN:           "arn:aws:sqs:us-west-2:123456789012:billet-spot-interruptions",
			InstanceProfileRoleARN: "arn:aws:iam::123456789012:role/billet-node",
			Builder:                true,
		},
		"policy-per-deployment.json": {
			Owner:  "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
			Region: "us-west-2",
			Cache: &Cache{
				Bucket: "billet-cache-example", Prefix: "production",
				KMSKeyARN: "arn:aws:kms:us-west-2:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			},
			SpotQueueARN:           "arn:aws:sqs:us-west-2:123456789012:billet-spot-interruptions",
			InstanceProfileRoleARN: "arn:aws:iam::123456789012:role/billet-node",
			Builder:                true,
		},
		// THE CONTROL PLANE'S OWN POLICY, which is a different principal from the
		// node's: it puts archives somewhere other than the disk it protects and
		// fetches them back, and does nothing with compute at all.
		"policy-backup.json": {
			Region: "us-west-2", NoCompute: true,
			Backup: &Backup{
				Bucket: "billet-backups-example", Prefix: "billet-backups",
				KMSKeyARN: "arn:aws:kms:us-west-2:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			},
		},
		"policy-china.json": {
			Partition: "aws-cn", Region: "cn-north-1",
			Cache: &Cache{
				Bucket: "billet-cache-cn", Prefix: "production",
				KMSKeyARN: "arn:aws-cn:kms:cn-north-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
			},
			SpotQueueARN:           "arn:aws-cn:sqs:cn-north-1:123456789012:billet-spot",
			InstanceProfileRoleARN: "arn:aws-cn:iam::123456789012:role/billet-node",
		},
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := in.Build()
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			got, err := p.JSON()
			if err != nil {
				t.Fatalf("JSON: %v", err)
			}

			// The same regeneration contract tfpolicy has: an intended
			// generator change updates the golden through the test itself, so
			// the diff is reviewed where the change is.
			if os.Getenv("UPDATE_AWS_POLICY") == "1" {
				body := append(append([]byte{}, got...), byte(10))
				if err := os.WriteFile("testdata/"+name, body, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}

				return
			}

			want, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatalf("read golden: %v (regenerate with UPDATE_AWS_POLICY=1)", err)
			}

			if strings.TrimRight(string(want), "\n") != string(got) {
				t.Errorf("the generator no longer matches testdata/%s. If this change is "+
					"intended, regenerate the golden. Got:\n%s", name, got)
			}
		})
	}
}

// THE PER-DEPLOYMENT KMS KEY IS THE CROSS-DEPLOYMENT READ BOUNDARY: every KMS
// grant in the policy must name exactly the deployment's own key, because a
// role that can use ANY other key (or "*") can clone another deployment's
// snapshot and read its cache. The clone-source statement scopes the parent
// snapshot by its owner tag, which in per-deployment mode is the exact value
// and in account-wide mode is only presence — so tags alone leave every billet
// deployment in an account able to clone every other's, and a tag is not a
// secret in either mode. The live half of this claim is measured with
// iam:SimulateCustomPolicy (see the package doc): this pins the
// machine-checkable half, that no statement widens KMS beyond the one key.
func TestPerDeploymentKMSKeyScopesEveryKMSGrant(t *testing.T) {
	const key = "arn:aws:kms:us-west-2:1:key/k"

	p, err := Inputs{
		Owner: "o", Region: "us-west-2",
		Cache: &Cache{Bucket: "b", Prefix: "p", KMSKeyARN: key},
	}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	kmsStatements := 0
	for _, s := range p.Statement {
		hasKMS := false
		for _, a := range s.Action {
			// IAM action matching is case-insensitive, and a wildcard action
			// grants KMS too — both must count, or "KMS:Decrypt" / "*" on a
			// wide Resource would slip past this boundary test.
			la := strings.ToLower(a)
			if strings.HasPrefix(la, "kms:") || la == "*" || strings.HasPrefix(la, "kms*") {
				hasKMS = true
			}
		}
		if !hasKMS {
			continue
		}
		kmsStatements++
		if len(s.Resource) != 1 || s.Resource[0] != key {
			t.Errorf("statement %s grants KMS actions on %v, not exactly the deployment key",
				s.Sid, s.Resource)
		}
	}
	if kmsStatements != 2 {
		t.Errorf("expected the Use and Grant KMS statements, found %d", kmsStatements)
	}
}

// THE PAYLOAD GRANT REACHES BILLET'S OWN OBJECTS AND NOTHING ELSE IN THE BUCKET.
//
// `billet ami build --payload-bucket` stages one archive per build at the bucket
// ROOT, named billet-payload-<digest>-<nonce>.tar.gz, and payloadStager refuses a
// key containing a slash — so the resource can be scoped by that object name
// rather than by a prefix an operator would have to keep clear. Asserted as the
// whole resource, because widening it to `<bucket>/*` is the obvious edit and it
// hands this role every object an operator keeps beside the payloads.
func TestThePayloadGrantIsScopedToBilletsOwnObjects(t *testing.T) {
	p, err := Inputs{
		Owner: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Builder: true,
		Payload: &Payload{Bucket: "billet-ami-payloads"},
	}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	s := stmt(t, p, "BilletAMIBuilderPayload")

	want := []string{"arn:aws:s3:::billet-ami-payloads/billet-payload-*"}
	if !slices.Equal(s.Resource, want) {
		t.Errorf("the payload grant acts on %v, want %v", s.Resource, want)
	}

	// THREE ACTIONS, AND THE READ IS NOT OPTIONAL: the builder hands the guest a
	// PRESIGNED url, which grants no more than its signer holds, so without
	// s3:GetObject the build downloads its own payload and gets a 403.
	wantActions := []string{"s3:PutObject", "s3:GetObject", "s3:DeleteObject"}
	if !slices.Equal(s.Action, wantActions) {
		t.Errorf("the payload grant permits %v, want %v", s.Action, wantActions)
	}

	// AND IT CARRIES NO CONDITION, deliberately: the objects are created by this
	// same role moments earlier and carry no tags, so a tag condition would deny
	// the read-back and the cleanup of billet's own archive.
	if len(s.Condition) != 0 {
		t.Errorf("the payload grant is conditioned on %v; a tag condition here denies "+
			"the builder its own object", s.Condition)
	}
}

// A PAYLOAD BUCKET WITHOUT A BUILDER IS REFUSED RATHER THAN IGNORED.
//
// Nothing but `billet ami build` touches that bucket, so granting it to a plain
// node role widens the identity every job's instance is launched by for a
// command it never runs. Silently dropping it is worse than refusing: the
// operator believes they granted something, and the build fails on a permission
// they think they gave.
func TestAPayloadBucketNeedsABuilder(t *testing.T) {
	_, err := Inputs{
		Owner:   "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
		Payload: &Payload{Bucket: "billet-ami-payloads"},
	}.Build()
	if err == nil {
		t.Fatal("a payload bucket without Builder must be refused")
	}
	if !strings.Contains(err.Error(), "only meaningful for a builder") {
		t.Errorf("the refusal must say why, got %v", err)
	}
}

// A WIDENING BUCKET NAME IS REFUSED, the same rule the backup bucket follows: a
// `*` in an ARN component reaches every bucket sharing the prefix.
func TestAPayloadBucketRefusesAWideningName(t *testing.T) {
	for _, bucket := range []string{"", "billet-*", "billet-payloads/extra"} {
		t.Run(bucket, func(t *testing.T) {
			_, err := Inputs{
				Owner: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Builder: true,
				Payload: &Payload{Bucket: bucket},
			}.Build()
			if err == nil {
				t.Fatalf("bucket %q must be refused", bucket)
			}
		})
	}
}

// A BUILDER WITHOUT A PAYLOAD BUCKET GRANTS NOTHING ON S3, so the ec2-only
// builder grant stays what it was for every deployment whose installers still
// fit in user data.
func TestABuilderWithoutAPayloadBucketTouchesNoS3(t *testing.T) {
	p, err := Inputs{Owner: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Builder: true}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, s := range p.Statement {
		for _, a := range s.Action {
			if strings.HasPrefix(a, "s3:") {
				t.Errorf("%s grants %s without a payload bucket", s.Sid, a)
			}
		}
	}
}
