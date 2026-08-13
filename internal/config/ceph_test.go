package config

import (
	"strings"
	"testing"
)

// cephBlock is the storage section validConfig carries, so a case can remove or
// replace the whole thing rather than patching it line by line.
const cephBlock = `  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
`

// withoutCeph is validConfig with no storage at all.
func withoutCeph(t *testing.T) string {
	t.Helper()

	body := strings.Replace(validConfig, cephBlock, "", 1)
	if strings.Contains(body, "ceph:") {
		t.Fatal("the ceph block in validConfig has changed, so this case removes nothing")
	}

	return body
}

// withCeph is validConfig with its storage section replaced.
func withCeph(t *testing.T, replacement string) string {
	t.Helper()

	body := strings.Replace(validConfig, cephBlock, replacement, 1)
	if body == validConfig {
		t.Fatal("the ceph block in validConfig has changed, so this case patches nothing")
	}

	return body
}

// A HOST THAT BOOTS MICROVMS NEEDS SOMEWHERE TO BOOT THEM FROM.
//
// Golden images, the per-job root clone taken from one, and every cache are RBD
// images, so a firecracker node with no cluster has nothing to launch. Refusing
// at load is the difference between one diagnostic naming the missing section and
// a first job that fails on an absent rootfs.
func TestAFirecrackerNodeWithoutStorageIsRefused(t *testing.T) {
	t.Parallel()

	_, err := Load(writeConfig(t, withoutCeph(t)))
	if err == nil {
		t.Fatal("Load accepted a firecracker node with no ceph section")
	}

	if !strings.Contains(err.Error(), "node.ceph is required") {
		t.Errorf("the error does not name the missing section: %v", err)
	}
}

// STORAGE NOTHING WILL READ IS REFUSED, NOT IGNORED.
//
// A container has nowhere to attach a block device and an ec2 node's compute runs
// in a region that cannot reach this cluster, so a ceph block on either is a
// deployment that believes it has a cache and does not. Silence there is the
// worse failure: everything validates, every job runs, and every one of them runs
// cold.
func TestStorageOnABackendThatCannotAttachOneIsRefused(t *testing.T) {
	t.Parallel()

	// DERIVED FROM THE PROVIDER LIST, not written out. A guard listing the
	// backends that exist today passes a test that lists the same ones, and the
	// next backend added would inherit acceptance from a switch written before it
	// existed — which is the mistake provider.Classify already exists to avoid.
	for _, kind := range allProviders {
		if kind == ProviderFirecracker {
			continue
		}

		provider := string(kind)

		t.Run(provider, func(t *testing.T) {
			t.Parallel()

			// The node's provider only, not every tier's: a tier declaring a backend
			// no host runs is a deployment that has not finished being written, and
			// the refusal under test is about the NODE section.
			body := strings.Replace(validConfig,
				"  provider: firecracker\n", "  provider: "+provider+"\n", 1)
			if !strings.Contains(body, "  provider: "+provider+"\n") || !strings.Contains(body, cephBlock) {
				t.Fatal("the node block in validConfig has changed, so this case patches nothing")
			}

			_, err := Load(writeConfig(t, body))
			if err == nil {
				t.Fatalf("Load accepted a %s node carrying a ceph section", provider)
			}

			if !strings.Contains(err.Error(), "node.ceph is set") {
				t.Errorf("the error does not name the field: %v", err)
			}
		})
	}
}

// BILLET DOES NOT AUTHENTICATE TO A CLUSTER AS AN ADMINISTRATOR.
//
// `admin` is what the rbd command picks on its own when nothing names an
// identity, which is exactly what makes it worth an error rather than a comment:
// an admin key can delete a pool, so a node holding one turns "this host was
// compromised" into "the site's storage is gone", and nothing about the
// deployment looks different until that day.
//
// Both halves are asserted, because a default that quietly resolved to admin
// would satisfy a test that only checked the explicit case.
func TestTheDefaultCephIdentityIsNotAnAdministrator(t *testing.T) {
	t.Parallel()

	t.Run("unset defaults to billet", func(t *testing.T) {
		t.Parallel()

		cfg, err := Load(writeConfig(t, validConfig))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if got := cfg.Node.Ceph.User; got != DefaultCephUser {
			t.Errorf("node.ceph.user = %q, want %q", got, DefaultCephUser)
		}
	})

	t.Run("admin is refused", func(t *testing.T) {
		t.Parallel()

		body := withCeph(t, cephBlock+"    user: admin\n")

		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Fatal("Load accepted an admin identity")
		}

		// The diagnostic, not the shape: an operator who is told only "invalid
		// user" has no reason to believe billet rather than their own config.
		if !strings.Contains(err.Error(), "can delete the pools") {
			t.Errorf("the error does not say why an admin key is refused: %v", err)
		}

		if !strings.Contains(err.Error(), "ceph auth get-or-create") {
			t.Errorf("the error does not say how to make a scoped identity: %v", err)
		}
	})
}

