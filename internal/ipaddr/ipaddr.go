// Package ipaddr is the single place where Ship parses, canonicalizes, and
// compares IP addresses for host pins, provider listings, and host facts.
//
// Addresses are parsed with net/netip. IPv4-mapped IPv6 addresses
// (::ffff:203.0.113.10) are unmapped to plain IPv4, so the two spellings
// compare equal. Zoned addresses (fe80::1%eth0) stay strict: the zone is kept
// and must match exactly, and a zoned address is never unmapped.
//
// Canonical forms are for comparison only. Ship never rewrites the address it
// stores in host facts or connects to: SSH known_hosts entries are keyed by
// the exact spelling, so changing it would break strict host-key checking.
package ipaddr

import (
	"net/netip"
	"strings"
)

// Parse parses value as an IP address and canonicalizes it.
func Parse(value string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return netip.Addr{}, false
	}
	if addr.Zone() == "" {
		addr = addr.Unmap()
	}
	return addr, true
}

// Is reports whether value is an IPv4 or IPv6 address.
func Is(value string) bool {
	_, ok := Parse(value)
	return ok
}

// Canonical returns the canonical spelling of an IP address (lowercase,
// shortest IPv6 form, IPv4-mapped addresses unmapped). ok is false when value
// is not an IP address.
func Canonical(value string) (string, bool) {
	addr, ok := Parse(value)
	if !ok {
		return "", false
	}
	return addr.String(), true
}

// Equal reports whether a and b are the same IP address. Values that are not
// IP addresses are never equal to anything.
func Equal(a, b string) bool {
	left, ok := Parse(a)
	if !ok {
		return false
	}
	right, ok := Parse(b)
	if !ok {
		return false
	}
	return left == right
}
