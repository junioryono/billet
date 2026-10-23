package node

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// npmRegistryHost is the one origin the npm cache intercepts (ADR-020).
const npmRegistryHost = "registry.npmjs.org"

// npmObject is the kind of registry object a request names.
type npmObject string

const (
	npmPackageDocument npmObject = "package"
	npmVersionDocument npmObject = "version"
	npmTarball         npmObject = "tarball"
)

// npmVariant is which representation of a document the store keys and asks for.
// A tarball has none.
type npmVariant string

const (
	npmFull        npmVariant = "full"
	npmAbbreviated npmVariant = "abbreviated"
	npmNoVariant   npmVariant = ""
)

const (
	// npmAbbreviatedAccept is the one header the node sends upstream for an
	// abbreviated document, whatever the client wrote.
	npmAbbreviatedAccept = "application/vnd.npm.install-v1+json"
	// npmFullAccept is the one header the node sends upstream for a full document.
	npmFullAccept = "application/json"
	// npmClientAbbreviatedAccept is what npm's own client sends for an abbreviated
	// document (npm/pacote lib/registry.js, read 2026-10-22).
	npmClientAbbreviatedAccept = "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*"
)

// npmRequest is a request the store may answer: what it names, and the one
// canonical spelling of that name the upstream fetch and the store's key share.
type npmRequest struct {
	object  npmObject
	variant npmVariant
	// path is the canonical path, the one the node asks the registry for.
	path string
	head bool
	// ifNoneMatch is the client's own validator, answered locally when it names
	// the stored entry and is never sent upstream.
	ifNoneMatch string
}

// key is the store's name for the entry the request reads.
func (r npmRequest) key() string { return string(r.variant) + " " + r.path }

var (
	// npmNamePart is one segment of a package name. A registry name is at most
	// 214 characters, never begins with a dot or an underscore, and is URL-safe;
	// a legacy name may carry capitals. Anything outside this is spliced, so the
	// pattern may be narrower than npm's own rule, never wider.
	npmNamePart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,213}$`)
	// npmScopePart is a scope without its @. Scopes are lower case.
	npmScopePart = regexp.MustCompile(`^[a-z0-9][a-z0-9._~-]{0,213}$`)
	// npmVersion is a semantic version as the registry stores one, with no v:
	// SemVer 2.0.0's grammar, so a numeric prerelease identifier has no leading
	// zero.
	npmVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
		`(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
		`(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	// npmTarballFile is a tarball's file name.
	npmTarballFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~+-]{0,250}\.tgz$`)
)

// classifyNpmRequest reports whether the store may answer req, and as what.
//
// ONE RULE PARSES THE PATH, and it reads the raw path the client sent, so the
// store's key, the classification and the upstream fetch can never disagree
// about which object a spelling names. Anything it does not recognise exactly is
// spliced, never normalised into something the store would answer: a dot
// segment, a repeated or trailing slash, a percent-encoding other than the
// scope's separator, a query.
func classifyNpmRequest(req *http.Request, held func(key string) bool) (npmRequest, bool) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return npmRequest{}, false
	}
	for _, name := range []string{"Authorization", "Cookie", "Npm-Otp", "Range", "If-Range",
		"Content-Type", "Expect"} {
		if _, present := req.Header[name]; present {
			return npmRequest{}, false
		}
	}
	if req.ContentLength != 0 || len(req.TransferEncoding) > 0 {
		return npmRequest{}, false
	}
	if req.URL == nil || req.URL.RawQuery != "" || req.URL.ForceQuery || req.URL.Fragment != "" ||
		req.URL.Opaque != "" || req.URL.Host != "" {
		return npmRequest{}, false
	}
	if !npmAcceptsIdentity(req.Header.Values("Accept-Encoding")) {
		return npmRequest{}, false
	}
	parsed, ok := parseNpmPath(req.URL.EscapedPath())
	if !ok {
		return npmRequest{}, false
	}
	if parsed.object != npmTarball {
		variant, ok := npmDocumentVariant(req.Header.Values("Accept"))
		if !ok {
			return npmRequest{}, false
		}
		parsed.variant = variant
	}
	matches := req.Header.Values("If-None-Match")
	if len(matches) > 1 {
		return npmRequest{}, false
	}
	if len(matches) == 1 {
		parsed.ifNoneMatch = matches[0]
	}
	if req.Method == http.MethodHead {
		// A HEAD NEVER CREATES AN ENTRY: one the store lacks is the registry's.
		if held == nil || !held(parsed.key()) {
			return npmRequest{}, false
		}
		parsed.head = true
	}

	return parsed, true
}

