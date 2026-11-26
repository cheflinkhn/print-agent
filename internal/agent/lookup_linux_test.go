//go:build linux

package agent

import (
	"net/netip"
	"testing"
)

func TestParseARPTable(t *testing.T) {
	const table = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.37     0x1         0x2         84:72:07:3c:50:65     *        wlan0
192.168.1.1      0x1         0x2         dc:a6:33:01:13:05     *        wlan0
192.168.1.60     0x1         0x0         00:00:00:00:00:00     *        wlan0
192.168.1.61     0x1         0x0         84:72:07:3c:50:65     *        wlan0
192.168.1.62     0x20        0x2         84:72:07:3c:50:65     *        ib0
192.168.1.63     0x1         0x6         aa:bb:cc:dd:ee:ff     *        wlan0
a short line
`
	got := parseARPTable([]byte(table))

	want := map[string]string{
		"192.168.1.37": "84:72:07:3C:50:65",
		"192.168.1.1":  "DC:A6:33:01:13:05",
		// Complete and permanent: a row someone typed in still counts.
		"192.168.1.63": "AA:BB:CC:DD:EE:FF",
	}
	if len(got) != len(want) {
		t.Errorf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for addr, mac := range want {
		if got := got[netip.MustParseAddr(addr)].String(); got != mac {
			t.Errorf("%s = %s, want %s", addr, got, mac)
		}
	}
}
