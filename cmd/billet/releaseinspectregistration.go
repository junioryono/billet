package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// The node's registration record, as the inspector reads it: USABLE EVIDENCE
// OR UNKNOWN. The node writes `/run/billet/registration/current` after every
// successful registration (internal/nodeclient/record.go); the inspector
// reports it as known only when it is present, well-formed, root-owned 0600,
// read inside the node's own sample, written by THIS invocation of the node,
// naming THIS node and THIS deployment, and carrying an endpoint in its
// canonical spelling. Everything else is unknown with its reason, so a
// consumer never reads a failed read as absence, a foreign file as the node's,
// or a previous incarnation's record as the running one's.

// nodeRegistrationRecordPath is where the node publishes its registration
// record and where the inspector reads it: under the node unit's own
// RuntimeDirectory on Linux, and nowhere on a Mac, where no manager makes one.
// THE ONE SPELLING, for the node command and the inspector alike.
func nodeRegistrationRecordPath(platform string) string {
	if platform == "darwin" {
		return ""
	}

	return "/run/billet/registration/current"
}

// registrationRecordPath is the record's path for this host, a variable so a
// fixture can point the inspector at a temporary one.
var registrationRecordPath = nodeRegistrationRecordPath(hostOS)

// maxRegistrationRecordBytes bounds the record: a longer file is not one the
// node wrote.
const maxRegistrationRecordBytes = 4096

// The reader's seams, each wrapping the executed operation so a fixture can
// count, fail or observe it: the directory's Lstat, the record's identity-first
// open (which returns the fstat the ownership and mode are judged on), and the
// bounded read through the admitted descriptor.
var (
	registrationLstat = os.Lstat
	registrationOpen  = func(path string) (*os.File, os.FileInfo, error) {
		return regularfile.Open(path, regularfile.Options{NoFollow: true})
	}
	registrationRead = regularfile.ReadAllLimited
)

// registrationRecord is the record's seven members, exactly.
type registrationRecord struct {
	Node         string `json:"node"`
	Deployment   string `json:"deployment"`
	Incarnation  string `json:"incarnation"`
	InvocationID string `json:"invocation_id"`
	Endpoint     string `json:"endpoint"`
	RegisteredAt string `json:"registered_at"`
}

// registrationRecordFields is the allowlist a record's members are validated
// against BY NAME, after decoding into a map: the struct decoder's
// case-insensitive matching would admit "Schema" for "schema".
var registrationRecordFields = []string{"schema", "node", "deployment", "incarnation", "invocation_id", "endpoint", "registered_at"}

// hex32 is the shape of an incarnation and of a systemd invocation id.
var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// registrationEvidence is what the sample carries out: the record when it
// could be read and decoded, or why not.
type registrationEvidence struct {
	record *registrationRecord
	why    string
	// unreadable says the read FAILED (an examination, an open, a reopen or
	// a read that errored), which is never absence and never invalidity.
	unreadable bool
	// invalid says a record was found and is not one billet wrote (its
	// directory a link, its owner, mode or size wrong, its bytes malformed),
	// which is never absence: waiting for it to become valid is waiting for
	// nothing.
	invalid bool
}

