package guestreport

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean is s as the agent may send it in a field bounded at limit bytes: every
// invalid byte and every control character replaced by '?', then cut to at most
// limit bytes on a character boundary.
func Clean(s string, limit int) string {
	var b strings.Builder

	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			r = '?'
		}

		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}

		b.WriteRune(r)
	}

	return b.String()
}

// checkText refuses s if it is empty, over limit bytes, or holds a control
// character. It is never quoted: the guest wrote it.
func checkText(s string, limit int, where string) error {
	if s == "" {
		return refuse(ErrEmpty, where)
	}

	if len(s) > limit {
		return refuse(ErrNameTooLong, where)
	}

	if strings.ContainsFunc(s, unicode.IsControl) {
		return refuse(ErrControlCharacter, where)
	}

	return nil
}
