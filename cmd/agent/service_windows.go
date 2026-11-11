//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName        = "CheflinkPrintAgent"
	serviceDisplayName = "Cheflink Print Agent"
	serviceDescription = "Fetches kitchen chits from Cheflink and prints them on the venue's network printer."

	// After a crash Windows restarts the service; the failure count resets
	// after a day without one.
	restartDelay     = 5 * time.Second
	failureResetSecs = 24 * 60 * 60
	stopWait         = 30 * time.Second
)

func runningAsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

// runService hands run to the Windows service manager and returns when the
// service has stopped.
func runService(run func(context.Context) error) error {
	return svc.Run(serviceName, &handler{run: run})
}

type handler struct {
	run func(context.Context) error
}

func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			if err != nil {
				return false, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		}
	}
}

func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Windows service manager (run this from an administrator prompt): %w", err)
	}
	return m, nil
}

func openService() (*mgr.Mgr, *mgr.Service, error) {
	m, err := connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(serviceName)
	if err != nil {
		m.Disconnect()
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nil, errors.New("the service is not installed; run \"agent install\" first")
		}
		return nil, nil, err
	}
	return m, s, nil
}

// installService registers the agent to start with Windows, running from
// where the program is now and keeping its files in dir.
func installService(dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(serviceName); err == nil {
		existing.Close()
		return errors.New("the service is already installed; run \"agent uninstall\" first to change it")
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		StartType:   mgr.StartAutomatic,
	}, "run", "-dir", dir)
	if err != nil {
		return err
	}
	defer s.Close()

	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: restartDelay}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, failureResetSecs); err != nil {
		return fmt.Errorf("installed, but could not set it to restart after a crash: %w", err)
	}
	fmt.Printf("Installed the %q service. It runs %s and keeps its files in %s.\n", serviceDisplayName, exe, dir)
	fmt.Println("Start it now with \"agent start\"; it also starts by itself with Windows.")
	return nil
}

func uninstallService() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	// Stop it first if it runs; a service that is not running answers with
	// an error, which is fine.
	_, _ = s.Control(svc.Stop)
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Printf("Removed the %q service.\n", serviceDisplayName)
	return nil
}

func startService() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if err := s.Start(); err != nil {
		return err
	}
	fmt.Printf("Started the %q service.\n", serviceDisplayName)
	return nil
}

func stopService() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	status, err := s.Control(svc.Stop)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(stopWait)
	for status.State != svc.Stopped {
		if time.Now().After(deadline) {
			return errors.New("the service did not stop in time")
		}
		time.Sleep(300 * time.Millisecond)
		if status, err = s.Query(); err != nil {
			return err
		}
	}
	fmt.Printf("Stopped the %q service.\n", serviceDisplayName)
	return nil
}