// A CACHE IS THROWN AWAY AND A GOLDEN IMAGE IS NOT.
//
// Eviction walks the cache pool, and "delete the cache" has to stay something an
// operator can do to a whole pool. Neither survives the images living there too,
// so one name for both is refused rather than merged.
func TestImagesAndCachesCannotShareAPool(t *testing.T) {
	t.Parallel()

	body := withCeph(t, `  ceph:
    image_pool: billet
    cache_pool: billet
`)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted one pool serving as both")
	}

	if !strings.Contains(err.Error(), "cannot share a pool") {
		t.Errorf("the error does not explain the refusal: %v", err)
	}
}

// A POOL NAME IS HALF OF A STRING BILLET BUILDS, so what matters is whether the
// whole string still addresses what it meant to.
//
// Every case is measured rather than reasoned: Ceph will happily create `a/b`,
// `a@b` and `a b`, so the refusals are about billet's own `pool/image` and
// `pool/image@snap` specs — plus a leading dot, which Ceph itself refuses, and a
// leading dash, which rbd reads as an unrecognised option when the spec is
// positional.
func TestAPoolNameBilletCannotAddressIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		pool string
		want string
	}{
		{name: "empty", pool: "", want: "is required"},
		{name: "a slash", pool: "billet/images", want: "different pool"},
		{name: "a snapshot separator", pool: "billet@images", want: "snapshot name"},
		{name: "ceph's own namespace", pool: ".mgr", want: "reserves for its"},
		{name: "something rbd reads as an option", pool: "-p", want: "starting with a dash"},
		{name: "any leading dash, not the fixture", pool: "-billet-images", want: "starting with a dash"},
		{name: "any of ceph's own namespace", pool: ".billet", want: "reserves for its"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			errs := CheckCeph(CephConfig{
				User:      DefaultCephUser,
				ImagePool: tc.pool,
				CachePool: "billet-cache",
			})
			if len(errs) == 0 {
				t.Fatalf("CheckCeph accepted image_pool %q", tc.pool)
			}

			joined := joinErrors(errs)
			if !strings.Contains(joined, tc.want) {
				t.Errorf("the error does not say why %q is refused: %s", tc.pool, joined)
			}

			if !strings.Contains(joined, "image_pool") {
				t.Errorf("the error does not name the field: %s", joined)
			}
		})
	}
}

// BOTH POOLS ARE CHECKED, not only the first one written.
//
// The rules are applied by one helper called twice, which is exactly the shape
// that ships with the second call missing — and a cache pool nobody validated is
// the one billet writes to on every job.
func TestTheCachePoolIsCheckedToo(t *testing.T) {
	t.Parallel()

	errs := CheckCeph(CephConfig{
		User:      DefaultCephUser,
		ImagePool: "billet-images",
		CachePool: "billet/cache",
	})
	if len(errs) == 0 {
		t.Fatal("CheckCeph accepted a cache pool with a slash in it")
	}

	if joined := joinErrors(errs); !strings.Contains(joined, "cache_pool") {
		t.Errorf("the error does not name the field: %s", joined)
	}
}

// A NODE RUNS AS A SERVICE, whose working directory is not the one the operator
// was standing in.
//
// A relative ceph.conf resolves against `/` under systemd, so it is found while
// testing by hand and missing in production — and the symptom is a cluster that
// cannot be reached rather than a path that was wrong.
func TestARelativeCephPathIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, block, field string }{
		{
			name:  "conf_path",
			block: cephBlock + "    conf_path: ceph.conf\n",
			field: "conf_path",
		},
		{
			name:  "keyring_path",
			block: cephBlock + "    keyring_path: keys/billet.keyring\n",
			field: "keyring_path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(writeConfig(t, withCeph(t, tc.block)))
			if err == nil {
				t.Fatalf("Load accepted a relative %s", tc.field)
			}

			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the error does not name the field: %v", err)
			}

			if !strings.Contains(err.Error(), "runs as a service") {
				t.Errorf("the error does not say why a relative path is refused: %v", err)
			}
		})
	}
}

