package launchd

import (
	"errors"
	"strings"
)

// maxAgentBytes bounds an installed agent billet will compare. A shipped agent is a few
// KiB; anything larger is not one, and reading it whole is a host's file making billet
// allocate whatever it says.
const maxAgentBytes = 64 << 10

// sameAgent reports whether an installed agent is the one this build ships: the same
// bytes, once the comments and the whitespace between elements are taken out of both.
//
// NOT THE FILES' WHOLE BYTES. #720 reworded only the comment at the top of a shipped
// agent, and every Mac installed before that release then refused billet's own agent as
// an operator's edit (#787). A comment and the indentation say nothing launchd reads.
//
// NOR AN XML READING OF THEM: that would call a CDATA or an entity spelling of a value, a
// comment inside a <true></true> or a non-breaking space between elements the same, and
// Apple's plist parser refuses each, so billet would keep an agent launchd cannot load.
// So the comparison stays on the bytes: a comment is dropped only between elements, and
// whitespace only between elements and only XML's four whitespace characters. Inside a
// value nothing is dropped, and a file billet cannot scan is another agent.
func sameAgent(installed, shipped string) bool {
	if len(installed) > maxAgentBytes {
		return false
	}

	a, err := agentLayoutFree(installed)
	if err != nil {
		return false
	}

	b, err := agentLayoutFree(shipped)
	if err != nil {
		return false
	}

	return a == b
}

// wroteThisAgent reports whether an agent is still exactly what this run installed, which
// is what a rollback asks before removing it: a comment an operator added since is theirs.
// A file past the cap was read only in part, and what was not read may be their edit.
func wroteThisAgent(installed, shipped string) bool {
	if len(installed) > maxAgentBytes {
		return false
	}

	return strings.TrimSpace(installed) == strings.TrimSpace(shipped)
}

// plistContainers hold elements and never a value of their own.
var plistContainers = map[string]bool{"plist": true, "dict": true, "array": true}

// agentLayoutFree is an agent's bytes without its comments and its layout: what sits
// between elements, where no value is. Everything else is kept as written.
func agentLayoutFree(body string) (string, error) {
	var (
		out strings.Builder
		// between is true where no value can be: before the first element, after any
		// closing or empty element, and inside a container.
		between = true
	)

	for i := 0; i < len(body); {
		switch {
		case strings.HasPrefix(body[i:], "<!--"):
			end := strings.Index(body[i+4:], "-->")
			if end < 0 {
				return "", errors.New("launchd: an agent's comment never ends")
			}

			comment := body[i : i+4+end+3]
			if !between {
				out.WriteString(comment)
			}

			i += len(comment)

		case body[i] == '<':
			end := strings.IndexByte(body[i:], '>')
			if end < 0 {
				return "", errors.New("launchd: an agent's element never ends")
			}

			tag := body[i : i+end+1]
			out.WriteString(tag)
			i += len(tag)

			between = tagEndsAValue(tag)

		default:
			end := strings.IndexByte(body[i:], '<')
			if end < 0 {
				end = len(body) - i
			}

			text := body[i : i+end]
			if !between || strings.Trim(text, " \t\r\n") != "" {
				out.WriteString(text)
			}

			i += len(text)
		}
	}

	return out.String(), nil
}

// tagEndsAValue reports whether what follows a tag is between elements: after a closing
// tag, an empty element, a declaration or a container's opening, and not after a value's.
func tagEndsAValue(tag string) bool {
	switch {
	case strings.HasPrefix(tag, "</"), strings.HasSuffix(tag, "/>"),
		strings.HasPrefix(tag, "<?"), strings.HasPrefix(tag, "<!"):
		return true
	}

	name := strings.TrimSuffix(strings.TrimPrefix(tag, "<"), ">")
	if i := strings.IndexAny(name, " \t\r\n"); i >= 0 {
		name = name[:i]
	}

	return plistContainers[name]
}
