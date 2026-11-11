//go:build !windows

package main

import (
	"context"
	"errors"
)

var errWindowsOnly = errors.New("the service commands only exist on Windows; on this system use \"agent run\" under systemd or a similar supervisor")

func runningAsService() bool { return false }

func runService(func(context.Context) error) error { return errWindowsOnly }

func installService(string) error { return errWindowsOnly }

func uninstallService() error { return errWindowsOnly }

func startService() error { return errWindowsOnly }

func stopService() error { return errWindowsOnly }
