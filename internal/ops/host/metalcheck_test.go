package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ebss3"
)

// A METAL NODE'S CHECK REFUSES WHAT ITS STORE WOULD REFUSE AT START, before it reads the
// bucket: an instance it cannot name, and one in another zone than its volumes are made
// in, by the store's own rule.
func TestTheMetalCacheCheckRefusesWhatTheStoreWould(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Node: &config.NodeConfig{
		Provider: config.ProviderFirecracker, Site: "aws-us-west-2a",
		EBSS3: &config.EBSS3Config{Region: "us-west-2", AvailabilityZone: "us-west-2a", Bucket: "billet-cache-example"},
	}}
	for name, tc := range map[string]struct {
		instance func(context.Context) (ebss3.Host, error)
		want     string
	}{
		"an instance it cannot read": {
			func(context.Context) (ebss3.Host, error) { return ebss3.Host{}, errors.New("no metadata service") },
			"could not read which instance that is",
		},
		"an instance in another zone": {
			func(context.Context) (ebss3.Host, error) {
				return ebss3.Host{InstanceID: "i-0123456789abcdef0", AvailabilityZone: "us-west-2b"}, nil
			},
			"attaches only in its own zone",
		},
		"not an instance": {
			func(context.Context) (ebss3.Host, error) {
				return ebss3.Host{InstanceID: "mi-0123", AvailabilityZone: "us-west-2a"}, nil
			},
			"is not an EC2 instance",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			env := cli.Env{Stdout: &out, Stderr: io.Discard, Stdin: strings.NewReader(""), Getenv: os.Getenv}
			err := checkMetalCacheStore(t.Context(), env, cfg, nil, tc.instance)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the check answered %v; want a refusal saying %q", err, tc.want)
			}
			if strings.Contains(out.String(), "attach to this instance") {
				t.Errorf("a refused instance was reported as one volumes attach to: %q", out.String())
			}
		})
	}
}