// AN ABSOLUTE PATH IS ACCEPTED, which is the other direction of the rule above.
//
// A guard tested in one direction only proves the config was refused, not that it
// was refused for the stated reason — a check that rejected every path would pass
// the test above unchanged.
func TestAnAbsoluteCephPathIsAccepted(t *testing.T) {
	t.Parallel()

	body := withCeph(t, cephBlock+
		"    conf_path: /etc/ceph/ceph.conf\n"+
		"    keyring_path: /etc/ceph/ceph.client.billet.keyring\n")

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load refused absolute paths: %v", err)
	}

	if got := cfg.Node.Ceph.ConfPath; got != "/etc/ceph/ceph.conf" {
		t.Errorf("conf_path = %q", got)
	}
}

// WHAT IS CHECKED AND WHAT IS USED MUST BE THE SAME STRING.
//
// YAML strips whitespace from a plain scalar and keeps it inside quotes, so an
// ordinary-looking file reaches Ceph with a padded pool name — which validation
// trimmed to check and then never wrote back. The same defect the ec2 block had.
func TestCephValuesAreTrimmed(t *testing.T) {
	t.Parallel()

	body := withCeph(t, `  ceph:
    user: "  billet  "
    image_pool: "\tbillet-images \n"
    cache_pool: "  billet-cache  "
    conf_path: "  /etc/ceph/ceph.conf  "
    keyring_path: "  /etc/ceph/ceph.client.billet.keyring  "
`)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, tc := range []struct{ field, got, want string }{
		{"user", cfg.Node.Ceph.User, "billet"},
		{"image_pool", cfg.Node.Ceph.ImagePool, "billet-images"},
		{"cache_pool", cfg.Node.Ceph.CachePool, "billet-cache"},
		{"conf_path", cfg.Node.Ceph.ConfPath, "/etc/ceph/ceph.conf"},
		{"keyring_path", cfg.Node.Ceph.KeyringPath, "/etc/ceph/ceph.client.billet.keyring"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
}

// THE EXPORTED ENTRY POINT ENFORCES THE RULE ITSELF.
//
// CheckCeph exists because the client's constructor is exported and cannot assume
// its configuration came through Load. An empty identity is refused there rather
// than defaulted, because a caller that skipped normalization would otherwise hand
// rbd no identity at all — and rbd's own default is client.admin, which is the one
// answer this whole rule exists to prevent.
func TestCheckCephRefusesAnUnnamedIdentity(t *testing.T) {
	t.Parallel()

	errs := CheckCeph(CephConfig{ImagePool: "billet-images", CachePool: "billet-cache"})
	if len(errs) == 0 {
		t.Fatal("CheckCeph accepted a config naming no identity")
	}

	if joined := joinErrors(errs); !strings.Contains(joined, "node.ceph.user") {
		t.Errorf("the error does not name the field: %s", joined)
	}
}

// EVERY PROBLEM AT ONCE, which is what Validate exists for: an operator fixing one
// field and re-running to find the next is the failure mode it avoids.
func TestCheckCephReportsEveryProblem(t *testing.T) {
	t.Parallel()

	errs := CheckCeph(CephConfig{
		User:        "admin",
		ImagePool:   "",
		CachePool:   ".mgr",
		ConfPath:    "ceph.conf",
		KeyringPath: "billet.keyring",
	})

	joined := joinErrors(errs)
	for _, want := range []string{"node.ceph.user", "image_pool", "cache_pool", "conf_path", "keyring_path"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s is not reported: %s", want, joined)
		}
	}
}

func joinErrors(errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, err.Error())
	}

	return strings.Join(parts, "\n")
}

// A CONFIG THAT WAS CORRECT LAST WEEK MUST NOT FAIL BAFFLINGLY.
//
// Pre-1.0 billet is allowed to break an existing config; it is not allowed to
// break one with `field zfs_pool not found in type config.FirecrackerConfig`,
// which names the key the operator wrote and nothing about what replaced it.
// KnownFields cannot tell a removal from a typo, so the removal is listed.
func TestTheOldZFSKeyExplainsWhatReplacedIt(t *testing.T) {
	t.Parallel()

	body := strings.Replace(withoutCeph(t),
		"    kernel_image: /var/lib/billet/vmlinux\n",
		"    kernel_image: /var/lib/billet/vmlinux\n    zfs_pool: tank\n", 1)
	if !strings.Contains(body, "zfs_pool: tank") {
		t.Fatal("the firecracker block has changed, so this case adds nothing")
	}

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a config still naming zfs_pool")
	}

	for _, want := range []string{"node.ceph", "image_pool", "cache_pool", "adr-003"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not point at %s: %v", want, err)
		}
	}
}

