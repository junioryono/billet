package config

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Pool host sources a config may name.
const (
	PoolSourceAWSMetal   = "aws-metal"
	PoolSourcePhoenixNAP = "phoenixnap"
)

// Pool defaults, used when the block leaves them out.
const (
	DefaultPoolInterval       = 30 * time.Second
	DefaultPoolRefusalBackoff = 10 * time.Minute
)

// PoolConfig is server.pool: a pool of rented bare metal the control plane orders
// Firecracker hosts from, ahead of demand (ADR-030).
type PoolConfig struct {
	// JoinServer is the node-wire address an ordered host dials to join, host:port,
	// as that host reaches it.
	JoinServer string `yaml:"join_server"`
	// BootstrapAddr is the enrollment listener an ordered host dials, when the
	// deployment serves one apart from the node wire.
	BootstrapAddr string `yaml:"bootstrap_addr,omitempty"`
	// BootPrelude is a script an ordered host runs on its first boot before `billet
	// join`: it installs billet and what a Firecracker node needs. Absolute.
	BootPrelude string `yaml:"boot_prelude"`
	// Interval is how often the pool looks at demand and its orders, a Go duration.
	Interval string `yaml:"interval,omitempty"`
	// RefusalBackoff is how long a choice a source refused (no stock, a quota) is
	// passed over before it is asked again, a Go duration.
	RefusalBackoff string `yaml:"refusal_backoff,omitempty"`
	// Architectures says, per architecture, how many hosts the pool keeps and may
	// hold, and where it orders them, in order of preference.
	Architectures map[Arch]PoolArch `yaml:"architectures"`
	// AWSMetal is the AWS bare-metal source.
	AWSMetal *PoolAWSMetal `yaml:"aws_metal,omitempty"`
	// PhoenixNAP is the phoenixNAP Bare Metal Cloud source.
	PhoenixNAP *PoolPhoenixNAP `yaml:"phoenixnap,omitempty"`
}

// PoolArch is one architecture's part of the pool.
type PoolArch struct {
	// Floor is how many hosts the pool keeps whatever the demand.
	Floor int `yaml:"floor"`
	// Max is the most hosts the pool holds, ordered, placed or uncertain.
	Max int `yaml:"max"`
	// Order is where hosts come from, first choice first.
	Order []PoolChoice `yaml:"order"`
}

// PoolChoice is one place a host may be ordered from.
type PoolChoice struct {
	Source   string `yaml:"source"`
	Shape    string `yaml:"shape"`
	Location string `yaml:"location,omitempty"`
}

// PoolShape is one machine type a source may order, with its architecture, which the
// source's own catalogue does not reliably say.
type PoolShape struct {
	Type string `yaml:"type"`
	Arch Arch   `yaml:"arch"`
}

// PoolAWSMetal orders bare-metal instances in one subnet.
type PoolAWSMetal struct {
	Region           string   `yaml:"region"`
	SubnetID         string   `yaml:"subnet_id"`
	AvailabilityZone string   `yaml:"availability_zone"`
	SecurityGroupIDs []string `yaml:"security_group_ids"`
	// InstanceProfile is the role a pool host runs under, for the site's store.
	InstanceProfile string `yaml:"instance_profile,omitempty"`
	// Images is the AMI a pool host boots, per architecture.
	Images map[Arch]string `yaml:"images"`
	Shapes []PoolShape     `yaml:"shapes"`
}

// PoolPhoenixNAP orders phoenixNAP servers. The API client's id and secret are read
// from the environment variables named here, never from the config.
type PoolPhoenixNAP struct {
	ClientIDEnv     string      `yaml:"client_id_env"`
	ClientSecretEnv string      `yaml:"client_secret_env"`
	Shapes          []PoolShape `yaml:"shapes"`
}

// PoolInterval is the effective interval.
func (p *PoolConfig) PoolInterval() time.Duration {
	if d, err := time.ParseDuration(p.Interval); err == nil && d > 0 {
		return d
	}

	return DefaultPoolInterval
}

// PoolRefusalBackoff is the effective backoff.
func (p *PoolConfig) PoolRefusalBackoff() time.Duration {
	if d, err := time.ParseDuration(p.RefusalBackoff); err == nil && d > 0 {
		return d
	}

	return DefaultPoolRefusalBackoff
}

// ShapeArch is the architecture a source's shape was declared with, and whether it
// was.
func (p *PoolConfig) ShapeArch(source, shape string) (Arch, bool) {
	var shapes []PoolShape
	switch {
	case source == PoolSourceAWSMetal && p.AWSMetal != nil:
		shapes = p.AWSMetal.Shapes
	case source == PoolSourcePhoenixNAP && p.PhoenixNAP != nil:
		shapes = p.PhoenixNAP.Shapes
	}
	for _, s := range shapes {
		if s.Type == shape {
			return s.Arch, true
		}
	}

	return "", false
}

