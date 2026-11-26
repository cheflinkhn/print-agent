package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// The printer in these tests, the way a label shows it and the way the
	// agent writes it.
	printerMAC     = "aa-bb-cc-dd-ee-ff"
	printerMACText = "AA:BB:CC:DD:EE:FF"
	otherMAC       = "11:22:33:44:55:66"

	// Where the server thinks the printer is, and where it has moved to.
	oldAddr = "192.168.1.50"
	newAddr = "192.168.1.37"

	notOnTheNetwork = "printer " + printerMACText + " is not on this computer's network (looked on 192.168.1.0/24)"
)

// fakeNetwork is a venue's network as the Finder sees it: which device is at
// which address. It stands in for dialling, for the lookup and for reading
// the MAC behind an address.
type fakeNetwork struct {
	t *testing.T

	mu      sync.Mutex
	devices map[string]*fakeDevice
	// dialled are the addresses connected to, in order; sweeps counts the
	// lookups.
	dialled []string
	sweeps  int
	// offline makes the computer have no network at all.
	offline bool
	// anonymous makes the MAC behind every address unknowable, as for a
	// printer behind a router.
	anonymous bool
}

type fakeDevice struct {
	mac MAC
	// printer accepts connections for the device; nil refuses them.
	printer *fakePrinter
}

func newFakeNetwork(t *testing.T) *fakeNetwork {
	return &fakeNetwork{t: t, devices: map[string]*fakeDevice{}}
}

func (n *fakeNetwork) attach(f *Finder) {
	f.dial, f.lookup, f.macAt = n.dial, n.lookup, n.macAt
}

// put places a device at an address. accepts says whether its port is open.
func (n *fakeNetwork) put(addr, mac string, accepts bool) *fakePrinter {
	n.t.Helper()
	parsed, ok := ParseMAC(mac)
	if !ok {
		n.t.Fatalf("bad MAC %q", mac)
	}
	device := &fakeDevice{mac: parsed}
	if accepts {
		device.printer = newFakePrinter(n.t)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.devices[addr] = device
	return device.printer
}

func (n *fakeNetwork) remove(addr string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.devices, addr)
}

func (n *fakeNetwork) dial(ctx context.Context, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.dialled = append(n.dialled, host)
	device := n.devices[host]
	n.mu.Unlock()
	if device == nil || device.printer == nil {
		return nil, errors.New("connection refused")
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", device.printer.listener.Addr().String())
}

func (n *fakeNetwork) lookup(_ context.Context, mac MAC) (Lookup, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.offline {
		return Lookup{}, ErrNoNetwork
	}
	n.sweeps++
	result := Lookup{Searched: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}}
	for addr, device := range n.devices {
		if device.mac == mac {
			result.Found = append(result.Found, netip.MustParseAddr(addr))
		}
	}
	slices.SortFunc(result.Found, netip.Addr.Compare)
	return result, nil
}

func (n *fakeNetwork) macAt(_ context.Context, addr netip.Addr) (MAC, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	device := n.devices[addr.String()]
	if device == nil || n.anonymous {
		return MAC{}, false
	}
	return device.mac, true
}

func (n *fakeNetwork) dials() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.dialled...)
}

func (n *fakeNetwork) swept() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sweeps
}

// macJob is a job for the printer with printerMAC, which the server believes
// to be at host.
func macJob(id, host string) *Job {
	return &Job{
		ID:      id,
		Label:   "Table 4",
		Printer: Printer{Host: host, Port: 9100, MAC: printerMAC},
		Data:    base64.StdEncoding.EncodeToString([]byte("one chit")),
	}
}

func equal(got []string, want ...string) bool {
	return slices.Equal(got, want)
}

