# Retained ordinary drift rows

The machine-readable catalog is `retained-drift.json`. Each row binds the complete normalized task mapping and all inherited include/block conditions as text; the catalog also binds every operand-producing fact, register, task variable and role variable/default definition. A changed definition requires re-review even when the destination spelling stays the same. This table is a review aid, not the gate input.

| File / task | Operand source or parity rationale |
|---|---|
| tasks/upgrade-inspect.yml / Resolve billet state paths through every existing ancestor | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/upgrade-recover.yml / Read the transaction pointer before recovery | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-recover.yml / Inspect what serves the state directory before recovery | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-recover.yml / Resolve the recovery mount and expected device real paths | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Read the transaction pointer before finalization | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Open the committed ledger to operator commands | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Durably open the committed ledger to operator commands | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Inspect committed service activity before replacing a probe | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Inspect committed service unit availability | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Inspect committed service process identifiers | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Detect quiescent probes that must become full services | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Start the committed billet server before admitting compute | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Start the committed billet node after the control plane | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Keep services outside the committed target stopped | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Inspect committed service stability baseline | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Observe committed services across the restart interval | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Inspect committed service stability after the restart interval | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Close the durable host-upgrade transaction after stable service startup | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-finalize.yml / Durably close the host-upgrade transaction | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Stop compute before recovering the authoritative ledger | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Stop the control plane before recovering its authoritative ledger | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Hide the installed executable while recovery owns the ledger | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Fence the ledger throughout recovery | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Wait for every billet process started before executable removal to finish | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Inspect service process state before recovering upgrade inputs | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Inspect service cgroups before recovering upgrade inputs | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Write the isolated recovery validation configuration | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Unfence the isolated ledger snapshot for validation | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Prove the durable ledger snapshot is valid before replacing current state | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Remove current ledger entries before restoring a completed snapshot | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Restore the previous control-plane ledger | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Disable candidate services before restoring previous units | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Remove candidate persistent and runtime enablement links | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Restore configuration and units that existed before the upgrade | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Remove configuration and units created only for the interrupted upgrade | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Reload restored systemd units before restarting the previous version | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Restore persistent service enablement | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Restore runtime-only service enablement | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Restore disabled service state | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Inspect restored service enablement | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Restore the previous billet binary | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Keep a failed first installation without an invented predecessor | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Durably flush every restored host filesystem before rollback commit | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Record the durable rollback commit before opening administration | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/upgrade-rollback.yml / Durably publish the rollback commit before opening administration | parity:historical recovery only; the retained entry has no interrupted transaction and binary changes refuse before ordinary work |
| tasks/endpoint-decision.yml / Observe the node unit on a host with no billet | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/endpoint-decision.yml / Compare the endpoints without an executable to ask | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/endpoint-decision.yml / Ask for the endpoint migration's dry run | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/node-environment.yml / Inspect the installed node environment | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/node-environment.yml / Prove the retained environment files are readable or optionally absent | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/node-environment.yml / Inspect the node environment again before replacement | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/unit-readiness.yml / Inspect installed service readiness types before ordinary convergence | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/retirement-node-config.yml / Check the exact node configuration and operation document | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/dryrun-policy.yml / Inspect pending Billet service policy during a dry run | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/dryrun-policy.yml / Inspect the loaded identity of Billet units during a dry run | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/dryrun-policy.yml / Inspect the loaded ExecStart of Billet units during a dry run | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/dryrun-policy.yml / Read the running manager's unit lookup paths during a dry run | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/dryrun-policy.yml / Resolve on-disk aliases of the Billet units the way the manager would | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/installed-identity.yml / Reload systemd before inspecting installed service policy | parity:shared manager reload or node service lifecycle; node enable/stop/start superset is admitted |
| tasks/installed-identity.yml / Inspect an existing billet account | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/installed-identity.yml / Inspect the installed billet server unit before account mutation | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/installed-identity.yml / Inspect persistent server enablement before account comparison | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/installed-identity.yml / Prove the retained account without replacing its identity | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/installed-identity.yml / Inspect the installed billet server identity before account mutation | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/installed-identity.yml / Inspect the retained node account | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/account.yml / Check for processes owned by an adopted account | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/account.yml / Create the billet service group | parity:verified retained account disables account creation and adoption |
| tasks/account.yml / Create or adopt the billet service account | parity:verified retained account disables account creation and adoption |
| tasks/account.yml / Install billet host dependencies | parity:package installation and maintainer scripts are the ordinary node-host operation, outside path admission (R7.7) |
| tasks/account.yml / Create billet directories | loop:billet_common_directories; loop:billet_server_directories; loop:billet_node_directories |
| tasks/account.yml / Reload systemd before inspecting host-upgrade service overlays | parity:binary transaction only; retained binary changes refuse before ordinary work |
| tasks/account.yml / Inspect billet service drop-ins before the upgrade transaction | parity:binary transaction only; retained binary changes refuse before ordinary work |
| tasks/account.yml / Preserve configuration and units before replacing them | parity:binary transaction only; retained binary changes refuse before ordinary work |
| tasks/account.yml / Preserve the previous billet binary before live mutation | parity:binary transaction only; retained binary changes refuse before ordinary work |
| tasks/account.yml / Ask the candidate whether its probe can be told to hold | parity:binary transaction only; retained binary changes refuse before ordinary work |
| tasks/account.yml / Stage candidate systemd units for the upgrade transaction | parity:binary transaction only; retained binary changes refuse before ordinary work |
| tasks/account.yml / Install the billet systemd units | loop:billet_unit_operands |
| tasks/account.yml / Install the GitHub App private key without replacing an existing identity | parity:E removes github and targets; no credential copy is selected |
| tasks/account.yml / Install the further targets' GitHub App private keys without replacing an existing identity | parity:E removes github and targets; no credential copy is selected |
| tasks/account.yml / Stage candidate configuration for the upgrade transaction | parity:binary transaction only; retained binary changes refuse before ordinary work |
| tasks/account.yml / Render billet configuration | const:/etc/billet/billet.yaml; E:billet_effective_config |
| tasks/cache-tls.yml / Create the EC2 cache TLS directories when absent | E:node.cache.tls_cert; E:node.cache.tls_key |
| tasks/cache-tls.yml / Install the EC2 cache TLS certificate | E:node.cache.tls_cert; E:node.cache.tls_key |
| tasks/cache-tls.yml / Install the EC2 cache TLS private key | E:node.cache.tls_cert; E:node.cache.tls_key |
| tasks/firecracker.yml / Create the private Firecracker staging directory | inventory:billet_firecracker_stage; inventory:billet_firecracker_version; inventory:ansible_facts.architecture |
| tasks/firecracker.yml / Download the verified Firecracker release | inventory:billet_firecracker_stage; inventory:billet_firecracker_version; inventory:ansible_facts.architecture |
| tasks/firecracker.yml / Unpack Firecracker | inventory:billet_firecracker_stage; inventory:billet_firecracker_version; inventory:ansible_facts.architecture |
| tasks/firecracker.yml / Install the versioned Firecracker binaries | const:/usr/local/bin; inventory:billet_firecracker_version |
| tasks/firecracker.yml / Link the selected Firecracker version onto PATH | const:/usr/local/bin/firecracker; const:/usr/local/bin/jailer; inventory:billet_firecracker_version |
| tasks/network.yml / Predict the IP forwarding change | const:/etc/sysctl.d/99-billet-node.conf |
| tasks/network.yml / Predict billet guest bridge device changes | loop:billet_networks |
| tasks/network.yml / Predict billet guest bridge address changes | loop:billet_networks |
| tasks/network.yml / Predict billet dnsmasq configuration changes | loop:billet_networks |
| tasks/network.yml / Predict the billet dnsmasq unit change | const:/etc/systemd/system/billet-dnsmasq@.service |
| tasks/network.yml / Predict the billet guest firewall change | const:/etc/billet/network.nft |
| tasks/network.yml / Predict the billet network service change | const:/etc/systemd/system/billet-network.service |
| tasks/network.yml / Inspect whether the billet node unit exists | parity:read-only systemd/IP observation with exact argv bound |
| tasks/network.yml / Drain billet compute before changing guest networking | parity:shared node stop or node-owned networking service lifecycle (R7.3) |
| tasks/network.yml / Enable IP forwarding for guest bridges | const:/etc/sysctl.d/99-billet-node.conf |
| tasks/network.yml / Inspect DHCP units for removed guest networks | parity:read-only systemd/IP observation with exact argv bound |
| tasks/network.yml / Stop and disable DHCP on removed guest networks | parity:shared node stop or node-owned networking service lifecycle (R7.3) |
| tasks/network.yml / Remove retired dnsmasq definitions | loop:billet_stale_network_names |
| tasks/network.yml / Remove retired guest bridge device definitions | loop:billet_stale_network_names |
| tasks/network.yml / Remove retired guest bridge address definitions | loop:billet_stale_network_names |
| tasks/network.yml / Inspect active network links before removing guest bridges | parity:read-only systemd/IP observation with exact argv bound |
| tasks/network.yml / Remove retired guest bridges after compute is drained | parity:kernel link removal after the shared node stop; no filesystem destination |
| tasks/network.yml / Configure billet guest bridge devices | loop:billet_networks |
| tasks/network.yml / Configure billet guest bridge addresses | loop:billet_networks |
| tasks/network.yml / Install dnsmasq configuration for each billet bridge | loop:billet_networks |
| tasks/network.yml / Install the billet dnsmasq unit | const:/etc/systemd/system/billet-dnsmasq@.service |
| tasks/network.yml / Install the billet guest firewall | const:/etc/billet/network.nft |
| tasks/network.yml / Install the billet network service | const:/etc/systemd/system/billet-network.service |
| tasks/network.yml / Wait for billet bridges to exist | parity:read-only systemd/IP observation with exact argv bound |
| tasks/network.yml / Enable billet guest firewall and NAT | parity:shared node stop or node-owned networking service lifecycle (R7.3) |
| tasks/network.yml / Enable DHCP and DNS on billet guest bridges | parity:shared node stop or node-owned networking service lifecycle (R7.3) |
| tasks/ceph.yml / Install Ceph bootstrap dependencies | parity:package installation and maintainer scripts are the ordinary node-host operation, outside path admission (R7.7) |
| tasks/ceph.yml / Work around Ubuntu uutils numeric owner handling | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/ceph.yml / Create the matching Ceph daemon account | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/ceph.yml / Bootstrap Ceph without opening root SSH | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/ceph.yml / Inspect Ceph's current device inventory | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/ceph.yml / Add each explicitly named available OSD device | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/ceph.yml / Inspect Ceph pools | parity:read-only Ceph/RBD observation; exact command and inventory operands are fingerprinted |
| tasks/ceph.yml / Create billet Ceph pools | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/ceph.yml / Initialize billet Ceph pools for RBD | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/ceph.yml / Configure billet Ceph pool replication | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/ceph.yml / Require clone-v2-compatible Ceph clients | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/ceph.yml / Create the scoped billet Ceph identity | inventory:billet_ceph_keyring_path |
| tasks/ceph.yml / Protect the billet Ceph keyring | inventory:billet_ceph_keyring_path |
| tasks/ceph.yml / Verify the scoped Ceph identity can read both pools | parity:read-only Ceph/RBD observation; exact command and inventory operands are fingerprinted |
| tasks/ceph.yml / Load the RBD kernel client after every boot | const:/etc/modules-load.d/billet-rbd.conf |
| tasks/ceph.yml / Load the RBD kernel client now | parity:Ceph bootstrap, device selection, daemon accounts, cluster operations and kernel module loading are shared node-host effects (R7.7) |
| tasks/main.yml / Remove disabled Ceph kernel module policy | const:/etc/modules-load.d/billet-rbd.conf |
| tasks/alerts.yml / Record ownership of a legacy billet SMTP configuration | const:/etc/billet/msmtprc-managed; inventory:_billet_alert_root |
| tasks/alerts.yml / Install the send-only mail path | parity:package installation and maintainer scripts are the ordinary node-host operation, outside path admission (R7.7) |
| tasks/alerts.yml / Install the protected SMTP configuration | const:/etc/msmtprc; inventory:_billet_alert_root |
| tasks/alerts.yml / Record ownership of the SMTP configuration | const:/etc/billet/msmtprc-managed; inventory:_billet_alert_root |
| tasks/alerts.yml / Verify SMTP authentication without sending mail | parity:SMTP authentication probe only; config pathname passed as argv, never Python source |
| tasks/alerts.yml / Install the Ceph health monitor | const:/usr/local/libexec/billet-ceph-health; inventory:_billet_alert_root |
| tasks/alerts.yml / Install the Ceph health systemd units | inventory:_billet_alert_root; const:/etc/systemd/system/billet-ceph-health.service; const:/etc/systemd/system/billet-ceph-health.timer |
| tasks/alerts.yml / Start the Ceph health timer | parity:node-owned health timer lifecycle; controller inertness is proved by the command |
| tasks/alerts.yml / Stop and disable the Ceph health timer | parity:node-owned health timer lifecycle; controller inertness is proved by the command |
| tasks/alerts.yml / Remove the Ceph health monitor and units | const:/usr/local/libexec/billet-ceph-health; const:/etc/systemd/system/billet-ceph-health.service; const:/etc/systemd/system/billet-ceph-health.timer; inventory:_billet_alert_root |
| tasks/alerts.yml / Remove billet-owned SMTP credentials | const:/etc/msmtprc; inventory:_billet_alert_root |
| tasks/alerts.yml / Remove the SMTP ownership marker | const:/etc/billet/msmtprc-managed; inventory:_billet_alert_root |
| tasks/images-refresh.yml / Install the guest image refresh units | const:/etc/systemd/system/billet-images-refresh.service; const:/etc/systemd/system/billet-images-refresh.timer |
| tasks/images-refresh.yml / Enable the guest image refresh timer when the deployment updates itself | parity:node-owned image refresh timer lifecycle (R7.3) |
| tasks/images-refresh.yml / Stop and disable the guest image refresh timer on a host that boots no guests | parity:node-owned image refresh timer lifecycle (R7.3) |
| tasks/images-refresh.yml / Remove the guest image refresh units from a host that boots no guests | const:/etc/systemd/system/billet-images-refresh.service; const:/etc/systemd/system/billet-images-refresh.timer |
| tasks/services.yml / Inspect billet service state before the upgrade transaction | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Inspect billet service enablement before the upgrade transaction | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Prove every configured guest image is compatible with the candidate binary | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Persist the host-upgrade recovery manifest before live mutation | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Durably commit host-upgrade recovery inputs before publishing the pointer | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Stage this transaction's atomic host-upgrade claim | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Durably stage the atomic host-upgrade claim | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Claim the host upgrade without replacing another transaction | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Claim the host upgrade inside the converge guard | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Durably publish the transaction pointer inside the guard | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Durably publish the active host-upgrade pointer before live mutation | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Read back the transaction pointer before live mutation | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Gracefully stop the billet node before changing its guest contract | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Pull, boot-verify, and promote every compatible guest needed | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Gracefully stop the billet server after compute has drained | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Hide the old executable before establishing the maintenance fence | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Fence the ledger against operator commands during migration and rollback | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Durably fence every ledger mount before exposing the candidate | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Wait for operator commands started before the fence to finish | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Preserve the stopped control-plane ledger | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Record that the stopped ledger existed before migration | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Durably flush the stopped ledger snapshot before marking it complete | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Commit the durable ledger snapshot before exposing the candidate | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Durably publish ledger snapshot completion before exposing the candidate | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Install the candidate billet binary after fencing and ledger preservation | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Install candidate configuration after preserving the stopped ledger | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Install candidate systemd units after preserving the stopped ledger | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Load candidate systemd units before starting either upgraded service | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Inspect candidate service drop-ins before readiness probes | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Probe the candidate binary for the typed maintenance flag | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Validate and migrate with the new billet binary as the only ledger writer | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Return the migrated ledger to the control-plane account before restart | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Start the quiescent upgraded server readiness probe | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Start the quiescent upgraded node readiness probe | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Inspect upgraded readiness-probe stability baseline | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Observe upgraded readiness probes across the restart interval | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Inspect upgraded readiness-probe stability before committing the transaction | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Install steady-state units without the quiescent probe | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Load steady-state units before opening administration | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Persist desired billet service enablement while the ledger remains fenced | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Durably flush every mutated host filesystem before commit | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Record the durable host-upgrade commit before opening administration | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Durably publish the host-upgrade commit before opening administration | parity:binary transaction only; retirement-binary-refusal.yml rejects it before ordinary work |
| tasks/services.yml / Validate a retained node with its installed environment | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| tasks/endpoint-migration.yml / Run the endpoint migration | parity:shared migration/receipt command owns its runtime footprint and is followed by strict closing (R7.2) |
| tasks/endpoint-migration.yml / Keep the migration's evidence for the receipt | const:/tmp; parity:private tempfile children allocated after admission, written and removed without a recursive parent operation (R7.2) |
| tasks/endpoint-migration.yml / Write the migration's evidence | const:/tmp; parity:private tempfile children allocated after admission, written and removed without a recursive parent operation (R7.2) |
| tasks/endpoint-migration.yml / Confirm the node's registration on the controller | parity:read-only confirmation on the selected other controller; self-confirmation is refused pending #14 |
| tasks/endpoint-migration.yml / Keep the registration's confirmation for the receipt | const:/tmp; parity:private tempfile children allocated after admission, written and removed without a recursive parent operation (R7.2) |
| tasks/endpoint-migration.yml / Write the registration's confirmation | const:/tmp; parity:private tempfile children allocated after admission, written and removed without a recursive parent operation (R7.2) |
| tasks/endpoint-migration.yml / Write the migration's receipt | parity:shared migration/receipt command owns its runtime footprint and is followed by strict closing (R7.2) |
| tasks/endpoint-migration.yml / Remove the migration's temporary files | const:/tmp; parity:private tempfile children allocated after admission, written and removed without a recursive parent operation (R7.2) |
| tasks/services.yml / Restart the billet node after non-binary inputs change | parity:shared manager reload or node service lifecycle; node enable/stop/start superset is admitted |
| tasks/services.yml / Enable and start the billet node | parity:shared manager reload or node service lifecycle; node enable/stop/start superset is admitted |
| tasks/endpoint-receipt.yml / Refresh the endpoint receipt | parity:receipt command owns durable publication and revalidates running evidence (R7.2) |
| tasks/services.yml / Stop and disable the billet node unless it may run here | parity:shared manager reload or node service lifecycle; node enable/stop/start superset is admitted |
| tasks/retirement-settled-closing.yml / Check settled closing through the retirement command | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| handlers/main.yml / Reload systemd | parity:shared manager reload or node service lifecycle; node enable/stop/start superset is admitted |
| handlers/main.yml / Inspect whether the manager reports a pending reload after unit-file changes | parity:read-only observation or node validation; exact full module arguments and definitions are bound |
| handlers/main.yml / Reload systemd after the role's own unit-file changes | parity:shared manager reload or node service lifecycle; node enable/stop/start superset is admitted |
| handlers/main.yml / Reload billet guest networking | parity:kernel network/firewall application; configuration file operands were admitted before rendering |
| handlers/main.yml / Restart billet dnsmasq | parity:shared manager reload or node service lifecycle; node enable/stop/start superset is admitted |
| handlers/main.yml / Reload billet firewall | parity:kernel network/firewall application; configuration file operands were admitted before rendering |