// readRegistrationRecord reads the record at path under the reader's rules:
// the directory examined by Lstat (a symlink or a non-directory refuses), the
// record opened identity first and never through a link, judged on the fstat
// of the admitted descriptor (a regular file, root-owned, mode 0600, at most
// the bound), its bytes read through that descriptor, and decoded under the
// exact member set. Every refusal names the operation that refused; an
// absence is the one positive ENOENT of the open.
func readRegistrationRecord(path string) registrationEvidence {
	dir := filepath.Dir(path)

	dirInfo, err := registrationLstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A positive absence: a node that has not registered yet, or a
		// release before the record, has no directory here.
		return registrationEvidence{why: "no registration directory at " + dir}
	case err != nil:
		return registrationEvidence{why: fmt.Sprintf("examine the registration directory %s: %v", dir, err), unreadable: true}
	case dirInfo.Mode()&os.ModeSymlink != 0:
		return registrationEvidence{why: "the registration directory " + dir + " is a symlink", invalid: true}
	case !dirInfo.IsDir():
		return registrationEvidence{why: "the registration directory " + dir + " is not a directory", invalid: true}
	}

	f, info, err := registrationOpen(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !errors.Is(err, regularfile.ErrReopen):
		return registrationEvidence{why: "no registration record at " + path}
	case errors.Is(err, regularfile.ErrNotRegular) || (errors.Is(err, syscall.ELOOP) && runtime.GOOS == "darwin"):
		// A link at the name is invalid on every platform, and each platform
		// says it its own way: Linux's O_PATH|O_NOFOLLOW open admits the link's
		// identity and the regular-file rule refuses it, so an ELOOP there is a
		// loop met on the WAY to the name (could-not-tell, below); a Mac's
		// O_NOFOLLOW open answers ELOOP for the link itself.
		return registrationEvidence{why: fmt.Sprintf("the registration record %s is not a regular file: %v", path, err),
			invalid: true}
	case err != nil:
		return registrationEvidence{why: fmt.Sprintf("open the registration record %s: %v", path, err), unreadable: true}
	}

	defer func() { _ = f.Close() }()

	// OWNERSHIP AND MODE ARE THE ADMITTED INODE'S, read from the fstat the open
	// returned and never from a lookup beside it; the conversion is the one the
	// guard uses.
	uid, ok := ownerFromInfo(info)
	if !ok {
		return registrationEvidence{why: "the registration record carries no owner this platform reports", invalid: true}
	}

	if uid != 0 {
		return registrationEvidence{why: fmt.Sprintf("the registration record is owned by uid %d, want root", uid), invalid: true}
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		return registrationEvidence{why: fmt.Sprintf("the registration record is mode %04o, want 0600", perm), invalid: true}
	}

	// THE ADMITTED INODE'S SIZE, from the same fstat: a record over the bound at
	// admission is refused whatever it holds by the time it is read, and the
	// bounded read below still refuses one that grows afterwards.
	if info.Size() > maxRegistrationRecordBytes {
		return registrationEvidence{why: fmt.Sprintf("the registration record is larger than %d bytes", maxRegistrationRecordBytes), invalid: true}
	}

	body, err := registrationRead(f, path, maxRegistrationRecordBytes)
	if err != nil {
		if errors.Is(err, regularfile.ErrTooLarge) {
			return registrationEvidence{why: fmt.Sprintf("the registration record is larger than %d bytes", maxRegistrationRecordBytes), invalid: true}
		}

		return registrationEvidence{why: fmt.Sprintf("read the registration record %s: %v", path, err), unreadable: true}
	}

	rec, err := decodeRegistrationRecord(body)
	if err != nil {
		return registrationEvidence{why: "the registration record is malformed: " + err.Error(), invalid: true}
	}

	return registrationEvidence{record: rec}
}

