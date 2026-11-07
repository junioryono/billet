package launchd

import (
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"strings"
)

// sameAgent reports whether an installed agent is the one this build ships: the same
// property list, every key and value as written, with the comments and the whitespace
// between elements left out.
//
// NOT THE FILES' BYTES. #720 reworded only the comment at the top of a shipped agent,
// and every Mac installed before that release then refused billet's own agent as an
// operator's edit (#787). A comment and the indentation say nothing launchd reads, so
// they are not a difference; any key, value, element or attribute is, and so is text
// inside a value even when it is only whitespace. A file that does not read as XML is
// different from everything, so it is refused as an edit is.
func sameAgent(installed, shipped string) bool {
	a, err := agentTokens(installed)
	if err != nil {
		return false
	}

	b, err := agentTokens(shipped)
	if err != nil {
		return false
	}

	return slices.Equal(a, b)
}

// plistContainers are the elements whose text between children is layout, never a value.
var plistContainers = map[string]bool{"plist": true, "dict": true, "array": true}

// agentTokens is a property list as the tokens launchd's reading depends on.
func agentTokens(body string) ([]string, error) {
	d := xml.NewDecoder(strings.NewReader(body))
	d.Strict = true

	var (
		out   []string
		stack []string
	)

	for {
		// A STRICT DECODER ENDS A FILE CUT INSIDE AN ELEMENT WITH AN ERROR, not io.EOF.
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}

		if err != nil {
			return nil, err
		}

		switch t := tok.(type) {
		case xml.Comment:
			continue
		case xml.CharData:
			// LAYOUT ONLY OUTSIDE A VALUE: whitespace at the top level or between a
			// container's children. Inside <string> or <key> every byte is the value.
			if strings.TrimSpace(string(t)) == "" && (len(stack) == 0 || plistContainers[stack[len(stack)-1]]) {
				continue
			}

			out = append(out, "text\x00"+string(t))
		case xml.StartElement:
			stack = append(stack, t.Name.Local)

			attrs := make([]string, 0, len(t.Attr))
			for _, a := range t.Attr {
				attrs = append(attrs, a.Name.Space+"\x00"+a.Name.Local+"\x00"+a.Value)
			}

			slices.Sort(attrs)

			out = append(out, "start\x00"+t.Name.Space+"\x00"+t.Name.Local+"\x00"+strings.Join(attrs, "\x01"))
		case xml.EndElement:
			stack = stack[:len(stack)-1]

			out = append(out, "end\x00"+t.Name.Space+"\x00"+t.Name.Local)
		case xml.ProcInst:
			out = append(out, "pi\x00"+t.Target+"\x00"+strings.TrimSpace(string(t.Inst)))
		case xml.Directive:
			out = append(out, "directive\x00"+strings.Join(strings.Fields(string(t)), " "))
		}
	}
}
