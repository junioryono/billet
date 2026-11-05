package ec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"

	"github.com/junioryono/billet/internal/provider"
)

// A POOL HOST IS NOT A JOB'S INSTANCE, and it carries neither the owner tag nor a
// lease's name. List fails a node's whole inventory on an owner-tagged instance whose
// Name names no lease, so a pool host tagged like a job's instance would stop every
// EC2 node in its deployment from reporting. Its own tags keep the two apart.
const (
	poolOwnerTag = "sh.billet.pool-owner"
	poolOrderTag = "sh.billet.pool-order"
	poolNamePfx  = "billet-pool-"
)

// poolOrderPattern bounds an order identifier: it becomes a ClientToken (at most 64
// ASCII characters), a tag filter value (where `*` is a wildcard) and a Name.
var poolOrderPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ErrHostNotFound is EC2 saying it has no such instance, which is never proof the
// instance is gone: DescribeInstances is eventually consistent.
var ErrHostNotFound = errors.New("ec2: no such instance")

// ErrLaunchNotSent is a pool launch refused before RunInstances was called, so no
// instance can exist for it.
var ErrLaunchNotSent = errors.New("ec2: the pool launch was not sent")

// ErrNotAPoolHost is an instance that does not carry this deployment's pool tag, which
// a pool source neither describes nor terminates.
var ErrNotAPoolHost = errors.New("ec2: the instance is not one of this deployment's pool hosts")

// Code reports the EC2 API error code in an error chain, and whether there was one.
func Code(err error) (string, bool) { return codeOf(err) }

// CapacityRefusal reports whether a code is AWS refusing a launch for want of capacity,
// synchronously and having started nothing.
func CapacityRefusal(code string) bool { return capacityRefusal(code) }

// Hosts launches, reads and ends the long-lived bare-metal hosts of a pool (ADR-028)
// through this provider's client, so there is one EC2 client.
type Hosts struct{ p *Provider }

// Hosts is the provider's pool-host surface.
func (p *Provider) Hosts() *Hosts { return &Hosts{p: p} }

// HostLaunch is one pool host to start.
type HostLaunch struct {
	// Order is the pool's order, which becomes the launch's ClientToken, so a launch
	// whose answer was lost and is sent again starts no second host.
	Order        string
	InstanceType string
	Image        string
	// UserData is the host's first boot, carrying its join token.
	UserData []byte
}

// A LAUNCH CARRIES THE JOIN TOKEN IN ITS USER DATA, so it redacts itself, on a value
// receiver, wherever it is rendered.
func (l HostLaunch) String() string {
	return "ec2.HostLaunch{order=" + l.Order + " type=" + l.InstanceType + " image=" + l.Image + " user data redacted}"
}

// GoString covers %#v.
func (l HostLaunch) GoString() string { return l.String() }

// Format catches every verb.
func (l HostLaunch) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, l.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps the user data out of anything that serialises a launch.
func (l HostLaunch) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

// LogValue is what slog consults.
func (l HostLaunch) LogValue() slog.Value { return slog.StringValue(l.String()) }

// HostRecord is one pool host as EC2 describes it.
type HostRecord struct {
	ID           string
	State        string
	InstanceType string
	PrivateIP    string
	Order        string
}

