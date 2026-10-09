package config

import (
	"fmt"
	"slices"
	"strings"
)

// SiteConfig declares one place compute runs.
//
// A STRUCT RATHER THAN A STRING, because a site declares both placement identity
// and its intended storage backend — Ceph at a bare-metal site, EBS and S3 in a
// cloud region. The control plane validates both parts when a remote node
// registers, so split configs cannot create two storage authorities for one
// logical site.
type SiteConfig struct {
	// Name is what a node and a tier refer to this site by.
	Name string `yaml:"name"`
	// Store is the storage local to this site. Ceph serves host-backed compute;
	// EBS snapshots and S3 serve AWS without exposing the home cluster over a WAN.
	Store SiteStoreKind `yaml:"store"`
}

// SiteStoreKind selects the storage implementation local to one site.
type SiteStoreKind string

const (
	// SiteStoreCeph is an RBD cluster on the site's own storage network.
	SiteStoreCeph SiteStoreKind = "ceph"
	// SiteStoreEBSS3 stores block generations in EBS and fenced state in S3.
	SiteStoreEBSS3 SiteStoreKind = "ebs-s3"
)

// Valid reports whether this is a recognized storage backend name.
func (s SiteStoreKind) Valid() bool {
	return s == SiteStoreCeph || s == SiteStoreEBSS3
}

// validateSites checks the declared places, and everything that refers to one.
//
// FAIL CLOSED ON A NAME THAT WAS NEVER DECLARED, which is the whole reason a
// site is a declared block. A free string cannot tell a typo from a new place,
// so "hom" would become a site of its own with an empty cache, and every job
// there would run cold while the deployment looked healthy. There is no signal
// after startup that says which of those two an operator meant, so this is the
// only moment it can be caught.
func (c *Config) validateSites() []error {
	var errs []error

	declared := make(map[string]bool, len(c.Sites))

	// refused holds the name an operator MEANT by a declaration billet turned
	// down for its padding, so a reference to it is not reported a second time
	// as undeclared. Without it, one stray space produced a diagnostic saying
	// "this config declares no sites" about a config that declares one, which
	// sends the operator to add a block they already wrote.
	//
	// The name still does not enter declared: nothing may validate against a
	// spelling no node can ever present.
	refused := make(map[string]bool)

	for i, s := range c.Sites {
		// THE NAME IS TAKEN EXACTLY AS WRITTEN, and this loop used to trim it.
		// That trim was the bug: nothing wrote the trimmed value back, so
		// validation authorised `home` while nodeplane.WithSites keyed its
		// authority map — and alloc's placement compared — the padded original.
		// A site declared as " home " therefore accepted `tiers[].site: home` at
		// load and then refused every node reporting `home`, permanently.
		name := s.Name

		if strings.TrimSpace(name) == "" {
			errs = append(errs, fmt.Errorf("sites[%d]: a site must have a name; nothing can "+
				"refer to one without it", i))

			continue
		}

		switch err := checkIdentityPadding(fmt.Sprintf("sites[%d]: site name", i), name); {
		case err != nil:
			errs = append(errs, err)
			refused[strings.TrimSpace(name)] = true
		case declared[name]:
			errs = append(errs, fmt.Errorf("sites[%d]: site %q is declared twice; a site is "+
				"where a cache lives, so two answers to that is one too many", i, name))

			continue
		default:
			declared[name] = true
		}

		// THE STORE IS ITS OWN FIELD AND ITS OWN MISTAKE. A padded name does not
		// make `store: magic-disk` any less wrong, and reporting them one load
		// at a time costs an edit cycle for a file whose problems were both
		// visible at once — which is the thing Validate exists to avoid.
		if !s.Store.Valid() {
			errs = append(errs, fmt.Errorf("sites[%d] (%s): store %q is not one of ceph or "+
				"ebs-s3; storage is selected per site and cannot be inferred from whichever "+
				"node registered first", i, name, s.Store))
		}
	}

	// NODE.SITE IS NOT CHECKED HERE, and that is not an omission.
	//
	// This file may be the NODE's, on another machine, where a sites block has no
	// reason to exist — sites are the control plane's to declare. Checking it
	// against a local block would refuse exactly the deployment this feature is
	// for: a node that correctly names one of the server's places, in a config
	// that has never heard of them.
	//
	// The claim is checked where the answer lives, when the node registers. See
	// nodeplane.WithSites.
	for i := range c.Tiers {
		where := fmt.Sprintf("tiers[%d] (%s): site", i, c.Tiers[i].Label)

		// A SITED TIER CAN NEVER REACH A CODEBUILD NODE, AND THE SYMPTOM IS SILENCE.
		// Placement confines a sited tier to hosts AT that site, and a codebuild
		// node declares none (see validateCodeBuildNode) — so a tier listing
		// codebuild beside a site has a provider that is never eligible for it.
		// Nothing refuses that at runtime: the fallback simply never fires, and a
		// tier whose only provider is codebuild advertises 0 while `billet check`
		// reports everything healthy. Measured on the first live acceptance run,
		// where the job queued with no line saying why.
		if c.Tiers[i].Site != "" &&
			slices.Contains(c.Tiers[i].AcceptableProviders(), ProviderCodeBuild) {
			errs = append(errs, fmt.Errorf("%s %q confines this tier to nodes at that site, "+
				"but its providers include codebuild, and a codebuild node cannot declare a "+
				"site (a build attaches to no cache authority) — so codebuild could never be "+
				"placed for this tier and the fallback would silently never fire; drop the "+
				"site or drop codebuild from providers", where, c.Tiers[i].Site))
		}

		// Padding first, and instead of the membership check: a padded reference
		// is not a declared site either, and reporting both sends the operator
		// looking for a missing sites entry when the answer is two spaces.
		if err := checkIdentityPadding(where, c.Tiers[i].Site); err != nil {
			errs = append(errs, err)

			continue
		}

		// The same rule from the declaration's side: this tier names the site the
		// operator meant, and the reason it does not exist is already reported
		// against the declaration that has the space in it.
		if refused[c.Tiers[i].Site] {
			continue
		}

		errs = append(errs, siteRefError(where, c.Tiers[i].Site, declared, len(c.Sites) > 0)...)
	}

	return errs
}

