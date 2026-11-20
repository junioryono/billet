# internal/nodeplane

The control plane's half of the node wire: routes, guards, dispatch, and the compute barrier loop. Load `billet-node-wire` and `billet-capacity` before changing anything here.

- A registration proves who a node is; only a command proves what it may do. `/v1/register` authenticates before it reads a body.
- The plane decodes bodies strictly (`DisallowUnknownFields`, and one JSON value with nothing after it) and a node decodes responses leniently, so a new field is gated on the protocol version.
- Commands to a host are a queue with a ten-minute timeout that starts when queued. A node runs one at a time, except launches on a provider that allows several (four on Firecracker) and, from VersionDestroysBeside, destroys there, which run beside its loop one at a time; a tend or a sweep there takes a turn in the destroys' line, and every other command waits for both. A poll names the destroys the node holds, and only those it does not name are given back as lost. `takeLocked` keeps back a launch while a destroy of its request is out to that node, and every destroy past `nodeapi.MaxDestroysRunning` out; a destroy's proof forgets only the ownership records delivered by the time it was taken (`leaseOwner.seq`).
- An inventory is an observation, never permission to claim another node's lease, and a proof of absence is two observations spanning the grace.

Gates (CI runs them on every push; locally, `scripts/gates/verify.sh`): `scripts/gates/check.sh`.
