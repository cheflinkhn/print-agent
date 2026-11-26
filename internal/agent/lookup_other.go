//go:build !linux && !windows

package agent

import (
	"context"
	"errors"
	"net/netip"
	"runtime"
)

// The lookup is written for the systems the agent is installed on: Windows
// and Linux. Anywhere else a job prints by its address alone.

func probeMAC(context.Context, MAC, []probe) ([]netip.Addr, error) {
	return nil, errors.New("looking a printer up by its MAC is not supported on " + runtime.GOOS)
}

func neighbourMAC(context.Context, netip.Addr, netip.Addr) (MAC, bool) {
	return MAC{}, false
}
