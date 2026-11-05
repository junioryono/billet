# Bare-metal hosts

What was measured of the two kinds of rented bare metal [ADR-026](../decisions/adr-026-rented-bare-metal-pool.md) builds a pool from, and when. Every AWS number is from `us-west-2`, every phoenixNAP number from its API on the day named.

## AWS bare-metal instances, 2026-11-05

**How.** Each instance was launched from Ubuntu 24.04's own image into a public subnet, with `--instance-initiated-shutdown-behavior terminate` and a user-data script that wrote what it found to the serial console, which `get-console-output` read back, and then shut the instance down. Launch is the instance's `LaunchTime` as `describe-instances` reported it; boot is when the user-data script began, with the kernel's own uptime beside it. The second pair also timed an 8 GiB sequential write and read of the first instance-store disk with `dd` and `O_DIRECT`, and the single-thread speed of `openssl speed sha256` on 16 KiB blocks for 5 s, alone and then with a busy loop on every other cpu. Single-thread speed was measured on the host, not inside a guest, and with no cpu pinned; the guest's is owed with the first Firecracker node on one of these. The instance-store sizes, the sustained clock and the SMT of each type are what `describe-instance-types` says, not what the probe read.

| Type | Launch to boot | Kernel uptime at boot | vCPU, memory | CPU | KVM | Instance store | On demand |
|---|---|---|---|---|---|---|---|
| `c7i.metal-24xl` | 29 s | 12.65 s | 96, 188 GiB | Xeon Platinum 8488C | yes | none | $4.284/h |
| `c7g.metal` | 21 s | 10.52 s | 64, 126 GiB | Neoverse-V1 | yes | none | $2.320/h |
| `z1d.metal` | 47 s | 14.51 s | 48, 378 GiB | Xeon Platinum 8151 | yes | 2 × 900 GB | $4.464/h |
| `m7gd.metal` | 23 s | 10.61 s | 64, 251 GiB | Neoverse-V1 | yes | 2 × 1900 GB | $3.417/h |

| Type | 8 GiB write | 8 GiB read | sha256, one thread alone | with every other cpu busy |
|---|---|---|---|---|
| `z1d.metal` | 8.09 s, 1.0 GiB/s | 3.73 s, 2.1 GiB/s | 516 MB/s | 269 MB/s |
| `m7gd.metal` | 4.37 s, 1.8 GiB/s | 3.00 s, 2.7 GiB/s | 1,629 MB/s | 1,630 MB/s |

- **These four reached their first script 21 to 47 s after launch.** The kernel had been up for 10 to 15 s of that; what took the rest (AWS's allocation, power-on, firmware) was not separated. Four launches set no bound, and this is not the time from an order to a registered node, which is owed.
- **The first two types do not fit the pool's shape.** `c7i.metal-24xl` and `c7g.metal` have 2 GiB per vCPU and no instance store. `describe-instance-types` filtered to bare metal with instance storage and at least 4 GiB per vCPU names, among others, `m7gd.metal` (64 arm64 vCPU, $3.417/h), `m8gd.metal-24xl` (96 arm64, $5.535/h), `z1d.metal` (48 x86, which AWS rates at a sustained 4.0 GHz, $4.464/h), `m6id.metal` (128 x86, $7.594/h) and `m8id.metal-48xl` (192 x86, $12.530/h).
- **On these two hosts, a busy host halved the x86 thread and left the arm64 one alone.** On `z1d.metal` a single thread lost 48% when the other 47 cpus were busy; one of those 47 is its own core's second hyperthread, and the all-core clock and the shared memory system changed too, and this run did not separate them, so the cause is not known. `m7gd.metal`, which AWS lists without SMT, lost nothing. The two architectures' sha256 numbers are not comparable with each other: Neoverse-V1 has SHA-2 instructions and this Xeon generation does not, and no cpu flags were read to confirm either.
- **The quota.** Running on-demand standard instances, the quota every type above draws from, was 256 vCPU on this account after a raise; before it, the quota was below the 96 one `c7i.metal-24xl` needs on its own.
- **The quota refusal.** A launch of one `m8id.metal-96xl` (384 vCPU, past the 256 quota) at 13:36 was refused synchronously with `VcpuLimitExceeded` ("You have requested more vCPU capacity than your current vCPU limit of 256 allows for the instance bucket that the specified instance type belongs to"), and a lookup by its Name tag found no instance, so nothing started. The AWS source reads that code as a quota answer. Every other launch was accepted on the first call; a no-stock answer was not met, and the source reads the capacity codes the `ec2` provider already treats as one.

## phoenixNAP Bare Metal Cloud, 2026-11-05

**How.** An API client created in the portal, its credentials read from files and given to `curl` through a netrc file, and a token from the client-credentials grant at `auth.phoenixnap.com`, whose scope came back as `bmc.read bmc`.

- **The catalogue.** `GET /billing/v1/products?productCategory=SERVER` listed 456 hourly plans across six locations (Ashburn, Chicago, Phoenix, Seattle, Amsterdam, Singapore), each type also offered on a reservation of one, twelve, twenty-four or thirty-six months. The cheapest hourly type was $0.08/h (a 4-core Xeon E3 with SATA SSD); the cheapest with NVMe $0.16/h (`s1.c1.small`, 6-core, 12-thread Xeon E-2276G, 64 GiB, 1 TB NVMe), which already fits the pool's 4 GiB per thread; the 8-core, 16-thread Xeon E-2388G with 64 GiB and 1 TB NVMe (`s2.c2.small`) is $0.31/h.
- **Stock is per location and small.** `GET /billing/v1/product-availability?productCategory=SERVER&showOnlyMinQuantityAvailable=true` reported `s2.c2.small` available in five locations, nine at most (Chicago); the larger types showed one to seven each, mostly in Phoenix and Ashburn. A pool on this source must order where the stock is, and read it before every order.
- **A new account may hold one server.** `GET /bmc/v1/quotas` answered `Servers` with a limit of 1, `Public IPs` 15, `Public IPv6s` 1 and `Storage Networks` 3000 GB. A pool cannot grow past one host here until that is raised.
- **The order was refused.** `POST /bmc/v1/servers` for one hourly `s1.c1.small` in Ashburn answered `403` with `The request failed since this resource cannot be accessed by the provided credentials.`, although the same token read the catalogue, the stock, the quotas and the (empty) server list. Whether the API client lacks permission to create, or the account cannot yet order, was not established; the boot time, the guest's speed and the NVMe of a phoenixNAP server are owed until an order is accepted.

## Still to measure

- A no-stock and a quota refusal from each source, read as the scaler will read them.
- The time from an order to a registered node, with the order-bound join of ADR-026.
- Single-thread speed inside a Firecracker guest under full load, on each candidate, with cpus pinned so a sibling hyperthread is told apart from the rest of the load.
- `m6id.metal`, `m8gd.metal-24xl` and `m8id.metal-48xl`, which fit the shape on paper and were not launched.
- What `DescribeInstances` answers for a terminated metal instance, and for how long, which is what proves an AWS pool host gone.
- Reading a host's key fingerprint from its console with `GetConsoleOutput`, as the join of ADR-026 does.
- A store volume attached to a metal instance and handed to a Firecracker guest.
- phoenixNAP: everything after an accepted order.
