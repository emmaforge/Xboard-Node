package kernel

import (
	"net"
	"strings"
)

// NormalizeDeviceIP canonicalizes a source address for device counting.
//
// IPv4 addresses are returned unchanged (in canonical form). IPv6
// addresses are collapsed to their /64 network address: a single device
// with IPv6 privacy extensions rotates through many addresses inside one
// /64, and counting each full address as a separate device wrongly trips
// the device limit for ordinary IPv6 users. Collapsing to /64 matches how
// residential IPv6 is normally delegated (one /64 per household/device
// network) and keeps counting consistent across nodes, since the panel
// aggregates the same canonical values.
//
// The trade-off is deliberate: several genuinely distinct devices sharing
// one /64 (e.g. behind the same IPv6 prefix) count as a single device.
// That under-counts in shared-prefix environments, but the previous
// behaviour over-counted every IPv6 device, which is the worse failure
// for a limit meant to bound devices, not addresses.
//
// Unparseable input is returned unchanged so callers never lose the
// original value for logging or reporting.
func NormalizeDeviceIP(s string) string {
	s = strings.TrimSpace(s)
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}