// PoolEnrollAddr is the enrollment listener an ordered host dials: server.pool's
// bootstrap_addr when it is given, else the TLS router's enroll_addr, else
// server.bootstrap_listen when it names a host one can dial. Empty when there is none,
// which validation refuses.
func (s *ServerConfig) PoolEnrollAddr() string {
	if s.Pool != nil && strings.TrimSpace(s.Pool.BootstrapAddr) != "" {
		return strings.TrimSpace(s.Pool.BootstrapAddr)
	}
	if s.TLSRouter != nil && strings.TrimSpace(s.TLSRouter.EnrollAddr) != "" {
		return strings.TrimSpace(s.TLSRouter.EnrollAddr)
	}
	addr := strings.TrimSpace(s.BootstrapListen)
	if host, _, err := net.SplitHostPort(addr); err == nil && dialableFromElsewhere(host) {
		return addr
	}

	return ""
}

// dialableFromElsewhere is whether host names this machine to another one: not empty,
// not a wildcard, and not loopback, which a rented host would dial as itself.
func dialableFromElsewhere(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return false
	}
	ip := net.ParseIP(h)

	return ip == nil || !(ip.IsLoopback() || ip.IsUnspecified())
}

// validatePool checks server.pool.
func (s *ServerConfig) validatePool() []error {
	p := s.Pool
	if p == nil {
		return nil
	}
	var errs []error

	if host, port, err := net.SplitHostPort(p.JoinServer); err != nil || port == "" || !dialableFromElsewhere(host) {
		errs = append(errs, fmt.Errorf("server.pool.join_server %q must be the host:port an ordered host dials "+
			"to reach the node wire: no wildcard and no loopback, which the host would dial as itself", p.JoinServer))
	}
	if p.BootstrapAddr != "" {
		if host, port, err := net.SplitHostPort(p.BootstrapAddr); err != nil || port == "" || !dialableFromElsewhere(host) {
			errs = append(errs, fmt.Errorf("server.pool.bootstrap_addr %q must be the host:port an ordered host "+
				"dials: no wildcard and no loopback", p.BootstrapAddr))
		}
	}
	// AN ORDERED HOST JOINS OVER THE NETWORK OR NOT AT ALL: the node wire wants a
	// certificate it does not have yet, so without an enrollment listener it could
	// never ask, and it is billed meanwhile.
	switch {
	case strings.TrimSpace(s.BootstrapListen) == "":
		errs = append(errs, errors.New("server.pool needs server.bootstrap_listen: without it this control "+
			"plane enrolls nothing over the network, so no host the pool orders could ever join"))
	case s.PoolEnrollAddr() == "":
		errs = append(errs, fmt.Errorf("server.pool.bootstrap_addr is required: server.bootstrap_listen %q "+
			"names no host an ordered host can dial (a wildcard or loopback)", s.BootstrapListen))
	}
	if p.BootPrelude == "" || !filepath.IsAbs(p.BootPrelude) {
		errs = append(errs, fmt.Errorf("server.pool.boot_prelude %q must be an absolute path to the script a "+
			"host runs before it joins", p.BootPrelude))
	}
	for field, v := range map[string]string{"interval": p.Interval, "refusal_backoff": p.RefusalBackoff} {
		if v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("server.pool.%s %q must be a positive Go duration", field, v))
		}
	}

	if p.AWSMetal == nil && p.PhoenixNAP == nil {
		errs = append(errs, errors.New("server.pool names no source: give it aws_metal, phoenixnap, or both"))
	}
	errs = append(errs, p.AWSMetal.validate()...)
	errs = append(errs, p.PhoenixNAP.validate()...)

	if len(p.Architectures) == 0 {
		errs = append(errs, errors.New("server.pool.architectures is empty, so the pool would order nothing"))
	}
	archs := make([]Arch, 0, len(p.Architectures))
	for a := range p.Architectures {
		archs = append(archs, a)
	}
	sort.Slice(archs, func(i, j int) bool { return archs[i] < archs[j] })
	for _, a := range archs {
		errs = append(errs, p.validateArch(a, p.Architectures[a])...)
	}

	return errs
}

