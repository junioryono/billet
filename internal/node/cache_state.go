package node

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/junioryono/billet/internal/provider"
)

const cacheSessionDirectory = "cache-sessions"

type durableCacheSession struct {
	Token    string                                `json:"token"`
	Instance string                                `json:"instance"`
	Trust    provider.TrustClass                   `json:"trust"`
	Closed   bool                                  `json:"closed"`
	Slots    [provider.MaxVolumes]*cacheAttachment `json:"slots"`
}

func (s *CacheService) loadSessions() error {
	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return fmt.Errorf("node: create cache custody directory: %w", err)
	}

	entries, err := os.ReadDir(s.stateDir)
	if err != nil {
		return fmt.Errorf("node: read cache custody directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		body, err := os.ReadFile(filepath.Join(s.stateDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("node: read cache custody %s: %w", entry.Name(), err)
		}
		var record durableCacheSession
		if err := json.Unmarshal(body, &record); err != nil {
			return fmt.Errorf("node: cache custody %s is not valid json: %w", entry.Name(), err)
		}
		if err := record.valid(entry.Name()); err != nil {
			return err
		}
		if _, duplicate := s.byToken[record.Token]; duplicate {
			return fmt.Errorf("node: duplicate cache custody token in %s", entry.Name())
		}
		if _, duplicate := s.byInstance[record.Instance]; duplicate {
			return fmt.Errorf("node: duplicate cache custody for instance %s", record.Instance)
		}

		session := &cacheSession{
			token: record.Token, instance: record.Instance, trust: record.Trust,
			closed: record.Closed, slots: record.Slots, admit: make(chan struct{}, 1),
		}
		s.byToken[record.Token] = session
		s.byInstance[record.Instance] = record.Token
	}

	return nil
}

func (r durableCacheSession) valid(filename string) error {
	raw, err := hex.DecodeString(r.Token)
	if err != nil || len(raw) != 32 || filename != r.Token+".json" {
		return fmt.Errorf("node: cache custody file %s has an invalid token identity", filename)
	}
	if strings.TrimSpace(r.Instance) == "" {
		return fmt.Errorf("node: cache custody file %s names no instance", filename)
	}
	if r.Trust != provider.TrustTrusted && r.Trust != provider.TrustUntrusted {
		return fmt.Errorf("node: cache custody file %s has unknown trust %q", filename, r.Trust)
	}

	return nil
}

// persistSession is called while session.mu is held.
func (s *CacheService) persistSession(session *cacheSession) error {
	record := durableCacheSession{
		Token: session.token, Instance: session.instance, Trust: session.trust,
		Closed: session.closed, Slots: session.slots,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("node: encode cache custody for %s: %w", session.instance, err)
	}

	temporary, err := os.CreateTemp(s.stateDir, ".session-")
	if err != nil {
		return fmt.Errorf("node: stage cache custody for %s: %w", session.instance, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	workErr := errors.Join(temporary.Chmod(0o600), writeAndSync(temporary, encoded), temporary.Close())
	if workErr != nil {
		return fmt.Errorf("node: write cache custody for %s: %w", session.instance, workErr)
	}

	target := filepath.Join(s.stateDir, session.token+".json")
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("node: install cache custody for %s: %w", session.instance, err)
	}

	return syncDirectory(s.stateDir)
}

func writeAndSync(file *os.File, body []byte) error {
	if _, err := file.Write(body); err != nil {
		return err
	}

	return file.Sync()
}

func (s *CacheService) removeSession(session *cacheSession) error {
	path := filepath.Join(s.stateDir, session.token+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("node: remove cache custody for %s: %w", session.instance, err)
	}

	return syncDirectory(s.stateDir)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("node: open cache custody directory: %w", err)
	}
	defer directory.Close()

	if err := directory.Sync(); err != nil {
		return fmt.Errorf("node: sync cache custody directory: %w", err)
	}

	return nil
}
