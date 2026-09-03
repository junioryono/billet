package awspolicy

import (
	"strings"
	"testing"
)

// identityPolicy builds a control-plane policy carrying only the identity
// statements.
func identityPolicy(t *testing.T, in Inputs) Policy {
	t.Helper()

	p, err := in.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return p
}

// THE IDENTITY GRANT READS AND WRITES, AND NEVER DELETES.
//
// A controller reads the App key on every token mint and the authority at every
// start, and writes the authority when it rotates one. Deleting either is an
// operator's act taken knowing what it is — the same reasoning that keeps
// s3:DeleteObject off the backup grant, and sharper here, because one of these
// values is a credential GitHub will not re-issue.
func TestTheIdentityGrantCannotDeleteAParameter(t *testing.T) {
	p := identityPolicy(t, Inputs{
		NoCompute: true,
		Identity:  &Identity{Prefix: "/billet/prod"},
	})

	for _, s := range p.Statement {
		for _, action := range s.Action {
			if strings.Contains(strings.ToLower(action), "delete") {
				t.Errorf("%s grants %s; billet never deletes its own identity material",
					s.Sid, action)
			}
		}
	}
}

// AND IT IS SCOPED TO ONE PREFIX.
//
// Every sibling path is another deployment's App key and another deployment's
// certificate authority, so a resource reaching one of them is not a widened
// grant — it is a different deployment's identity.
func TestTheIdentityGrantIsScopedToOneDeploymentsPrefix(t *testing.T) {
	p := identityPolicy(t, Inputs{
		NoCompute: true,
		Identity:  &Identity{Prefix: "/billet/prod"},
	})

	var found bool

	for _, s := range p.Statement {
		if s.Sid != "BilletIdentityParameters" {
			continue
		}

		found = true

		for _, r := range s.Resource {
			// THE LEADING SLASH IS DROPPED FROM THE ARN, and getting that wrong is
			// silent: `parameter//billet/...` matches nothing, so the policy grants
			// no access at all and the failure is a control plane that cannot read
			// its own key.
			if strings.Contains(r, "parameter//") {
				t.Errorf("%s has a doubled separator and would match no parameter: %s", s.Sid, r)
			}

			if !strings.HasSuffix(r, ":parameter/billet/prod/*") {
				t.Errorf("%s is scoped to %q, which is not this deployment's prefix", s.Sid, r)
			}
		}
	}

	if !found {
		t.Fatal("an identity policy rendered no parameter statement")
	}
}

// A RELATIVE PREFIX IS REFUSED RATHER THAN NORMALISED.
//
// A relative Parameter Store name is a DIFFERENT parameter, so a policy built
// from one grants access to something billet does not use — and the failure is a
// control plane that cannot read its own identity, reported as an AWS denial that
// names neither.
func TestARelativeIdentityPrefixIsRefused(t *testing.T) {
	_, err := Inputs{NoCompute: true, Identity: &Identity{Prefix: "billet/prod"}}.Build()
	if err == nil {
		t.Fatal("a relative identity prefix was accepted")
	}

	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("the refusal should say the prefix is absolute; got: %v", err)
	}
}

// A WILDCARD IN THE PREFIX IS REFUSED, because it is the boundary.
func TestAWildcardIdentityPrefixIsRefused(t *testing.T) {
	if _, err := (Inputs{
		NoCompute: true, Identity: &Identity{Prefix: "/billet/*"},
	}).Build(); err == nil {
		t.Fatal("a wildcard identity prefix was accepted")
	}
}

// AND THE KMS GRANT IS USABLE ONLY THROUGH SSM.
//
// A compromised control-plane role must not be able to use the key that encrypts
// this deployment's identity on anything else.
func TestTheIdentityKMSGrantIsConfinedToSSM(t *testing.T) {
	const key = "arn:aws:kms:us-west-2:111122223333:key/abc"

	p := identityPolicy(t, Inputs{
		NoCompute: true,
		Region:    "us-west-2",
		Identity:  &Identity{Prefix: "/billet/prod", KMSKeyARN: key},
	})

	var found bool

	for _, s := range p.Statement {
		if s.Sid != "BilletIdentityKMSUse" {
			continue
		}

		found = true

		eq, ok := s.Condition["StringEquals"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no StringEquals condition: %+v", s.Sid, s.Condition)
		}

		if via := eq["kms:ViaService"]; via != "ssm.us-west-2.amazonaws.com" {
			t.Errorf("%s is confined to %v, want ssm.us-west-2.amazonaws.com", s.Sid, via)
		}
	}

	if !found {
		t.Fatal("a KMS-encrypted identity store rendered no key statement")
	}
}

// AND A CUSTOMER-MANAGED KEY WITHOUT A REGION IS REFUSED.
//
// The condition needs one, and rendering the statement without it would produce a
// grant that is confined to nothing.
func TestAnIdentityKMSKeyWithoutARegionIsRefused(t *testing.T) {
	if _, err := (Inputs{
		NoCompute: true,
		Identity: &Identity{
			Prefix:    "/billet/prod",
			KMSKeyARN: "arn:aws:kms:us-west-2:111122223333:key/abc",
		},
	}).Build(); err == nil {
		t.Fatal("a KMS-encrypted identity store was accepted with no region")
	}
}
