package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// The server offers a job again when no result arrives in 30 seconds, and a
// second agent would then print the same chit. So one attempt at a job that
// names its printer by MAC is held to the 25 seconds a job without one can
// take (5 to connect, 20 to write), whatever it has to do to find the
// printer:
//
//	the address that worked last time   up to 3 s
//	the address the server gave         up to 3 s
//	looking the MAC up                  up to 3 s
//	the address the lookup found        up to 3 s
//	                                    ---------
//	finding and connecting              at most 12 s
//	writing the job                     what is left: at least 13 s
const (
	attemptTimeout = 25 * time.Second
	findTimeout    = 12 * time.Second
	macDialTimeout = 3 * time.Second
	lookupTimeout  = 3 * time.Second

	// A printer that is switched off fails every job, and the agent retries
	// every few seconds. It is looked for this often, not every time.
	sweepEvery = 30 * time.Second

	// How many printers' addresses are remembered. A venue has a handful.
	maxRemembered = 64
)

type limits struct {
	attempt, find, dial, lookup time.Duration
}

// Finder sends jobs to printers, and finds a printer again when its address
// has changed.
//
// A router hands a printer its address and can hand it another. When a job
// carries the printer's MAC, the address is only a hint: the Finder tries
// it, and if the printer is not there it looks the MAC up on the computer's
// own networks and prints at the address it is at now. A job with no MAC is
// sent to its address and nowhere else.
//
// A Finder is used from one goroutine.
type Finder struct {
	Log *log.Logger

	// How the Finder reaches the venue's network; replaced in tests.
	dial   func(ctx context.Context, addr string) (net.Conn, error)
	lookup func(ctx context.Context, mac MAC) (Lookup, error)
	macAt  func(ctx context.Context, addr netip.Addr) (MAC, bool)
	now    func() time.Time
	limits limits

	// known is where each printer last printed. It is tried first, so a
	// changed address costs one lookup and not one per chit. In memory is
	// enough: after a restart the server's hint is the new address, or one
	// lookup finds it again.
	known map[MAC]netip.Addr
	// sweeps is the last lookup for each printer, reused for sweepEvery.
	sweeps map[MAC]sweep
	// badMAC is the last unusable MAC logged, so it is logged once.
	badMAC string
}

type sweep struct {
	at     time.Time
	result Lookup
}

// NewFinder builds a Finder that uses this computer's network.
func NewFinder(logger *log.Logger) *Finder {
	return &Finder{
		Log: logger,
		dial: func(ctx context.Context, addr string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", addr)
		},
		lookup: LookupMAC,
		macAt:  MACAt,
		now:    time.Now,
		limits: limits{attempt: attemptTimeout, find: findTimeout, dial: macDialTimeout, lookup: lookupTimeout},
		known:  map[MAC]netip.Addr{},
		sweeps: map[MAC]sweep{},
	}
}

// Send writes a job to its printer. For a job that carries the printer's
// MAC it returns the address the job printed on, which may not be the one
// the job gave; for any other job it returns "".
func (f *Finder) Send(ctx context.Context, p Printer, data []byte) (host string, err error) {
	if p.MAC == "" {
		return "", SendToPrinter(ctx, p, data)
	}
	mac, ok := ParseMAC(p.MAC)
	if !ok {
		// A bad MAC must not cost a chit: print by address, as without one.
		if p.MAC != f.badMAC {
			f.badMAC = p.MAC
			f.Log.Printf("ignoring the printer's MAC %q, which is not twelve hex digits; printing by its address only", p.MAC)
		}
		return "", SendToPrinter(ctx, p, data)
	}

	// No address can take a job for a port that does not exist, so do not
	// go looking for one.
	if err := p.checkPort(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, f.limits.attempt)
	defer cancel()
	conn, addr, err := f.connect(ctx, p, mac)
	if err != nil {
		return "", err
	}
	deadline, _ := ctx.Deadline()
	if limit := time.Now().Add(writeTimeout); limit.Before(deadline) {
		deadline = limit
	}
	if err := writeJob(conn, p.at(addr).String(), data, deadline); err != nil {
		return "", err
	}
	f.remember(mac, p, addr)
	return addr.String(), nil
}

// attempt is one address that was tried and did not take the job.
type attempt struct {
	addr netip.Addr
	// err is the whole sentence; cause is only why the connection failed.
	err, cause error
	// other is the MAC that answered, when it was another device's.
	other MAC
}

