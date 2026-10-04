# Measured facts

Part of the `billet-infra-terraform-ansible` skill: what was measured, dated where it was recorded.

- `DeleteProject` succeeds during a live build; the build runs to completion and `BatchGetBuilds` keeps answering for it.
- A lexical time comparison in the destroy guard was seven hours wrong on a laptop in a non-UTC zone.
- Two terraform modules were added and went green through every gate without being validated, linted or scanned, because the module list was a constant.
- `tflint --config` with a relative path: silently linted with bundled rules only.
- trivy 0.74 needs `GOEXPERIMENT=jsonv2` to install under Go 1.26.
- The bridge's dnsmasq at its defaults holds 150 names and forwards 150 queries at once; a fleet's guests ask for the same few hundred names thousands of times an hour, so the role sets `billet_guest_dns_cache_size: 10000` (the largest dnsmasq holds without warning) and `billet_guest_dns_forward_max: 1000` (2026-09-11). The guard table drops every guest-to-host packet but DHCP, DNS and the cache endpoint, so a guest's `ping` to its gateway fails on a healthy host and proves nothing.
- A compute host behind a residential gateway shares that gateway's NAT table with everything else at the site. For the reference deployment's gateway class its users REPORT a cap of 8192 sessions on older firmware and 32767 on newer, with new connections dropped near it; the deployment's own session count was not read (2026-09-11, `docs/reference/records/shared-uplink.md`), so the cap is a reported number and the saturation a hypothesis consistent with the symptoms, not a measurement.
