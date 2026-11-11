// Command agent is the Cheflink print agent. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"cheflink-print-agent/internal/agent"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

const usage = `Cheflink print agent %s

Fetches kitchen chits from Cheflink and prints them on a network printer.

Usage:
  agent pair -server URL -code CODE   Connect this PC to a venue
  agent run                           Fetch and print until stopped
  agent test-print -printer IP:PORT   Print a test line, without Cheflink
  agent version                       Print the version

Windows service (from an administrator prompt):
  agent install                       Register the service; it starts with Windows
  agent start | stop                  Start or stop the service
  agent uninstall                     Remove the service

Every command takes -dir FOLDER, where config.json, state.json and agent.log
are kept. The default is the folder the program is in.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	command, args := os.Args[1], os.Args[2:]

	var err error
	switch command {
	case "pair":
		err = cmdPair(args)
	case "run":
		err = cmdRun(args)
	case "test-print":
		err = cmdTestPrint(args)
	case "install":
		err = withDir(command, args, installService)
	case "uninstall":
		err = withDir(command, args, func(string) error { return uninstallService() })
	case "start":
		err = withDir(command, args, func(string) error { return startService() })
	case "stop":
		err = withDir(command, args, func(string) error { return stopService() })
	case "version", "-version", "--version":
		fmt.Println(version)
	case "help", "-h", "-help", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newFlags builds a command's flag set with the shared -dir flag.
func newFlags(command string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	dir := fs.String("dir", "", "folder for config.json, state.json and agent.log (default: the program's folder)")
	return fs, dir
}

// resolveDir returns the agent's folder as an absolute path, creating it.
func resolveDir(dir string) (string, error) {
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		dir = filepath.Dir(exe)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0o755)
}

func withDir(command string, args []string, do func(dir string) error) error {
	fs, dirFlag := newFlags(command)
	fs.Parse(args)
	dir, err := resolveDir(*dirFlag)
	if err != nil {
		return err
	}
	return do(dir)
}

func cmdPair(args []string) error {
	fs, dirFlag := newFlags("pair")
	server := fs.String("server", "", "the Cheflink address the venue signs in at, e.g. https://example.com")
	code := fs.String("code", "", "the pairing code from the venue's page")
	hostname, _ := os.Hostname()
	name := fs.String("name", hostname, "how this PC shows up on the venue's page")
	insecure := fs.Bool("allow-insecure-http", false, "development only: allow plain http to a server on another machine")
	fs.Parse(args)

	if *server == "" || *code == "" {
		return errors.New("pair needs -server and -code")
	}
	dir, err := resolveDir(*dirFlag)
	if err != nil {
		return err
	}
	base, err := agent.CheckServerURL(*server, *insecure)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := agent.Pair(ctx, base, *code, *name, version)
	if err != nil {
		return err
	}
	cfg := agent.Config{
		ServerURL:         *server,
		Token:             result.Token,
		AgentName:         *name,
		AllowInsecureHTTP: *insecure,
	}
	path := filepath.Join(dir, agent.ConfigFileName)
	if err := cfg.Save(path); err != nil {
		return err
	}
	if result.Venue != "" {
		fmt.Printf("Paired with %s.\n", result.Venue)
	} else {
		fmt.Println("Paired.")
	}
	fmt.Printf("Saved to %s. A running agent picks it up by itself.\n", path)
	return nil
}

func cmdRun(args []string) error {
	fs, dirFlag := newFlags("run")
	fs.Parse(args)
	dir, err := resolveDir(*dirFlag)
	if err != nil {
		return err
	}

	// As a Windows service there is no console: log to a file. In a
	// terminal, log to the terminal.
	asService := runningAsService()
	var out io.Writer = os.Stderr
	if asService {
		file, err := agent.OpenLogFile(filepath.Join(dir, agent.LogFileName))
		if err != nil {
			return err
		}
		defer file.Close()
		out = file
	}
	logger := log.New(out, "", log.LstdFlags)

	ledger, warning := agent.OpenLedger(filepath.Join(dir, agent.StateFileName))
	if warning != nil {
		logger.Printf("warning: %v", warning)
	}
	a := agent.New(filepath.Join(dir, agent.ConfigFileName), ledger, logger, version)

	if asService {
		return runService(a.Run)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}

// cmdTestPrint prints one line and cuts, with no server involved: it proves
// this PC can reach the printer.
func cmdTestPrint(args []string) error {
	fs, _ := newFlags("test-print")
	target := fs.String("printer", "", "the printer's address, e.g. 192.168.1.50:9100")
	fs.Parse(args)
	if *target == "" {
		return errors.New("test-print needs -printer IP:PORT")
	}
	host, portText, err := net.SplitHostPort(*target)
	if err != nil {
		return fmt.Errorf("-printer must look like 192.168.1.50:9100: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("-printer must look like 192.168.1.50:9100: port %q is not a number", portText)
	}

	// ESC @ (initialize), the text, five line feeds, GS V 0 (full cut).
	data := []byte{0x1B, 0x40}
	data = append(data, "Cheflink print agent "+version+"\n"...)
	data = append(data, time.Now().Format("2006-01-02 15:04:05")+"\n\n\n\n\n"...)
	data = append(data, 0x1D, 0x56, 0x00)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	printer := agent.Printer{Host: host, Port: port}
	if err := agent.SendToPrinter(ctx, printer, data); err != nil {
		return err
	}
	fmt.Printf("Sent %d bytes to %s. The printer should print two lines and cut.\n", len(data), printer)
	return nil
}