func (p *PoolConfig) validateArch(a Arch, pa PoolArch) []error {
	var errs []error
	field := "server.pool.architectures." + string(a)
	if !a.Valid() {
		return []error{fmt.Errorf("%s: %q is not an architecture billet knows", field, a)}
	}
	switch {
	case pa.Floor < 0:
		errs = append(errs, fmt.Errorf("%s.floor %d must not be negative", field, pa.Floor))
	case pa.Max < 1:
		errs = append(errs, fmt.Errorf("%s.max %d must be at least 1", field, pa.Max))
	case pa.Max < pa.Floor:
		errs = append(errs, fmt.Errorf("%s.max %d is below its floor %d", field, pa.Max, pa.Floor))
	}
	if len(pa.Order) == 0 {
		errs = append(errs, fmt.Errorf("%s.order is empty, so this architecture has nowhere to order from", field))
	}
	for i, c := range pa.Order {
		at := fmt.Sprintf("%s.order[%d]", field, i)
		switch c.Source {
		case PoolSourceAWSMetal:
			if p.AWSMetal == nil {
				errs = append(errs, fmt.Errorf("%s names aws-metal, which server.pool.aws_metal does not configure", at))

				continue
			}
			if c.Location != "" && c.Location != p.AWSMetal.AvailabilityZone {
				errs = append(errs, fmt.Errorf("%s.location %q is not the subnet's zone %s: the AWS source orders only "+
					"in its subnet's zone", at, c.Location, p.AWSMetal.AvailabilityZone))
			}
			if p.AWSMetal.Images[a] == "" {
				errs = append(errs, fmt.Errorf("%s orders %s hosts from aws-metal, and server.pool.aws_metal.images "+
					"names no %s image", at, a, a))
			}
		case PoolSourcePhoenixNAP:
			if p.PhoenixNAP == nil {
				errs = append(errs, fmt.Errorf("%s names phoenixnap, which server.pool.phoenixnap does not configure", at))

				continue
			}
			if c.Location == "" {
				errs = append(errs, fmt.Errorf("%s.location is required for phoenixnap: its stock is per location", at))
			}
		default:
			errs = append(errs, fmt.Errorf("%s.source %q must be %s or %s", at, c.Source, PoolSourceAWSMetal, PoolSourcePhoenixNAP))

			continue
		}
		shapeArch, ok := p.ShapeArch(c.Source, c.Shape)
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s.shape %q is not one of %s's declared shapes", at, c.Shape, c.Source))
		case shapeArch != a:
			errs = append(errs, fmt.Errorf("%s.shape %s is %s, not %s", at, c.Shape, shapeArch, a))
		}
	}

	return errs
}

func validatePoolShapes(field string, shapes []PoolShape) []error {
	var errs []error
	if len(shapes) == 0 {
		errs = append(errs, fmt.Errorf("%s is empty", field))
	}
	seen := map[string]bool{}
	for i, s := range shapes {
		switch {
		case s.Type == "":
			errs = append(errs, fmt.Errorf("%s[%d].type is required", field, i))
		case seen[s.Type]:
			errs = append(errs, fmt.Errorf("%s[%d].type %s is declared twice", field, i, s.Type))
		case !s.Arch.Valid():
			errs = append(errs, fmt.Errorf("%s[%d].arch %q is not an architecture billet knows", field, i, s.Arch))
		}
		seen[s.Type] = true
	}

	return errs
}

func (m *PoolAWSMetal) validate() []error {
	if m == nil {
		return nil
	}
	var errs []error
	if err := CheckEC2Region(m.Region); err != nil {
		errs = append(errs, fmt.Errorf("server.pool.aws_metal.region: %w", err))
	}
	if m.SubnetID == "" {
		errs = append(errs, errors.New("server.pool.aws_metal.subnet_id is required"))
	}
	if m.AvailabilityZone == "" {
		errs = append(errs, errors.New("server.pool.aws_metal.availability_zone is required: an EBS volume lives in "+
			"one zone, and a pool host orders in its site's"))
	}
	errs = append(errs, CheckEC2SecurityGroups("server.pool.aws_metal.security_group_ids", m.SecurityGroupIDs, true)...)
	for a, image := range m.Images {
		if !a.Valid() || image == "" {
			errs = append(errs, fmt.Errorf("server.pool.aws_metal.images.%s %q must name an AMI for a known "+
				"architecture", a, image))
		}
	}

	// A POOL HOST RUNS FIRECRACKER, WHICH NEEDS KVM, and on AWS only a bare-metal type
	// has it. Every such type names it (`m7gd.metal`, `c7i.metal-24xl`).
	for i, s := range m.Shapes {
		if !strings.Contains(s.Type, ".metal") {
			errs = append(errs, fmt.Errorf("server.pool.aws_metal.shapes[%d].type %q is not a bare-metal type; a pool "+
				"host runs Firecracker, which needs the KVM only a .metal type has", i, s.Type))
		}
	}

	return append(errs, validatePoolShapes("server.pool.aws_metal.shapes", m.Shapes)...)
}

func (n *PoolPhoenixNAP) validate() []error {
	if n == nil {
		return nil
	}
	var errs []error
	for field, name := range map[string]string{"client_id_env": n.ClientIDEnv, "client_secret_env": n.ClientSecretEnv} {
		if !envVarName.MatchString(name) {
			errs = append(errs, fmt.Errorf("server.pool.phoenixnap.%s %q must name an environment variable; "+
				"the credential itself never goes in the config", field, name))
		}
	}
	if n.ClientIDEnv != "" && n.ClientIDEnv == n.ClientSecretEnv {
		errs = append(errs, errors.New("server.pool.phoenixnap names one variable for both the client id and secret"))
	}

	return append(errs, validatePoolShapes("server.pool.phoenixnap.shapes", n.Shapes)...)
}
