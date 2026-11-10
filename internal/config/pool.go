package config

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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
	// MinPoolIdleAfter is the shortest idle_after: the pool reads a tier's demand from
	// reports up to five minutes old, so a host idle for less may be one a report has
	// not yet said is wanted.
	MinPoolIdleAfter = 5 * time.Minute
)

// PoolInitFlags are the only `billet init` flags a pool host's join may be handed: what
// a Firecracker node's config needs and the join cannot know. On AWS metal that includes
// its site, the site's ebs-s3 store and the cache listener its guests reach (ADR-030).
// Every other flag init takes is the join's to decide (its config path, its node name, its
// provider, --force), and init takes a flag's last value, so one more here would
// override it.
var PoolInitFlags = []string{"kernel-image", "ceph-user", "ceph-keyring", "site", "ebs-s3-region",
	"ebs-s3-availability-zone", "ebs-s3-bucket", "ebs-s3-prefix", "ebs-s3-kms-key-id", "cache-listen",
	"cache-guest-endpoint"}

// poolStoreFlags are the PoolInitFlags that ask for the site's ebs-s3 store.
var poolStoreFlags = []string{"ebs-s3-region", "ebs-s3-availability-zone", "ebs-s3-bucket", "ebs-s3-prefix",
	"ebs-s3-kms-key-id"}

// checkPoolInitArgs refuses init_args that are not PoolInitFlags, each at most once,
// as `--flag value` or `--flag=value` with one dash or two as init's parser reads them,
// every word one word of the boot: a NUL bash drops and a line break would end the
// join's line. A value is the flag's whatever it starts with: init's parser takes the
// word after a string flag as its value, so `--ceph-user --force` names a user and
// forces nothing.
//
// AND EACH VALUE IS ONE THE NODE'S CONFIG WILL TAKE, by the node's own rules, because
// a value only the host's init refuses is a host rented that cannot join.
func checkPoolInitArgs(args []string) []error {
	_, errs := readPoolInitArgs(args)

	return errs
}

// readPoolInitArgs is checkPoolInitArgs, answering the values it read as well, by flag.
func readPoolInitArgs(args []string) (map[string]string, []error) {
	var errs []error
	seen := map[string]bool{}
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" || strings.ContainsAny(a, "\x00\n\r") || !utf8.ValidString(a) {
			errs = append(errs, fmt.Errorf("server.pool.init_args[%d] %q must be one non-empty word of UTF-8 with "+
				"no NUL or line break", i, a))

			continue
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "---") || !slices.Contains(PoolInitFlags, name) {
			errs = append(errs, fmt.Errorf("server.pool.init_args[%d] %q is not one of %v: every other flag of "+
				"billet init is the join's to decide", i, a, PoolInitFlags))

			continue
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("server.pool.init_args names --%s twice", name))
		}
		seen[name] = true
		if !inline {
			if i+1 >= len(args) {
				errs = append(errs, fmt.Errorf("server.pool.init_args ends with --%s and no value", name))

				break
			}
			i++
			value = args[i]
			if value == "" || strings.ContainsAny(value, "\x00\n\r") || !utf8.ValidString(value) {
				errs = append(errs, fmt.Errorf("server.pool.init_args[%d] %q must be one non-empty word of UTF-8 "+
					"with no NUL or line break", i, value))

				continue
			}
		}
		// AN EMPTY VALUE IS REFUSED IN EITHER SPELLING, one rule for `--flag=` and `--flag ''`:
		// init would fill in its own default, which leaving the flag out says plainly.
		if value == "" {
			errs = append(errs, fmt.Errorf("server.pool.init_args gives --%s no value; leave the flag out for "+
				"init's own default", name))

			continue
		}
		values[name] = value
	}

	// AS THE NODE'S CONFIG WILL HOLD THEM: trimmed, as loading it trims, and a user that
	// trims to nothing given billet's own, as init and loading both give it.
	if kernel, ok := values["kernel-image"]; ok && !filepath.IsAbs(strings.TrimSpace(kernel)) {
		errs = append(errs, fmt.Errorf("server.pool.init_args' --kernel-image %q must be an absolute path, as "+
			"node.firecracker.kernel_image must", kernel))
	}
	if _, user := values["ceph-user"]; user || values["ceph-keyring"] != "" {
		ceph := CephConfig{
			User: strings.TrimSpace(values["ceph-user"]), KeyringPath: strings.TrimSpace(values["ceph-keyring"]),
			ImagePool: "billet-images", CachePool: "billet-cache",
		}
		if ceph.User == "" {
			ceph.User = DefaultCephUser
		}
		for _, err := range CheckCeph(ceph) {
			errs = append(errs, fmt.Errorf("server.pool.init_args: %w", err))
		}
	}

	// THE CACHE AND THE STORE, by the rules init holds them to on the host, which would
	// otherwise refuse them there, on a host rented and unable to join.
	if (values["cache-listen"] == "") != (values["cache-guest-endpoint"] == "") {
		errs = append(errs, errors.New("server.pool.init_args gives --cache-listen and --cache-guest-endpoint "+
			"together or not at all"))
	}
	if store := poolStore(values); store != nil {
		if values["site"] == "" {
			errs = append(errs, errors.New("server.pool.init_args names an ebs-s3 store and no --site: cache keys "+
				"are scoped by site"))
		}
		if values["cache-listen"] == "" {
			errs = append(errs, errors.New("server.pool.init_args names an ebs-s3 store and no --cache-listen: the "+
				"store backs the guest cache and nothing else"))
		}
		for _, err := range CheckEBSS3(*store) {
			errs = append(errs, fmt.Errorf("server.pool.init_args: %w", err))
		}
	}

	return values, errs
}

