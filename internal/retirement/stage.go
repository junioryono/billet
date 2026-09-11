package retirement

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"

	"github.com/junioryono/billet/internal/regularfile"
)

// MaxStageBytes bounds the staged serverless configuration: a rendering of
// billet.yaml is kilobytes, and a longer file is not one billet staged.
const MaxStageBytes = 1 << 20

// StagePresence is the three-valued answer a stage read gives.
type StagePresence int

const (
	StageFileAbsent StagePresence = iota
	StageFilePresent
	StageFileUnreadable
)

// WriteStage publishes the serverless rendering's EXACT bytes at the stage's
// fixed path, root 0600 under the private retirement directory, by temp,
// fsync and rename; a link at the stage's name is replaced, never followed.
func WriteStage(body []byte) error {
	if len(body) > MaxStageBytes {
		return fmt.Errorf("retirement: refuse to stage %d bytes, longer than the %d its reader admits", len(body), MaxStageBytes)
	}

	if err := EnsureRetiredDir(); err != nil {
		return err
	}

	return publish(StagePath(), body, 0o600)
}

// ReadStage reads the stage through the identity-first open, never following
// a link at its name; absence is the positive ENOENT and nothing else.
func ReadStage() ([]byte, StagePresence, error) {
	body, err := regularfile.ReadFile(StagePath(), MaxStageBytes, regularfile.Options{NoFollow: true})

	switch {
	case err == nil:
		return body, StageFilePresent, nil
	case errors.Is(err, fs.ErrNotExist) && !errors.Is(err, regularfile.ErrReopen):
		return nil, StageFileAbsent, nil
	default:
		return nil, StageFileUnreadable, fmt.Errorf("retirement: read the stage %s: %w", StagePath(), err)
	}
}

// Digest is the hex sha256 of a document, the one spelling the journal
// records.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:])
}

// DecodeDocument decodes one JSON document strictly into a pointer: no
// repeated member, no member the type does not spell exactly, nothing after
// the document. A map or an interface admits any member name, and its values
// are held to the element type.
func DecodeDocument(raw []byte, into any) error {
	return strictDecode(raw, into)
}
