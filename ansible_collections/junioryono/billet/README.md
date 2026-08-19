# billet Ansible collection

`junioryono.billet.host` turns an already installed Linux machine into a billet control plane and Firecracker compute host. It installs the billet binary supplied by the operator, creates the service identity and systemd units, installs Firecracker and Ceph client tooling, loads the RBD kernel client now and after every boot, creates isolated DHCP/NAT guest bridges, renders `billet.yaml`, checks the Ceph pools, and configures a periodic Ceph health check. The guest policy admits the configured image-verification callback only on the trusted bridge, so `billet images verify` works without exposing arbitrary host ports. It drains the node before changing any live guest-network input and restarts it after a selected Firecracker binary changes. A binary upgrade is transactional: before replacing desired-state files the role preserves the installed configuration and units, derives the ledger from the installed `server.state_dir`, refuses to combine a ledger move with a binary upgrade, stages the candidate, proves every configured Firecracker image or fetches and boot-verifies each compatible signed replacement, drains the node, stops the server, preserves the prior binary and stopped ledger under `/var/lib/billet/upgrades`, installs and validates the candidate as the only ledger writer, then starts the server before the node. A failure first proves both upgraded processes inactive, withdraws every generation imported for that attempt from rollback handles written before promotion, restores the prior configuration, units, binary and ledger, reloads systemd, and verifies the previously active services returned. If a process cannot be proved inactive, rollback stops before modifying the ledger and retains the recovery directory for manual inspection. It never creates a GitHub App, invents capacity, chooses disks, or starts billet until the supplied configuration passes `billet check`. An existing GitHub App private key is never replaced implicitly: a matching source is accepted idempotently, while a different source is refused so credential rotation remains an explicit operator action. Setting either service enable flag to false stops and disables a service previously installed by the role. Setting `billet_alert_email` back to empty or disabling Ceph stops and removes the Ceph health timer and removes `/etc/msmtprc` only when this role created it, including configurations created before the ownership sidecar existed.

The role can bootstrap a new single-host Ceph cluster only when `billet_ceph_bootstrap` is explicitly true and `billet_ceph_devices` names every device to consume. That path is destructive by definition and is never inferred from available disks.

Set `billet_binary_src` to the Linux binary the role should install. A repository can pin the collection and binary to one release by downloading it into a controller-side staging directory, then passing the verified file directly to the role:

```bash
billet_stage=$(mktemp -d)
curl -fsSL https://raw.githubusercontent.com/junioryono/billet/v0.3.0/scripts/install.sh | BILLET_VERSION=v0.3.0 BILLET_OS=linux BILLET_ARCH=amd64 BILLET_INSTALL_DIR="$billet_stage" sh
ansible-playbook site.yml -e "billet_binary_src=$billet_stage/billet"
```

A Billet contributor can instead build the binary from the same checkout as the collection. Keeping the binary input explicit makes the requested version auditable; the role owns the safe drain, image-compatibility, migration, recovery, and restart sequence when that input changes. An unchanged candidate and compatible recorded generation are metadata-only checks, so an ordinary second converge neither downloads the guest again nor reports a binary change.

See `roles/host/defaults/main.yml` for the complete input surface.

`junioryono.billet.development_host` is a separate, reusable developer-machine layer for Debian-family systemd hosts and macOS. It installs current Caddy and Terraform from their publishers' repositories, installs mkcert, stages and validates a caller-defined certificate/key pair before installation, and keeps a caller-supplied Caddyfile running across reboots through systemd or launchd. The role has no application or domain defaults: a repository supplies its SANs, proxy configuration and environment, while Billet owns package verification, CA trust, private-key permissions and service lifecycle. Disabling the proxy unloads and removes the prior systemd or launchd definition.

The role defaults to a localhost-only certificate and an empty proxy routing table. Set `billet_development_tls_sans` and `billet_development_proxy_config_src` for a real project. On macOS, Homebrew must already be installed; every formula and the launchd service are then converged by the role. See `roles/development_host/defaults/main.yml` for the complete input surface.

Run the playbook with `--check --diff` before converging a new or existing machine. The role still runs read-only identity, package and version probes in check mode, while deferring generated certificate validation until the files exist, so a first dry run can describe the installation without failing on tools it has not installed yet.

```yaml
- name: Configure development machines
  hosts: development
  gather_facts: true
  roles:
    - role: junioryono.billet.development_host
      billet_development_tls_sans:
        - "*.dev.example.test"
        - dev.example.test
        - localhost
        - 127.0.0.1
        - "::1"
      billet_development_proxy_config_src: "{{ playbook_dir }}/Caddyfile"
      billet_development_proxy_environment:
        DEVELOPMENT_CERT_FILE: "{{ billet_development_tls_cert_path }}"
        DEVELOPMENT_KEY_FILE: "{{ billet_development_tls_key_path }}"
```

Add a new Linux or macOS machine to that inventory group and rerun the playbook; package installation, local trust, certificate renewal on SAN changes, configuration validation and boot persistence all converge through the same role.
