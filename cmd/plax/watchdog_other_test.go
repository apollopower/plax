//go:build !linux

package main

import "testing"

func TestCLI_RunPlaxPackageWatchdogBoundary(t *testing.T) {
	t.Skip("runPlax watchdog containment requires Linux child-subreaper support")
}
