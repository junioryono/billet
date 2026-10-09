package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// CephConfig points this host at the Ceph cluster its site keeps.
//
// STORAGE BELONGS TO THE SITE, NOT TO THE COMPUTE BACKEND, which is why this is a
// sibling of the firecracker block rather than a field inside it. Golden images,
// per-job root clones and every cache in the plane are RBD images in one cluster,
// and the code that mounts a cache is not the code that boots a microVM. What
// lives here is the half a HOST holds: where the monitors are, who this machine
// authenticates as, and which pools that identity may use.
//
// It replaced a `zfs_pool` key. A ZFS clone exists only on the machine that took
// it, so a cache written to one pinned every repository to the host that first
// built it — the storage half of billet being a one-machine product. RBD presents
// the same NVMe as a pool any node at the site can map.
type CephConfig struct {
	// ConfPath is the ceph.conf naming this cluster's monitors. Empty means Ceph's
	// own search path, which finds /etc/ceph/ceph.conf.
	ConfPath string `yaml:"conf_path,omitempty"`

	// User is the RADOS identity billet authenticates as, WITHOUT the `client.`
	// prefix.
	//
	// DEFAULTS TO billet RATHER THAN admin, which is what the rbd command picks on
	// its own. An admin key can delete a pool, so defaulting to it would make a
	// compromised node able to destroy the cluster it caches in rather than the
	// images it was handed — and it would do so silently, because everything works.
	User string `yaml:"user,omitempty"`

	// KeyringPath holds that identity's secret. Empty means Ceph's own search path,
	// which finds /etc/ceph/ceph.<user>.keyring.
	KeyringPath string `yaml:"keyring_path,omitempty"`

	// ImagePool holds golden images and the per-job root clones taken from them.
	ImagePool string `yaml:"image_pool"`

	// CachePool holds cache volumes: sticky disks, build state, the layer cache.
	//
	// SEPARATE FROM ImagePool, and validation refuses one name for both. Not for
	// tuning — RBD's object size is a per-IMAGE property, so the reason ZFS wanted
	// two datasets does not survive the move. It is about blast radius: a cache is
	// disposable and a golden image is not, so "throw the cache away" has to be
	// something an operator can do to a whole pool without taking the images with
	// it, and a garbage collector walking one pool must not be able to reach the
	// other.
	CachePool string `yaml:"cache_pool"`
}

// validateCephNode reports everything wrong with this host's storage.
//
// TWO DIRECTIONS, because both mistakes are silent. A firecracker node with no
// ceph block has nowhere to put a golden image or a cache, and would fail on the
// first launch with a message about a missing rootfs rather than about the
// storage that was never configured. A node of any other backend WITH one has
// written down a cluster nothing will ever read.
func (c *Config) validateCephNode() []error {
	// A PROVIDER THAT IS NOT A PROVIDER GETS ONE DIAGNOSTIC, not two. validateNode
	// already refuses an unknown backend by name, and a second error telling the
	// operator that their typo "cannot attach a block device" is billet asserting
	// something about a string it does not recognise. validateGuestOSRules skips
	// its relational checks on an invalid enum for the same reason.
	if !c.Node.Provider.Valid() {
		return nil
	}

	if c.Node.Provider != ProviderFirecracker {
		if c.Node.Ceph != nil {
			// NAMED, NOT CHARACTERISED. Firecracker is the only backend that reads
			// this block, so anything else would carry a cluster nothing consults —
			// and the reasons differ per backend, so the message gives the one that
			// applies rather than a sentence about containers that is untrue of tart.
			return []error{fmt.Errorf("node.ceph is set but this node's provider is %s, and only "+
				"firecracker reads it (%s), so this host would carry a cluster nothing "+
				"consults", c.Node.Provider, cephRefusalReason(c.Node.Provider))}
		}

		return nil
	}

	if c.Node.Ceph == nil {
		return []error{errors.New("node.ceph is required when provider is firecracker: golden " +
			"images, per-job root clones and every cache are RBD images, so a firecracker host " +
			"with no cluster has nothing to boot a guest from")}
	}

	return CheckCeph(*c.Node.Ceph)
}

// cephRefusalReason says why THIS backend has no use for a storage block.
//
// One clause per backend rather than one sentence covering all of them: a
// container really does have nowhere to attach a block device, an ec2 node's
// compute really is in a region that cannot reach the cluster, and neither is
// true of tart, which simply does not read this yet.
func cephRefusalReason(p ProviderKind) string {
	switch p {
	case ProviderDocker:
		return "a container has nowhere to attach a block device"
	case ProviderEC2:
		return "an ec2 node's compute runs in a region that cannot reach this cluster"
	case ProviderCodeBuild:
		return "a codebuild build has nowhere to attach a block device and runs in a region " +
			"that cannot reach this cluster"
	case ProviderTart, ProviderFirecracker:
		return "nothing in that backend reads it"
	default:
		return "nothing in that backend reads it"
	}
}