// decodeRegistrationRecord decodes the record's bytes under the exact member
// set: one JSON object, no duplicate member, no trailing bytes, exactly the
// seven names in any order, schema 1, every other member a non-empty string,
// the two identifiers 32 lowercase hex characters, the time RFC 3339.
func decodeRegistrationRecord(body []byte) (*registrationRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}

	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("not a JSON object")
	}

	members := map[string]json.RawMessage{}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("not JSON: %w", err)
		}

		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("a member name that is not a string")
		}

		if _, dup := members[key]; dup {
			return nil, fmt.Errorf("the member %q appears twice", key)
		}

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("the member %q: %w", key, err)
		}

		members[key] = raw
	}

	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("bytes follow the object")
	}

	for _, name := range registrationRecordFields {
		if _, ok := members[name]; !ok {
			return nil, fmt.Errorf("the member %q is missing", name)
		}
	}

	for name := range members {
		known := false

		for _, want := range registrationRecordFields {
			if name == want {
				known = true
			}
		}

		if !known {
			return nil, fmt.Errorf("the member %q is not one the node writes", name)
		}
	}

	// THE NUMERIC TOKEN `1`, by its bytes: json.Number would also accept the
	// string "1", and 1.0 is not the schema the node writes.
	if string(bytes.TrimSpace(members["schema"])) != "1" {
		return nil, fmt.Errorf("schema is %s, want 1", string(members["schema"]))
	}

	rec := &registrationRecord{}

	for name, into := range map[string]*string{
		"node": &rec.Node, "deployment": &rec.Deployment, "incarnation": &rec.Incarnation,
		"invocation_id": &rec.InvocationID, "endpoint": &rec.Endpoint, "registered_at": &rec.RegisteredAt,
	} {
		if bytes.Equal(bytes.TrimSpace(members[name]), []byte("null")) {
			return nil, fmt.Errorf("the member %q is null", name)
		}

		var s string
		if err := json.Unmarshal(members[name], &s); err != nil {
			return nil, fmt.Errorf("the member %q is not a string", name)
		}

		if s == "" {
			return nil, fmt.Errorf("the member %q is empty", name)
		}

		*into = s
	}

	// THE FORMAT BEFORE ANY COMPARISON: a malformed identifier is refused as
	// malformed whatever the unit's own identifier says.
	if !hex32.MatchString(rec.Incarnation) {
		return nil, errors.New("the incarnation is not 32 hex characters")
	}

	if !hex32.MatchString(rec.InvocationID) {
		return nil, errors.New("the invocation_id is not 32 hex characters")
	}

	if _, err := time.Parse(time.RFC3339Nano, rec.RegisteredAt); err != nil {
		return nil, fmt.Errorf("registered_at is not RFC 3339: %w", err)
	}

	return rec, nil
}

// registrationIdentity is what the record must name to be this node's: the
// node's name and its deployment, each from the source the node itself takes
// it from (nodeDeploymentID, claimIdentity), or the reason the comparison
// cannot be made.
type registrationIdentity struct {
	node, deployment string
	why              string
	absent           bool   // the deployment is positively unminted, not unread
	contradiction    string // the configured name and the certificate's disagree: a positive fact, also in why
}

// expectedRegistrationIdentity derives the identity from the configuration
// the report parsed, WITHOUT MINTING: the name is the configured `node.name`
// or, without one, the bundle leaf's CommonName; the deployment is the
// bundle's Organization when there is a bundle (the certificate outranks the
// file, as it does for the node), else the server's identity file when this
// host has a server section, else the node state directory's own identity
// file, each peeked and never created.
func expectedRegistrationIdentity(cfg *config.Config) registrationIdentity {
	if cfg == nil || cfg.Node == nil {
		return registrationIdentity{why: "this host's configuration has no node section"}
	}

	id := registrationIdentity{node: cfg.Node.Name}

	if cfg.Node.TLS != nil {
		leafPEM, err := readPublicFile(cfg.Node.TLS.CertPath)
		if err != nil {
			return registrationIdentity{why: fmt.Sprintf("read %s: %v", cfg.Node.TLS.CertPath, err)}
		}

		leaves, err := wirecert.ParseCertificates(leafPEM)
		if err != nil || len(leaves) != 1 {
			return registrationIdentity{why: cfg.Node.TLS.CertPath + " does not hold exactly one certificate"}
		}

		cn := leaves[0].Subject.CommonName

		switch {
		case id.node == "":
			id.node = cn
		case cn != id.node:
			// THE NODE'S OWN STARTUP RULE: an explicit name must be the
			// certificate's, because the control plane authorises by the
			// certificate; a configuration that disagrees never starts, so
			// the identity it names is a contradiction and not a name.
			why := fmt.Sprintf("node.name is %q but %s was issued for %q", id.node, cfg.Node.TLS.CertPath, cn)

			return registrationIdentity{node: id.node, why: why, contradiction: why}
		}

		if len(leaves[0].Subject.Organization) != 1 || leaves[0].Subject.Organization[0] == "" {
			return registrationIdentity{why: cfg.Node.TLS.CertPath + " names no single, non-empty deployment in its Organization"}
		}

		id.deployment = leaves[0].Subject.Organization[0]

		return id
	}

	dir := cfg.Node.StateDir
	if cfg.Server != nil {
		dir = cfg.Server.IdentityDir
	}

	deployment, found, err := state.PeekDeploymentID(dir)
	switch {
	case err != nil:
		return registrationIdentity{why: fmt.Sprintf("read the deployment identity in %s: %v", dir, err)}
	case !found:
		// THE NAME IS KEPT: an unminted deployment says nothing about the node
		// the configuration or its certificate names.
		return registrationIdentity{node: id.node,
			why: "no deployment identity is minted in " + dir + ", so the record's deployment cannot be judged", absent: true}
	}

	id.deployment = deployment

	return id
}

