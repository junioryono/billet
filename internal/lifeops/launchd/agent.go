package launchd

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// declared is what a plist ASKS FOR, as opposed to what launchd loaded.
//
// The two are compared, and they are different things: launchd reads a plist
// once at bootstrap and keeps what it read, so a running job and the file it
// came from drift apart the moment somebody edits the file.
type declared struct {
	Label            string
	Arguments        []string
	Environment      map[string]string
	ExitTimeout      int
	ExitTimeoutKnown bool
	RunAtLoad        bool
}

// declaredAgent reads a plist billet ships.
//
// PARSED BY AN XML PARSER, not searched. These files are mostly comments —
// the reasoning is the point of them — and every value in them appears in that
// prose as well as in the key that matters. A substring search would be
// satisfied by the explanation, which is how a comparison against the shipped
// agent passes with the shipped agent's real settings deleted.
func declaredAgent(body string) (declared, error) {
	var (
		out     declared
		decoder = xml.NewDecoder(strings.NewReader(body))
		depth   int
		inKey   bool
		keyText string
		pending string
	)

	for {
		tok, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return declared{}, fmt.Errorf("launchd: the agent is not well-formed XML: %w", err)
		}

		switch el := tok.(type) {
		case xml.StartElement:
			switch {
			case el.Name.Local == "dict" && pending == "":
				depth++

			case depth != 1:
				// Nested inside something that is not a top-level setting.

			case pending != "":
				if err := out.take(decoder, &el, pending); err != nil {
					return declared{}, err
				}

				pending = ""

			case el.Name.Local == "key":
				inKey = true
				keyText = ""
			}

		case xml.CharData:
			if inKey {
				keyText += string(el)
			}

		case xml.EndElement:
			switch {
			case el.Name.Local == "dict" && pending == "":
				depth--

			case el.Name.Local == "key" && inKey:
				inKey = false

				if depth == 1 {
					pending = strings.TrimSpace(keyText)
				}
			}
		}
	}

	if out.Label == "" {
		return declared{}, errors.New("launchd: the agent declares no Label")
	}

	return out, nil
}

// take consumes the value element belonging to one top-level key.
//
// EXACTLY THE NEXT ELEMENT, consumed whole. Scanning forward for the next
// character data instead means an EMPTY value falls through to the FOLLOWING
// key's — the same defect deploy/units_test.go's parser was written to avoid,
// where `<key>ExitTimeOut</key><integer></integer>` picked up the next setting's
// number and the check passed on a plist launchd would reject.
func (d *declared) take(decoder *xml.Decoder, el *xml.StartElement, key string) error {
	switch key {
	case "Label":
		var v struct {
			Text string `xml:",chardata"`
		}

		if err := decoder.DecodeElement(&v, el); err != nil {
			return fmt.Errorf("launchd: read Label: %w", err)
		}

		d.Label = strings.TrimSpace(v.Text)

	case "ProgramArguments":
		var v struct {
			Items []string `xml:"string"`
		}

		if err := decoder.DecodeElement(&v, el); err != nil {
			return fmt.Errorf("launchd: read ProgramArguments: %w", err)
		}

		d.Arguments = v.Items

	case "EnvironmentVariables":
		var v struct {
			Keys   []string `xml:"key"`
			Values []string `xml:"string"`
		}

		if err := decoder.DecodeElement(&v, el); err != nil {
			return fmt.Errorf("launchd: read EnvironmentVariables: %w", err)
		}

		if len(v.Keys) != len(v.Values) {
			return fmt.Errorf("launchd: EnvironmentVariables has %d keys and %d values, so "+
				"billet cannot tell which belongs to which", len(v.Keys), len(v.Values))
		}

		d.Environment = map[string]string{}
		for i, k := range v.Keys {
			d.Environment[strings.TrimSpace(k)] = strings.TrimSpace(v.Values[i])
		}

	case "ExitTimeOut":
		var v struct {
			Text string `xml:",chardata"`
		}

		if err := decoder.DecodeElement(&v, el); err != nil {
			return fmt.Errorf("launchd: read ExitTimeOut: %w", err)
		}

		n, err := strconv.Atoi(strings.TrimSpace(v.Text))
		if err != nil {
			return fmt.Errorf("launchd: ExitTimeOut is %q, not a number", v.Text)
		}

		d.ExitTimeout, d.ExitTimeoutKnown = n, true

	case "RunAtLoad":
		d.RunAtLoad = el.Name.Local == "true"

		if err := decoder.Skip(); err != nil {
			return fmt.Errorf("launchd: read RunAtLoad: %w", err)
		}

	default:
		if err := decoder.Skip(); err != nil {
			return fmt.Errorf("launchd: skip %s: %w", key, err)
		}
	}

	return nil
}
