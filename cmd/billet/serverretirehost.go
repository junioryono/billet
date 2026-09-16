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
	"unicode"
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

// walkTraverses models a consumer which can create missing parents. It judges
// each intermediate component, including after '..' returns to an existing
// ancestor. It never creates the hypothetical directories itself.
func walkTraverses(path, identityDir string) (bool, string, error) {
	if !supportedRetirePath(path) {
		return false, "", fmt.Errorf("unsupported absolute path spelling %q", path)
	}
	inside := func(p string) bool { return underOrEqual(identityDir, p) }
	remaining := strings.Split(path, "/")
	cur, missing := "/", ""
	rootInfo, err := retireWalkLstat(cur)
	if err != nil || !rootInfo.IsDir() {
		return false, "filesystem root could not be observed", nil
	}
	observed := map[string]os.FileInfo{cur: rootInfo}
	consistent := func(name string, info os.FileInfo) bool {
		before, seen := observed[name]
		if !seen {
			observed[name] = info
			return true
		}
		if before == nil || info == nil {
			return before == nil && info == nil
		}
		return os.SameFile(before, info) && before.Mode() == info.Mode() &&
			(before.Mode()&os.ModeSymlink == 0 || before.ModTime().Equal(info.ModTime()))
	}
	links := 0
	for len(remaining) > 0 {
		comp := remaining[0]
		remaining = remaining[1:]
		switch comp {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			if missing != "" && !underOrEqual(missing, cur) {
				missing = ""
			}
			if inside(cur) {
				return true, "", nil
			}
			if missing == "" {
				info, err := retireWalkLstat(cur)
				if err != nil || !info.IsDir() || !consistent(cur, info) {
					return false, "inconsistent revisited component: " + cur, nil
				}
			}
			continue
		}
		next := filepath.Join(cur, comp)
		// The traversed name matters even when it is a link leading outside.
		if inside(next) {
			return true, "", nil
		}
		if missing != "" {
			cur = next
			continue
		}
		info, lerr := retireWalkLstat(next)
		switch {
		case errors.Is(lerr, fs.ErrNotExist):
			if !consistent(next, nil) {
				return false, "inconsistent revisited component: " + next, nil
			}
			// ENOENT is absence only while the observed parent still exists.
			parent, err := retireWalkLstat(cur)
			if err != nil || !parent.IsDir() || !consistent(cur, parent) {
				return false, "inconsistent missing-path parent: " + cur, nil
			}
			cur, missing = next, next
			continue
		case lerr != nil:
			return false, fmt.Sprintf("examine %s: %v", next, lerr), nil
		}
		if !consistent(next, info) {
			return false, "inconsistent revisited component: " + next, nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > maxWalkLinks {
				return false, fmt.Sprintf("%s: more than %d symlinks on the way", path, maxWalkLinks), nil
			}
			target, err := retireWalkReadlink(next)
			if err != nil {
				return false, fmt.Sprintf("read the link %s: %v", next, err), nil
			}
			if target == "" || strings.IndexFunc(target, unsafeRetirePathRune) >= 0 {
				return false, "", fmt.Errorf("unsupported symlink spelling at %s", next)
			}
			again, err := retireWalkLstat(next)
			if err != nil || !os.SameFile(info, again) || info.Mode() != again.Mode() || !info.ModTime().Equal(again.ModTime()) {
				return false, "link changed during observation: " + next, nil
			}
			if filepath.IsAbs(target) {
				cur = "/"
			}
			remaining = append(strings.Split(target, "/"), remaining...)
			continue
		}
		if len(remaining) > 0 && !info.IsDir() {
			return false, "", fmt.Errorf("%s: %s is not a directory", path, next)
		}
		cur = next
	}
	return inside(cur), "", nil
}

var retireWalkLstat = os.Lstat
var retireWalkReadlink = os.Readlink

func unsafeRetirePathRune(r rune) bool {
	return unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune(`%\"'`, r)
}

func supportedRetirePath(path string) bool {
	return filepath.IsAbs(path) && strings.IndexFunc(path, unsafeRetirePathRune) < 0
}
