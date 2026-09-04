package state

// THE POSTGRESQL TIMELINE'S PUBLISHED SUMS.
//
// Its own table because a checksum is over one engine's own statement bytes, and
// nothing compares one across timelines. The rule it enforces is identical to
// migrationsAreFrozen's: these bytes are recorded in every PostgreSQL
// deployment's schema_migrations, so editing one — including its comments, its
// whitespace, or the order of its statements — is a control plane that will not
// start.
//
// FROZEN FROM THE DAY IT SHIPS. There is no PostgreSQL deployment yet, which is
// exactly why the table is written now rather than later: the first one will
// exist before anybody thinks to add it.
var pgMigrationsAreFrozen = map[int]struct{ Name, Sum string }{
	1:  {"nodes", "051423e0e80f6386346e2431b1e8e3f605685dfbda5ed63c24a2b005ba7afa7c"},
	2:  {"leases", "3676590bface8e89e363e761ebb37d9fc27c30063d79799e85536318399133ab"},
	3:  {"cache_generations", "a3f8da5fcd54c0264eb862c7dcf81a4c34e8ac825374dea21e8fba899720c7fa"},
	4:  {"job_history", "78b51732899a479091eb10067d4b088b22c8315498551f08715392bc79c24eac"},
	5:  {"lease_placement", "4815cfa796cb5b5be8385e1b45dece7b446693ede62ba975df5a39ffca8cbc69"},
	6:  {"lease_guest_os", "f5de50af61b72d5de417e41ddfb93d9ca7d033fcd56b33b62cf281ac84e16b37"},
	7:  {"lease_placement_facts", "4716132e8d20d42257e45857e063d60fc0562985e27afd069c5832f55274bd15"},
	8:  {"lease_request_id", "8c356911fc2d86dbafc9da51cce6c44c2cefe75345a009f4a573a162d94f5568"},
	9:  {"lease_provider_list", "0cfb1bd67dba477f4d0897fd2422e11019c64e6ab154b88873556f7ea8517565"},
	10: {"node_site", "77dcaf66c9849fb9eb925ad904ac960b5f62fc3cd5e406ad03af0fc44b043a0d"},
	11: {"node_liveness", "8dd17027381e725690aa0d2463316e9cd487e4687c02d1bff1dc79902887641d"},
	12: {"cert_revocation", "48b28969ccc9f8816356e86ebd4d9499399611feab2e3b371e94c20e20155822"},
	13: {"node_enrollment", "d419f0def90a076a6939edf2c989b9424ff6be7551da18db968a2849ea9c744e"},
	14: {"join_tokens", "130aa9c8087ce3e60efc583b613ed94855fc0b5a39a881824915abcee9330326"},
	15: {"issued_certs", "bc457d2e8af47588f23e74a9707b7ca6e2f74d6ef32a9cc4aef01199dd5a5cef"},
	16: {"lease_quarantine", "1c8a293803ad7d39087001cb7969ee52429e557e865505cd4b02ddcacfb3ae41"},
	17: {"strict_trust_tables", "6e0358907e661fa31ac736559f99f8eda19495232ddf94134f70a5a66e5eda1e"},
	18: {"node_revocations", "1149b3b6e9e6535697154ce6bed4c330bdcc800854bcd7628a75ee2020b9bfe6"},
	19: {"ec2_shape_accounting", "edfb686a81ed8816fc220c9acfa61c276e000c35ae81b2df825538c3f8b6a0c1"},
	20: {"custody_visibility", "0c04b1cca8fd49815d878c15eaa25a47d362aea3dd98479a3568e7fbf2eaaec5"},
	21: {"lease_failure_reason", "6023850e1532558b9e69e85fc846348555f12fa23a7123c3790aac72c9b43882"},
	22: {"pending_completions", "6ee247702dbb726554f33cf3d6d012b17510fab76d9124c251d1e3aa0510fdb3"},
	23: {"pending_completion_lease", "baf597c103e76794b7e4f776e55629a0f55da44f0ea846fca93d76bfdbdec55a"},
	24: {"pending_completion_recovery", "c2305f72d496dbe62e55392f0535aaf7b58ad790eb046ee31527811a4880632b"},
	25: {"pending_completion_acknowledgement", "f70964de8af26653ca74d9bc1cb3973834c902ea8386c92c28e6f685060e929e"},
	26: {"direct_assignment_identity", "86c5d0e9b7b6f82c373fb31945bdc4d7abb0cf96e331db0a251bfa92ac17b229"},
	27: {"cache_interception_policy", "b1d9df6f7af2303190e4563753eb5e968410e45592a275767535134dafa749c9"},
	28: {"pool_runner_identity", "6f488a3855066a74d23e4040d41dee51aa7a10b513e83d304f3f5d285e22afab"},
	29: {"pool_slot_identity", "cf32dd9cbee8bd8b8695aac8855839e4a120e1f28666598bc1039978f5dd1aba"},
	30: {"lease_deregistered", "0944953f87287ce93c340f28ad2ca159c97d1001ef1b4fb622ed38c3c834b7c7"},
	31: {"admission", "05602acd5d739d7acd62b72688e0724896a5349136ee3d3c212ca980e17665fc"},
	32: {"scale_set_provenance", "79f076e2adcfef3865cd696a88ac8ae91a47a0baf84319aaff862b5a22a26007"},
	33: {"node_inventory", "58ab34dca21ff7c070b2f9fe952727023bde9384384682513c45e67e9e1c1baf"},
	34: {"node_wire_version", "aab00be796039840a935c709e2a43e43cd06342341c270d6e4d858a9606d7641"},
	35: {"job_disruption", "581e7ac22f4d3f9bc47434d9c3567bd6c104cec9cd632326d232795904266d40"},
	36: {"pending_completion_lease_index", "5ba52879134f41aa6db30b6f71637992e5ec902f129b8ea2267b6efa8abd4ec8"},
	37: {"compute_barrier", "c6fcb2d58cbe763e1ba0e6be255a39774e369c588516240e2f384934223c3cd2"},
	38: {"force_destroy", "04020e8df1719dee64f77c864af52c289062103f6c26d18e96b9c844f1c382d2"},
	39: {"rollout", "68253f2a9f2ea6a5f78b476b7f3f6fadf290b34935a61aa73f443d55bd382464"},
	40: {"rollout_dispatch_epoch", "07c5aadb848a0b872d69011dc0e0b3ad754d152147e960fc63cdb865b75517ca"},
	41: {"node_digest", "de4f2edad3a21bbe5756a7dbc8fd9e6a52a4d7884f0638ccda26df44c22951c8"},
	42: {"rollout_converged_digest", "829ac12cf6612368298cab1400692cd0fe1d985b1e5c4cf6fb24d2178628c195"},
	43: {"codebuild_fleet", "a2d67dfa5e72fd5e3606be683f2f30704a7aa8d50721fb93d57fa0ee9d97a422"},
	44: {"controller_claim", "706e52aa792dc8650862baf0e95801e73e2758a69bd698f5a13551bb78e8e134"},
	45: {"deployment_binding", "51b37a126e4827d436fd75119cd5b218369dda208133aa4ec4b456dd035ec0c7"},
	46: {"codebuild_registration_sweep", "120efb5947ef58910668779c1570976d0356a62fcd320fbefbafdee3286319a3"},
	47: {"lease_holder_incarnation", "e2b163c22ec6cf00f7c325af102f4a6fdeb0e619d9c100f3c0d7b19b8dd48ece"},
	48: {"release_watermark", "4a4306526e5d0ffd5e8dc3b34e241f7dd403942c82b1dd3a7fe437e8d6324bd6"},
}
