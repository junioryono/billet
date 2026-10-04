package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"

	"github.com/junioryono/billet/internal/regularfile"
)

// AppKey is the App's private key in PEM, the credential that can mint a token
// for a whole organization.
//
// ITS OWN TYPE, NOT []byte, so every rendering path redacts it: a key that
// reaches a log, a %v or a serialized struct prints the sentence below rather
// than the PEM. The five methods each cover a path the others do not (see App),
// on value receivers so a key reached through a field is covered too. What the
// key is FOR still reads the bytes, and an AppKey is assignable to a []byte
// parameter as it is.
type AppKey []byte

const redactedAppKey = "[redacted app private key]"

// MaxKeySize bounds a key file read. A real App key is a couple of kilobytes;
// anything larger is a misconfiguration, and reading it whole would be the
// misconfiguration's problem to solve rather than billet's.
const MaxKeySize = 64 << 10

// Validate reports whether the bytes are a usable App key.
func (k AppKey) Validate() error { return ValidatePrivateKey(k) }

// String redacts the key.
func (AppKey) String() string { return redactedAppKey }

// GoString covers %#v, which does not consult String.
func (k AppKey) GoString() string { return k.String() }

// Format makes EVERY verb safe, not only the ones fmt.Stringer covers; %x on
// a byte slice would otherwise hex-dump the key.
func (k AppKey) Format(s fmt.State, verb rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(s, k.String())

	_ = verb
}

// MarshalJSON keeps the key out of anything that serializes it; encoding/json
// would otherwise base64 a byte slice, which is the key in another spelling.
func (k AppKey) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(k.String())
	if err != nil {
		return nil, fmt.Errorf("github: marshal redacted app key: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection.
func (k AppKey) LogValue() slog.Value { return slog.StringValue(k.String()) }

// ReadPrivateKeyFile validates the App key at path and returns it.
//
// ONE IMPLEMENTATION for every reader of the file: `billet check`, `billet
// server`, the backup and the key publisher. They had diverged once: check
// rejected a non-regular file, bounded the read, worked from a single
// descriptor, could not be hung by a FIFO, and refused group- or world-readable
// modes, while the server did os.ReadFile and parsed the result. So `billet
// check` refused a mode-0644 organization credential that `billet server` would
// happily start with, which is the wrong way round for the command that runs
// unattended.
//
// Opened ONCE, through regularfile, and inspected through that descriptor:
// stat-then-read is two lookups of one name, so the size, type and mode could
// describe a different inode than the bytes that get parsed, and a plain open of
// a FIFO blocks forever rather than returning.
func ReadPrivateKeyFile(path string) (AppKey, error) {
	f, info, err := regularfile.Open(path, regularfile.Options{})
	if err != nil {
		return nil, fmt.Errorf("github.private_key_path %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if info.Size() == 0 {
		return nil, fmt.Errorf(
			"github.private_key_path %s is empty; an interrupted `billet github-app create` leaves "+
				"a placeholder there. Remove it and re-run that command", path)
	}

	if info.Size() > MaxKeySize {
		return nil, fmt.Errorf("github.private_key_path %s is %d bytes; that is not an App key",
			path, info.Size())
	}

	// Group and other bits on a private key are a local exposure. Checked on
	// unix only: Windows permissions are ACL-based and these bits are meaningless
	// there, so testing them would produce a false alarm on every Windows host.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf(
				"github.private_key_path %s is mode %04o; it is readable beyond its owner. "+
					"Run: chmod 600 %s", path, perm, path)
		}
	}

	// Bounded for real: the size check above describes the inode at that moment,
	// while this limit holds regardless.
	pemBytes, err := regularfile.ReadAllLimited(f, path, MaxKeySize)
	if err != nil {
		if errors.Is(err, regularfile.ErrTooLarge) {
			return nil, fmt.Errorf("github.private_key_path %s is larger than %d bytes; that is not an App key",
				path, MaxKeySize)
		}

		return nil, fmt.Errorf("read github.private_key_path %s: %w", path, err)
	}

	// Parsed, not merely read: a truncated PEM is exactly what an interrupted
	// write leaves, and it fails at the first API call rather than here.
	if err := ValidatePrivateKey(pemBytes); err != nil {
		return nil, fmt.Errorf("github.private_key_path %s: %w", path, err)
	}

	return AppKey(pemBytes), nil
}
