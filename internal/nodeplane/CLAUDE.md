# internal/nodeplane

The control plane's half of the node wire: routes, guards, dispatch, and the compute barrier loop. Load `billet-node-wire` and `billet-capacity` before changing anything here.

- A registration proves who a node is; only a command proves what it may do. `/v1/register` authenticates before it reads a body.
- The plane decodes bodies strictly (`DisallowUnknownFields`) and a node decodes responses leniently, so a new field is gated on the protocol version.
- Commands to a host are a queue with a ten-minute timeout that starts when queued. A node runs one at a time, except launches on a provider that allows several (four on Firecracker), and every other command waits for the launches in flight.
- An inventory is an observation, never permission to claim another node's lease, and a proof of absence is two observations spanning the grace.

Gates: `make check`.