// siteRefError checks one reference to a site.
//
// AN EMPTY AUTHORITY SET IS TWO DIFFERENT SITUATIONS, and telling an operator
// the wrong one sends them to the wrong edit. `declared` holds the names that
// came through validation intact, so it is empty both when the file has no
// sites block at all and when it has one whose every entry was refused — for a
// blank name or for surrounding whitespace. Using its length as a proxy for the
// first answered "this config declares no sites; add a sites block" about a
// config that has one, which is advice to write something already written.
//
// declaredAny is therefore passed separately: it says whether the file HAS a
// block, which is the question the first branch is actually asking.
func siteRefError(where, site string, declared map[string]bool, declaredAny bool) []error {
	if site == "" {
		return nil
	}

	switch {
	case !declaredAny:
		return []error{fmt.Errorf("%s is %q, but this config declares no sites; add a sites "+
			"block naming it", where, site)}

	// The block exists and nothing usable came out of it, so every entry has
	// already produced its own diagnostic. Pointing at those is the actionable
	// answer: this reference may well be right once they are fixed.
	case len(declared) == 0:
		return []error{fmt.Errorf("%s is %q, and this config's sites block declares no usable "+
			"name — every entry in it was refused above. Fix those; this reference may already "+
			"be correct", where, site)}

	case !declared[site]:
		return []error{fmt.Errorf("%s is %q, which is not a declared site (have %s)",
			where, site, strings.Join(sortedKeys(declared), ", "))}
	}

	return nil
}
