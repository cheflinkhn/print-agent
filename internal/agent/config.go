// Package agent is the Cheflink print agent: it asks a Cheflink server for
// print jobs and writes each job's bytes to a printer on the local network.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	// ConfigFileName and the files below live together in the agent's folder.
	ConfigFileName = "config.json"
	StateFileName  = "state.json"
	LogFileName    = "agent.log"

	defaultPoll = 2 * time.Second
	minPoll     = 1 * time.Second
	maxPoll     = 60 * time.Second
)

// Config is what pairing writes to config.json.
type Config struct {
	// ServerURL is the Cheflink address the venue signs in at.
	ServerURL string `json:"server_url"`
	// Token identifies this agent to the server. Pairing issues it.
	Token string `json:"token"`
	// AgentName is how the agent shows up on the venue's page.
	AgentName string `json:"agent_name"`
	// PollSeconds is how often to ask for jobs when the server gives no hint.
	PollSeconds int `json:"poll_seconds,omitempty"`
	// AllowInsecureHTTP lets the token travel over plain http to a server
	// that is not on this machine. For development only.
	AllowInsecureHTTP bool `json:"allow_insecure_http,omitempty"`
}

// LoadConfig reads and checks config.json.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, fmt.Errorf("not paired yet: %s does not exist (run the pair command)", path)
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if cfg.Token == "" {
		return cfg, fmt.Errorf("not paired yet: %s has no token (run the pair command)", path)
	}
	if _, err := CheckServerURL(cfg.ServerURL, cfg.AllowInsecureHTTP); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Save writes the config where only its owner can read it: it holds the token.
func (c Config) Save(path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(raw, '\n'), 0o600)
}

// PollInterval is the configured polling interval, within sane bounds.
func (c Config) PollInterval() time.Duration {
	if c.PollSeconds <= 0 {
		return defaultPoll
	}
	return clampPoll(time.Duration(c.PollSeconds) * time.Second)
}

func clampPoll(d time.Duration) time.Duration {
	return min(max(d, minPoll), maxPoll)
}

// CheckServerURL parses the server address and refuses plain http unless the
// server is on this machine or the caller explicitly allows it, so the token
// is never sent in the clear by accident.
func CheckServerURL(raw string, allowInsecure bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("server address %q is not a URL like https://example.com", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecure && !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("server address %q must use https", raw)
		}
	default:
		return nil, fmt.Errorf("server address %q must start with https://", raw)
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writeFileAtomic replaces path in one step, so a crash or a power cut
// mid-write never leaves half a file behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