// poolStore is the ebs-s3 store init_args name, as the node's config will hold it, or nil
// when they name none.
func poolStore(values map[string]string) *EBSS3Config {
	named := false
	for _, flag := range poolStoreFlags {
		if _, ok := values[flag]; ok {
			named = true
		}
	}
	if !named {
		return nil
	}

	store := &EBSS3Config{
		Region: values["ebs-s3-region"], AvailabilityZone: values["ebs-s3-availability-zone"],
		Bucket: values["ebs-s3-bucket"], Prefix: values["ebs-s3-prefix"], KMSKeyID: values["ebs-s3-kms-key-id"],
	}
	store.normalize()

	return store
}

// validatePoolSite checks what init_args say about a site against what this control
// plane declares: a site it does not declare, or one whose store is not ebs-s3 beside a
// store init_args name, is a host that registers and is refused; and an ebs-s3 store in
// another zone than the pool orders in is one whose volumes no host can attach.
func (c *Config) validatePoolSite() []error {
	if c.Server == nil || c.Server.Pool == nil {
		return nil
	}

	values, _ := readPoolInitArgs(c.Server.Pool.InitArgs)
	store := poolStore(values)
	var errs []error

	if site := values["site"]; site != "" {
		var declared *SiteConfig
		for i := range c.Sites {
			if c.Sites[i].Name == site {
				declared = &c.Sites[i]
			}
		}
		switch {
		case declared == nil:
			errs = append(errs, fmt.Errorf("server.pool.init_args' --site %q is not a site this control plane "+
				"declares, so every host the pool orders would be refused at registration", site))
		case store != nil && declared.Store != SiteStoreEBSS3:
			errs = append(errs, fmt.Errorf("server.pool.init_args name an ebs-s3 store, and site %q's store is %s",
				site, declared.Store))
		case store == nil && declared.Store == SiteStoreEBSS3:
			errs = append(errs, fmt.Errorf("site %q's store is ebs-s3, so server.pool.init_args must name it "+
				"(--ebs-s3-*) for the hosts the pool puts there", site))
		}
	}

	if m := c.Server.Pool.AWSMetal; store != nil && m != nil && store.AvailabilityZone != m.AvailabilityZone {
		errs = append(errs, fmt.Errorf("server.pool.init_args' --ebs-s3-availability-zone %q is not %q, where "+
			"server.pool.aws_metal orders its hosts: a volume attaches only in its own zone",
			store.AvailabilityZone, m.AvailabilityZone))
	}

	return errs
}

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
	// InitArgs are the arguments an ordered host's `billet join` hands to `billet init`
	// after `--`: a Firecracker node's kernel image and Ceph identity, at the paths the
	// prelude prepared, which the node's generated config needs. Only PoolInitFlags.
	InitArgs []string `yaml:"init_args,omitempty"`
	// Interval is how often the pool looks at demand and its orders, a Go duration.
	Interval string `yaml:"interval,omitempty"`
	// RefusalBackoff is how long a choice a source refused (no stock, a quota) is
	// passed over before it is asked again, a Go duration.
	RefusalBackoff string `yaml:"refusal_backoff,omitempty"`
	// IdleAfter is how long a joined host must have held nothing before the pool asks
	// for its release, a Go duration, at least MinPoolIdleAfter. The release then drains
	// it and seals it only on proof it holds nothing (ADR-030). Empty, the pool never
	// shrinks itself and a person releases a host.
	IdleAfter string `yaml:"idle_after,omitempty"`
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