// THE HINT IS KEYED ON THE WHOLE "field X not found in type Y" STRING, not on the
// key alone.
//
// The same key name can legitimately exist in another section, and advice about
// the deployment lock attached to an unrelated `lock_dir` is worse than no advice:
// it sends an operator to a field they did not touch.
func TestAHintIsNotOfferedForADifferentSectionsKey(t *testing.T) {
	t.Parallel()

	// node.lock_dir is where lock_dir actually lives, so a bogus one under
	// `github:` must draw the plain unknown-field error.
	body := strings.Replace(validConfig, "  org: acme\n", "  org: acme\n  lock_dir: /run/billet\n", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted an unknown field under github")
	}

	if strings.Contains(err.Error(), "has moved to node.lock_dir") {
		t.Errorf("a key in an unrelated section drew the server.lock_dir advice: %v", err)
	}
}

// AN IDENTITY THAT WOULD AUTHENTICATE AS SOMETHING ELSE IS REFUSED — and only
// that, because it is the only shape that survived being measured.
//
// rbd prefixes the value of --id with `client.` itself, so `--id client.billet`
// asks about `client.client.billet`: against a working cluster it answers
// `(13) Permission denied` while the plain form lists the pool.
//
// The second half of this test is the more important one. A leading dash, a space
// and a slash were ALSO refused, on the reasoning that they would not survive an
// argv — and running it says otherwise: --id consumes the next token whatever it
// starts with, and `ceph auth get-or-create` accepts `client.a/b` and `client.a b`.
// A rule whose stated reason is untrue is worse than no rule, so those are gone,
// and this asserts they stay gone.
func TestAnIdentityBilletCannotPassIsRefused(t *testing.T) {
	t.Parallel()

	for _, user := range []string{"client.billet", "client.anything-at-all"} {
		t.Run("the prefix rbd adds itself: "+user, func(t *testing.T) {
			t.Parallel()

			errs := CheckCeph(CephConfig{
				User:      user,
				ImagePool: "billet-images",
				CachePool: "billet-cache",
			})
			if len(errs) == 0 {
				t.Fatalf("CheckCeph accepted %q, which carries the client. prefix", user)
			}

			if joined := joinErrors(errs); !strings.Contains(joined, "rbd adds") {
				t.Errorf("the error does not say why it is refused: %s", joined)
			}
		})
	}

	for _, tc := range []struct{ name, user string }{
		{name: "a leading dash, which --id consumes as a value", user: "-weird"},
		{name: "a slash, which ceph auth accepts", user: "billet/node"},
		{name: "a space, which ceph auth accepts", user: "bil let"},
	} {
		t.Run("addressable: "+tc.name, func(t *testing.T) {
			t.Parallel()

			errs := CheckCeph(CephConfig{
				User:      tc.user,
				ImagePool: "billet-images",
				CachePool: "billet-cache",
			})
			if len(errs) != 0 {
				t.Errorf("CheckCeph refused %q, which is addressable: %s", tc.user, joinErrors(errs))
			}
		})
	}
}

// A VALUE exec CANNOT CARRY AT ALL is refused before it can produce a failure that
// names neither the field nor the byte.
//
// YAML can encode a NUL, and one passes every other shape check here — including
// filepath.IsAbs — while exec rejects the argument before rbd starts.
func TestANulByteIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		cfg   CephConfig
		field string
	}{
		{
			name:  "in the identity",
			cfg:   CephConfig{User: "bil\x00let", ImagePool: "billet-images", CachePool: "billet-cache"},
			field: "node.ceph.user",
		},
		{
			name:  "in a pool",
			cfg:   CephConfig{User: "billet", ImagePool: "billet\x00images", CachePool: "billet-cache"},
			field: "node.ceph.image_pool",
		},
		{
			name: "in the keyring path",
			cfg: CephConfig{User: "billet", ImagePool: "billet-images", CachePool: "billet-cache",
				KeyringPath: "/etc/ceph/k\x00.keyring"},
			field: "node.ceph.keyring_path",
		},
		{
			// The two a loop written from the first three forgets.
			name:  "in the cache pool",
			cfg:   CephConfig{User: "billet", ImagePool: "billet-images", CachePool: "cache\x00pool"},
			field: "node.ceph.cache_pool",
		},
		{
			name: "in the conf path",
			cfg: CephConfig{User: "billet", ImagePool: "billet-images", CachePool: "billet-cache",
				ConfPath: "/etc/ceph/\x00ceph.conf"},
			field: "node.ceph.conf_path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			errs := CheckCeph(tc.cfg)
			if len(errs) == 0 {
				t.Fatalf("CheckCeph accepted a NUL %s", tc.name)
			}

			joined := joinErrors(errs)
			if !strings.Contains(joined, tc.field) {
				t.Errorf("the error does not name the field: %s", joined)
			}

			if !strings.Contains(joined, "NUL") {
				t.Errorf("the error does not say what is wrong: %s", joined)
			}
		})
	}
}

