//go:build windows

package agent

import (
	"context"
	"encoding/binary"
	"net/netip"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// How long to wait for the MAC behind an address the agent has just
// connected to. A device that completed a connection answers in milliseconds.
const neighbourTimeout = time.Second

var procSendARP = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("SendARP")

// probeMAC asks every address for its MAC with SendARP, all at once, and
// returns the first that answers with the wanted one.
//
// SendARP asks the network each time and does not answer from the system's
// table, so an old row cannot send the agent to where the printer was. It
// needs no administrator rights and no raw socket. A call for an address
// nobody has takes about three seconds to give up and cannot be interrupted,
// so each runs on its own and the stragglers are left to finish after this
// function has returned.
func probeMAC(ctx context.Context, mac MAC, probes []probe) ([]netip.Addr, error) {
	if err := procSendARP.Find(); err != nil {
		return nil, err
	}
	answers := make(chan netip.Addr, len(probes))
	for _, p := range probes {
		go func() {
			if got, ok := sendARP(p.from, p.addr); ok && got == mac {
				answers <- p.addr
			} else {
				answers <- netip.Addr{}
			}
		}()
	}
	for range probes {
		select {
		case <-ctx.Done():
			return nil, nil
		case addr := <-answers:
			if addr.IsValid() {
				return []netip.Addr{addr}, nil
			}
		}
	}
	return nil, nil
}

// neighbourMAC asks addr for its MAC from this computer's address from.
func neighbourMAC(ctx context.Context, from, addr netip.Addr) (MAC, bool) {
	if procSendARP.Find() != nil {
		return MAC{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, neighbourTimeout)
	defer cancel()
	type answer struct {
		mac MAC
		ok  bool
	}
	answered := make(chan answer, 1)
	go func() {
		mac, ok := sendARP(from, addr)
		answered <- answer{mac, ok}
	}()
	select {
	case <-ctx.Done():
		return MAC{}, false
	case a := <-answered:
		return a.mac, a.ok
	}
}

// sendARP calls iphlpapi's SendARP(DestIP, SrcIP, pMacAddr, PhyAddrLen). It
// blocks until the address answers or Windows gives up.
func sendARP(from, addr netip.Addr) (MAC, bool) {
	if !from.Is4() || !addr.Is4() {
		return MAC{}, false
	}
	// An IPAddr is the address's four bytes in network order, as they lie in
	// memory; Windows is little-endian everywhere it runs.
	dst, src := addr.As4(), from.As4()
	// Windows documents the buffer as two ULONGs, of which six bytes are
	// used for an Ethernet address.
	var buf [8]byte
	size := uint32(len(MAC{}))
	status, _, _ := procSendARP.Call(
		uintptr(binary.LittleEndian.Uint32(dst[:])),
		uintptr(binary.LittleEndian.Uint32(src[:])),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	var mac MAC
	if status != 0 || int(size) != len(mac) {
		return MAC{}, false
	}
	copy(mac[:], buf[:])
	if mac == (MAC{}) {
		return MAC{}, false
	}
	return mac, true
}
