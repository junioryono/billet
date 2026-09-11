package retirement

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// strictDecode refuses unknown members and trailing content, so a record from
// a later shape is a typed refusal rather than a partial read.
func strictDecode(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	if dec.More() {
		return errors.New("decode: trailing content after the document")
	}

	return nil
}
