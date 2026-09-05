# converge-cloudflare-warp

Reach Billet hosts from CI as a **WARP client enrolled with a service token**, scoped by one Gateway rule. No per-host Access application, no SSH proxy, no inbound port.

**This is optional.** Billet needs no inbound connectivity to run jobs — nodes always dial outbound to the control plane. This module is about *configuration management*. See [Reaching your hosts to converge them](../../../docs/deploying/reaching-hosts.md) for the alternatives.

## When this is the right route

Your hosts are **already reachable on private addresses** inside your Zero Trust network: a machine at home that is a Mesh node, a controller in a VPC that a `cloudflared` connector advertises a route to. What is missing is a way for a GitHub-hosted runner to be *on* that network for the length of a converge. This module puts it there.

It is the route that worked first time on a real hybrid deployment where [Route C](../converge-cloudflare)'s Access-for-Infrastructure SSH proxy never engaged: the target, the CA, the identity-provider pin and the hostname were all correct, no certificate was ever issued, and the Access logs stayed empty. A WARP client and plain SSH have no proxy to engage.

## What it creates

| Resource | What it is |
|---|---|
| `cloudflare_zero_trust_access_service_token.ci` | The token the runner enrols with. `create_before_destroy`, so a rotation never leaves CI holding a token authorised for nothing. |
| `cloudflare_zero_trust_access_policy.enrol` | A **Service Auth** (`non_identity`) policy including exactly that token: "this token may enrol a device". An `allow` decision with a service-token include never matches, because a service token presents no identity for `allow` to admit. |
| `cloudflare_zero_trust_gateway_policy.allow` | One **L4 allow** rule keyed on `non_identity@<team>.cloudflareaccess.com`, admitting exactly `host_addresses`. This is the enrolled device's *entire* reach. |

## What it deliberately does not do

