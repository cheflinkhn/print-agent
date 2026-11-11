package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"time"
)

// The protocol is described in docs/protocol.md. Keep the two in step.
const (
	StatusPrinted = "printed"
	StatusFailed  = "failed"

	requestTimeout  = 15 * time.Second
	maxResponseSize = 2 * MaxJobBytes
	maxErrorLength  = 500
)

// ErrUnauthorized means the server does not accept the agent's token: it was
// never paired with this server, or the venue revoked it.
var ErrUnauthorized = errors.New("the server does not accept this agent's token; pair it again")

var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Job is one thing to print, as the server hands it out.
type Job struct {
	ID string `json:"id"`
	// Label says what the job is, for the log: "Table 4 · Round 2".
	Label   string  `json:"label"`
	Printer Printer `json:"printer"`
	// Data is the printer's bytes, base64 encoded. The agent does not look
	// inside them.
	Data string `json:"data"`
}

// Bytes decodes and checks the job's payload.
func (j Job) Bytes() ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(j.Data)
	if err != nil {
		return nil, fmt.Errorf("the job's data is not valid base64: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("the job has no data")
	}
	if len(data) > MaxJobBytes {
		return nil, fmt.Errorf("the job is %d bytes, over the %d byte limit", len(data), MaxJobBytes)
	}
	return data, nil
}

// Client talks to one Cheflink server as one paired agent.
type Client struct {
	base    *url.URL
	token   string
	version string
	http    *http.Client
}

// NewClient builds a client from a checked config.
func NewClient(cfg Config, version string) (*Client, error) {
	base, err := CheckServerURL(cfg.ServerURL, cfg.AllowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:    base,
		token:   cfg.Token,
		version: version,
		http:    &http.Client{Timeout: requestTimeout},
	}, nil
}

// PairResult is what redeeming a pairing code returns.
type PairResult struct {
	Token string `json:"token"`
	// Venue is the venue's name, to confirm the code was the right one.
	Venue string `json:"venue"`
}

// Pair redeems a pairing code issued on the venue's page for a token.
func Pair(ctx context.Context, server *url.URL, code, name, version string) (PairResult, error) {
	c := &Client{base: server, version: version, http: &http.Client{Timeout: requestTimeout}}
	var result PairResult
	body := map[string]string{"code": code, "name": name, "version": version}
	status, err := c.post(ctx, []string{"pair"}, body, &result)
	if err != nil {
		return result, err
	}
	if status != http.StatusOK || result.Token == "" {
		return result, fmt.Errorf("the server answered %d without a token", status)
	}
	return result, nil
}

// NextJob claims the next job for this agent. It returns nil when there is
// nothing to print, and the polling interval the server asks for, if any.
func (c *Client) NextJob(ctx context.Context) (*Job, time.Duration, error) {
	var result struct {
		Job         *Job `json:"job"`
		PollSeconds int  `json:"poll_seconds"`
	}
	body := map[string]string{"version": c.version}
	if _, err := c.post(ctx, []string{"jobs", "next"}, body, &result); err != nil {
		return nil, 0, err
	}
	hint := time.Duration(result.PollSeconds) * time.Second
	if result.Job == nil {
		return nil, hint, nil
	}
	if !jobIDPattern.MatchString(result.Job.ID) {
		return nil, hint, fmt.Errorf("the server sent a job with an unusable id %q", result.Job.ID)
	}
	return result.Job, hint, nil
}

// Report tells the server how a job ended. A job the server no longer knows
// (already settled, expired or deleted) counts as reported: there is nothing
// left to tell.
func (c *Client) Report(ctx context.Context, id, status, errMsg string) error {
	if len(errMsg) > maxErrorLength {
		errMsg = errMsg[:maxErrorLength]
	}
	body := map[string]string{"status": status}
	if errMsg != "" {
		body["error"] = errMsg
	}
	_, err := c.post(ctx, []string{"jobs", id, "result"}, body, nil)
	var gone *statusError
	if errors.As(err, &gone) {
		switch gone.status {
		case http.StatusNotFound, http.StatusConflict, http.StatusGone:
			return nil
		}
	}
	return err
}

// statusError is a response outside the 2xx range.
type statusError struct {
	status  int
	message string
}

func (e *statusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("the server answered %d", e.status)
	}
	return fmt.Sprintf("the server answered %d: %s", e.status, e.message)
}

func (c *Client) post(ctx context.Context, path []string, body, into any) (int, error) {
	endpoint := c.base.JoinPath(append([]string{"api", "print-agent"}, path...)...)
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("cheflink-print-agent/%s (%s/%s)", c.version, runtime.GOOS, runtime.GOARCH))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach the server: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("the server's answer was cut short: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if c.token != "" {
			return resp.StatusCode, ErrUnauthorized
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &problem)
		return resp.StatusCode, &statusError{status: resp.StatusCode, message: problem.Error}
	}
	if into != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, into); err != nil {
			return resp.StatusCode, fmt.Errorf("the server's answer is not the expected JSON: %w", err)
		}
	}
	return resp.StatusCode, nil
}
