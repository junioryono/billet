package state

import "testing"

// EVERY MIGRATION THAT HAS SHIPPED IS FROZEN, AND THIS IS WHAT ENFORCES IT.
//
// A migration is identified by its version AND a checksum of its statement
// bytes, so `Open` refuses a ledger whose recorded checksum does not match the
// binary's — which is correct, and is also a control plane that will not start.
// The comment above `migrations` has always said "never edit or reorder an
// existing entry", and TestMigrationVersionsAreUniqueAndAscending has always
// held the versions to it. Nothing pinned the published statement BYTES or
// names, which is the half the rule was broken by — with a comment.
//
// MEASURED, NOT IMAGINED: two explanatory lines were added inside migration 1's
// CREATE TABLE (ef84c7b). The SQL was unchanged in every sense that matters to
// SQLite, the checksum covers the bytes, and every ledger written before that
// commit stopped opening — found on the reference host, whose control plane
// answered `migration 1 (nodes) was applied with different SQL than this binary
// contains` against a database that was perfectly fine.
//
// So the sums below are a record of what has been PUBLISHED. Adding a migration
// adds a line. Changing one — including its comments, its whitespace, or the
// order of its statements — fails here, in CI, instead of on somebody's host
// during an upgrade. If a line here has to change, the honest answer is almost
// always a new migration instead.
var migrationsAreFrozen = map[int]struct{ Name, Sum string }{
	1:  {"nodes", "f9d182cd09112bc4f227734f90c06cd58a20a1e17f39e9f331a9d578b75d4574"},
	2:  {"leases", "bd7c37ee04238d657a1ca665056558f13317324e29c9202a3f5dd9148e6dd462"},
	3:  {"cache_generations", "68ab2303fcc7ba622e28d83326b0d2605c63bc67a01596857760eb3bfecd2a5f"},
	4:  {"job_history", "90d528a1b8ae213716a25400f97c6c63709b21a48aa24928fb67b786bc918342"},
	5:  {"lease_placement", "d430770febaf8ee0622021661900265570a2a93e846a54683018b0e6b7f69232"},
	6:  {"lease_guest_os", "ae14256fd1e9210c1d7c9409e311b28d5f053dac7cd7b3591a438144553785da"},
	7:  {"lease_placement_facts", "3fd6dfdee48ad59f57760f92ef8f61ff5401368fc74675fc7bc434db2ab8581f"},
	8:  {"lease_request_id", "5d9c22fdde6854edee677778f4c43b4ce9381585ba50df3d541eaf200b28d705"},
	9:  {"lease_provider_list", "e96a600a0ff93e12eb70f7ea4fd9979055925f0753d880cfef00698f16e8b03c"},
	10: {"node_site", "6d3e13494557b06aaef45a8f4e57129572e0935b43cf7905e6652fb40406a002"},
	11: {"node_liveness", "de371098d2af92070a9c23d4ead2b17fec46eb4e32f6e34f1196d04488e7f549"},
	12: {"cert_revocation", "89d003db6ab76cac213b9b372b851defffacfb4dcd81c0aa9de30382ede465fa"},
	13: {"node_enrollment", "797a9c4fa4a7140532d2b4b34e57e94330bbda58d1163c758b6d39107cedefd5"},
	14: {"join_tokens", "98baec445039e41b32cc19356c4327825742d0a6e04309377136ecdbfc4dc2d7"},
	15: {"issued_certs", "23ace08a8728a2e95e3b2ddc9485f616ebe53b877d830c4c31a3587eb1eb9140"},
	16: {"lease_quarantine", "20c12e51239b57b26501f8839d67c087e3f8c1e2f44d5120e401be7e506f3a0f"},
	17: {"strict_trust_tables", "f34b17abd11cb38b09d40a427d9bf4134b4dfaf73bfd5dc3ba1a90f476c3c933"},
	18: {"node_revocations", "52f2deabc38503c15d5b8bdb274264f8a77f318e6ead5db0f5068906d6fd4cec"},
	19: {"ec2_shape_accounting", "3961b589433bb94f3de33eb277feaa34f4bca3377276485d5242022ad8c266f3"},
	20: {"custody_visibility", "23f2056d3c6745c9c0ead46b4063a229f4f73e7e36fbc97c46842b31f4d0115b"},
	21: {"lease_failure_reason", "f686693c9c850b6f0dbe4c3e67464f25091411789b5369f8172034e173fd888b"},
	22: {"pending_completions", "46f19127d14300947d7919f8e0963c1ce27f3ead8b09d6382193e5093dfe86e6"},
	23: {"pending_completion_lease", "34ce52002356ffdd578ff4bfac98a11d9f12ba9a67ae0434ee80f91a12b896fa"},
	24: {"pending_completion_recovery", "ab201b57594f9c8ec76ff52e19715cc2995533b6909cacfc5a96979ff64f0da7"},
	25: {"pending_completion_acknowledgement", "a0122cd1a20d08d02658016a0ea9125293819507c8db9a0d50d471233a2622c0"},
	26: {"direct_assignment_identity", "7af5dcfbe806adb5a50977c887469e0ab6be23304226cb6c2bed1a5bc56e771d"},
	27: {"cache_interception_policy", "38568fe9216942e132d4a8948dc30fca8c3d796c4855f67ae3295be47132d9e2"},
	28: {"pool_runner_identity", "f5c2ec58cc95d0500ea82755a087da87f467918cd013c0e9cc876ac160eb21a0"},
	29: {"pool_slot_identity", "9a59af9c6a83c7ee192f66f112643c81f7bc1f02d1027d4f861063185159376c"},
	30: {"lease_deregistered", "77da2a9ceca081ab4050df37f0f67dd575497951713167faf948cf3a8a1fb75b"},
	31: {"admission", "485e0f54c823ed0c214b6174a3399a62e4bf0ea116af6cdfbd0f050ee12b94a7"},
	32: {"scale_set_provenance", "f1b86d399cb1761827b5be88bff263c879b8b1dd8dae823103dfc95f831776f7"},
	33: {"node_inventory", "a32b7718fd0f0f049c9119cebad2c0811519243c6bf5ca77ac3b4e717a91fe15"},
	34: {"node_wire_version", "8dd67c4a993b11f89eefcf74a1d3824170982261d1be087db8871a781c73c526"},
	35: {"job_disruption", "f82ec7afdd692de7af6b4b5bb340c32234868dd6bc15fea241cd3861e1b6753d"},
	36: {"pending_completion_lease_index", "be83cb5000ea62791f1314712fd45ae246b4590aa64c5d8369aded989c4d24a9"},
	37: {"compute_barrier", "1059e72328766af681320952a213b33f9f83f14f0b24fac69287894f44e3f94d"},
	38: {"force_destroy", "dfc24b73449c7ee9a7ffc0f19b06aa7f96161bb051b565bc75422beb3d090606"},
	39: {"rollout", "b1a1fe04bf45aed9cb99aa4963d7cf7eeca7fd772196db3339a731831c518aa7"},
	40: {"rollout_dispatch_epoch", "5f334ba2e0958e1fb6f4abe98b66379c7c528a3182266b913ceef10d75cd7422"},
	41: {"node_digest", "1b407c588dd60af3f6826848e4e309556482a6cdaa4a90c22353dad8331bc8d1"},
	42: {"rollout_converged_digest", "3bda4e22bad2e03e3c82802aca4953bfde6515abdec9c831b098b7ec0f12722a"},
	43: {"codebuild_fleet", "f56a928944bb47b3e43fc44fc7df0d2b8ec4161ae12a96c423da35704676e8bb"},
	44: {"controller_claim", "b5135c0c1c32f4ecf3965dd46741828de3ab2bad0763bb4f368c615fbfae5c40"},
	45: {"deployment_binding", "6cc49c0a2bbccb9cba02100a6e06c9a770edd371a9e87d5b3596c1410ba68988"},
	46: {"codebuild_registration_sweep", "e4c2039a1bb13c3dfd589b6be5642a76858882c8a101b67a83a7312d99f651f6"},
	47: {"lease_holder_incarnation", "4f95b57519565556debad4d0b41c87edf48f79af13db7ba6de4fbe5802f1d3a0"},
	48: {"release_watermark", "edaad1bef5b7708f999548cce3e2530c994f80a6a90b5dcce30372b3f1d9f5bf"},
}

