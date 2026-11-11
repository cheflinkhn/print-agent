// Command fakeserver stands in for Cheflink while the agent is developed: it
// speaks the protocol in docs/protocol.md and hands out sample kitchen chits.
// It is a development tool. It keeps everything in memory and its token is
// not a secret.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// A claimed job the agent never reported goes back in the queue.
	claimTimeout = 30 * time.Second
	// A job nobody printed in this long is dropped: a chit that late is
	// worse than no chit.
	jobLifetime = 10 * time.Minute
	pollSeconds = 2
)

type job struct {
	id        string
	label     string
	data      []byte
	created   time.Time
	claimedAt time.Time
	attempts  int
	done      bool
}

type server struct {
	code        string
	printerHost string
	printerPort int
	codePage    int

	mu   sync.Mutex
	jobs []*job
	seq  int
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve the protocol on")
	printer := flag.String("printer", "", "the printer jobs are addressed to, e.g. 192.168.1.50:9100")
	sink := flag.String("sink", "", "also run a pretend printer on this address, e.g. 127.0.0.1:9100, and print to it when -printer is not given")
	code := flag.String("code", "123456", "the pairing code to accept")
	every := flag.Duration("every", 0, "queue a sample chit this often, e.g. 20s (default: only when Enter is pressed)")
	codePage := flag.Int("codepage", 16, "the number sent with ESC t to select the printer's code page for Latin-1 text")
	flag.Parse()

	if *sink != "" {
		go runSink(*sink)
		if *printer == "" {
			*printer = *sink
		}
	}
	if *printer == "" {
		log.Fatal("give -printer IP:PORT, or -sink 127.0.0.1:9100 to try it without a printer")
	}
	host, portText, err := net.SplitHostPort(*printer)
	if err != nil {
		log.Fatalf("-printer must look like 192.168.1.50:9100: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		log.Fatalf("-printer must look like 192.168.1.50:9100: %v", err)
	}

	s := &server{code: *code, printerHost: host, printerPort: port, codePage: *codePage}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/print-agent/pair", s.pair)
	mux.HandleFunc("POST /api/print-agent/jobs/next", s.next)
	mux.HandleFunc("POST /api/print-agent/jobs/{id}/result", s.result)
	mux.HandleFunc("POST /dev/enqueue", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"id": s.enqueue()})
	})

	if *every > 0 {
		go func() {
			for range time.Tick(*every) {
				s.enqueue()
			}
		}()
	}
	go func() {
		// Enter queues a chit. When stdin is closed (run in the background)
		// this loop just ends.
		lines := bufio.NewScanner(os.Stdin)
		for lines.Scan() {
			s.enqueue()
		}
	}()

	log.Printf("fake Cheflink on http://%s, pairing code %s, printing to %s", *listen, *code, *printer)
	log.Print("press Enter to queue a sample chit")
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func (s *server) token() string { return "dev-" + s.code }

func (s *server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") == "Bearer "+s.token() {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unknown or revoked token"})
	return false
}

func (s *server) pair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Code != s.code {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wrong or expired pairing code"})
		return
	}
	log.Printf("paired agent %q", body.Name)
	writeJSON(w, http.StatusOK, map[string]string{"token": s.token(), "venue": "Fake Venue"})
}

func (s *server) next(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	answer := map[string]any{"job": nil, "poll_seconds": pollSeconds}
	for _, j := range s.jobs {
		if j.done {
			continue
		}
		if now.Sub(j.created) > jobLifetime {
			j.done = true
			log.Printf("job %s (%s) expired unprinted", j.id, j.label)
			continue
		}
		if !j.claimedAt.IsZero() && now.Sub(j.claimedAt) < claimTimeout {
			continue
		}
		j.claimedAt = now
		j.attempts++
		log.Printf("job %s (%s) claimed, attempt %d", j.id, j.label, j.attempts)
		answer["job"] = map[string]any{
			"id":      j.id,
			"label":   j.label,
			"printer": map[string]any{"host": s.printerHost, "port": s.printerPort},
			"data":    base64.StdEncoding.EncodeToString(j.data),
		}
		break
	}
	writeJSON(w, http.StatusOK, answer)
}

