# Rules for every backend

Part of the `billet-providers-local` skill. Each rule is stated in full, with the measurement or incident behind it.

**`Accepts` is asked before anything expensive.** Minting a registration and then being refused leaves it on GitHub with nothing to consume it, one orphan per pull request. `docker` refuses untrusted work outright; `firecracker` refuses it until `node.firecracker.untrusted_bridge` names a separate network; `tart` refuses it until `node.tart.untrusted_isolation: softnet`; and in tart `Accepts` and the flag builder are one function, because written as two a mechanism admitted and not applied boots untrusted work on the trusted bridge.

**`List` errors rather than answering short.** Reconciliation frees the capacity of every lease absent from an inventory, so the dangerous failure is a short, empty, successful answer. `created` is not `running` (a container that exists and was never started never will be), and an unrecognised state still counts as running, because the caller destroys what is not running.

**One process writes the launch verdict from a closed vocabulary.** What billet may say about a failed launch comes from a status file its own launcher wrote (`launching` before the identity check, `started` before the pid, `command-missing`), never the guest's stdout or stderr, because the registration travels on that delivery's stdin and a guest whose shell is `cat >&2` reflects a credential into a node log. billet reads the status and the pid in one query: `launching` with no pid is conclusive, `started` with no pid is transient once and conclusive on a second observation. `command-missing` is conclusive, because retrying a command the guest lacks burns the node's single slot for the full ten-minute timeout on every job.

**A cancelled command is not a returned one.** `exec.CommandContext` kills the process and then `Wait` blocks until the output pipes close, which anything the process spawned holds open: a `tart list` whose child slept sixty seconds took all sixty despite a 300ms deadline. `cmd.WaitDelay` is what makes a bound real.

**Never quote the guest.** A launch failure destroys the VM, so naming `billet-runner.log` points at a machine that no longer exists, and quoting its head lets a job (dispatched the moment the runner registers) choose the bytes billet prints.