func TestAPrinterWhoseAddressChangedIsFoundByItsMAC(t *testing.T) {
	h := newHarness(t)
	printer := h.network.put(newAddr, printerMAC, true)
	h.server.queue = []*Job{macJob("a1", oldAddr), macJob("a2", oldAddr)}

	h.agent.step(context.Background())

	printer.printed(t, 1)
	if got := h.server.reported(); !equal(got, "a1:printed@"+newAddr) {
		t.Errorf("reports = %v, want the job printed and the address it printed on", got)
	}
	if got := h.network.dials(); !equal(got, oldAddr, newAddr) {
		t.Errorf("dialled %v, want the hint first and then where the printer is", got)
	}
	moved := "printer " + printerMACText + " moved from " + oldAddr + " to " + newAddr
	if !strings.Contains(h.log.String(), moved) {
		t.Errorf("the log does not say %q:\n%s", moved, h.log.String())
	}

	// The server has not caught up and still sends the old address. The
	// agent remembers: no second lookup, no dial of the old address, and
	// the move is not logged again.
	h.agent.step(context.Background())

	printer.printed(t, 1)
	if got := h.server.reported(); !equal(got, "a1:printed@"+newAddr, "a2:printed@"+newAddr) {
		t.Errorf("reports = %v", got)
	}
	if got := h.network.dials(); !equal(got, oldAddr, newAddr, newAddr) {
		t.Errorf("dialled %v, want the remembered address tried first", got)
	}
	if n := h.network.swept(); n != 1 {
		t.Errorf("looked the printer up %d times, want once", n)
	}
	if n := strings.Count(h.log.String(), "moved from"); n != 1 {
		t.Errorf("the move was logged %d times, want once:\n%s", n, h.log.String())
	}
}

func TestAPrinterAtItsAddressPrintsWithoutALookup(t *testing.T) {
	h := newHarness(t)
	printer := h.network.put(oldAddr, printerMAC, true)
	h.server.queue = []*Job{macJob("a1", oldAddr)}

	h.agent.step(context.Background())

	printer.printed(t, 1)
	// The address is reported even though it did not change: the server
	// decides whether to save it.
	if got := h.server.reported(); !equal(got, "a1:printed@"+oldAddr) {
		t.Errorf("reports = %v", got)
	}
	if n := h.network.swept(); n != 0 {
		t.Errorf("looked the printer up %d times, want none", n)
	}
	if strings.Contains(h.log.String(), "moved") {
		t.Errorf("nothing moved:\n%s", h.log.String())
	}
}

// A bad MAC must not cost a chit: the job prints by its address, exactly as
// a job with no MAC, and the log says so once.
func TestAMalformedMACIsIgnoredAndLoggedOnce(t *testing.T) {
	h := newHarness(t)
	first, second := h.job("a1", "one chit"), h.job("a2", "another")
	first.Printer.MAC, second.Printer.MAC = "AA:BB:CC", "AA:BB:CC"
	h.server.queue = []*Job{first, second}

	h.agent.step(context.Background())
	h.agent.step(context.Background())

	h.printer.printed(t, 2)
	if got := h.server.reported(); !equal(got, "a1:printed", "a2:printed") {
		t.Errorf("reports = %v, want both printed with no address", got)
	}
	if n := strings.Count(h.log.String(), "ignoring the printer's MAC"); n != 1 {
		t.Errorf("the bad MAC was logged %d times, want once:\n%s", n, h.log.String())
	}
	if n := h.network.swept(); n != 0 {
		t.Errorf("looked a printer up %d times, want none", n)
	}
}

// A printer left switched off fails every few seconds. It is looked for at
// most once every 30 seconds, and every failure names the MAC and the
// networks searched.
func TestAPrinterThatIsOffIsNotLookedForInALoop(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	h.agent.finder.now = func() time.Time { return now }
	h.server.queue = []*Job{macJob("a1", oldAddr), macJob("a1", oldAddr), macJob("a1", oldAddr), macJob("a1", oldAddr)}

	for _, wait := range []time.Duration{5 * time.Second, 5 * time.Second, 21 * time.Second, 0} {
		if got := h.agent.step(context.Background()); got != failedPrintWait {
			t.Errorf("wait = %v, want %v", got, failedPrintWait)
		}
		now = now.Add(wait)
	}

	// Attempts at 0, 5, 10 and 31 seconds: lookups at 0 and 31.
	if n := h.network.swept(); n != 2 {
		t.Errorf("looked the printer up %d times in 31 seconds, want 2", n)
	}
	got := h.server.reported()
	if len(got) != 4 {
		t.Fatalf("reports = %v", got)
	}
	for _, report := range got {
		if report != "a1:failed:"+notOnTheNetwork {
			t.Errorf("report = %q, want %q", report, "a1:failed:"+notOnTheNetwork)
		}
	}
	if n := strings.Count(h.log.String(), "did not print"); n != 1 {
		t.Errorf("the failure was logged %d times, want once:\n%s", n, h.log.String())
	}
}

