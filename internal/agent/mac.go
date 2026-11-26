package agent

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// MAC is a network device's hardware address: what a printer is, where its
// IP address is only where it was last seen.
type MAC [6]byte

// macSeparators are dropped before a MAC is read, so it can be written the
// way a label, a router or Windows shows it.
var macSeparators = strings.NewReplacer(":", "", "-", "", ".", "", " ", "")

// ParseMAC reads a MAC written with colons, dashes or no separators, in
// either case. Anything that is not twelve hex digits is refused, and so is
// the all-zero address: the system's tables use it for "no answer yet".
func ParseMAC(text string) (MAC, bool) {
	var mac MAC
	digits := macSeparators.Replace(text)
	if len(digits) != 2*len(mac) {
		return MAC{}, false
	}
	if _, err := hex.Decode(mac[:], []byte(digits)); err != nil {
		return MAC{}, false
	}
	if mac == (MAC{}) {
		return MAC{}, false
	}
	return mac, true
}

func (m MAC) String() string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", m[0], m[1], m[2], m[3], m[4], m[5])
}