// connect returns a connection to the printer with this MAC, and the
// address it is at.
func (f *Finder) connect(ctx context.Context, p Printer, mac MAC) (net.Conn, netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, f.limits.find)
	defer cancel()

	var failed []attempt
	try := func(addr netip.Addr) net.Conn {
		if slices.ContainsFunc(failed, func(a attempt) bool { return a.addr == addr }) {
			return nil
		}
		conn, failure := f.connectTo(ctx, p, addr, mac)
		if failure != nil {
			failed = append(failed, *failure)
		}
		return conn
	}

	// Where it printed last time, then where the server says it is.
	if addr, ok := f.known[mac]; ok {
		if conn := try(addr); conn != nil {
			return conn, addr, nil
		}
	}
	hint, hintErr := netip.ParseAddr(p.Host)
	if hintErr == nil {
		if conn := try(hint); conn != nil {
			return conn, hint, nil
		}
	}

	// It is at neither. Find where it is now.
	found, lookupErr := f.sweep(ctx, mac)
	for _, addr := range found.Found {
		if conn := try(addr); conn != nil {
			return conn, addr, nil
		}
	}
	return nil, netip.Addr{}, explain(p, mac, failed, found, lookupErr)
}

// connectTo opens the printer's port at one address and checks who answered.
//
// An address that used to be the printer's can be handed to another device.
// When the system can tell whose MAC is behind the address and it is not the
// printer's, the connection is dropped unused and that MAC is returned. When
// it cannot tell, the connection is used: losing a chit is worse than the
// small risk of a wrong device.
func (f *Finder) connectTo(ctx context.Context, p Printer, addr netip.Addr, mac MAC) (net.Conn, *attempt) {
	// The same check as for any job: only a private address is ever dialled,
	// whether the server gave it or the lookup found it.
	target, err := p.at(addr).Addr()
	if err != nil {
		return nil, &attempt{addr: addr, err: err, cause: err}
	}
	dialCtx, cancel := context.WithTimeout(ctx, f.limits.dial)
	defer cancel()
	conn, err := f.dial(dialCtx, target)
	if err != nil {
		return nil, &attempt{addr: addr, err: fmt.Errorf("cannot reach the printer at %s: %w", target, err), cause: err}
	}
	if other, ok := f.macAt(ctx, addr); ok && other != mac {
		conn.Close()
		err := fmt.Errorf("%s is now another device (%s), not printer %s", addr, other, mac)
		return nil, &attempt{addr: addr, err: err, cause: err, other: other}
	}
	return conn, nil
}

// sweep looks the MAC up, at most once every sweepEvery. In between it
// answers what the last lookup found.
func (f *Finder) sweep(ctx context.Context, mac MAC) (Lookup, error) {
	if last, ok := f.sweeps[mac]; ok && f.now().Sub(last.at) < sweepEvery {
		return last.result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, f.limits.lookup)
	defer cancel()
	result, err := f.lookup(ctx, mac)
	if err != nil {
		// Nothing was asked of the network, so there is nothing to space out.
		return result, err
	}
	if len(f.sweeps) >= maxRemembered {
		clear(f.sweeps)
	}
	f.sweeps[mac] = sweep{at: f.now(), result: result}
	return result, nil
}

// remember notes where the printer printed, and says so once when that is
// not where it was.
func (f *Finder) remember(mac MAC, p Printer, addr netip.Addr) {
	previous, ok := f.known[mac]
	if !ok {
		previous, _ = netip.ParseAddr(p.Host)
	}
	switch {
	case previous == addr:
	case previous.IsValid():
		f.Log.Printf("printer %s moved from %s to %s", mac, previous, addr)
	default:
		f.Log.Printf("printer %s found at %s", mac, addr)
	}
	if len(f.known) >= maxRemembered {
		clear(f.known)
	}
	f.known[mac] = addr
}

// explain turns a failed search into the one sentence the server is told.
// Cheflink tells the venue's staff what to do from it, so the cases that
// call for different actions read differently.
func explain(p Printer, mac MAC, failed []attempt, found Lookup, lookupErr error) error {
	switch {
	case errors.Is(lookupErr, ErrNoNetwork):
		return ErrNoNetwork
	case lookupErr != nil:
		// The lookup could not be done at all. Say what the address did, as
		// for a job with no MAC.
		if len(failed) > 0 {
			return fmt.Errorf("%w; and printer %s could not be looked up: %v", failed[0].err, mac, lookupErr)
		}
		return fmt.Errorf("printer %s could not be looked up: %w", mac, lookupErr)
	}

	// The MAC is on the network, at an address that did not take the job.
	for _, addr := range found.Found {
		for _, a := range failed {
			if a.addr == addr && a.other == (MAC{}) {
				return fmt.Errorf("printer %s is at %s and refuses connections on port %d: %w", mac, addr, p.Port, a.cause)
			}
		}
	}

	// The MAC is nowhere this computer can see.
	networks := make([]string, len(found.Searched))
	for i, network := range found.Searched {
		networks[i] = network.String()
	}
	message := fmt.Sprintf("printer %s is not on this computer's network (looked on %s)", mac, strings.Join(networks, ", "))
	for _, a := range failed {
		if a.other != (MAC{}) {
			message += fmt.Sprintf("; %s is now another device (%s)", a.addr, a.other)
		}
	}
	return errors.New(message)
}
