// Package notices carries the third-party notices of everything the billet binary
// links or contains, as MIT, BSD and Apache-2.0 require of a binary that
// redistributes them. scripts/mknotices writes THIRD_PARTY_NOTICES; its test fails
// when the committed file is not what the generator writes.
package notices

import _ "embed"

//go:embed THIRD_PARTY_NOTICES
var text string

// Text is the notices file, whole.
func Text() string { return text }
