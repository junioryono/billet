//go:build !darwin

package usage

// hostNet reads a device's received and sent bytes and packets from
// /sys/class/net, which is where a Linux host keeps them.
func (r Reader) hostNet(t Target) ([4]int64, bool) { return r.sysfsNet(t) }