// normalize fills in the identity billet authenticates as and trims what it later
// passes to the rbd client verbatim.
//
// THE SAME REASON EC2Config IS NORMALIZED. Validation trimmed to check and the
// caller used the raw string, so `image_pool: "  tank  "` passed the shape check
// and was then handed to Ceph with its padding. YAML strips whitespace from a
// plain scalar but keeps it inside quotes, so an ordinary-looking file reaches it.
func (p *CephConfig) normalize() {
	if p == nil {
		return
	}

	p.ConfPath = strings.TrimSpace(p.ConfPath)
	p.User = strings.TrimSpace(p.User)
	p.KeyringPath = strings.TrimSpace(p.KeyringPath)
	p.ImagePool = strings.TrimSpace(p.ImagePool)
	p.CachePool = strings.TrimSpace(p.CachePool)

	if p.User == "" {
		p.User = DefaultCephUser
	}
}

// DefaultCephUser is the RADOS identity billet authenticates as when the config
// names none. Deliberately not `admin` — see CephConfig.User.
const DefaultCephUser = "billet"

// DefaultCephConf is where Ceph's own search path finds a cluster's monitors, and
// therefore where billet is talking to when node.ceph.conf_path says nothing.
const DefaultCephConf = "/etc/ceph/ceph.conf"

// ConfPathOrDefault names the file this configuration reaches the cluster
// through, so an operator reading `billet check` can see WHICH cluster answered.
//
// An empty conf_path is not "no file" — it is Ceph's search path, which finds
// DefaultCephConf. Printing the empty string there tells an operator with two
// clusters nothing at all.
func (p *CephConfig) ConfPathOrDefault() string {
	if p == nil || p.ConfPath == "" {
		return DefaultCephConf
	}

	return p.ConfPath
}

// CheckCeph refuses a storage block billet cannot safely act on.
//
// EXPORTED AND CALLED FROM BOTH SIDES, like CheckEC2Endpoint and for the same
// reason: the client's constructor is exported and cannot assume its
// configuration came through Load. A rule enforced in only one of the two has a
// second entry point that does not enforce it.
//
// It takes a VALUE, so a caller cannot hand it a nil pointer and read the empty
// result as approval.
func CheckCeph(p CephConfig) []error {
	var errs []error

	// PADDING IS REFUSED HERE RATHER THAN TRIMMED, because this function decides
	// and the CALLER executes. Load has already normalized, so a padded value
	// reaching this point came from a caller that did not — and trimming only for
	// the decision is the exact defect the ec2 block shipped with: `region: "  x  "`
	// passed the shape check and was then signed with its padding. Every field is
	// listed, so adding one to the struct without adding it here is visible.
	for _, f := range []struct{ field, value string }{
		{"user", p.User},
		{"image_pool", p.ImagePool},
		{"cache_pool", p.CachePool},
		{"conf_path", p.ConfPath},
		{"keyring_path", p.KeyringPath},
	} {
		if f.value != strings.TrimSpace(f.value) {
			errs = append(errs, fmt.Errorf("node.ceph.%s %q has leading or trailing whitespace; "+
				"billet passes it to the rbd client exactly as written", f.field, f.value))
		}

		// A NUL CANNOT BE CARRIED BY AN ARGV AT ALL. YAML can encode one, and it
		// passes every shape check here — including filepath.IsAbs — so without this
		// billet accepts a configuration that exec refuses before rbd ever starts,
		// with an error naming neither the field nor the byte.
		if strings.IndexByte(f.value, 0) >= 0 {
			errs = append(errs, fmt.Errorf("node.ceph.%s contains a NUL byte, which cannot be "+
				"passed to a command at all", f.field))
		}
	}

	// admin IS THE DEFAULT THE rbd COMMAND WOULD PICK, which is what makes naming
	// it here worth an error rather than a comment. An admin key can delete a pool;
	// a node holding one turns "this host was compromised" into "the site's storage
	// was deleted", and nothing about the deployment looks different until then.
	//
	// An EMPTY user is refused for the same reason rather than defaulted here: a
	// config loaded through Load has already been given DefaultCephUser, so an empty
	// one reaching this point came from a caller that skipped normalization — and
	// handing rbd no identity makes it pick client.admin.
	if user := strings.TrimSpace(p.User); user == "" || user == "admin" {
		errs = append(errs, fmt.Errorf("node.ceph.user is %q: billet will not authenticate to a "+
			"cluster as an administrator, because that key can delete the pools this node only "+
			"needs to read and clone; create a scoped identity with `ceph auth get-or-create "+
			"client.%s mon 'profile rbd' osd 'profile rbd pool=<images>, profile rbd "+
			"pool=<cache>'`", p.User, DefaultCephUser))
	} else {
		errs = append(errs, checkCephIdentity(user)...)
	}

	errs = append(errs, checkCephPool("image_pool", p.ImagePool)...)
	errs = append(errs, checkCephPool("cache_pool", p.CachePool)...)

	// ONE NAME FOR BOTH is refused rather than merged. A cache is disposable and a
	// golden image is not: eviction has to be able to walk the cache pool, and
	// "delete everything in it" has to stay a thing an operator can do, neither of
	// which survives the images living there too.
	if p.ImagePool != "" && p.ImagePool == p.CachePool {
		errs = append(errs, fmt.Errorf("node.ceph.image_pool and node.ceph.cache_pool are both "+
			"%q: golden images are rebuilt from a recipe and caches are thrown away, so they "+
			"cannot share a pool that eviction is allowed to walk", p.ImagePool))
	}

	errs = append(errs, checkCephPath("conf_path", p.ConfPath)...)
	errs = append(errs, checkCephPath("keyring_path", p.KeyringPath)...)

	return errs
}

