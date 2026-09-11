package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/regularfile"
)

// THE ENDPOINT RECEIPT, as the inspector and the receipt command read it: the
// durable proof that a node's endpoint migration was confirmed by the
// controller (or that an ordinary converge found the running node, the
// installed configuration and the rendering on one endpoint), bound to the
// node's invocation and registration incarnation. Read under the
// registration record's rules (the directory examined by Lstat, the file
// opened identity-first and never through a link, root 0600, bounded,
// decoded under the exact member set), and TYPED: present, absent (the
// open's positive ENOENT), invalid (a file that is not a receipt, with why)
// or unknown (a failed examination, open, reopen or read), so a consumer
// never reads a failed read as absence.

// endpointReceiptPath is where the receipt lives on Linux: a fixed path under
// the state root, root-owned, and nowhere on a Mac.
func endpointReceiptPathFor(platform string) string {
	if platform == "darwin" {
		return ""
	}

	return "/var/lib/billet/node/endpoint-migration.json"
}

// receiptPath is the receipt's path for this host, a variable so a fixture
// can point both readers at a temporary one.
var receiptPath = endpointReceiptPathFor(hostOS)

// maxReceiptBytes bounds the receipt: a longer file is not one billet wrote.
const maxReceiptBytes = 4096

// receiptSchema is the receipt's schema number.
const receiptSchema = 1

// endpointReceipt is the receipt's ten members, exactly.
type endpointReceipt struct {
	Schema            int    `json:"schema"`
	Run               string `json:"run"`
	Node              string `json:"node"`
	Deployment        string `json:"deployment"`
	InstalledSHA256   string `json:"installed_sha256"`
	InstalledEndpoint string `json:"installed_endpoint"`
	EffectiveEndpoint string `json:"effective_endpoint"`
	InvocationID      string `json:"invocation_id"`
	Incarnation       string `json:"incarnation"`
	WrittenAt         string `json:"written_at"`
}

// receiptFields is the allowlist a receipt's members are validated against
// by name.
var receiptFields = []string{"schema", "run", "node", "deployment", "installed_sha256", "installed_endpoint",
	"effective_endpoint", "invocation_id", "incarnation", "written_at"}

// evidentialEqual says whether two receipts agree on the eight members that
// carry evidence (everything but `run` and `written_at`).
func (r endpointReceipt) evidentialEqual(o endpointReceipt) bool {
	return r.Schema == o.Schema && r.Node == o.Node && r.Deployment == o.Deployment &&
		r.InstalledSHA256 == o.InstalledSHA256 && r.InstalledEndpoint == o.InstalledEndpoint &&
		r.EffectiveEndpoint == o.EffectiveEndpoint && r.InvocationID == o.InvocationID && r.Incarnation == o.Incarnation
}

// The reader's seams, wrapping the executed operations as the registration
// record's do.
var (
	receiptLstat = os.Lstat
	receiptOpen  = func(path string) (*os.File, os.FileInfo, error) {
		return regularfile.Open(path, regularfile.Options{NoFollow: true})
	}
	receiptRead = regularfile.ReadAllLimited
	// receiptOwnerOf reads a file's owner; a test stands root in for its own
	// account, as the guard's tests do.
	receiptOwnerOf = ownerFromInfo
)

// receiptPresence types what the reader found.
type receiptPresence string

const (
	receiptPresent    receiptPresence = "present"
	receiptAbsent     receiptPresence = "absent"
	receiptInvalid    receiptPresence = "invalid"
	receiptUnreadable receiptPresence = "unreadable"
)

// receiptEvidence is what a read carries out: the presence, the receipt when
// present, and why otherwise.
type receiptEvidence struct {
	presence receiptPresence
	receipt  *endpointReceipt
	why      string
	// info is the examined file's metadata (the name's, for a name that is
	// not a regular file; the opened descriptor's otherwise), so a writer
	// can prove the file it judged is still the file at the name.
	info os.FileInfo
}

