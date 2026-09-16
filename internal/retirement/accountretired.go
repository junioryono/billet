package retirement

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/junioryono/billet/internal/regularfile"
)

// ReadRetiredServiceAccount is the privileged observer's descriptor-bound
// account assertion; it never repairs ownership or adopts an existing account.
func ReadRetiredServiceAccount() (ServiceAccount, error) {
	path := ServiceAccountPath()
	f, info, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
	if err != nil {
		return ServiceAccount{}, err
	}
	defer func() { _ = f.Close() }()
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() || info.Mode().Perm()&0o022 != 0 || owner.Nlink != 1 {
		return ServiceAccount{}, errors.New("retired service-account record is not owned, singly linked and protected from other writers")
	}
	body, err := io.ReadAll(io.LimitReader(f, maxAccountBytes+1))
	if err != nil {
		return ServiceAccount{}, err
	}
	if len(body) > maxAccountBytes {
		return ServiceAccount{}, errors.New("retired service-account record exceeds its read bound")
	}
	var account ServiceAccount
	if err := strictDecode(body, &account); err != nil {
		return account, err
	}
	if err := account.validate(); err != nil {
		return account, fmt.Errorf("retired service-account: %w", err)
	}
	return account, nil
}
