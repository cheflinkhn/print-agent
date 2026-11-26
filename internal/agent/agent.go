package agent

import (
	"context"
	"errors"
	"log"
	"os"
	"time"
)

const (
	// How long to wait before asking again after each kind of trouble.
	unpairedWait    = 15 * time.Second
	failedPrintWait = 5 * time.Second
	firstBackoff    = 2 * time.Second
	maxBackoff      = 30 * time.Second
)

// Agent is the polling loop: report what is owed, claim a job, print it,
// report it.
//
// A lost chit is worse than a duplicate one, so the agent prints at least
// once: a job is only remembered as printed after the printer accepted it. If
// the agent dies in the middle of a job, the server offers the job again and
// it prints again. The ledger covers the common case, a "printed" report that
// did not arrive, where the job must not print a second time.
type Agent struct {
	ConfigPath string
	Ledger     *Ledger
	Log        *log.Logger
	Version    string

	// finder sends a job to its printer, wherever that is now.
	finder *Finder

	client     *Client
	configTime time.Time
	using      string
	poll       time.Duration
	backoff    time.Duration
	// trouble is the last problem logged, so a problem that lasts an hour
	// is one line in the log and not two thousand. printTrouble is the same
	// for a job that keeps failing: a printer left switched off is retried
	// every few seconds.
	trouble      string
	printTrouble string
}

// Run polls until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.Log.Printf("Cheflink print agent %s started", a.Version)
	for {
		wait := a.step(ctx)
		if ctx.Err() != nil {
			a.Log.Print("Cheflink print agent stopped")
			return nil
		}
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// step does one round and returns how long to wait before the next.
func (a *Agent) step(ctx context.Context) time.Duration {
	if err := a.loadConfig(); err != nil {
		a.noteTrouble(err.Error())
		return unpairedWait
	}
	if err := a.flushReports(ctx); err != nil {
		return a.serverTrouble(err)
	}
	job, hint, err := a.client.NextJob(ctx)
	if err != nil {
		return a.serverTrouble(err)
	}
	a.troubleOver()
	if hint > 0 {
		a.poll = clampPoll(hint)
	}
	if job == nil {
		return a.poll
	}
	// A job in hand is finished even if the agent is asked to stop meanwhile:
	// half a chit is worse than a slow shutdown. Its timeouts bound the wait.
	if !a.handle(context.WithoutCancel(ctx), job) {
		return failedPrintWait
	}
	// Ask again at once, so a burst of tickets prints without gaps.
	return 0
}

// loadConfig (re)reads config.json when it is new or has changed, so pairing
// again takes effect without restarting the agent.
func (a *Agent) loadConfig() error {
	info, err := os.Stat(a.ConfigPath)
	if err == nil && a.client != nil && info.ModTime().Equal(a.configTime) {
		return nil
	}
	a.client = nil
	cfg, err := LoadConfig(a.ConfigPath)
	if err != nil {
		return err
	}
	client, err := NewClient(cfg, a.Version)
	if err != nil {
		return err
	}
	a.client = client
	a.poll = cfg.PollInterval()
	if info != nil {
		a.configTime = info.ModTime()
	}
	// Say so once per pairing, not on every retry of a rejected token.
	if using := cfg.ServerURL + " as " + cfg.AgentName; using != a.using {
		a.using = using
		a.Log.Printf("using server %s", using)
	}
	return nil
}

// flushReports delivers the "printed" reports the server has not had yet.
func (a *Agent) flushReports(ctx context.Context) error {
	for _, id := range a.Ledger.Unreported() {
		if err := a.client.ReportPrinted(ctx, id, a.Ledger.PrintedOn(id)); err != nil {
			return err
		}
		a.markReported(id)
	}
	return nil
}

// handle prints one job and reports it. It returns false when the job did
// not print.
func (a *Agent) handle(ctx context.Context, job *Job) bool {
	if a.Ledger.Printed(job.ID) {
		// The server did not get the first report and is offering the job
		// again. Tell it again; do not print again.
		a.Log.Printf("job %s (%s) was already printed; reporting it again", job.ID, job.Label)
		a.reportPrinted(ctx, job.ID)
		return true
	}

	data, err := job.Bytes()
	// host is where the job printed, when its printer was found by MAC.
	var host string
	if err == nil {
		host, err = a.finder.Send(ctx, job.Printer, data)
	}
	if err != nil {
		if failure := job.ID + ": " + err.Error(); failure != a.printTrouble {
			a.printTrouble = failure
			a.Log.Printf("job %s (%s) did not print, retrying until it does: %v", job.ID, job.Label, err)
		}
		// Best effort: if this report is lost, the server's own timeout
		// puts the job back in the queue.
		if reportErr := a.client.ReportFailed(ctx, job.ID, err.Error()); reportErr != nil {
			a.Log.Printf("job %s: could not report the failure: %v", job.ID, reportErr)
		}
		return false
	}

	a.printTrouble = ""
	printedOn := job.Printer
	if host != "" {
		printedOn.Host = host
	}
	a.Log.Printf("printed job %s (%s) on %s, %d bytes", job.ID, job.Label, printedOn, len(data))
	// The address goes in the ledger with the job: the report may have to
	// be sent again after a restart.
	if err := a.Ledger.MarkPrinted(job.ID, host); err != nil {
		a.Log.Printf("job %s: could not save the ledger: %v", job.ID, err)
	}
	a.reportPrinted(ctx, job.ID)
	return true
}

func (a *Agent) reportPrinted(ctx context.Context, id string) {
	if err := a.client.ReportPrinted(ctx, id, a.Ledger.PrintedOn(id)); err != nil {
		// Stays unreported in the ledger; flushReports retries it.
		a.Log.Printf("job %s: could not report it as printed yet: %v", id, err)
		return
	}
	a.markReported(id)
}

func (a *Agent) markReported(id string) {
	if err := a.Ledger.MarkReported(id); err != nil {
		a.Log.Printf("job %s: could not save the ledger: %v", id, err)
	}
}

// serverTrouble logs a failed request once and returns a growing wait.
func (a *Agent) serverTrouble(err error) time.Duration {
	a.noteTrouble(err.Error())
	if errors.Is(err, ErrUnauthorized) {
		// Pairing again rewrites config.json; pick that up on the next round.
		a.client = nil
		return unpairedWait
	}
	if a.backoff == 0 {
		a.backoff = firstBackoff
	} else {
		a.backoff = min(a.backoff*2, maxBackoff)
	}
	return a.backoff
}

func (a *Agent) noteTrouble(message string) {
	if message != a.trouble {
		a.trouble = message
		a.Log.Printf("waiting: %s", message)
	}
}

func (a *Agent) troubleOver() {
	if a.trouble != "" {
		a.Log.Print("connected to the server again")
	}
	a.trouble = ""
	a.backoff = 0
}

// New builds an agent that keeps its files in dir.
func New(configPath string, ledger *Ledger, logger *log.Logger, version string) *Agent {
	return &Agent{
		ConfigPath: configPath,
		Ledger:     ledger,
		Log:        logger,
		Version:    version,
		finder:     NewFinder(logger),
	}
}
