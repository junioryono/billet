package retirement

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/junioryono/billet/internal/regularfile"
)

// ServiceAccount is the account the host's services run as, as a privileged
// installer recorded it.
//
// A ROOT-PROVIDED ASSERTION, NOT A PROOF: nothing here verifies that the
// services actually run as this account. The record is what the lock owners and
// the hand-back give things to, written by `local up`, the package's
// postinstall and the host role from the account each of them validated.
type ServiceAccount struct {
	User  string `json:"user"`
	UID   int    `json:"uid"`
	Group string `json:"group"`
	GID   int    `json:"gid"`
}

// maxAccountBytes bounds a read of the record: four short fields.
const maxAccountBytes = 4096

// ErrNoServiceAccount is the positive absence of the record: a host no
// installer has prepared. Distinct from a failed read, which is never absence.
var ErrNoServiceAccount = errors.New("retirement: no service account is recorded on this host")

// ReadServiceAccount reads the record, or says positively that there is none.
func ReadServiceAccount() (ServiceAccount, error) {
	raw, err := regularfile.ReadFile(ServiceAccountPath(), maxAccountBytes, regularfile.Options{})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ServiceAccount{}, ErrNoServiceAccount
		}

		return ServiceAccount{}, fmt.Errorf("retirement: read %s: %w", ServiceAccountPath(), err)
	}

	var acct ServiceAccount
	if err := strictDecode(raw, &acct); err != nil {
		return ServiceAccount{}, fmt.Errorf("retirement: %s: %w", ServiceAccountPath(), err)
	}

	if err := acct.validate(); err != nil {
		return ServiceAccount{}, fmt.Errorf("retirement: %s: %w", ServiceAccountPath(), err)
	}

	return acct, nil
}

func (a ServiceAccount) validate() error {
	switch {
	case a.User == "" || a.Group == "":
		return errors.New("names an empty user or group")
	case a.UID <= 0 || a.GID <= 0:
		// Root is never the service account, and a zero would be the value an
		// unset field decodes to.
		return errors.New("names uid or gid 0, which is not a service account")
	}

	return nil
}

// WriteServiceAccount publishes the record durably: a temporary file beside it,
// synced, renamed over the name, the directory synced. Root only; the record is
// world-readable because the services read it.
func WriteServiceAccount(acct ServiceAccount) error {
	if err := acct.validate(); err != nil {
		return fmt.Errorf("retirement: refuse to record %+v: %w", acct, err)
	}

	body, err := json.Marshal(acct)
	if err != nil {
		return fmt.Errorf("retirement: encode the service account: %w", err)
	}

	return publish(ServiceAccountPath(), append(body, '\n'), 0o644)
}

// publish installs bytes at path by temp, fsync, rename and directory fsync,
// so a crash leaves either the old file or the new one and never a torn one.
// Publishing is the one hook a test injects a failure into: every durable
// write this package makes (the journal, the stage, the status, the service
// account) goes through `publish`, so a test that fails one path proves the
// order of the writes around it. Nil in production.
var Publishing func(path string) error