// WHAT WAS VALIDATED AND WHAT IS EXECUTED MUST BE THE SAME STRING, and the
// exported entry point is where they can differ.
//
// Load normalizes before validating, so padding never survives that path. A caller
// that builds a CephConfig itself has not normalized, and trimming only for the
// decision is exactly the defect the ec2 block shipped with: the check passed and
// the padded value was used. So CheckCeph refuses padding rather than tolerating
// it, and a tab inside a pool name is refused too — checking for " " admits one.
func TestCheckCephRefusesWhatItWouldOtherwiseHaveToTrim(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  CephConfig
		want string
	}{
		{
			name: "a padded identity",
			cfg:  CephConfig{User: " billet ", ImagePool: "billet-images", CachePool: "billet-cache"},
			want: "node.ceph.user",
		},
		{
			name: "a padded pool",
			cfg:  CephConfig{User: "billet", ImagePool: " billet-images ", CachePool: "billet-cache"},
			want: "node.ceph.image_pool",
		},
		{
			name: "a padded path",
			cfg: CephConfig{User: "billet", ImagePool: "billet-images", CachePool: "billet-cache",
				ConfPath: "/etc/ceph/ceph.conf "},
			want: "node.ceph.conf_path",
		},
		{
			name: "a padded keyring path",
			cfg: CephConfig{User: "billet", ImagePool: "billet-images", CachePool: "billet-cache",
				KeyringPath: " /etc/ceph/billet.keyring"},
			want: "node.ceph.keyring_path",
		},
		{
			// The cache pool is the field a loop written from the image pool
			// forgets, and it is the one billet writes to on every job.
			name: "a padded cache pool",
			cfg:  CephConfig{User: "billet", ImagePool: "billet-images", CachePool: "billet-cache\t"},
			want: "node.ceph.cache_pool",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			errs := CheckCeph(tc.cfg)
			if len(errs) == 0 {
				t.Fatalf("CheckCeph accepted %s", tc.name)
			}

			if joined := joinErrors(errs); !strings.Contains(joined, tc.want) {
				t.Errorf("the error does not name what is wrong: %s", joined)
			}
		})
	}
}

// EVERY REMOVED KEY IS ANSWERED AT ONCE.
//
// KnownFields reports all the unknown fields it found in one error, and validation
// exists to stop an operator fixing one field and re-running to find the next. A
// hint loop that returns on its first match sends them round again.
func TestEveryRemovedKeyIsExplainedInOnePass(t *testing.T) {
	t.Parallel()

	body := strings.Replace(withoutCeph(t), "  state_dir: /var/lib/billet/server\n",
		"  state_dir: /var/lib/billet/server\n  lock_dir: /run/billet/locks\n", 1)
	body = strings.Replace(body, "    kernel_image: /var/lib/billet/vmlinux\n",
		"    kernel_image: /var/lib/billet/vmlinux\n    zfs_pool: tank\n", 1)

	for _, must := range []string{"lock_dir: /run/billet/locks", "zfs_pool: tank"} {
		if !strings.Contains(body, must) {
			t.Fatalf("the fixture does not carry %q, so this case patches nothing", must)
		}
	}

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a config naming two removed keys")
	}

	if !strings.Contains(err.Error(), "node.lock_dir") {
		t.Errorf("the error does not say where lock_dir went: %v", err)
	}

	if !strings.Contains(err.Error(), "node.ceph") {
		t.Errorf("the error does not say what replaced zfs_pool: %v", err)
	}

	// ONCE EACH. A loop that appended every match twice would satisfy presence
	// while handing the operator the same paragraph over and over.
	for _, once := range []string{"has moved to node.lock_dir", "node.firecracker.zfs_pool is gone"} {
		if n := strings.Count(err.Error(), once); n != 1 {
			t.Errorf("%q appears %d times: %v", once, n, err)
		}
	}
}