func (s *server) result(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is not JSON"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.id != r.PathValue("id") {
			continue
		}
		switch body.Status {
		case "printed":
			j.done = true
			log.Printf("job %s (%s) PRINTED", j.id, j.label)
		case "failed":
			// Back in the queue for the next poll.
			j.claimedAt = time.Time{}
			log.Printf("job %s (%s) FAILED: %s", j.id, j.label, body.Error)
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be printed or failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
}

func (s *server) enqueue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	table := 1 + s.seq%12
	j := &job{
		id:      fmt.Sprintf("dev-%d-%d", time.Now().Unix(), s.seq),
		label:   fmt.Sprintf("Mesa %d · sample %d", table, s.seq),
		data:    sampleChit(s.codePage, table, s.seq),
		created: time.Now(),
	}
	s.jobs = append(s.jobs, j)
	log.Printf("queued job %s (%s), %d bytes", j.id, j.label, len(j.data))
	return j.id
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// sampleChit builds a kitchen chit in ESC/POS, close to what Cheflink will
// send: large type, modifiers, a note, accented Spanish and a cut. Cheflink's
// real layout is built on the server; this one only exercises the printer.
func sampleChit(codePage, table, seq int) []byte {
	const (
		normal     = 0x00
		tall       = 0x01
		tallWide   = 0x11
		alignLeft  = 0
		alignCentr = 1
	)
	var b []byte
	raw := func(bytes ...byte) { b = append(b, bytes...) }
	text := func(s string) { b = append(b, latin1(s)...) }
	size := func(n byte) { raw(0x1D, 0x21, n) }   // GS ! n
	align := func(n byte) { raw(0x1B, 0x61, n) }  // ESC a n
	bold := func(on byte) { raw(0x1B, 0x45, on) } // ESC E n
	rule := func() { size(normal); text(strings.Repeat("-", 42) + "\n") }

	raw(0x1B, 0x40)                 // ESC @: initialize
	raw(0x1B, 0x74, byte(codePage)) // ESC t n: code page

	align(alignCentr)
	text("Restaurante de prueba\n")
	size(tallWide)
	bold(1)
	text(fmt.Sprintf("Mesa %d\n", table))
	bold(0)
	size(tall)
	text(fmt.Sprintf("Cuenta %d · Ronda 1\n", 100+seq))
	align(alignLeft)
	rule()
	text(fmt.Sprintf("Enviado  %s\n", time.Now().Format("15:04")))
	text("Por      andrés\n")
	rule()

	line := func(name string, extras ...string) {
		size(tallWide)
		bold(1)
		text("1 x " + name + "\n")
		bold(0)
		size(tall)
		for _, extra := range extras {
			text("   " + extra + "\n")
		}
	}
	line("Baleada", "+ Aguacate", "+ Huevo")
	line("Pollo asado", "Sin cebolla, término ¾")
	line("Limonada", "+ Hierbabuena")
	rule()

	align(alignCentr)
	text("ñ Ñ á é í ó ú ü ¿ ¡ °\n")
	text("3 artículos\n")

	raw(0x1B, 0x64, 4)    // ESC d 4: feed past the cutter
	raw(0x1D, 0x56, 0x00) // GS V 0: full cut
	return b
}

// latin1 encodes text for a printer set to a Latin-1 compatible code page
// (Windows-1252 is one for the letters Spanish uses).
func latin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r < 0x100 {
			out = append(out, byte(r))
		} else {
			out = append(out, '?')
		}
	}
	return out
}

// runSink is a pretend printer: it accepts jobs on a TCP port and shows what
// arrived, so the whole loop can be tried with no hardware.
func runSink(addr string) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("sink: %v", err)
	}
	log.Printf("pretend printer listening on %s", addr)
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("sink: %v", err)
			continue
		}
		go func() {
			defer conn.Close()
			data, _ := io.ReadAll(conn)
			cut := "no cut command"
			if strings.Contains(string(data), "\x1d\x56") {
				cut = "ends with a cut"
			}
			log.Printf("pretend printer received %d bytes, %s:\n%s", len(data), cut, readable(data))
		}()
	}
}

// readable keeps a job's text and drops its control bytes, roughly: enough to
// recognise the chit in a terminal.
func readable(data []byte) string {
	var out strings.Builder
	for _, c := range data {
		switch {
		case c == '\n':
			out.WriteByte('\n')
		case c >= 0x20 && c < 0x7F:
			out.WriteByte(c)
		case c >= 0xA0:
			out.WriteRune(rune(c))
		}
	}
	return out.String()
}
