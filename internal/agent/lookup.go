package agent

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
)

const (
	// A sweep asks at most this many networks, and at most 254 addresses on
	// each. Every address asked becomes a row in the system's neighbour
	// table for a few seconds. That table is small (1024 rows on Linux) and
	// everything else on the computer needs room in it too.
	maxSweptNetworks = 3
	sweepBits        = 24
)

// ErrNoNetwork means the computer has no address on a private network, so
// there is nowhere to look for a printer.
var ErrNoNetwork = errors.New("this computer is not connected to a network")

// localNet is one private IPv4 network this computer is directly on.
type localNet struct {
	// self is this computer's address on it.
	self netip.Addr
	// prefix is the whole network, as the interface is configured.
	prefix netip.Prefix
	// sweep is the part a lookup asks: all of it, or the 254 addresses
	// around self when the network is larger.
	sweep netip.Prefix
}

// probe is one address to ask, and the address of this computer to ask from.
type probe struct {
	addr, from netip.Addr
}

// The lookup has one small file per system (lookup_linux.go,
// lookup_windows.go, lookup_other.go), each with these two functions:
//
// probeMAC asks every probe who is there and returns the addresses that
// answer with mac, the most trustworthy first. It returns when it has an
// answer or when ctx ends, whichever is first.
//
// neighbourMAC returns the MAC behind an address on one of this computer's
// networks, or false when the system cannot tell.

// localNetworks lists the private IPv4 networks of the interfaces that are
// up, in the system's order.
func localNetworks() ([]localNet, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var nets []localNet
	for _, iface := range interfaces {
		const wanted = net.FlagUp | net.FlagRunning
		if iface.Flags&wanted != wanted || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipNet.IP)
			ones, bits := ipNet.Mask.Size()
			if !ok || bits != 32 {
				continue
			}
			if n, ok := newLocalNet(netip.PrefixFrom(ip.Unmap(), ones)); ok {
				nets = append(nets, n)
			}
		}
	}
	return nets, nil
}

// newLocalNet describes the network an interface address is on. It refuses
// what a lookup must not touch: anything that is not a private IPv4 network
// with room for another device.
func newLocalNet(address netip.Prefix) (localNet, bool) {
	self := address.Addr()
	if !self.Is4() || !self.IsPrivate() || address.Bits() > 30 {
		return localNet{}, false
	}
	n := localNet{self: self, prefix: address.Masked(), sweep: address.Masked()}
	if address.Bits() < sweepBits {
		n.sweep = netip.PrefixFrom(self, sweepBits).Masked()
	}
	return n, true
}

// probes lists the addresses to ask on this network: every one in the swept
// part but its first (the network), its last (the broadcast) and this
// computer's own.
func (n localNet) probes() []probe {
	var out []probe
	// Stopping while the next address is still inside leaves out the last.
	for addr := n.sweep.Addr().Next(); n.sweep.Contains(addr.Next()); addr = addr.Next() {
		if addr != n.self {
			out = append(out, probe{addr: addr, from: n.self})
		}
	}
	return out
}

// Lookup is what looking for a MAC found.
type Lookup struct {
	// Found are the addresses that answer with the MAC, the most
	// trustworthy first. It is usually one, or none.
	Found []netip.Addr
	// Searched are the networks that were asked.
	Searched []netip.Prefix
}

// LookupMAC finds where a MAC is now on the private networks this computer
// is directly on. It needs no special rights. ctx bounds how long it looks;
// a printer that is there answers in about a second.
func LookupMAC(ctx context.Context, mac MAC) (Lookup, error) {
	nets, err := localNetworks()
	if err != nil {
		return Lookup{}, err
	}
	if len(nets) == 0 {
		return Lookup{}, ErrNoNetwork
	}

	var result Lookup
	var probes []probe
	for _, n := range nets {
		if slices.Contains(result.Searched, n.sweep) {
			continue
		}
		if len(result.Searched) == maxSweptNetworks {
			break
		}
		result.Searched = append(result.Searched, n.sweep)
		probes = append(probes, n.probes()...)
	}

	found, err := probeMAC(ctx, mac, probes)
	if err != nil {
		return result, err
	}
	// The system's table can hold more than was asked for. Keep only what is
	// on one of this computer's own private networks.
	for _, addr := range found {
		onOwnNetwork := slices.ContainsFunc(nets, func(n localNet) bool {
			return n.prefix.Contains(addr) && addr != n.self
		})
		if onOwnNetwork && !slices.Contains(result.Found, addr) {
			result.Found = append(result.Found, addr)
		}
	}
	return result, nil
}

// MACAt returns the MAC of the device behind an address this computer has
// just talked to. It answers false when that cannot be known: the address is
// this computer, is behind a router, or the system does not say.
func MACAt(ctx context.Context, addr netip.Addr) (MAC, bool) {
	nets, err := localNetworks()
	if err != nil {
		return MAC{}, false
	}
	for _, n := range nets {
		if n.prefix.Contains(addr) && addr != n.self {
			return neighbourMAC(ctx, n.self, addr)
		}
	}
	return MAC{}, false
}