// readEndpointReceipt reads the receipt at path under the reader's rules.
func readEndpointReceipt(path string) receiptEvidence {
	dir := filepath.Dir(path)

	dirInfo, err := receiptLstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return receiptEvidence{presence: receiptAbsent, why: "no receipt directory at " + dir}
	case err != nil:
		return receiptEvidence{presence: receiptUnreadable, why: fmt.Sprintf("examine the receipt directory %s: %v", dir, err)}
	case dirInfo.Mode()&os.ModeSymlink != 0:
		return receiptEvidence{presence: receiptInvalid, why: "the receipt directory " + dir + " is a symlink"}
	case !dirInfo.IsDir():
		return receiptEvidence{presence: receiptInvalid, why: "the receipt directory " + dir + " is not a directory"}
	}

	// THE NAME EXAMINED BEFORE THE OPEN: a link or a special file at the
	// name is positively not a receipt, and is never opened (a FIFO would
	// wait); an absence here is the positive ENOENT.
	nameInfo, err := receiptLstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return receiptEvidence{presence: receiptAbsent, why: "no receipt at " + path}
	case err != nil:
		return receiptEvidence{presence: receiptUnreadable, why: fmt.Sprintf("examine the receipt %s: %v", path, err)}
	case nameInfo.Mode()&os.ModeSymlink != 0:
		return receiptEvidence{presence: receiptInvalid, why: "the receipt " + path + " is a symlink", info: nameInfo}
	case !nameInfo.Mode().IsRegular():
		return receiptEvidence{presence: receiptInvalid, why: fmt.Sprintf("the receipt %s is %s, not a regular file", path,
			nameInfo.Mode().Type()), info: nameInfo}
	}

	f, info, err := receiptOpen(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !errors.Is(err, regularfile.ErrReopen):
		return receiptEvidence{presence: receiptAbsent, why: "no receipt at " + path}
	case errors.Is(err, regularfile.ErrNotRegular):
		return receiptEvidence{presence: receiptInvalid, why: fmt.Sprintf("the receipt %s: %v", path, err)}
	case err != nil:
		return receiptEvidence{presence: receiptUnreadable, why: fmt.Sprintf("open the receipt %s: %v", path, err)}
	}

	defer func() { _ = f.Close() }()

	uid, ok := receiptOwnerOf(info)
	if !ok {
		return receiptEvidence{presence: receiptInvalid, why: "the receipt carries no owner this platform reports", info: info}
	}

	if uid != 0 {
		return receiptEvidence{presence: receiptInvalid, why: fmt.Sprintf("the receipt is owned by uid %d, want root", uid), info: info}
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		return receiptEvidence{presence: receiptInvalid, why: fmt.Sprintf("the receipt is mode %04o, want 0600", perm), info: info}
	}

	if info.Size() > maxReceiptBytes {
		return receiptEvidence{presence: receiptInvalid, why: fmt.Sprintf("the receipt is larger than %d bytes", maxReceiptBytes), info: info}
	}

	body, err := receiptRead(f, path, maxReceiptBytes)
	if err != nil {
		if errors.Is(err, regularfile.ErrTooLarge) {
			return receiptEvidence{presence: receiptInvalid, why: fmt.Sprintf("the receipt is larger than %d bytes", maxReceiptBytes), info: info}
		}

		return receiptEvidence{presence: receiptUnreadable, why: fmt.Sprintf("read the receipt %s: %v", path, err)}
	}

	rec, err := decodeEndpointReceipt(body)
	if err != nil {
		return receiptEvidence{presence: receiptInvalid, why: "the receipt is malformed: " + err.Error(), info: info}
	}

	return receiptEvidence{presence: receiptPresent, receipt: rec, info: info}
}

// decodeEndpointReceipt decodes the receipt's bytes under the exact member
// set: one JSON object, no duplicate member, no trailing bytes, exactly the
// ten names, schema 1, every other member a non-empty string, the two
// identifiers 32 lowercase hex characters, the digest 64, the endpoints
// canonical, the time RFC 3339.
func decodeEndpointReceipt(body []byte) (*endpointReceipt, error) {
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

	for _, name := range receiptFields {
		if _, ok := members[name]; !ok {
			return nil, fmt.Errorf("the member %q is missing", name)
		}
	}

	for name := range members {
		known := false

		for _, want := range receiptFields {
			if name == want {
				known = true
			}
		}

		if !known {
			return nil, fmt.Errorf("the member %q is not one billet writes", name)
		}
	}

	if string(bytes.TrimSpace(members["schema"])) != "1" {
		return nil, fmt.Errorf("schema is %s, want 1", string(members["schema"]))
	}

	rec := &endpointReceipt{Schema: receiptSchema}

	for name, into := range map[string]*string{
		"run": &rec.Run, "node": &rec.Node, "deployment": &rec.Deployment, "installed_sha256": &rec.InstalledSHA256,
		"installed_endpoint": &rec.InstalledEndpoint, "effective_endpoint": &rec.EffectiveEndpoint,
		"invocation_id": &rec.InvocationID, "incarnation": &rec.Incarnation, "written_at": &rec.WrittenAt,
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

	if err := checkHolder(rec.Run); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}

	if !hex32.MatchString(rec.InvocationID) {
		return nil, errors.New("the invocation_id is not 32 hex characters")
	}

	if !hex32.MatchString(rec.Incarnation) {
		return nil, errors.New("the incarnation is not 32 hex characters")
	}

	if !sha256Hex.MatchString(rec.InstalledSHA256) {
		return nil, errors.New("the installed_sha256 is not 64 lowercase hex digits")
	}

	for name, text := range map[string]string{"installed_endpoint": rec.InstalledEndpoint, "effective_endpoint": rec.EffectiveEndpoint} {
		if _, err := endpoint.ParseCanonical(text); err != nil {
			return nil, fmt.Errorf("%s is not a canonical endpoint: %w", name, err)
		}
	}

	if _, err := time.Parse(time.RFC3339Nano, rec.WrittenAt); err != nil {
		return nil, fmt.Errorf("written_at is not RFC 3339: %w", err)
	}

	return rec, nil
}

// receiptReport is the known form of host.endpoint_receipt.
type receiptReport struct {
	Presence string           `json:"presence"`
	Receipt  *endpointReceipt `json:"receipt,omitempty"`
	Why      string           `json:"why,omitempty"`
}

// hostEndpointReceipt is host.endpoint_receipt: unknown on darwin, unknown
// for a failed examination, and otherwise the typed presence, judged whether
// or not the node runs.
func hostEndpointReceipt() maybe {
	if hostOS == "darwin" || receiptPath == "" {
		return unknown("no endpoint receipt on this platform")
	}

	ev := readEndpointReceipt(receiptPath)

	switch ev.presence {
	case receiptUnreadable:
		return unknown(ev.why)
	case receiptPresent:
		return known(receiptReport{Presence: string(receiptPresent), Receipt: ev.receipt})
	case receiptAbsent:
		return known(receiptReport{Presence: string(receiptAbsent)})
	default:
		return known(receiptReport{Presence: string(receiptInvalid), Why: ev.why})
	}
}
