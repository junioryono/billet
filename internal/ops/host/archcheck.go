package host

import (
	"fmt"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider/ec2"
)

// ec2TierArchMismatch refuses an EC2 tier whose AMI boots another architecture
// than the tier names (ADR-022).
//
// REFUSED, because placement would buy the tier a shape of its arch and the EC2
// node would then refuse every launch: the AMI is the authority on what it boots,
// and a tier naming the other architecture can never run. An image whose
// architecture is unknown, or not yet resolved, is reported elsewhere and judged
// by nothing here.
func ec2TierArchMismatch(cfg *config.Config, images []ec2.ImageInfo) error {
	arch := make(map[string]config.Arch, len(images))
	for _, img := range images {
		if img.Found && img.Arch != "" {
			arch[img.ImageID] = img.Arch
		}
	}

	var wrong []string

	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		if t.Arch == "" || !t.AcceptsProvider(config.ProviderEC2) {
			continue
		}

		ami := t.ImageFor(config.ProviderEC2)
		if got, ok := arch[ami]; ok && got != t.Arch {
			wrong = append(wrong, fmt.Sprintf("%s names %s and its AMI %s is %s", t.Label, t.Arch, ami, got))
		}
	}

	if len(wrong) == 0 {
		return nil
	}

	return fmt.Errorf("an EC2 tier's AMI boots another architecture than the tier names, so "+
		"every launch would be refused: %s; build the AMI with `billet ami build --arch`, or name "+
		"the image's architecture", strings.Join(wrong, "; "))
}

// archBeforeEveryHost refuses a catalogue that names an architecture while a
// registered host runs a billet that predates it (ADR-022).
//
// A BILLET FROM BEFORE IT REFUSES THE FILE: config decodes billet.yaml strictly,
// so an `arch` written into a file an older host reads stops that host from
// starting, and an older control plane cannot place by it either. Every host is
// upgraded first. A host is judged by the newest wire it said it speaks: below
// VersionArchitectures its build does not know the field, and a host whose
// registration predates recording the range (zero) cannot say, so it is held to
// the same answer. A controller is not recorded in the ledger, so it cannot be
// judged here; the message says to upgrade every controller too. A host an
// operator decommissioned is gone, and judged again if it registers, which clears
// that.
func archBeforeEveryHost(cfg *config.Config, hosts []alloc.NodeWire, gone map[string]bool) error {
	if !config.CatalogueNamesArch(cfg.Tiers) {
		return nil
	}

	var older []string

	for _, h := range hosts {
		if !gone[h.Name] && h.Max < nodeapi.VersionArchitectures {
			release := h.Release
			if release == "" {
				release = "a release it did not name"
			}

			older = append(older, h.Name+" ("+release+")")
		}
	}

	if len(older) == 0 {
		return nil
	}

	slices.Sort(older)

	return fmt.Errorf("the tiers name an architecture, and %d registered host(s) run a billet "+
		"from before tiers could: %s. Such a billet refuses a billet.yaml that carries arch and "+
		"places no tier by it, so upgrade every host, and every controller, before writing arch "+
		"into a file any of them reads", len(older), strings.Join(older, ", "))
}
