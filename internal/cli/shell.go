package cli

import "strings"

// ShellArg renders s so it survives being pasted into a POSIX shell as one word.
//
// A value that is already a single shell word — the common case, an ordinary
// path or org name — is returned unchanged so the printed command reads
// naturally. Anything else is wrapped in single quotes, with each embedded
// single quote written as the four-character break-out quote-backslash-quote-quote,
// because inside single quotes the shell treats every other byte literally. An
// empty string becomes an empty single-quoted pair so it stays a visible, valid
// argument rather than vanishing.
func ShellArg(s string) string {
	if s == "" {
		return "''"
	}

	for _, r := range s {
		if !shellSafeRune(r) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}

	return s
}

// shellSafeRune reports whether r can stand unquoted in a POSIX shell word. The
// set is deliberately conservative — alphanumerics and the punctuation a path,
// org or flag value ordinarily carries — so anything outside it is quoted rather
// than reasoned about.
func shellSafeRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		strings.ContainsRune("_-./@:", r)
}

// ShellArgs quotes a whole argument list for pasting into a shell.
func ShellArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, ShellArg(a))
	}

	return strings.Join(quoted, " ")
}