// Launch starts one pool host. Its errors keep EC2's code and drop EC2's message,
// because the request carried the join token.
func (h *Hosts) Launch(ctx context.Context, l HostLaunch) (HostRecord, error) {
	p := h.p
	switch {
	case !poolOrderPattern.MatchString(l.Order):
		return HostRecord{}, fmt.Errorf("%w: pool order %q is not 1 to 48 lowercase letters, digits and dashes",
			ErrLaunchNotSent, l.Order)
	case l.InstanceType == "" || l.Image == "":
		return HostRecord{}, fmt.Errorf("%w: a pool host needs an instance type and an image", ErrLaunchNotSent)
	case len(l.UserData) > maxUserData:
		return HostRecord{}, fmt.Errorf("%w: a pool host's boot script is %d bytes and ec2 accepts %d",
			ErrLaunchNotSent, len(l.UserData), maxUserData)
	}

	params := url.Values{}
	params.Set("Action", "RunInstances")
	params.Set("ImageId", l.Image)
	params.Set("InstanceType", l.InstanceType)
	params.Set("MinCount", "1")
	params.Set("MaxCount", "1")
	params.Set("ClientToken", "pool-"+l.Order)
	p.setNetwork(params, provider.TrustTrusted)
	// IMDSv2 REQUIRED, ONE HOP, as for a job's instance: the user data holds the join
	// token, and a guest's packets leave through the host's NAT one hop further away.
	params.Set("MetadataOptions.HttpTokens", "required")
	params.Set("MetadataOptions.HttpPutResponseHopLimit", "1")
	if p.cfg.InstanceProfile != "" {
		params.Set("IamInstanceProfile.Name", p.cfg.InstanceProfile)
	}
	name := poolNamePfx + l.Order
	for i, kind := range []string{"instance", "volume"} {
		n := strconv.Itoa(i + 1)
		params.Set("TagSpecification."+n+".ResourceType", kind)
		params.Set("TagSpecification."+n+".Tag.1.Key", nameTag)
		params.Set("TagSpecification."+n+".Tag.1.Value", name)
		params.Set("TagSpecification."+n+".Tag.2.Key", poolOwnerTag)
		params.Set("TagSpecification."+n+".Tag.2.Value", p.owner)
		params.Set("TagSpecification."+n+".Tag.3.Key", poolOrderTag)
		params.Set("TagSpecification."+n+".Tag.3.Value", l.Order)
	}
	params.Set("UserData", base64.StdEncoding.EncodeToString(l.UserData))

	var out runInstancesResponse
	if err := p.api.call(ctx, params, &out); err != nil {
		return HostRecord{}, fmt.Errorf("ec2: launch pool host %s on %s: %w", l.Order, l.InstanceType, withoutMessage(err))
	}
	if len(out.Instances) == 0 || out.Instances[0].InstanceID == "" {
		return HostRecord{}, fmt.Errorf("ec2: the launch of pool host %s was accepted and described no instance", l.Order)
	}
	p.log.Info("launched pool host", "order", l.Order, "instance", out.Instances[0].InstanceID, "type", l.InstanceType)

	rec := hostRecord(out.Instances[0])
	rec.Order = l.Order

	return rec, nil
}

// Find reads back the host an order launched. ok is false only when EC2 listed none;
// two live hosts for one order is an error, never a choice.
func (h *Hosts) Find(ctx context.Context, order string) (HostRecord, bool, error) {
	if !poolOrderPattern.MatchString(order) {
		return HostRecord{}, false, fmt.Errorf("ec2: pool order %q is not 1 to 48 lowercase letters, digits and dashes", order)
	}
	items, err := h.describe(ctx, url.Values{
		"Filter.1.Name":    {"tag:" + poolOwnerTag},
		"Filter.1.Value.1": {h.p.owner},
		"Filter.2.Name":    {"tag:" + poolOrderTag},
		"Filter.2.Value.1": {order},
	})
	if err != nil {
		return HostRecord{}, false, err
	}
	var live []HostRecord
	var ended *HostRecord
	for _, item := range items {
		// AN INSTANCE THE FILTER RETURNED THAT IS NOT A WELL-FORMED POOL HOST is not
		// skipped, whatever its order tag says: a list read as none licenses ordering
		// again, and a missing order tag is not a different order.
		if !h.isPoolHost(item) {
			return HostRecord{}, false, fmt.Errorf("%w: an instance listed for order %s", ErrNotAPoolHost, order)
		}
		if v, _ := item.tag(poolOrderTag); v != order {
			continue
		}
		rec := hostRecord(item)
		if rec.State == "terminated" {
			ended = &rec

			continue
		}
		live = append(live, rec)
	}
	switch {
	case len(live) == 1:
		return live[0], true, nil
	case len(live) > 1:
		return HostRecord{}, false, fmt.Errorf("ec2: %d live pool hosts carry order %s", len(live), order)
	case ended != nil:
		return *ended, true, nil
	}

	return HostRecord{}, false, nil
}