## Template sweep

| Template | Variable classifications | Fixed writes and consumer boundary |
|---|---|---|
| billet-ceph-health.service.j2 |  | No fixed-path output or shell redirection; consumer applies constant service/timer policy or typed network data. The template destination is separately admitted. |
| billet-ceph-health.timer.j2 |  | No fixed-path output or shell redirection; consumer applies constant service/timer policy or typed network data. The template destination is separately admitted. |
| billet-dnsmasq@.service.j2 |  | StateDirectory=billet-dnsmasq/%i is systemd-owned per-instance runtime state; interface names are typed and supply no separators. ExecStart has no shell or redirection. The daemon lease database is listed in dnsmasq.conf.j2. |
| billet-images-refresh.service.j2 |  | No shell redirection. billet images refresh owns kernel/image staging and durable installation, outside preventive operand admission per R7.2; the template supplies only constant argv and writable roots. |
| billet-images-refresh.timer.j2 |  | No fixed-path output or shell redirection; consumer applies constant service/timer policy or typed network data. The template destination is separately admitted. |
| billet-network.service.j2 |  | No fixed-path output or shell redirection; consumer applies constant service/timer policy or typed network data. The template destination is separately admitted. |
| billet-node.service.j2 | billet_effective_config — E:node paths and provider configuration parsed by the Go config loader; billet_firecracker_enabled — typed:branch condition emits constant unit directives only; billet_node_environment_files — typed:installed EnvironmentFile path and optionality checked by environment_specs and descriptor reader; billet_notify_ready — typed:branch condition emits notify or exec constants only; billet_server_environment — constant:retained node uses explicit installed billet_node_environment_files, bypassing this fallback; billet_server_environment_path — constant:/etc/billet/server.env; retained template uses typed installed environment instead; billet_server_prepare_only — constant:false on retained route; node_should_run is mandatory; billet_systemd_notify_ready — typed:keyed observed Type chooses notify or exec constants; billet_systemd_upgrade_probe — constant:false on retained route; binary transactions are refused; billet_systemd_upgrade_probe_hold — constant:false on retained route; binary transactions are refused; environment_file — typed:installed EnvironmentFile path and optionality checked by environment_specs and descriptor reader | No shell redirection. RuntimeDirectory is systemd-owned; the node command owns registration, TLS renewal and custody writes through existing Go durable file paths, rechecked at closing. Runtime footprints are R7.2 scope. |
| billet-server.service.j2 | billet_candidate_server_state_dir — constant:unreachable controller transaction template; retained unit inventory contains only billet-node.service; billet_ledger_mount_unit_name — constant:unreachable controller transaction template; retained unit inventory contains only billet-node.service; billet_ledger_volume_id — constant:unreachable controller transaction template; retained unit inventory contains only billet-node.service; billet_notify_ready — typed:branch condition emits notify or exec constants only; billet_server_environment — constant:retained node uses explicit installed billet_node_environment_files, bypassing this fallback; billet_server_environment_path — constant:/etc/billet/server.env; retained template uses typed installed environment instead; billet_server_prepare_only — constant:false on retained route; node_should_run is mandatory; billet_service_group — constant:unreachable controller transaction template; retained unit inventory contains only billet-node.service; billet_service_user — constant:unreachable controller transaction template; retained unit inventory contains only billet-node.service; billet_systemd_notify_ready — typed:keyed observed Type chooses notify or exec constants; billet_systemd_upgrade_probe — constant:false on retained route; binary transactions are refused; billet_systemd_upgrade_probe_hold — constant:false on retained route; binary transactions are refused | Unreachable on retained route: only dormant candidate/steady-state transaction templates reference it, and binary transactions refuse before ordinary work. No retired controller template is rendered by the retained unit inventory. |
| billet.yaml.j2 | billet_config_document — E:whole rendering is serialized and parsed by the Go config loader before mutation | No fixed-path output or shell redirection; consumer applies constant service/timer policy or typed network data. The template destination is separately admitted. |
| bridge.netdev.j2 | item — typed:network loop fields are literal interface names and IPv4 values from validate-interpreted-inputs.yml | No fixed-path output or shell redirection; consumer applies constant service/timer policy or typed network data. The template destination is separately admitted. |
| bridge.network.j2 | item — typed:network loop fields are literal interface names and IPv4 values from validate-interpreted-inputs.yml | No fixed-path output or shell redirection; consumer applies constant service/timer policy or typed network data. The template destination is separately admitted. |
| ceph-health.sh.j2 | billet_alert_email — typed:single mailbox without controls; shell consumer uses quote; billet_alert_from — typed:single mailbox without controls; shell consumer uses quote | /var/lib/billet/health/ceph.last: mktemp in the same health directory, write only its private name, then mv -fT replaces a planted final symlink, including one naming a directory; a symlink is never read as the previous value. mkdir is a constant health directory. mail uses shell-quoted typed mailboxes; SMTP logs to syslog. |
| dnsmasq.conf.j2 | billet_guest_dns_cache_size — typed:int filter emits only a decimal integer; billet_guest_dns_forward_max — typed:int filter emits only a decimal integer; billet_guest_dns_servers — typed:IPv4 or IPv6 literals; item — typed:network loop fields are literal interface names and IPv4 values from validate-interpreted-inputs.yml; server — typed:IPv4 or IPv6 literal validated in validate-interpreted-inputs.yml | The DHCP lease database is daemon-owned runtime state under /var/lib/billet-dnsmasq/<typed-interface>/dnsmasq.leases, not shell redirection. systemd StateDirectory owns the directory. Daemon internals are R7.7 scope; inventory cannot add a leasefile/logfile directive because every interpolated field is typed. |
| msmtprc.j2 | billet_alert_from — typed:single mailbox without controls; shell consumer uses quote; billet_smtp_host — typed:single-line DNS hostname or IP literal; billet_smtp_password — typed:single line without controls; billet_smtp_port — typed:decimal port 1..65535; billet_smtp_username — typed:single line without controls | No file output directive: syslog LOG_MAIL replaces logfile /var/log/msmtp.log. TLS, STARTTLS and the trust bundle pathname are constants; all five interpolated SMTP operands are typed before templates. |
| network.nft.j2 | billet_effective_config — E:node paths and provider configuration parsed by the Go config loader; billet_guest_denied_ipv4 — typed:IPv4 network literals; billet_guest_denied_ipv6 — typed:IPv6 network literals; billet_guest_host_tcp_ports — typed:decimal ports 1..65535; billet_guest_host_udp_ports — typed:decimal ports 1..65535; billet_image_verify_port — typed:decimal port 1..65535; default is E-derived; billet_networks — typed:unique interface names and literal IPv4 fields/prefixes; network — typed:network loop fields are literal interface names and IPv4 values from validate-interpreted-inputs.yml | No fixed-path output or shell redirection; consumer applies constant service/timer policy or typed network data. The template destination is separately admitted. |