// checkCephIdentity refuses an identity that would authenticate as something else.
//
// ONE RULE, AND IT IS THE ONLY ONE THAT SURVIVED BEING MEASURED. rbd prefixes the
// value of --id with `client.` itself, so `--id client.billet` asks the cluster
// about `client.client.billet`: run against a working deployment it answers
// `(13) Permission denied` while the plain form lists the pool, and the error names
// an entity the operator never created.
//
// The shapes that were refused alongside it are not refused any more, because the
// reasons given for them were reasoning rather than measurement — the mistake this
// repo has already made once, with the runner-group validator. `rbd --id -weird`
// does NOT read the value as an option: --id requires a value and program_options
// consumes the next token whatever it starts with, so a leading dash is addressable.
// Whitespace and slashes are addressable too — `ceph auth get-or-create` accepts
// `client.a/b` and `client.a b`, and each is a single argv element. A pool name is
// different, and checkCephPool says why.
func checkCephIdentity(user string) []error {
	if strings.HasPrefix(user, "client.") {
		return []error{fmt.Errorf("node.ceph.user %q carries the `client.` prefix, which rbd adds "+
			"itself: billet would authenticate as client.%s, which is not an entity your cluster "+
			"has", user, user)}
	}

	return nil
}

// checkCephPool refuses a pool name billet cannot address.
//
// EVERY RULE HERE IS PINNED TO MEASURED BEHAVIOUR, because Ceph is more permissive
// than it looks: it creates pools named `a/b`, `a@b`, `a b` and `a\tb` without
// complaint. So the question is never "is this a legal pool name", it is "does
// billet address it correctly", and the answers came from running rbd rather than
// from reading it.
//
//   - `/` and `@` — a pool name is only half of what billet builds. Images are
//     addressed as `pool/image` and snapshots as `pool/image@snap`, so either
//     character makes the spec parse somewhere else.
//   - a leading `-` — and NOT because of `-p`, which consumes the next token
//     whatever it starts with. Those specs are POSITIONAL arguments:
//     `rbd info -weirdpool/nothing` answers `unrecognised option
//     '-weirdpool/nothing'` where the same call on an ordinary pool reaches the
//     image.
//   - a leading `.` — refused by Ceph itself ("pool names beginning with . are not
//     allowed"), so this is billet saying so at load rather than at first launch.
//
// Interior whitespace was refused too and is not any more: it is addressable at
// every layer measured, and a rule whose stated reason is untrue is worse than no
// rule. Padding is a different matter and CheckCeph refuses it.
func checkCephPool(field, pool string) []error {
	if pool == "" {
		return []error{fmt.Errorf("node.ceph.%s is required", field)}
	}

	for _, bad := range []struct {
		what   string
		reason string
	}{
		{"/", "billet addresses an image as pool/image, so a slash points at a different pool"},
		{"@", "billet addresses a snapshot as pool/image@snap, so an @ starts a snapshot name"},
	} {
		if strings.Contains(pool, bad.what) {
			return []error{fmt.Errorf("node.ceph.%s %q contains %q: %s", field, pool, bad.what, bad.reason)}
		}
	}

	if strings.HasPrefix(pool, ".") {
		return []error{fmt.Errorf("node.ceph.%s %q begins with a dot, which ceph reserves for its "+
			"own pools such as .mgr and refuses to create", field, pool)}
	}

	if strings.HasPrefix(pool, "-") {
		return []error{fmt.Errorf("node.ceph.%s %q begins with a dash: billet addresses images as "+
			"positional pool/image arguments, and rbd reads one starting with a dash as an "+
			"option it does not recognise", field, pool)}
	}

	return nil
}

// checkCephPath refuses a relative override.
//
// A NODE IS A SERVICE, and a service's working directory is not the one the
// operator was standing in when they wrote the config. A relative ceph.conf
// resolves against `/` under systemd, so it is found while testing by hand and
// missing in production — which surfaces as a cluster that cannot be reached
// rather than as a path that was wrong.
func checkCephPath(field, path string) []error {
	if path == "" {
		return nil
	}

	if !filepath.IsAbs(path) {
		return []error{fmt.Errorf("node.ceph.%s %q is relative; a node runs as a service, whose "+
			"working directory is not where you wrote this file", field, path)}
	}

	return nil
}
