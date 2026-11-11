package agent

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
)

const (
	// MaxJobBytes caps one job. A text chit is a few kilobytes.
	MaxJobBytes = 1 << 20

	dialTimeout  = 5 * time.Second
	writeTimeout = 20 * time.Second
)

// Printer is where a job goes: a raw TCP port on the venue's network.
type Printer struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Addr checks the printer's address and returns it as host:port.
//
// The address comes from the server, and the agent writes whatever bytes it
// is given to it. Only an IP on a private network (or this machine) is
// accepted, so a wrong or hostile job cannot make the agent talk to the
// internet.
func (p Printer) Addr() (string, error) {
	ip, err := netip.ParseAddr(p.Host)
	if err != nil {
		return "", fmt.Errorf("printer address %q is not an IP address", p.Host)
	}
	if !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
		return "", fmt.Errorf("printer address %s is not on a private network", ip)
	}
	if p.Port < 1 || p.Port > 65535 {
		return "", fmt.Errorf("printer port %d is out of range", p.Port)
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(p.Port)), nil
}

func (p Printer) String() string {
	return net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

// SendToPrinter opens the printer's port, writes the job and closes.
//
// A nil error means the printer's network side accepted every byte. A raw
// port gives no receipt: a printer that is out of paper may still accept the
// job and print it once paper is loaded.
func SendToPrinter(ctx context.Context, p Printer, data []byte) error {
	addr, err := p.Addr()
	if err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("cannot reach the printer at %s: %w", addr, err)
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("the printer at %s stopped accepting data: %w", addr, err)
	}
	return conn.Close()
}