**It does not touch your WARP enrolment application.** That application (the dashboard's *Device enrollment permissions*) is the one thing between every employee and their own network. Adopting it into Terraform means pinning every live field: in provider v5 an optional attribute omitted from config plans to `null` on an adopted resource, and `policies` is one of them — importing the application with `policies` omitted would plan to **remove the employee rule**, and on a root applied unattended that is an account-wide lockout. A module cannot know your live values, so it does not try.

Attach `enrollment_policy_id` yourself, once:

- in the dashboard: *Settings → WARP Client → Device enrollment permissions → Manage → add the reusable policy* named `<name>: service-token device enrolment`; or
- in your own root, if you already manage that application: add `{ id = module.converge.enrollment_policy_id, precedence = <after your employee rule> }` to its `policies`, with every other live field pinned and every unmanaged optional under `ignore_changes`.

Until it is attached the token enrols nothing, which is the failure you can see.

## Usage

```hcl
module "converge" {
  source = "github.com/junioryono/billet//terraform/modules/converge-cloudflare-warp?ref=v0.8.0"

  account_id     = var.cloudflare_account_id
  team_name      = "example"               # the <team> in <team>.cloudflareaccess.com
  host_addresses = ["100.96.0.14", "10.60.0.10"]
  precedence     = 3                       # before your block rule for the private ranges
}
```

`host_addresses` are **bare addresses, never `/32`**: Gateway stores `10.60.0.10/32` as `10.60.0.10` and reads it back that way, so a rule written with `/32`s is a permanent one-line diff on a module that must reach *No changes* after every apply. The variable refuses a prefix.

`precedence` has **no default** on purpose. Gateway precedence is one account-wide sequence: the rule must sort *before* whatever blocks your private ranges or it never matches, and a default number would land on whatever rule already holds it.

**The device profile must include the destinations.** WARP's split tunnel decides what enters the tunnel at all; a service-token device receives the default profile (or one whose match expression names `non_identity@`), and if that profile's include list does not carry the hosts' ranges the traffic never reaches Gateway and the rule never matters.

## Trade-offs, stated

- `non_identity@<team>.cloudflareaccess.com` is a **shared principal**: every service-token device in the account carries it. The Gateway rule is therefore the whole scope — one rule per fleet, not per token. A second fleet gets a second rule (or a second entry in this list), not a second token.
- The token is valid for `token_duration` (a year by default) and `terraform plan` does not warn on expiry. Put the date in a calendar.
- Rotate with the module's two rotation inputs together, **not** with `-replace`: set `client_secret_version` to the current version plus one (a fresh token is `1`; `terraform state show` has the current value) and `previous_client_secret_expires_at` to a timestamp a day or two out, apply, and Cloudflare mints a new secret under the same client id while the old one stays valid until then, so the enrolment policy stays satisfied while the CI secret is updated. Either input alone is refused: the provider itself refuses the expiry without the version (measured on v5), and a version without a window would invalidate the previous secret the moment CI needed it. A replace mints a new id, rewires the policy to it in the same apply, and leaves the old token authorised for nothing before the copy can happen — `create_before_destroy` orders that replacement, it does not give CI the overlap.

## In CI

Read the two halves once and set them as secrets:

```bash
terraform output -raw service_token_client_id     | gh secret set CF_CONVERGE_SERVICE_TOKEN_ID
terraform output -raw service_token_client_secret | gh secret set CF_CONVERGE_SERVICE_TOKEN_SECRET
```

The runner installs the WARP client from a **fingerprint-verified** signing key, writes `mdm.xml` from stdin so the secret never lands in argv or a world-readable file, restarts the daemon (the client reads the file once at startup), waits for *Connected*, and **proves the path with a TCP check before trusting it** — a device that enrolled and is then denied by Gateway looks identical to a connected one until the first SSH times out.

```bash
sudo mkdir -p /var/lib/cloudflare-warp
printf '%s\n' \
  '<dict>' \
  '  <key>organization</key><string>'"$CF_TEAM"'</string>' \
  '  <key>auth_client_id</key><string>'"$CF_CLIENT_ID"'</string>' \
  '  <key>auth_client_secret</key><string>'"$CF_CLIENT_SECRET"'</string>' \
  '  <key>service_mode</key><string>warp</string>' \
  '  <key>auto_connect</key><integer>1</integer>' \
  '  <key>onboarding</key><false/>' \
  '</dict>' \
  | sudo install -m 0600 -o root -g root /dev/stdin /var/lib/cloudflare-warp/mdm.xml
sudo systemctl restart warp-svc
warp-cli --accept-tos connect
# wait for `warp-cli --accept-tos status` to say Connected, then:
for addr in "$UBUNTU_HOST_ADDR" "$CONTROL_PLANE_ADDR"; do nc -z -w 5 "$addr" 22; done
```

Then Ansible connects over **plain SSH** with the fleet's own CI key and **committed host-key pins** (never an `ssh-keyscan` at job time: this job authenticates the host as much as the host authenticates it). At the end, `warp-cli --accept-tos registration delete`, so a registration is not left listed until inactivity expiry.

When the TCP check fails, Gateway's network log (*Logs → Network*, filter by destination) names the rule that matched. Three causes cover it: the device did not enrol (the policy is not attached to the enrolment application), the rule does not permit this destination for `non_identity@`, or the device profile's split tunnel does not include it.

## Inputs of note

`host_addresses` (bare IPv4 addresses), `precedence` (required, before your block rule), `team_name`, `client_secret_version` and `previous_client_secret_expires_at` (rotation, together, see above), `token_duration`.

## Outputs

| Output | |
|---|---|
| `service_token_client_id`, `service_token_client_secret` | The two halves of the credential, both sensitive. |
| `enrollment_policy_id` | The policy to attach to your WARP enrolment application. |
| `principal` | `non_identity@<team>.cloudflareaccess.com`, what the rule is keyed on and what Gateway's logs show. |
| `gateway_policy_id` | The L4 rule. |

## Tests

`terraform test` runs against a mocked cloudflare provider: the enrolment policy is Service Auth for exactly this token, the Gateway rule names exactly the given hosts as bare addresses and is keyed on the principal, and a `/32`, a hostname, an empty fleet and a team *domain* are each refused by their variable.