// The printer's old address now belongs to another device that also listens
// on the port. It must get nothing; the chit goes to the printer.
func TestAnAddressTakenByAnotherDeviceIsNotWrittenTo(t *testing.T) {
	h := newHarness(t)
	other := h.network.put(oldAddr, otherMAC, true)
	printer := h.network.put(newAddr, printerMAC, true)
	h.server.queue = []*Job{macJob("a1", oldAddr)}

	h.agent.step(context.Background())

	printer.printed(t, 1)
	if got := h.server.reported(); !equal(got, "a1:printed@"+newAddr) {
		t.Errorf("reports = %v", got)
	}
	// A connection was opened and dropped; the device read nothing from it.
	if jobs := other.printed(t, 1); len(jobs[0]) != 0 {
		t.Errorf("the other device was sent %q", jobs[0])
	}
}

func TestAnAddressTakenByAnotherDeviceWithThePrinterGone(t *testing.T) {
	h := newHarness(t)
	other := h.network.put(oldAddr, otherMAC, true)
	h.server.queue = []*Job{macJob("a1", oldAddr)}

	h.agent.step(context.Background())

	want := "a1:failed:" + notOnTheNetwork + "; " + oldAddr + " is now another device (" + otherMAC + ")"
	if got := h.server.reported(); !equal(got, want) {
		t.Errorf("reports = %v\nwant      [%s]", got, want)
	}
	if jobs := other.printed(t, 1); len(jobs[0]) != 0 {
		t.Errorf("the other device was sent %q", jobs[0])
	}
	if h.agent.Ledger.Printed("a1") {
		t.Error("a job that was not written must not be remembered as printed")
	}
}

// When the system cannot say whose MAC is behind an address, the job is
// written as it always was: losing a chit is worse than the small risk of a
// wrong device.
func TestAnAddressWhoseMACCannotBeReadIsWrittenTo(t *testing.T) {
	h := newHarness(t)
	h.network.anonymous = true
	printer := h.network.put(oldAddr, otherMAC, true)
	h.server.queue = []*Job{macJob("a1", oldAddr)}

	h.agent.step(context.Background())

	printer.printed(t, 1)
	if got := h.server.reported(); !equal(got, "a1:printed@"+oldAddr) {
		t.Errorf("reports = %v", got)
	}
}

// The printer is on the network and its port is shut. That is a different
// problem from a printer that is gone, and reads differently.
func TestAPrinterThatRefusesConnectionsIsReportedWhereItIs(t *testing.T) {
	h := newHarness(t)
	h.network.put(newAddr, printerMAC, false)
	h.server.queue = []*Job{macJob("a1", oldAddr)}

	h.agent.step(context.Background())

	want := "a1:failed:printer " + printerMACText + " is at " + newAddr + " and refuses connections on port 9100: connection refused"
	if got := h.server.reported(); !equal(got, want) {
		t.Errorf("reports = %v\nwant      [%s]", got, want)
	}
}

// A refused connection is not proof that the address moved: the Sewoo has
// refused one and accepted the next. The lookup then finds the printer where
// the hint said, it is not dialled twice in one attempt, and the retry a few
// seconds later prints without another lookup.
func TestAPrinterThatRefusesOnceWhereTheHintSaysPrintsOnTheRetry(t *testing.T) {
	h := newHarness(t)
	h.network.put(oldAddr, printerMAC, false)
	h.server.queue = []*Job{macJob("a1", oldAddr), macJob("a1", oldAddr)}

	h.agent.step(context.Background())

	want := "a1:failed:printer " + printerMACText + " is at " + oldAddr + " and refuses connections on port 9100: connection refused"
	if got := h.server.reported(); !equal(got, want) {
		t.Errorf("reports = %v\nwant      [%s]", got, want)
	}
	if got := h.network.dials(); !equal(got, oldAddr) {
		t.Errorf("dialled %v, want the address once", got)
	}

	printer := h.network.put(oldAddr, printerMAC, true)
	h.agent.step(context.Background())

	printer.printed(t, 1)
	if got := h.server.reported(); len(got) != 2 || got[1] != "a1:printed@"+oldAddr {
		t.Errorf("reports = %v", got)
	}
	if n := h.network.swept(); n != 1 {
		t.Errorf("looked the printer up %d times, want once", n)
	}
}

