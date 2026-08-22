// Package remote implements the built-in phone remote: a small HTTP
// server (off by default) that serves a phone-first control page, a live
// MP3 stream of the mastered output, a player for saved chunks, and
// control endpoints wired through the same controller the terminal UI
// uses. Every request needs a logged-in account (see package accounts).
// By default it binds only to localhost and, when present, the machine's
// Tailscale address.
package remote

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// tailnetPrefix is the CGNAT range Tailscale assigns addresses from.
var tailnetPrefix = netip.MustParsePrefix("100.64.0.0/10")

// interfaceAddrs is swappable so tests can feed fake interface lists.
var interfaceAddrs = net.InterfaceAddrs

// TailnetAddr returns the machine's Tailscale IPv4 address, or ok=false
// when no tailnet interface exists.
func TailnetAddr() (string, bool) {
	addrs, err := interfaceAddrs()
	if err != nil {
		return "", false
	}
	return tailnetAddrIn(addrs)
}

// MachineAddrs lists every IP address of the machine's interfaces (used
// to auto-allow Host values when binding a wildcard).
func MachineAddrs() []string {
	addrs, err := interfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
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
		out = append(out, ip.String())
	}
	return out
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

// Binding is the resolved listen plan: which addresses to listen on and
// which host names those imply for the allowlist.
type Binding struct {
	// Addrs are host:port listen addresses.
	Addrs []string
	// TailnetIP is the detected Tailscale address ("" when absent).
	TailnetIP string
	// ExtraHosts are hostnames/IPs implied by the binding that clients
	// may legitimately use in URLs (explicit bind entries; with a
	// wildcard, every machine interface address).
	ExtraHosts []string
	// Exposed reports whether any bound address is reachable beyond
	// loopback and the tailnet.
	Exposed bool
}

// ResolveBinding turns the configured extra binds into the listen plan.
// Binding is additive: localhost is always kept and the tailnet address
// is always detected and bound when present. A wildcard entry replaces
// the individual listens (it already covers them all).
func ResolveBinding(binds []string, port int) Binding {
	var b Binding
	if ip, ok := TailnetAddr(); ok {
		b.TailnetIP = ip
	}
	seen := map[string]bool{}
	add := func(host string) {
		if host != "" && !seen[host] {
			seen[host] = true
			b.Addrs = append(b.Addrs, net.JoinHostPort(host, strconv.Itoa(port)))
		}
	}
	wildcard := false
	for _, raw := range binds {
		host := strings.TrimSpace(raw)
		if host == "0.0.0.0" || host == "::" || host == "*" {
			wildcard = true
			continue
		}
		if host == "" {
			continue
		}
		if !isLoopbackHost(host) {
			b.Exposed = true
		}
		b.ExtraHosts = append(b.ExtraHosts, host)
	}
	if wildcard {
		b.Exposed = true
		b.Addrs = []string{net.JoinHostPort("0.0.0.0", strconv.Itoa(port))}
		b.ExtraHosts = append(b.ExtraHosts, MachineAddrs()...)
		return b
	}
	add("127.0.0.1")
	add(b.TailnetIP)
	for _, h := range b.ExtraHosts {
		add(h)
	}
	return b
}

// isLoopbackHost reports whether a bind entry stays on the local machine.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
