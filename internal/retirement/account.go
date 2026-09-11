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
func publish(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("retirement: stage %s: %w", path, err)
	}

	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

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

func syncDir(dir string) error {
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
	if err := refuseRepeatedMembers(raw); err != nil {
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

// refuseRepeatedMembers walks the document's tokens and refuses an object that
// names one member twice: encoding/json keeps the last value silently, and a
// record that says two things about one field is not a record billet wrote.
//
// The walk keeps one frame per open object or array, because a string inside
// an array nested in an object is a value and never a key; the decoder's own
// tokeniser has already refused malformed syntax by the time a token arrives.
func refuseRepeatedMembers(raw []byte) error {
	type frame struct {
		object    bool
		members   map[string]struct{}
		expectKey bool
	}

	dec := json.NewDecoder(bytes.NewReader(raw))

	var stack []*frame

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
				if top != nil && top.object {
					top.expectKey = true
				}

				stack = append(stack, &frame{
					object: d == '{', members: map[string]struct{}{}, expectKey: d == '{',
				})
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
	}
}
