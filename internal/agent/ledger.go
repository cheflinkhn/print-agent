package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	// A reported job is remembered this long, in case the server offers it
	// again; an unreported one a while longer, then given up on.
	keepReported   = 24 * time.Hour
	keepUnreported = 72 * time.Hour
)

// Ledger remembers, on disk, which jobs this agent has printed and whether
// the server knows. It is what stops a chit from printing twice when the
// "printed" report is lost on the way and the server offers the job again,
// and what gets that report delivered after a restart.
type Ledger struct {
	path string
	mu   sync.Mutex
	jobs map[string]*printedJob
	now  func() time.Time
}

type printedJob struct {
	At       time.Time `json:"at"`
	Reported bool      `json:"reported"`
}

type ledgerFile struct {
	Printed map[string]*printedJob `json:"printed"`
}

// OpenLedger loads the ledger, or starts an empty one. A file that cannot be
// read is set aside and reported as a warning, never as a failure: a damaged
// ledger must not stop the kitchen's printer.
func OpenLedger(path string) (ledger *Ledger, warning error) {
	l := &Ledger{path: path, jobs: map[string]*printedJob{}, now: time.Now}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, fmt.Errorf("cannot read %s, starting with an empty ledger: %w", path, err)
	}
	var file ledgerFile
	if err := json.Unmarshal(raw, &file); err != nil {
		_ = os.Rename(path, path+".bad")
		return l, fmt.Errorf("%s was damaged and has been set aside as %s.bad: %w", path, path, err)
	}
	for id, job := range file.Printed {
		if job != nil {
			l.jobs[id] = job
		}
	}
	return l, nil
}

// Printed says whether this agent already printed the job.
func (l *Ledger) Printed(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.jobs[id]
	return ok
}

// MarkPrinted records a job the printer accepted, not yet reported.
func (l *Ledger) MarkPrinted(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.jobs[id] = &printedJob{At: l.now()}
	return l.save()
}

// MarkReported records that the server knows the job was printed.
func (l *Ledger) MarkReported(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	job, ok := l.jobs[id]
	if !ok || job.Reported {
		return nil
	}
	job.Reported = true
	return l.save()
}

// Unreported lists the printed jobs the server has not been told about,
// oldest first.
func (l *Ledger) Unreported() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ids []string
	for id, job := range l.jobs {
		if !job.Reported {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := l.jobs[ids[i]], l.jobs[ids[j]]
		if a.At.Equal(b.At) {
			return ids[i] < ids[j]
		}
		return a.At.Before(b.At)
	})
	return ids
}

func (l *Ledger) save() error {
	now := l.now()
	for id, job := range l.jobs {
		age := now.Sub(job.At)
		if age > keepUnreported || (job.Reported && age > keepReported) {
			delete(l.jobs, id)
		}
	}
	raw, err := json.MarshalIndent(ledgerFile{Printed: l.jobs}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(l.path, append(raw, '\n'), 0o600)
}
