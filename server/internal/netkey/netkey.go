// Package netkey turns client addresses into keys for per-address limits.
package netkey

import "net"

// Of is the limiter key for an IP address: IPv4 addresses as they are,
// IPv6 by their /64, since one household or server usually has a whole
// /64. Anything that is not an IP address is "unknown".
func Of(addr string) string {
	ip := net.ParseIP(addr)
	switch {
	case ip == nil:
		return "unknown"
	case ip.To4() != nil:
		return ip.String()
	default:
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
}

// Public reports whether ip is a global unicast address outside the
// private, loopback, link-local and other special ranges: somewhere on the
// internet.
func Public(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !special(ip)
}

var specialNets = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, s := range []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // carrier-grade NAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved
		"64:ff9b:1::/48",  // local NAT64
		"2001:db8::/32",   // documentation
	} {
		_, n, _ := net.ParseCIDR(s)
		nets = append(nets, n)
	}
	return nets
}()

func special(ip net.IP) bool {
	for _, n := range specialNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