// parseNpmPath reads one of the three shapes a raw path may take.
func parseNpmPath(raw string) (npmRequest, bool) {
	if !strings.HasPrefix(raw, "/") || strings.Contains(raw, "//") {
		return npmRequest{}, false
	}
	segments := strings.Split(raw[1:], "/")
	if strings.HasPrefix(raw, "/@") {
		// A SCOPED DOCUMENT IS ONE SEGMENT, its separator escaped as npm's client
		// escapes it, in either case; every other scoped object is unescaped.
		if len(segments) == 1 {
			scope, name, ok := cutScopeEscape(segments[0])
			if !ok || !npmScopedName(scope, name) {
				return npmRequest{}, false
			}

			return npmRequest{object: npmPackageDocument, path: "/@" + scope + "%2f" + name}, true
		}
		scope := strings.TrimPrefix(segments[0], "@")
		if !npmScopedName(scope, segments[1]) {
			return npmRequest{}, false
		}
		prefix := "/@" + scope + "/" + segments[1]

		return parseNpmObject(prefix, segments[1], segments[2:])
	}
	if !npmNamePart.MatchString(segments[0]) {
		return npmRequest{}, false
	}
	if len(segments) == 1 {
		return npmRequest{object: npmPackageDocument, path: "/" + segments[0]}, true
	}

	return parseNpmObject("/"+segments[0], segments[0], segments[1:])
}

// parseNpmObject reads what follows a package's name: a version, or a tarball.
func parseNpmObject(prefix, name string, rest []string) (npmRequest, bool) {
	switch {
	case len(rest) == 1 && npmVersion.MatchString(rest[0]):
		return npmRequest{object: npmVersionDocument, path: prefix + "/" + rest[0]}, true
	case len(rest) == 2 && rest[0] == "-" && npmTarballFile.MatchString(rest[1]) &&
		strings.HasPrefix(rest[1], name+"-"):
		return npmRequest{object: npmTarball, path: prefix + "/-/" + rest[1]}, true
	default:
		return npmRequest{}, false
	}
}

// npmScopedName reports whether @scope/name is a package name: each part well
// formed, and the whole within the registry's 214 characters.
func npmScopedName(scope, name string) bool {
	return npmScopePart.MatchString(scope) && npmNamePart.MatchString(name) && len(scope)+len(name)+2 <= 214
}

// cutScopeEscape splits "@scope%2fname" at its one escaped separator.
func cutScopeEscape(segment string) (string, string, bool) {
	rest, ok := strings.CutPrefix(segment, "@")
	if !ok {
		return "", "", false
	}
	for _, separator := range []string{"%2f", "%2F"} {
		if scope, name, found := strings.Cut(rest, separator); found {
			if strings.Contains(name, "%") || strings.Contains(scope, "%") {
				return "", "", false
			}

			return scope, name, true
		}
	}

	return "", "", false
}

// npmDocumentVariant chooses a document's representation from a closed set of
// Accept headers, refusing everything else.
func npmDocumentVariant(values []string) (npmVariant, bool) {
	switch {
	case len(values) == 0:
		return npmFull, true
	case len(values) > 1:
		return npmNoVariant, false
	}
	switch strings.TrimSpace(values[0]) {
	case npmClientAbbreviatedAccept, npmAbbreviatedAccept:
		return npmAbbreviated, true
	case npmFullAccept, "*/*":
		return npmFull, true
	default:
		return npmNoVariant, false
	}
}

// npmAcceptsIdentity reports whether a client takes an answer with no content
// coding, which is the only kind the node serves.
//
// A QUALITY IT CANNOT READ REFUSES: the request is spliced rather than answered
// in a coding the client may have meant to refuse.
func npmAcceptsIdentity(values []string) bool {
	identity, wildcard := -1.0, -1.0
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			coding = strings.ToLower(strings.TrimSpace(coding))
			if coding == "" {
				continue
			}
			q := 1.0
			for _, param := range strings.Split(params, ";") {
				name, raw, found := strings.Cut(strings.TrimSpace(param), "=")
				if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
					continue
				}
				parsed, ok := npmQuality(strings.TrimSpace(raw))
				if !ok {
					return false
				}
				q = parsed
			}
			switch coding {
			case "identity":
				identity = q
			case "*":
				wildcard = q
			}
		}
	}
	switch {
	case identity >= 0:
		return identity > 0
	case wildcard >= 0:
		return wildcard > 0
	default:
		return true
	}
}

// npmQualityPattern is RFC 9110's qvalue: 0 or 1, with at most three decimals.
var npmQualityPattern = regexp.MustCompile(`^(0(\.\d{0,3})?|1(\.0{0,3})?)$`)

// npmQuality reads one qvalue, refusing any other spelling.
func npmQuality(raw string) (float64, bool) {
	if !npmQualityPattern.MatchString(raw) {
		return 0, false
	}
	q, err := strconv.ParseFloat(raw, 64)

	return q, err == nil
}