// Between two lookups the agent still tries what the last one found, so a
// printer that came back at its new address prints at the next retry.
func TestTheLastLookupIsUsedUntilTheNextOne(t *testing.T) {
	h := newHarness(t)
	h.network.put(newAddr, printerMAC, false)
	h.server.queue = []*Job{macJob("a1", oldAddr), macJob("a1", oldAddr)}

	h.agent.step(context.Background())
	printer := h.network.put(newAddr, printerMAC, true)
	h.agent.step(context.Background())

	printer.printed(t, 1)
	if got := h.server.reported(); len(got) != 2 || got[1] != "a1:printed@"+newAddr {
		t.Errorf("reports = %v", got)
	}
	if n := h.network.swept(); n != 1 {
		t.Errorf("looked the printer up %d times, want once", n)
	}
}

func TestAComputerWithNoNetworkSaysSo(t *testing.T) {
	h := newHarness(t)
	h.network.offline = true
	h.server.queue = []*Job{macJob("a1", oldAddr)}

	h.agent.step(context.Background())

	if got := h.server.reported(); !equal(got, "a1:failed:this computer is not connected to a network") {
		t.Errorf("reports = %v", got)
	}
}

// The check that keeps the agent off the internet also runs on what the
// lookup found, and on the hint of a job with a MAC.
func TestOnlyPrivateAddressesAreDialledForAMAC(t *testing.T) {
	h := newHarness(t)
	h.network.put("8.8.4.4", printerMAC, true)
	h.server.queue = []*Job{macJob("a1", "8.8.8.8")}

	h.agent.step(context.Background())

	if got := h.network.dials(); len(got) != 0 {
		t.Errorf("dialled %v, want nothing", got)
	}
	if got := h.server.reported(); len(got) != 1 || !strings.HasPrefix(got[0], "a1:failed:") {
		t.Errorf("reports = %v, want the job failed", got)
	}
}

// The "printed" report is lost and sent again after a restart, from the
// ledger. It must still say where the job printed.
func TestAReportSentAgainFromTheLedgerCarriesTheAddress(t *testing.T) {
	h := newHarness(t)
	printer := h.network.put(newAddr, printerMAC, true)
	h.server.queue = []*Job{macJob("a1", oldAddr)}
	h.server.failResults = 1

	h.agent.step(context.Background())
	printer.printed(t, 1)
	if got := h.server.reported(); len(got) != 0 {
		t.Fatalf("reports = %v, want none yet", got)
	}

	ledger, warning := OpenLedger(filepath.Join(h.dir, StateFileName))
	if warning != nil {
		t.Fatal(warning)
	}
	restarted := New(h.agent.ConfigPath, ledger, log.New(io.Discard, "", 0), "test")
	h.network.attach(restarted.finder)
	restarted.step(context.Background())

	if got := h.server.reported(); !equal(got, "a1:printed@"+newAddr) {
		t.Errorf("reports = %v", got)
	}
	if n := printer.count(); n != 1 {
		t.Errorf("the printer received %d jobs, want 1", n)
	}
}

// The server did not get the report and offers the job again: the second
// report says where it printed too, and nothing prints twice.
func TestAJobOfferedTwiceIsReportedWithItsAddressBothTimes(t *testing.T) {
	h := newHarness(t)
	printer := h.network.put(newAddr, printerMAC, true)
	h.server.queue = []*Job{macJob("a1", oldAddr), macJob("a1", oldAddr)}

	h.agent.step(context.Background())
	printer.printed(t, 1)
	h.agent.step(context.Background())

	if got := h.server.reported(); !equal(got, "a1:printed@"+newAddr, "a1:printed@"+newAddr) {
		t.Errorf("reports = %v", got)
	}
	if n := printer.count(); n != 1 {
		t.Errorf("the printer received %d jobs, want 1", n)
	}
}