func publish(path string, body []byte, mode os.FileMode) error {
	if Publishing != nil {
		if err := Publishing(path); err != nil {
			return err
		}
	}

	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("retirement: stage %s: %w", path, err)
	}

	tmpName := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()

		return fmt.Errorf("retirement: write %s: %w", tmpName, err)
	}

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()

		return fmt.Errorf("retirement: set the mode of %s: %w", tmpName, err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()

		return fmt.Errorf("retirement: sync %s: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		cleanup()

		return fmt.Errorf("retirement: close %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		cleanup()

		return fmt.Errorf("retirement: install %s: %w", path, err)
	}

	return syncDir(dir)
}

// SyncingDir runs before each directory flush this package makes, so a test
// can fail the flush that FOLLOWS a rename and stage the one remainder a
// publish can leave: the new file in place and its entry not yet durable. Nil
// in production.
var SyncingDir func(dir string) error

func syncDir(dir string) error {
	if SyncingDir != nil {
		if err := SyncingDir(dir); err != nil {
			return err
		}
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("retirement: open %s to sync it: %w", dir, err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("retirement: sync %s: %w", dir, err)
	}

	return nil
}

// strictDecode refuses unknown members, repeated members and trailing content,
// so a record from a later shape, or one that says two things about one field,
// is a typed refusal rather than a partial read.
//
// END OF DOCUMENT IS PROVED BY READING PAST IT. Decoder.More answers false for
// a trailing `]` or `}` as well as for EOF, so a valid record followed by a
// stray bracket passed it; the next token has to be io.EOF and nothing else.
func strictDecode(raw []byte, into any) error {
	if err := checkMembers(raw, reflect.TypeOf(into)); err != nil {
		return err
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("decode: trailing content after the document")
	}

	return nil
}

// checkMembers walks the document's tokens against the type it will be
// decoded into and refuses an object that names one member twice, or names
// one the type does not spell EXACTLY: encoding/json keeps the last of two
// values silently and matches a member name case-insensitively, so `"ROW"`
// would land in Row past DisallowUnknownFields, and a record that says two
// things about one field, or spells a field a way billet never writes, is
// not a record billet wrote.
//
// The walk keeps one frame per open object or array, carrying the Go type
// that object or array is held to (none for a map or an interface, which
// admit any member); a string inside an array nested in an object is a value
// and never a key; the decoder's own tokeniser has already refused malformed
// syntax by the time a token arrives.
func checkMembers(raw []byte, typ reflect.Type) error {
	type frame struct {
		object    bool
		members   map[string]struct{}
		expectKey bool
		// held is the struct an object's members are looked up in; elem an
		// array's or a map's element type; next the type of the value the
		// last key opens.
		held, elem, next reflect.Type
	}

	dec := json.NewDecoder(bytes.NewReader(raw))

	var stack []*frame

	rootType := derefType(typ)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("decode: %w", err)
		}

		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}

		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				// This delimiter is a VALUE of the enclosing object; once it
				// closes, the enclosing object's next token is a key again.
				valueType := rootType

				if top != nil {
					valueType = top.elem

					if top.object {
						top.expectKey = true
						valueType = top.next
					}
				}

				f := &frame{object: d == '{', members: map[string]struct{}{}, expectKey: d == '{'}

				switch {
				case valueType == nil:
				case d == '{' && valueType.Kind() == reflect.Struct:
					f.held = valueType
				case d == '{' && valueType.Kind() == reflect.Map:
					f.elem = derefType(valueType.Elem())
				case d == '[' && (valueType.Kind() == reflect.Slice || valueType.Kind() == reflect.Array):
					f.elem = derefType(valueType.Elem())
				}

				stack = append(stack, f)
			default:
				stack = stack[:len(stack)-1]
			}

			continue
		}

		if top == nil || !top.object {
			continue
		}

		if !top.expectKey {
			top.expectKey = true

			continue
		}

		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("decode: a member name is not a string (%v)", tok)
		}

		if _, dup := top.members[key]; dup {
			return fmt.Errorf("decode: the member %q appears twice", key)
		}

		top.members[key] = struct{}{}
		top.expectKey = false
		top.next = top.elem

		if top.held != nil {
			field, found := jsonField(top.held, key)
			if !found {
				return fmt.Errorf("decode: the member %q is not one this record carries", key)
			}

			top.next = derefType(field.Type)
		}
	}
}

// derefType is the type behind any pointers, or nil for none.
func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t
}

// jsonField finds the field of a struct that encoding/json writes under
// EXACTLY the name given, embedded structs flattened as the encoder flattens
// them.
func jsonField(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")

		if tag == "-" {
			continue
		}

		tagName, _, _ := strings.Cut(tag, ",")

		if tagName == "" && f.Anonymous {
			if embedded := derefType(f.Type); embedded.Kind() == reflect.Struct {
				if found, ok := jsonField(embedded, name); ok {
					return found, true
				}
			}

			continue
		}

		if !f.IsExported() {
			continue
		}

		if tagName == "" {
			tagName = f.Name
		}

		if tagName == name {
			return f, true
		}
	}

	return reflect.StructField{}, false
}
