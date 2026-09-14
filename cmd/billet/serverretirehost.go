package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The units a retirement stops, awaits or leaves installed, beside the two
// the transaction names.
const (
	backupServiceUnit = "billet-backup.service"
	backupTimerUnit   = "billet-backup.timer"
	upgradeTimerUnit  = "billet-upgrade.timer"
)

// hostAddress is one address this host holds, as the inspector reports it
// and as the request compares the survivor's against its own.
type hostAddress struct {
	// Address is the canonical spelling: an IPv4-mapped address unwrapped,
	// no zone.
	Address string `json:"address"`
	// Scope is host, link or global, as the address's own class says.
	Scope     string `json:"scope"`
	Interface string `json:"interface"`
	Index     int    `json:"index"`
}

// hostInterfaceAddresses is the seam the inspector and the request read this
// host's addresses through; production asks the kernel.
var hostInterfaceAddresses = interfaceAddresses

// interfaceAddresses enumerates every unicast address of every interface,
// in interface-index then address order.
func interfaceAddresses() ([]hostAddress, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list the interfaces: %w", err)
	}

	var out []hostAddress

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("list the addresses of %s: %w", iface.Name, err)
		}

		for _, a := range addrs {
			var ip net.IP

			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}

			addr, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}

			addr = addr.Unmap()

			out = append(out, hostAddress{Address: addr.String(), Scope: addressScope(addr), Interface: iface.Name, Index: iface.Index})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}

		return out[i].Address < out[j].Address
	})

	return out, nil
}

// addressScope classifies an address for the report: loopback is host,
// link-local is link, everything else global.
func addressScope(a netip.Addr) string {
	switch {
	case a.IsLoopback():
		return "host"
	case a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast():
		return "link"
	default:
		return "global"
	}
}

// mountinfoPath is where the kernel describes this process's mounts; a test
// writes its own.
var mountinfoPath = "/proc/self/mountinfo"

// mountEntry is one line of mountinfo: the mount's id and its mount point,
// with the kernel's octal escapes decoded.
type mountEntry struct {
	id    int
	point string
}

// readMountinfo parses mountinfo. The fields are space-separated; the mount
// point is the fifth, with spaces, tabs, newlines and backslashes escaped as
// \040, \011, \012 and \134.
func readMountinfo() ([]mountEntry, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", mountinfoPath, err)
	}

	defer func() { _ = f.Close() }()

	var entries []mountEntry

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), " ")
		if len(fields) < 5 {
			return nil, fmt.Errorf("%s: a line with %d fields is not mountinfo", mountinfoPath, len(fields))
		}

		id, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("%s: the mount id %q is not a number", mountinfoPath, fields[0])
		}

		point, err := unescapeMountField(fields[4])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", mountinfoPath, err)
		}

		entries = append(entries, mountEntry{id: id, point: point})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", mountinfoPath, err)
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("%s names no mounts", mountinfoPath)
	}

	return entries, nil
}

// unescapeMountField decodes the kernel's \ooo escapes.
func unescapeMountField(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}

	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])

			continue
		}

		if i+3 >= len(s) {
			return "", fmt.Errorf("a truncated escape in %q", s)
		}

		v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("the escape %q in %q is not octal", s[i:i+4], s)
		}

		b.WriteByte(byte(v))
		i += 3
	}

	return b.String(), nil
}

// mountOf answers the id of the mount holding path (already resolved) and
// whether path is itself a mount point: the entry with the longest mount
// point that is path or an ancestor of it, by components.
func mountOf(entries []mountEntry, path string) (int, bool, error) {
	best, bestLen, found := 0, -1, false

	for _, e := range entries {
		if !underOrEqual(e.point, path) {
			continue
		}

		if len(e.point) > bestLen {
			best, bestLen, found = e.id, len(e.point), true
		}
	}

	if !found {
		return 0, false, fmt.Errorf("no mount in %s holds %s", mountinfoPath, path)
	}

	return best, bestLen == len(path), nil
}

// underOrEqual says whether path is dir or lies under it, by components.
func underOrEqual(dir, path string) bool {
	if dir == path {
		return true
	}

	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}

	return strings.HasPrefix(path, dir+"/")
}

// maxWalkLinks is the kernel's symlink budget for one resolution.
const maxWalkLinks = 40

// walkTraverses answers whether resolving path from the root reaches the
// identity directory (resolved) or anything under it at ANY component,
// symlink targets included, and where it could not look. A component that is
// missing ends the walk with the remainder appended lexically; a component
// that cannot be examined for any other reason is could-not-tell. The
// identity directory is compared resolved, so a path spelled through a link
// to it is caught as well as one spelled through it.
func walkTraverses(path, identityDir string) (bool, string, error) {
	if !filepath.IsAbs(path) {
		return false, "", fmt.Errorf("%s is not absolute", path)
	}

	inside := func(p string) bool { return underOrEqual(identityDir, p) }

	remaining := splitComponents(path)
	cur := "/"
	links := 0

	for len(remaining) > 0 {
		comp := remaining[0]
		remaining = remaining[1:]

		switch comp {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)

			if inside(cur) {
				return true, "", nil
			}

			continue
		}

		next := filepath.Join(cur, comp)

		info, lerr := os.Lstat(next)

		switch {
		case errors.Is(lerr, fs.ErrNotExist):
			// The rest does not exist yet: its lexical shape is all there is,
			// and a missing component still under the identity directory would
			// be created there.
			rest := filepath.Join(append([]string{next}, remaining...)...)

			return inside(next) || inside(rest), "", nil
		case lerr != nil:
			return false, fmt.Sprintf("examine %s: %v", next, lerr), nil
		}

		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > maxWalkLinks {
				return false, "", fmt.Errorf("%s: more than %d symlinks on the way", path, maxWalkLinks)
			}

			target, rerr := os.Readlink(next)
			if rerr != nil {
				return false, fmt.Sprintf("read the link %s: %v", next, rerr), nil
			}

			if filepath.IsAbs(target) {
				cur = "/"
			}

			remaining = append(splitComponents(target), remaining...)

			continue
		}

		if inside(next) {
			return true, "", nil
		}

		if len(remaining) > 0 && !info.IsDir() {
			return false, "", fmt.Errorf("%s: %s is not a directory", path, next)
		}

		cur = next
	}

	return inside(cur), "", nil
}