// PoolIdleAfter is how long a joined host must have held nothing before the pool asks
// for its release, and whether the pool shrinks itself at all.
func (p *PoolConfig) PoolIdleAfter() (time.Duration, bool) {
	d, err := time.ParseDuration(p.IdleAfter)
	if err != nil || d < MinPoolIdleAfter {
		return 0, false
	}

	return d, true
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
// server.bootstrap_listen. Empty when the one chosen names no host another machine
// can dial (none, a wildcard, loopback), which validation refuses: WHATEVER IT CAME
// FROM, a rented host would dial it as itself.
func (s *ServerConfig) PoolEnrollAddr() string {
	var addr string
	switch {
	case s.Pool != nil && strings.TrimSpace(s.Pool.BootstrapAddr) != "":
		addr = s.Pool.BootstrapAddr
	case s.TLSRouter != nil && strings.TrimSpace(s.TLSRouter.EnrollAddr) != "":
		addr = s.TLSRouter.EnrollAddr
	default:
		addr = s.BootstrapListen
	}
	addr = strings.TrimSpace(addr)
	if host, _, err := net.SplitHostPort(addr); err == nil && dialableFromElsewhere(host) {
		return addr
	}

	return ""
}

// dialableFromElsewhere is whether host names this machine to another one: not empty,
// not a wildcard, and not loopback, which a rented host would dial as itself.
func dialableFromElsewhere(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	// A ZONE NAMES AN INTERFACE OF THE MACHINE THAT WROTE IT, never one a rented host
	// has, and net.ParseIP reads a zoned literal as no address at all.
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.Contains(h, "%") {
		return false
	}
	ip := net.ParseIP(h)

	return ip == nil || (!ip.IsLoopback() && !ip.IsUnspecified())
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
		errs = append(errs, fmt.Errorf("server.pool.bootstrap_addr is required: the enrollment address an "+
			"ordered host would dial (server.tls_router.enroll_addr, else server.bootstrap_listen %q) names no host "+
			"it can dial (a wildcard or loopback)", s.BootstrapListen))
	}
	if p.BootPrelude == "" || !filepath.IsAbs(p.BootPrelude) {
		errs = append(errs, fmt.Errorf("server.pool.boot_prelude %q must be an absolute path to the script a "+
			"host runs before it joins", p.BootPrelude))
	}
	errs = append(errs, checkPoolInitArgs(p.InitArgs)...)
	for field, v := range map[string]string{"interval": p.Interval, "refusal_backoff": p.RefusalBackoff} {
		if v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("server.pool.%s %q must be a positive Go duration", field, v))
		}
	}
	if p.IdleAfter != "" {
		if d, err := time.ParseDuration(p.IdleAfter); err != nil || d < MinPoolIdleAfter {
			errs = append(errs, fmt.Errorf("server.pool.idle_after %q must be a Go duration of at least %s: "+
				"the pool reads demand from reports up to that old, so a shorter one could release a host "+
				"a report has not yet said is wanted", p.IdleAfter, MinPoolIdleAfter))
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
