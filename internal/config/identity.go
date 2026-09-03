package config

import (
	"errors"
	"fmt"
	"strings"
)

// ONE REPRESENTATION PER IDENTITY-BEARING VALUE, and this file holds the half of
// that rule that REFUSES rather than normalizes.
//
// The failure it exists to prevent has now been found three times in this
// package: validation examines a trimmed copy, the runtime consumer uses the raw
// string, and the two silently disagree. CephConfig.normalize and
// EC2Config.normalize record the first two — a pool name that passed the shape
// check and was then handed to rbd with its padding. Both were fixed by
// normalizing and WRITING THE RESULT BACK, which is correct for a path, a pool
// or a region: nothing outside this machine has an opinion about them, and a
// trimmed one is unambiguously what the operator meant.
//
// An IDENTITY is different, and gets refused instead. A site name is matched
// against what a node's registration presents, on another machine; an
// organization is matched against what GitHub has. Trimming those quietly
// changes which deployment or which organization the config names, and the
// operator never sees that it happened — so billet says so and keeps their own
// bytes in the diagnostic. The set is: sites[].name, tiers[].site, node.site,
// github.org and tiers[].runner_group.
//
// Node names and tier labels need nothing here: labelRe admits no whitespace at
// all, and applyDefaults writes trimNodeName's result back before anything reads
// it.

// checkIdentityPadding refuses surrounding whitespace on a value billet matches
// exactly.
//
// where names the field, so one message serves every caller: the value is
// rendered quoted because padding is invisible otherwise, which is most of why
// this class of mistake survives to production.
func checkIdentityPadding(where, value string) error {
	if strings.TrimSpace(value) == value {
		return nil
	}

	return fmt.Errorf("%s %q has leading or trailing whitespace, and billet matches this value "+
		"exactly as written: the padding is part of the name rather than layout. Trimming it "+
		"here would mean validating one string and using another, so write it without the "+
		"padding", where, value)
}

// orgUnsafe are the characters that make github.org name a different
// organization than the one written, or no organization at all.
//
// MEASURED AGAINST BOTH CONSTRUCTIONS THE VALUE GOES THROUGH, not reasoned about
// — the same discipline checkRunnerGroup's list came from, and for the same
// reason: the two boundaries disagree about which characters matter, and only
// one of them reports anything. billet builds the scale-set client's config URL
// as "https://github.com/" + org UNESCAPED (cmd/billet), and the REST path as
// /orgs/ + url.PathEscape(org) + /installation (internal/github). PathEscape
// carries everything faithfully; the config URL is where the damage happens, and
// actions/scaleset v0.4.0's parseGitHubConfigFromURL is what reads it back —
// url.Parse, then the path split on "/".
//
// Sweeping printable ASCII through both returned exactly these four:
//
//   - '/' — a second path segment is a REPOSITORY. "acme/x" resolves to
//     organization "acme", repository "x", and NOTHING reports it. A trailing
//     one is worse still: parseGitHubConfigFromURL trims it, so "acme/" is
//     silently "acme".
//   - '#' and '?' — the rest of the value becomes the fragment or the query, so
//     the organization is the shorter string in front of it. "acme # prod"
//     resolves to "acme ".
//   - '%' — escapes are DECODED, so "%41" arrives as "A", and an incomplete one
//     ("acme%corp") makes url.Parse refuse the URL outright.
//
// Everything else survives both boundaries byte-for-byte, non-ASCII included, so
// nothing else is refused. TestCheckOrgAgreesWithTheBoundariesOverAllOfASCII
// sweeps the whole range rather than sampling it, so a character added to or
// removed from this set has to agree with what the boundaries do — a handpicked
// table cannot find one that is MISSING, which is the failure that matters.
//
// Surrounding whitespace survives them too — it is refused above as an identity
// mistake rather than as a transport one, which is a distinction worth keeping
// straight: a rule whose stated reason is untrue is worse than no rule.
const orgUnsafe = "#%/?"

// orgUnsafeReason says what each character does, so the diagnostic names the
// consequence rather than the rule.
var orgUnsafeReason = map[rune]string{
	'/': "the scale-set client reads a second path segment as a repository, so this resolves " +
		"to an organization and a repository rather than to the organization you wrote — and a " +
		"trailing slash is simply dropped",
	'#': "everything after it becomes the URL fragment, so the organization billet asks about " +
		"is the shorter string in front of it",
	'?': "everything after it becomes the URL query, so the organization billet asks about is " +
		"the shorter string in front of it",
	'%': "a percent escape is decoded before the name is read, so %41 arrives as A, and an " +
		"incomplete escape makes the organization URL unparseable",
}

// CheckOrg reports why github.org cannot be carried to GitHub as written, or nil.
//
// Exported for the same reason CheckRunnerGroup and CheckWorkflowRef are: `billet
// init` validates its --org flag against the one rule config validation applies,
// so a bad flag is refused by its own name rather than surfacing later as a
// config-load error blaming a generated file.
func CheckOrg(org string) error {
	// An empty and a whitespace-only value are the SAME fact — nobody named an
	// organization — and reporting the second as padding would send an operator
	// to remove spaces from a field they never filled in.
	if strings.TrimSpace(org) == "" {
		return errors.New("github.org is required")
	}

	if err := checkIdentityPadding("github.org", org); err != nil {
		return err
	}

	for _, r := range org {
		switch {
		// AN ASCII CONTROL, NOT unicode.IsControl. net/url refuses a control
		// BYTE — below 0x20, or 0x7f — and a multi-byte rune has none of those
		// bytes, so a C1 control such as U+0080 travels both constructions
		// completely unchanged. Refusing it under a message that says url.Parse
		// would reject it is a rule stating a reason that is not true, which is
		// the thing this file exists to stop doing; an org name nobody can
		// register is GitHub's 404 to give, not billet's.
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("github.org %q contains an ASCII control character (%U); billet "+
				"builds the organization URL from this name and url.Parse refuses control bytes, "+
				"so the client would fail to start rather than reach GitHub", org, r)
		case strings.ContainsRune(orgUnsafe, r):
			return fmt.Errorf("github.org %q contains %q: %s. Write the organization's login on "+
				"its own — the last path segment of its GitHub URL", org, r, orgUnsafeReason[r])
		}
	}

	return nil
}