// The server offers a job again after 30 seconds without a result, and a
// second agent would print it. Whatever the network does, one attempt ends
// inside its limit: here nothing answers, the lookup uses all its time, and
// the printer it finds accepts the connection and then reads nothing.
func TestOneAttemptCannotOutlastItsLimit(t *testing.T) {
	if attemptTimeout > 25*time.Second {
		t.Errorf("an attempt may take %v; with the report on top that passes the server's 30 second claim timeout", attemptTimeout)
	}
	if left := attemptTimeout - findTimeout; left < 10*time.Second {
		t.Errorf("finding the printer may leave only %v to write the job", left)
	}

	finder := NewFinder(log.New(io.Discard, "", 0))
	finder.limits = limits{attempt: 600 * time.Millisecond, find: 300 * time.Millisecond, dial: 100 * time.Millisecond, lookup: 100 * time.Millisecond}
	finder.macAt = func(context.Context, netip.Addr) (MAC, bool) { return MAC{}, false }
	finder.lookup = func(ctx context.Context, _ MAC) (Lookup, error) {
		<-ctx.Done()
		return Lookup{Found: []netip.Addr{netip.MustParseAddr(newAddr)}}, nil
	}
	finder.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, newAddr) {
			// A printer that accepts and then stalls.
			here, there := net.Pipe()
			t.Cleanup(func() { there.Close() })
			return here, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	start := time.Now()
	_, err := finder.Send(context.Background(), macJob("a1", oldAddr).Printer, make([]byte, 1<<16))
	took := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "stopped accepting data") {
		t.Errorf("err = %v, want the write to have timed out", err)
	}
	if took < 500*time.Millisecond || took > 900*time.Millisecond {
		t.Errorf("the attempt took %v, want about its 600ms limit", took)
	}
}

func TestParseMAC(t *testing.T) {
	want := MAC{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	for _, text := range []string{
		"AA:BB:CC:DD:EE:FF",
		"aa:bb:cc:dd:ee:ff",
		"AA-BB-CC-DD-EE-FF",
		"aabbccddeeff",
		"AABB.CCDD.EEFF",
		"Aa:bB-ccDDeeff",
	} {
		if got, ok := ParseMAC(text); !ok || got != want {
			t.Errorf("ParseMAC(%q) = %v, %v", text, got, ok)
		}
	}
	for _, text := range []string{
		"",
		"AA:BB:CC:DD:EE",
		"AA:BB:CC:DD:EE:FF:00",
		"AA:BB:CC:DD:EE:GG",
		"192.168.1.50",
		"00:00:00:00:00:00",
	} {
		if got, ok := ParseMAC(text); ok {
			t.Errorf("ParseMAC(%q) = %v, want it refused", text, got)
		}
	}
	if got := want.String(); got != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("String() = %q", got)
	}
}

// A lookup asks only private networks, and at most 254 addresses on each,
// never the network's own address, its broadcast address or this computer.
func TestTheSweepIsBounded(t *testing.T) {
	cases := []struct {
		address     string
		ok          bool
		sweep       string
		count       int
		first, last string
	}{
		{"192.168.1.8/24", true, "192.168.1.0/24", 253, "192.168.1.1", "192.168.1.254"},
		{"192.168.1.1/24", true, "192.168.1.0/24", 253, "192.168.1.2", "192.168.1.254"},
		{"192.168.1.254/24", true, "192.168.1.0/24", 253, "192.168.1.1", "192.168.1.253"},
		// A larger network: only the 254 addresses around the computer.
		{"172.22.7.1/16", true, "172.22.7.0/24", 253, "172.22.7.2", "172.22.7.254"},
		{"10.20.30.40/8", true, "10.20.30.0/24", 253, "10.20.30.1", "10.20.30.254"},
		// A smaller one: all of it.
		{"192.168.1.130/26", true, "192.168.1.128/26", 61, "192.168.1.129", "192.168.1.190"},
		{"192.168.1.5/30", true, "192.168.1.4/30", 1, "192.168.1.6", "192.168.1.6"},
		// Not a network with other devices on it, or not private.
		{"192.168.1.5/31", false, "", 0, "", ""},
		{"192.168.1.5/32", false, "", 0, "", ""},
		{"8.8.8.8/24", false, "", 0, "", ""},
		{"169.254.10.20/16", false, "", 0, "", ""},
		{"127.0.0.1/8", false, "", 0, "", ""},
		{"fd00::5/64", false, "", 0, "", ""},
	}
	for _, c := range cases {
		address := netip.MustParsePrefix(c.address)
		n, ok := newLocalNet(address)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.address, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		probes := n.probes()
		if n.sweep.String() != c.sweep || len(probes) != c.count {
			t.Errorf("%s: sweeps %d addresses of %s, want %d of %s", c.address, len(probes), n.sweep, c.count, c.sweep)
			continue
		}
		if first, last := probes[0].addr.String(), probes[len(probes)-1].addr.String(); first != c.first || last != c.last {
			t.Errorf("%s: sweeps %s to %s, want %s to %s", c.address, first, last, c.first, c.last)
		}
		for _, p := range probes {
			if p.addr == address.Addr() || p.from != address.Addr() {
				t.Errorf("%s: probe %+v", c.address, p)
				break
			}
		}
	}
}
