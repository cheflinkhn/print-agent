//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	arpTablePath = "/proc/net/arp"
	// The "discard" port. What is sent does not matter, only that the kernel
	// has to find out who is at the address before it can send it.
	sweepPort = 9
	// How often the table is read while the answers come in.
	arpPollEvery = 100 * time.Millisecond

	arpEthernet = 0x1 // HW type
	arpComplete = 0x2 // Flags: the row has an answer
)

// probeMAC sends one small UDP datagram to every address. That needs no
// rights, and it makes the kernel ask each address for its MAC and put the
// answers in its neighbour table, which is then read.
//
// The table also keeps rows from before, and an old row can say the printer
// is where it no longer is. So a row that this sweep produced is trusted
// over one that was already there: it ends the wait and comes first.
func probeMAC(ctx context.Context, mac MAC, probes []probe) ([]netip.Addr, error) {
	before, err := addressesOf(mac)
	if err != nil {
		return nil, err
	}
	for _, p := range probes {
		if ctx.Err() != nil {
			break
		}
		// A failed send is an address that cannot be asked; the others can.
		_ = sendDatagram(ctx, p.addr)
	}

	ticker := time.NewTicker(arpPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Nothing new answered. Rows from before are all there is.
			return addressesOf(mac)
		case <-ticker.C:
		}
		now, err := addressesOf(mac)
		if err != nil {
			return nil, err
		}
		var fresh, old []netip.Addr
		for _, addr := range now {
			if slices.Contains(before, addr) {
				old = append(old, addr)
			} else {
				fresh = append(fresh, addr)
			}
		}
		if len(fresh) > 0 {
			return append(fresh, old...), nil
		}
	}
}

// sendDatagram sends one byte to an address, from a socket of its own.
//
// The kernel holds a datagram until it knows who is at the address, which
// for an address nobody has is three seconds, and the datagram counts
// against its socket's send buffer all that time. One socket for the whole
// sweep fills up after some 220 addresses and stalls for those three
// seconds, with the rest of the network still to ask.
func sendDatagram(ctx context.Context, addr netip.Addr) error {
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, sweepPort)))
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	}
	_, err = conn.Write([]byte{0})
	return err
}

// neighbourMAC reads the row the kernel made when it connected to addr.
func neighbourMAC(_ context.Context, _, addr netip.Addr) (MAC, bool) {
	table, err := readARPTable()
	if err != nil {
		return MAC{}, false
	}
	mac, ok := table[addr]
	return mac, ok
}

func addressesOf(mac MAC) ([]netip.Addr, error) {
	table, err := readARPTable()
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	for addr, m := range table {
		if m == mac {
			addrs = append(addrs, addr)
		}
	}
	slices.SortFunc(addrs, netip.Addr.Compare)
	return addrs, nil
}

func readARPTable() (map[netip.Addr]MAC, error) {
	raw, err := os.ReadFile(arpTablePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read the neighbour table: %w", err)
	}
	return parseARPTable(raw), nil
}

// parseARPTable reads /proc/net/arp, keeping the rows that have an answer:
//
//	IP address       HW type     Flags       HW address            Mask     Device
//	192.168.1.37     0x1         0x2         84:72:07:3c:50:65     *        wlan0
//	192.168.1.60     0x1         0x0         00:00:00:00:00:00     *        wlan0
func parseARPTable(raw []byte) map[netip.Addr]MAC {
	table := map[netip.Addr]MAC{}
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) < 4 {
			continue
		}
		addr, err := netip.ParseAddr(fields[0])
		if err != nil {
			continue // the header
		}
		hwType, err1 := strconv.ParseUint(fields[1], 0, 32)
		flags, err2 := strconv.ParseUint(fields[2], 0, 32)
		if err1 != nil || err2 != nil || hwType != arpEthernet || flags&arpComplete == 0 {
			continue
		}
		if mac, ok := ParseMAC(fields[3]); ok {
			table[addr] = mac
		}
	}
	return table
}
