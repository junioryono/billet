package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// CodeBuildTrustConflict reports whether the tiers that can place on the named
// codebuild node span both trust classes, naming the tiers on each side.
//
// A CODEBUILD NODE IS ONE PROJECT AND ONE SERVICE ROLE, and that role reads the
// path every registration for the node is staged under; a VPC-connected build
// can read its own role from inside. So a fork's job on a node a trusted tier
// also names could read a trusted job's staged registration, which is a
// credential. The docs asked the operator to keep the two apart; this is the
// rule. A tier reaches a node when it pins it by name or names no node at all,
// so an unpinned tier reaches every codebuild node, and two unpinned tiers of
// different classes conflict on all of them (`node` is then "").
//
// EXPORTED BECAUSE TWO CALLERS NEED IT: config load, where the file is being
// written, and the control plane at registration, because the node's file may
// be the node's alone and the catalogue is the control plane's.
func CodeBuildTrustConflict(tiers []Tier, node string) error {
	var trusted, untrusted []string

	for i := range tiers {
		t := &tiers[i]
		if !slices.Contains(t.AcceptableProviders(), ProviderCodeBuild) {
			continue
		}

		if t.Node != "" && node != "" && t.Node != node {
			continue
		}

		if t.Node != "" && node == "" {
			// Asking about "every node" means the unpinned tiers alone.
			continue
		}

		switch t.Trust {
		case WorkloadUntrusted:
			untrusted = append(untrusted, t.Label)
		default:
			trusted = append(trusted, t.Label)
		}
	}

	if len(trusted) == 0 || len(untrusted) == 0 {
		return nil
	}

	sort.Strings(trusted)
	sort.Strings(untrusted)

	where := "every codebuild node, because none of these tiers pins one"
	if node != "" {
		where = fmt.Sprintf("codebuild node %q", node)
	}

	return fmt.Errorf("trusted tier(s) %s and untrusted tier(s) %s would share %s; "+
		"a CodeBuild build's service role reads every registration staged for its node, "+
		"so a fork's job there could read a trusted job's credential — give the "+
		"untrusted tier its own codebuild node, project and role",
		strings.Join(trusted, ", "), strings.Join(untrusted, ", "), where)
}

// validateCodeBuildTrust applies CodeBuildTrustConflict to every codebuild node
// the tiers name, and to the unpinned set.
func (c *Config) validateCodeBuildTrust() []error {
	var errs []error

	nodes := map[string]bool{"": true}

	for i := range c.Tiers {
		if slices.Contains(c.Tiers[i].AcceptableProviders(), ProviderCodeBuild) && c.Tiers[i].Node != "" {
			nodes[c.Tiers[i].Node] = true
		}
	}

	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		if err := CodeBuildTrustConflict(c.Tiers, name); err != nil {
			errs = append(errs, fmt.Errorf("tiers: %w", err))
		}
	}

	return errs
}