// Describe reads one pool host by its instance id.
func (h *Hosts) Describe(ctx context.Context, id string) (HostRecord, error) {
	if id == "" {
		return HostRecord{}, errors.New("ec2: describe needs an instance id")
	}
	items, err := h.describe(ctx, url.Values{"InstanceId.1": {id}})
	if err != nil {
		if code, ok := codeOf(err); ok && code == "InvalidInstanceID.NotFound" {
			return HostRecord{}, fmt.Errorf("%w: %s", ErrHostNotFound, id)
		}

		return HostRecord{}, err
	}
	for _, item := range items {
		if item.InstanceID != id {
			continue
		}
		if !h.isPoolHost(item) {
			return HostRecord{}, fmt.Errorf("%w: %s", ErrNotAPoolHost, id)
		}

		return hostRecord(item), nil
	}

	return HostRecord{}, fmt.Errorf("%w: %s", ErrHostNotFound, id)
}

// Terminate asks EC2 to end a pool host, after reading that it is one. It returns when
// the request is accepted, which is not the host ending: Describe's `terminated` is.
func (h *Hosts) Terminate(ctx context.Context, id string) error {
	if _, err := h.Describe(ctx, id); err != nil {
		return err
	}
	params := url.Values{}
	params.Set("Action", "TerminateInstances")
	params.Set("InstanceId.1", id)
	if err := h.p.api.call(ctx, params, nil); err != nil {
		return fmt.Errorf("ec2: terminate pool host %s: %w", id, err)
	}
	h.p.log.Info("requested pool host termination", "instance", id)

	return nil
}

// Console is the latest of what a pool host wrote to its serial console, which only
// that instance can write; the join reads the host's key fingerprint there (ADR-028).
func (h *Hosts) Console(ctx context.Context, id string) (string, error) {
	if _, err := h.Describe(ctx, id); err != nil {
		return "", err
	}

	return h.p.consoleOutput(ctx, id, true)
}

// isPoolHost is this deployment's pool tag, an order tag a pool could have written,
// and NOT the job instances' owner tag: an instance carrying both is neither, and
// nothing here describes, terminates or reads it.
func (h *Hosts) isPoolHost(item instanceItem) bool {
	owner, pooled := item.tag(poolOwnerTag)
	order, ordered := item.tag(poolOrderTag)
	_, jobTagged := item.tag(ownerTag)

	return pooled && owner == h.p.owner && ordered && poolOrderPattern.MatchString(order) && !jobTagged
}

// hostsDescribeResponse is DescribeInstances, with its root element checked: an answer
// with another root, or none, decodes into nothing and would read as no instances.
type hostsDescribeResponse struct {
	XMLName      xml.Name `xml:"DescribeInstancesResponse"`
	Reservations []struct {
		Instances []instanceItem `xml:"instancesSet>item"`
	} `xml:"reservationSet>item"`
	NextToken string `xml:"nextToken"`
}

func (h *Hosts) describe(ctx context.Context, params url.Values) ([]instanceItem, error) {
	params.Set("Action", "DescribeInstances")
	var items []instanceItem
	seen := map[string]struct{}{}
	for {
		var out hostsDescribeResponse
		if err := h.p.api.call(ctx, params, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Reservations {
			for _, item := range r.Instances {
				// AN INSTANCE WITHOUT AN ID CANNOT BE ACCOUNTED FOR, and dropping it
				// would turn an incomplete answer into a shorter one.
				if item.InstanceID == "" {
					return nil, errors.New("ec2: the api described an instance without an id; refusing a partial answer")
				}
				items = append(items, item)
			}
		}
		if out.NextToken == "" {
			return items, nil
		}
		if _, repeated := seen[out.NextToken]; repeated {
			return nil, errors.New("ec2: the api repeated a pagination token; refusing to loop")
		}
		seen[out.NextToken] = struct{}{}
		params.Set("NextToken", out.NextToken)
	}
}

func hostRecord(item instanceItem) HostRecord {
	order, _ := item.tag(poolOrderTag)

	return HostRecord{
		ID:           item.InstanceID,
		State:        item.State.Name,
		InstanceType: item.InstanceType,
		PrivateIP:    item.PrivateIP,
		Order:        order,
	}
}
