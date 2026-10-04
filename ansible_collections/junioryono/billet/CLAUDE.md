# The Ansible collection

The `host` role (a Linux control plane and Firecracker compute host), `ssh_access`, the connector roles, `macos_node`, `development_host`, and the shipped `fleet` playbook. Load `billet-infra-terraform-ansible` before changing anything here, `billet-releases-and-upgrades` for the role's upgrade transaction, and `billet-controller-retirement` for any `retirement*.yml`.

- A converge restarts services, so it is never driven from a runner billet manages.
- The role's upgrade transaction is the second implementation of the Go one, and it runs inside the converge guard.
- Every JSON a role reads has fixtures written by the Go test that produces it (`BILLET_UPDATE_FIXTURES=1`); never edit one by hand.
- A mutation run over a Python file removes its `__pycache__` after every restore and runs the check with bytecode writes off (`python3 -B`): a same-size mutant restored within the second is otherwise executed from the cached bytecode, and not every check here disables writes itself (`billet-testing`).

Gates: the scenario targets in the Makefile for the state you changed, `make host-upgrade-order unit-parity emitted-block-check` for the transaction, and `scripts/check-retirement-shards.py` for a retirement case.
