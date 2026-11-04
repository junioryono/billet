//go:build linux

package egress

import (
	"fmt"

	"github.com/ti-mo/conntrack"
	"github.com/ti-mo/netfilter"
)

// forgetConnections deletes every connection a guest's resolver owns, by the mark its
// redirect stamped on each. A connection keeps its translation for as long as it
// lives, whatever has happened to the rule that made it, so a guest that holds one UDP
// socket across a change of resolver would otherwise go on reaching the port it was
// first redirected to, and a port the kernel has since given to something else.
func forgetConnections(mark uint32) error {
	conn, err := conntrack.Dial(nil)
	if err != nil {
		return fmt.Errorf("open the connection tracker: %w", err)
	}
	defer conn.Close()

	if err := conn.FlushFilter(conntrack.NewFilter().Family(netfilter.ProtoIPv4).Mark(mark)); err != nil {
		return fmt.Errorf("delete the connections marked %#x: %w", mark, err)
	}

	return nil
}