// registrationReport is the known form of host.registration: exactly the six
// members a consumer reads.
type registrationReport struct {
	Node         string `json:"node"`
	Deployment   string `json:"deployment"`
	Incarnation  string `json:"incarnation"`
	InvocationID string `json:"invocation_id"`
	Endpoint     string `json:"endpoint"`
	RegisteredAt string `json:"registered_at"`
}

// judgeRegistration turns the sample's evidence into host.registration: the
// record's invocation must equal the node unit's sampled InvocationID (both
// non-empty), the record must name this node and this deployment, and its
// endpoint must be a canonical spelling under its own scheme.
func judgeRegistration(ev registrationEvidence, unitInvocation maybe, identity registrationIdentity) maybe {
	if ev.record == nil {
		return unknown(ev.why)
	}

	if !unitInvocation.known {
		return unknown("the node unit's invocation could not be read: " + unitInvocation.why)
	}

	inv, isString := unitInvocation.value.(string)
	if !isString || inv == "" {
		return unknown("the node unit reports no invocation, so the record's currency cannot be judged")
	}

	if ev.record.InvocationID != inv {
		return unknown(fmt.Sprintf("the record was written by invocation %s and the node unit is invocation %s",
			ev.record.InvocationID, inv))
	}

	if identity.why != "" {
		return unknown("the record's identity cannot be judged: " + identity.why)
	}

	if ev.record.Node != identity.node {
		return unknown(fmt.Sprintf("the record names the node %q and this host's node is %q", ev.record.Node, identity.node))
	}

	if ev.record.Deployment != identity.deployment {
		return unknown(fmt.Sprintf("the record names the deployment %s and this node's deployment is %s",
			ev.record.Deployment, identity.deployment))
	}

	if _, err := endpoint.ParseCanonical(ev.record.Endpoint); err != nil {
		return unknown("the record's endpoint is not a canonical spelling: " + err.Error())
	}

	return known(registrationReport{
		Node: ev.record.Node, Deployment: ev.record.Deployment, Incarnation: ev.record.Incarnation,
		InvocationID: ev.record.InvocationID, Endpoint: ev.record.Endpoint, RegisteredAt: ev.record.RegisteredAt,
	})
}

// hostRegistration is host.registration: unknown on darwin (no runtime record
// there, and no unit sample to bind one to), unknown when the node is not
// running (its sample is what reads the record), else the sample's evidence
// judged for currency and identity.
func hostRegistration(node inspectService, cfg *config.Config) maybe {
	if hostOS == "darwin" {
		return unknown("no runtime record on this platform")
	}

	// RUNNING IS A BOUND PROCESS, not the word "active": a node systemd reports
	// `deactivating` is draining, still serving what it holds and still able
	// to re-register, and its sample binds like any other; a unit with no main
	// process, or in a terminal state, has nothing a record can be bound to.
	active, isString := node.ActiveState.value.(string)
	if !node.ActiveState.known || !isString || active == "inactive" || active == "failed" ||
		!node.MainPID.known || node.MainPID.value == nil {
		return unknown("the node is not running, so no registration record is bound to a process")
	}

	return judgeRegistration(node.registration, node.InvocationID, expectedRegistrationIdentity(cfg))
}

// installedEndpoint is the installed configuration's node endpoint as the one
// representation, from the configuration observation the report already
// holds: null without a node section, unknown with the representation's own
// reason when it refuses a spelling the configuration accepted.
func installedEndpoint(cfg *config.Config, readable bool) maybe {
	if !readable {
		return unknown("the configuration could not be read")
	}

	if cfg == nil || cfg.Node == nil {
		return known(nil)
	}

	e, err := endpoint.Parse(cfg.Node.ServerAddr, cfg.Node.TLS != nil)
	if err != nil {
		return unknown(err.Error())
	}

	return known(e.String())
}
