package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token"

// fakeCheflink is the server side of the protocol, scripted by each test.
type fakeCheflink struct {
	t *testing.T

	mu sync.Mutex
	// queue is what jobs/next hands out, in order; nil entries mean "no job".
	queue []*Job
	// results are the reports received, as "id:status" or "id:status:error",
	// with "@host" after it when the report said where the job printed.
	results []string
	// failResults makes that many result reports answer 500 first.
	failResults int
	// rejectToken makes every authenticated call answer 401.
	rejectToken bool
	pollSeconds int
}

func (f *fakeCheflink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/api/print-agent/pair" {
		var body struct{ Code, Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Code != "123456" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"wrong or expired pairing code"}`)
			return
		}
		_, _ = io.WriteString(w, `{"token":"`+testToken+`","venue":"Test Venue"}`)
		return
	}

	if f.rejectToken || r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/api/print-agent/jobs/next":
		var job *Job
		if len(f.queue) > 0 {
			job, f.queue = f.queue[0], f.queue[1:]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"job": job, "poll_seconds": f.pollSeconds})
	case strings.HasSuffix(r.URL.Path, "/result"):
		if f.failResults > 0 {
			f.failResults--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		id := strings.Split(r.URL.Path, "/")[4]
		var body struct {
			Status, Error string
			Printer       struct{ Host string }
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		entry := id + ":" + body.Status
		if body.Error != "" {
			entry += ":" + body.Error
		}
		if body.Printer.Host != "" {
			entry += "@" + body.Printer.Host
		}
		f.results = append(f.results, entry)
		_, _ = io.WriteString(w, `{"ok":true}`)
	default:
		f.t.Errorf("unexpected request to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeCheflink) reported() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.results...)
}

// fakePrinter is a TCP listener that keeps what it is sent.
type fakePrinter struct {
	listener net.Listener
	mu       sync.Mutex
	jobs     [][]byte
	received chan struct{}
}

func newFakePrinter(t *testing.T) *fakePrinter {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePrinter{listener: listener, received: make(chan struct{}, 16)}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			data, _ := io.ReadAll(conn)
			conn.Close()
			p.mu.Lock()
			p.jobs = append(p.jobs, data)
			p.mu.Unlock()
			p.received <- struct{}{}
		}
	}()
	return p
}

func (p *fakePrinter) printer() Printer {
	addr := p.listener.Addr().(*net.TCPAddr)
	return Printer{Host: "127.0.0.1", Port: addr.Port}
}

// printed waits for want jobs to have arrived and returns them.
func (p *fakePrinter) printed(t *testing.T, want int) [][]byte {
	t.Helper()
	for range want {
		select {
		case <-p.received:
		case <-time.After(2 * time.Second):
			t.Fatalf("the printer received fewer than %d jobs", want)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.jobs...)
}

func (p *fakePrinter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.jobs)
}

type harness struct {
	agent   *Agent
	server  *fakeCheflink
	printer *fakePrinter
	// network is the venue's network as the finder sees it, for jobs that
	// name their printer by MAC. See finder_test.go.
	network *fakeNetwork
	log     *bytes.Buffer
	dir     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	server := &fakeCheflink{t: t}
	web := httptest.NewServer(server)
	t.Cleanup(web.Close)

	dir := t.TempDir()
	cfg := Config{ServerURL: web.URL, Token: testToken, AgentName: "test"}
	if err := cfg.Save(filepath.Join(dir, ConfigFileName)); err != nil {
		t.Fatal(err)
	}
	ledger, warning := OpenLedger(filepath.Join(dir, StateFileName))
	if warning != nil {
		t.Fatal(warning)
	}
	logged := &bytes.Buffer{}
	a := New(filepath.Join(dir, ConfigFileName), ledger, log.New(logged, "", 0), "test")
	network := newFakeNetwork(t)
	network.attach(a.finder)
	return &harness{agent: a, server: server, printer: newFakePrinter(t), network: network, log: logged, dir: dir}
}

func (h *harness) job(id, text string) *Job {
	return &Job{
		ID:      id,
		Label:   "Table 4",
		Printer: h.printer.printer(),
		Data:    base64.StdEncoding.EncodeToString([]byte(text)),
	}
}

func TestPrintsAJobAndReportsIt(t *testing.T) {
	h := newHarness(t)
	h.server.queue = []*Job{h.job("a1", "one chit")}

	wait := h.agent.step(context.Background())

	jobs := h.printer.printed(t, 1)
	if string(jobs[0]) != "one chit" {
		t.Errorf("the printer got %q", jobs[0])
	}
	if got := h.server.reported(); len(got) != 1 || got[0] != "a1:printed" {
		t.Errorf("reports = %v", got)
	}
	if wait != 0 {
		t.Errorf("after a printed job the agent should ask again at once, waited %v", wait)
	}
	if ids := h.agent.Ledger.Unreported(); len(ids) != 0 {
		t.Errorf("unreported = %v", ids)
	}
}

func TestNoJobWaitsForThePollInterval(t *testing.T) {
	h := newHarness(t)
	h.server.pollSeconds = 7

	if wait := h.agent.step(context.Background()); wait != 7*time.Second {
		t.Errorf("wait = %v, want the server's 7s", wait)
	}
}

// The server did not get the report and offers the job again: it must be
// reported again and must not print twice.
func TestAJobOfferedTwiceIsPrintedOnce(t *testing.T) {
	h := newHarness(t)
	h.server.queue = []*Job{h.job("a1", "one chit"), h.job("a1", "one chit")}

	h.agent.step(context.Background())
	h.printer.printed(t, 1)
	h.agent.step(context.Background())

	if n := h.printer.count(); n != 1 {
		t.Errorf("the printer received %d jobs, want 1", n)
	}
	if got := h.server.reported(); len(got) != 2 || got[1] != "a1:printed" {
		t.Errorf("reports = %v", got)
	}
}

// The "printed" report fails; the next round delivers it before asking for
// more work, and a restarted agent would too, from the ledger on disk.
func TestALostReportIsDeliveredLater(t *testing.T) {
	h := newHarness(t)
	h.server.queue = []*Job{h.job("a1", "one chit")}
	h.server.failResults = 1

	h.agent.step(context.Background())
	h.printer.printed(t, 1)
	if got := h.server.reported(); len(got) != 0 {
		t.Fatalf("reports = %v, want none yet", got)
	}

	// A new agent on the same folder, as after a restart.
	ledger, warning := OpenLedger(filepath.Join(h.dir, StateFileName))
	if warning != nil {
		t.Fatal(warning)
	}
	restarted := New(h.agent.ConfigPath, ledger, log.New(io.Discard, "", 0), "test")
	restarted.step(context.Background())

	if got := h.server.reported(); len(got) != 1 || got[0] != "a1:printed" {
		t.Errorf("reports = %v", got)
	}
	if n := h.printer.count(); n != 1 {
		t.Errorf("the printer received %d jobs, want 1", n)
	}
}

func TestAPrinterThatIsOffIsReportedAsFailed(t *testing.T) {
	h := newHarness(t)
	job := h.job("a1", "one chit")
	h.printer.listener.Close()
	h.server.queue = []*Job{job}

	wait := h.agent.step(context.Background())

	got := h.server.reported()
	if len(got) != 1 || !strings.HasPrefix(got[0], "a1:failed:cannot reach the printer") {
		t.Errorf("reports = %v", got)
	}
	if wait != failedPrintWait {
		t.Errorf("wait = %v, want %v", wait, failedPrintWait)
	}
	if h.agent.Ledger.Printed("a1") {
		t.Error("a failed job must not be remembered as printed")
	}
}

// A printer left switched off is retried every few seconds; the log says so
// once, and the job prints when the printer is back.
func TestAJobThatKeepsFailingIsLoggedOnceAndPrintsLater(t *testing.T) {
	h := newHarness(t)
	working := h.job("a1", "one chit")
	broken := *working
	broken.Printer = Printer{Host: "127.0.0.1", Port: 1}
	h.server.queue = []*Job{&broken, &broken, &broken, working}

	for range 4 {
		h.agent.step(context.Background())
	}

	h.printer.printed(t, 1)
	if n := strings.Count(h.log.String(), "did not print"); n != 1 {
		t.Errorf("the failure was logged %d times, want once:\n%s", n, h.log.String())
	}
	got := h.server.reported()
	if len(got) != 4 || got[3] != "a1:printed" {
		t.Errorf("reports = %v", got)
	}
}

func TestAJobForAPublicAddressIsRefused(t *testing.T) {
	h := newHarness(t)
	job := h.job("a1", "one chit")
	job.Printer = Printer{Host: "8.8.8.8", Port: 9100}
	h.server.queue = []*Job{job}

	h.agent.step(context.Background())

	got := h.server.reported()
	if len(got) != 1 || !strings.Contains(got[0], "not on a private network") {
		t.Errorf("reports = %v", got)
	}
}

func TestARevokedTokenWaitsAndSaysSoOnce(t *testing.T) {
	h := newHarness(t)
	h.server.rejectToken = true

	for range 3 {
		if wait := h.agent.step(context.Background()); wait != unpairedWait {
			t.Errorf("wait = %v, want %v", wait, unpairedWait)
		}
	}
	if n := strings.Count(h.log.String(), "pair it again"); n != 1 {
		t.Errorf("the problem was logged %d times, want once:\n%s", n, h.log.String())
	}

	// The venue pairs the PC again; the agent recovers by itself.
	h.server.rejectToken = false
	h.server.queue = []*Job{h.job("a1", "one chit")}
	h.agent.step(context.Background())
	h.printer.printed(t, 1)
}

func TestAServerThatIsDownBacksOff(t *testing.T) {
	h := newHarness(t)
	cfg := Config{ServerURL: "http://127.0.0.1:1", Token: testToken}
	if err := cfg.Save(h.agent.ConfigPath); err != nil {
		t.Fatal(err)
	}

	first := h.agent.step(context.Background())
	second := h.agent.step(context.Background())
	if first != firstBackoff || second != 2*firstBackoff {
		t.Errorf("waits = %v, %v; want %v then %v", first, second, firstBackoff, 2*firstBackoff)
	}
}

func TestUnpairedAgentWaits(t *testing.T) {
	dir := t.TempDir()
	ledger, _ := OpenLedger(filepath.Join(dir, StateFileName))
	logged := &bytes.Buffer{}
	a := New(filepath.Join(dir, ConfigFileName), ledger, log.New(logged, "", 0), "test")

	if wait := a.step(context.Background()); wait != unpairedWait {
		t.Errorf("wait = %v", wait)
	}
	if !strings.Contains(logged.String(), "not paired yet") {
		t.Errorf("log = %q", logged.String())
	}
}

func TestPair(t *testing.T) {
	web := httptest.NewServer(&fakeCheflink{t: t})
	defer web.Close()
	base, err := CheckServerURL(web.URL, false)
	if err != nil {
		t.Fatal(err)
	}

	result, err := Pair(context.Background(), base, "123456", "CAJA", "test")
	if err != nil || result.Token != testToken || result.Venue != "Test Venue" {
		t.Errorf("Pair = %+v, %v", result, err)
	}

	_, err = Pair(context.Background(), base, "000000", "CAJA", "test")
	if err == nil || !strings.Contains(err.Error(), "wrong or expired pairing code") {
		t.Errorf("a wrong code gave %v", err)
	}
}

func TestCheckServerURL(t *testing.T) {
	cases := []struct {
		url      string
		insecure bool
		ok       bool
	}{
		{"https://demo.example.com", false, true},
		{"http://localhost:8000", false, true},
		{"http://127.0.0.1:8080", false, true},
		{"http://192.168.1.10:8000", false, false},
		{"http://192.168.1.10:8000", true, true},
		{"ftp://example.com", false, false},
		{"example.com", false, false},
	}
	for _, c := range cases {
		_, err := CheckServerURL(c.url, c.insecure)
		if (err == nil) != c.ok {
			t.Errorf("CheckServerURL(%q, %v) = %v, want ok=%v", c.url, c.insecure, err, c.ok)
		}
	}
}

func TestPrinterAddr(t *testing.T) {
	cases := []struct {
		printer Printer
		ok      bool
	}{
		{Printer{Host: "192.168.1.50", Port: 9100}, true},
		{Printer{Host: "10.0.0.7", Port: 9100}, true},
		{Printer{Host: "127.0.0.1", Port: 9100}, true},
		{Printer{Host: "8.8.8.8", Port: 9100}, false},
		{Printer{Host: "printer.local", Port: 9100}, false},
		{Printer{Host: "192.168.1.50", Port: 0}, false},
		{Printer{Host: "192.168.1.50", Port: 70000}, false},
	}
	for _, c := range cases {
		_, err := c.printer.Addr()
		if (err == nil) != c.ok {
			t.Errorf("%+v: err = %v, want ok=%v", c.printer, err, c.ok)
		}
	}
}

func TestJobBytes(t *testing.T) {
	if _, err := (Job{Data: "not base64!"}).Bytes(); err == nil {
		t.Error("bad base64 was accepted")
	}
	if _, err := (Job{Data: ""}).Bytes(); err == nil {
		t.Error("an empty job was accepted")
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, MaxJobBytes+1))
	if _, err := (Job{Data: big}).Bytes(); err == nil {
		t.Error("an oversized job was accepted")
	}
}

func TestLedgerSurvivesAReopenAndForgetsOldJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	ledger, _ := OpenLedger(path)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ledger.now = func() time.Time { return now }

	if err := ledger.MarkPrinted("old", ""); err != nil {
		t.Fatal(err)
	}
	if err := ledger.MarkReported("old"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(keepReported + time.Hour)
	if err := ledger.MarkPrinted("new", "192.168.1.37"); err != nil {
		t.Fatal(err)
	}

	reopened, warning := OpenLedger(path)
	if warning != nil {
		t.Fatal(warning)
	}
	if reopened.Printed("old") {
		t.Error("a job reported over a day ago should be forgotten")
	}
	if !reopened.Printed("new") {
		t.Error("a fresh job was lost on reopen")
	}
	if ids := reopened.Unreported(); len(ids) != 1 || ids[0] != "new" {
		t.Errorf("unreported = %v", ids)
	}
	if host := reopened.PrintedOn("new"); host != "192.168.1.37" {
		t.Errorf("the address a job printed on was lost on reopen: %q", host)
	}
}

func TestADamagedLedgerIsSetAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	if err := writeFileAtomic(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, warning := OpenLedger(path)
	if warning == nil {
		t.Error("expected a warning")
	}
	if err := ledger.MarkPrinted("a1", ""); err != nil {
		t.Errorf("the ledger should work after a damaged file: %v", err)
	}
}

func TestSendToPrinterReportsAnUnreachablePrinter(t *testing.T) {
	err := SendToPrinter(context.Background(), Printer{Host: "127.0.0.1", Port: 1}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "cannot reach the printer") {
		t.Errorf("err = %v", err)
	}
}