func TestNoShippedMigrationHasBeenEdited(t *testing.T) {
	t.Parallel()

	requireTimelineIsFrozen(t, sqliteTimeline, migrationsAreFrozen, "migrationsAreFrozen")
}

// The same rule for the PostgreSQL timeline, which has its own bytes and so its
// own sums — nothing compares a checksum across timelines, and nothing should:
// they are over one engine's own statements. What ties the two together is
// versions and names, which TestBothTimelinesDeclareTheSameVersionsAndNames
// holds, and the derivation, which
// TestEveryPostgresMigrationIsItsSQLiteTwinTranslated holds.
func TestNoShippedPostgresMigrationHasBeenEdited(t *testing.T) {
	t.Parallel()

	requireTimelineIsFrozen(t, pgTimeline, pgMigrationsAreFrozen, "pgMigrationsAreFrozen")
}

// requireTimelineIsFrozen is the comparison itself, against a NAMED timeline and
// its published table.
//
// SHARED RATHER THAN COPIED, because this is the guard that stops a fleet
// failing to start and a second copy of it is a second thing to keep correct.
// The table's Go name is passed in so a failure says which one to edit.
func requireTimelineIsFrozen(
	t *testing.T, tl *timeline, frozen map[int]struct{ Name, Sum string }, table string,
) {
	t.Helper()

	// THE GUARD MUST NOT BE PRE-EMPTED BY THE THING IT GUARDS. A tree with two
	// migrations claiming one version, or a file the embed pattern missed, leaves
	// the set unreadable — and a guard that iterated an empty slice would pass,
	// which is why parsing stores its error rather than failing construction. See
	// timeline.loadErr.
	if tl.loadErr != nil {
		t.Fatalf("the embedded %s migration set did not load, so nothing below was checked: %v",
			tl.engine, tl.loadErr)
	}

	if len(tl.migrations) != len(frozen) {
		t.Errorf("%d %s migrations loaded, %d published; the loop below cannot account for the "+
			"difference on its own", len(tl.migrations), tl.engine, len(frozen))
	}

	seen := make(map[int]bool, len(tl.migrations))

	for _, m := range tl.migrations {
		if seen[m.Version] {
			t.Errorf("migration version %d appears twice", m.Version)
		}

		seen[m.Version] = true

		want, isFrozen := frozen[m.Version]
		if !isFrozen {
			// A NEW migration, which is the one legitimate way this table goes
			// stale. Name it rather than failing silently, so the fix is obvious.
			t.Errorf("%s migration %d (%s) is not in %s; add\n"+
				"\t%d: {%q, %q},",
				tl.engine, m.Version, m.Name, table, m.Version, m.Name, m.checksum())

			continue
		}

		if m.Name != want.Name {
			t.Errorf("migration %d is now named %q, was published as %q; a version is an "+
				"identity and renaming one makes two ledgers disagree about what ran",
				m.Version, m.Name, want.Name)
		}

		if got := m.checksum(); got != want.Sum {
			t.Errorf("migration %d (%s) has been EDITED: checksum %s, published %s.\n"+
				"Migrations are append-only — a ledger written by any earlier build "+
				"records the published sum and will refuse to open. Revert the edit "+
				"and add a new migration instead. Comments and whitespace inside the "+
				"statement count: they are part of the bytes.",
				m.Version, m.Name, got, want.Sum)
		}
	}

	// AND THE TABLE MUST NOT OUTLIVE ITS MIGRATIONS EITHER. A version deleted
	// from the list is the same defect wearing the other face: a ledger that
	// recorded it can never be reconciled with a binary that has forgotten it.
	for version, want := range frozen {
		if !seen[version] {
			t.Errorf("migration %d (%s) has been REMOVED; every ledger that applied it "+
				"records it forever", version, want.Name)
		}
	}
}
