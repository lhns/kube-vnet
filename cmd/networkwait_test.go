package main

import "testing"

// The wait is a convenience: whatever its arguments, it must exit 0 so the
// pod starts, never fail the init container.
func TestRunNetworkWait_BadArgumentsDoNotBlockThePod(t *testing.T) {
	for _, args := range [][]string{
		{"--no-such-flag"},
		{"--beacons=no-port"},
		{"--beacons=beacons:9444", "--max-wait=0s"},
		{"--beacons=beacons:9444", "--max-wait=soon"},
	} {
		if code := runNetworkWait(args); code != 0 {
			t.Errorf("runNetworkWait(%q) = %d, want 0", args, code)
		}
	}
}
