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
	// MAC, when the server knows it, is what the printer is. Host is then
	// only where it was last seen, and the Finder looks the printer up when
	// it is no longer there.
	MAC string `json:"mac,omitempty"`
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
	if err := p.checkPort(); err != nil {
		return "", err
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(p.Port)), nil
}

func (p Printer) checkPort() error {
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("printer port %d is out of range", p.Port)
	}
	return nil
}

// at is the same printer at another address.
func (p Printer) at(addr netip.Addr) Printer {
	p.Host = addr.String()
	return p
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
	return writeJob(conn, addr, data, time.Now().Add(writeTimeout))
}

// writeJob writes a job to a printer that accepted the connection, and
// closes the connection.
func writeJob(conn net.Conn, addr string, data []byte, deadline time.Time) error {
	defer conn.Close()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("the printer at %s stopped accepting data: %w", addr, err)
	}
	return conn.Close()
}
