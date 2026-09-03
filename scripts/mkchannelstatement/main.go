// Command mkchannelstatement writes one signed-channel pointer.
//
// A GO PROGRAM FOR THE REASON mkreleasemanifest IS ONE: the statement's schema is
// a contract between the publisher and every deployment that reads it, and a
// document assembled by jq is a second implementation of that schema. Here the
// writer imports the reader's own type and puts its output through the reader's
// own validation, so a publisher cannot emit a pointer its own fleet refuses —
// which is otherwise discovered when every deployment stops updating.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/junioryono/billet/internal/releasesource"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mkchannelstatement: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	channel := flag.String("channel", releasesource.ChannelStable, "the channel to write")
	tag := flag.String("tag", "", "the immutable release this channel names")
	manifest := flag.String("manifest-sha256", "",
		"the digest of that release's manifest, as published")
	out := flag.String("out", "", "where to write the statement")

	flag.Parse()

	if *tag == "" || *manifest == "" || *out == "" {
		return fmt.Errorf("--tag, --manifest-sha256 and --out are required")
	}

	// THE VALIDITY WINDOW IS THE PACKAGE'S, not a flag. An expiry a publisher can
	// choose is an expiry a publisher can set to a century, and a pointer with no
	// bounded lifetime cannot be told from a replay.
	statement, err := releasesource.NewChannelStatement(*channel, *tag, *manifest, time.Now())
	if err != nil {
		return err
	}

	body, err := statement.Marshal()
	if err != nil {
		return err
	}

	if err := os.WriteFile(*out, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}

	fmt.Printf("wrote %s: %s -> %s, valid until %s\n", *out, statement.Channel,
		statement.Tag, statement.ExpiresAt.Format(time.RFC3339))

	return nil
}
