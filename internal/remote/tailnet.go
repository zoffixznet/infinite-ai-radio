// Package remote implements the built-in phone remote: a small HTTP
// server (off by default) that serves a phone-first control page, a live
// MP3 stream of the mastered output, and control endpoints wired through
// the same controller the terminal UI uses. By default it binds only to
// localhost and, when present, the machine's Tailscale address, so the
// private tailnet is the trust boundary.
package remote

import (
	"net"
	"net/netip"
	"strconv"
)

// tailnetPrefix is the CGNAT range Tailscale assigns addresses from.
var tailnetPrefix = netip.MustParsePrefix("100.64.0.0/10")

// TailnetAddr returns the machine's Tailscale IPv4 address, or ok=false
// when no tailnet interface exists.
func TailnetAddr() (string, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", false
	}
	return tailnetAddrIn(addrs)
}

// tailnetAddrIn scans an address list for a Tailscale-range IPv4 address.
// Split out so tests can feed fake interface lists.
func tailnetAddrIn(addrs []net.Addr) (string, bool) {
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
		ip4 := ip.To4()
		if ip4 == nil {
			continue
		}
		addr, ok := netip.AddrFromSlice(ip4)
		if !ok {
			continue
		}
		if tailnetPrefix.Contains(addr) {
			return addr.String(), true
		}
	}
	return "", false
}

// BindAddrs resolves the listen addresses: an explicit override wins;
// otherwise localhost plus the tailnet address when present. tailnetIP is
// the detected address ("" when absent).
func BindAddrs(override string, port int) (addrs []string, tailnetIP string) {
	if override != "" {
		return []string{net.JoinHostPort(override, strconv.Itoa(port))}, ""
	}
	out := []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	if ip, ok := TailnetAddr(); ok {
		out = append(out, net.JoinHostPort(ip, strconv.Itoa(port)))
		tailnetIP = ip
	}
	return out, tailnetIP
}
