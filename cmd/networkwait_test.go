package main

import (
	"strings"
	"testing"
	"time"

	"github.com/lhns/kube-vnet/internal/networkwait"
)

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

// A timeout names what was missing: the beacons that never accepted, or the
// Service itself when it never resolved to any.
func TestWaitOutcome_Timeout(t *testing.T) {
	const host = "kube-vnet-network-beacon.kube-vnet-system.svc"
	for _, tc := range []struct {
		name string
		res  networkwait.Result
		want string
	}{
		{"never resolved", networkwait.Result{}, "beacon Service " + host + " never resolved"},
		{"beacons pending", networkwait.Result{Pending: []string{"10.0.0.2"}}, "never accepted: [10.0.0.2]"},
	} {
		if got := waitOutcome(tc.res, host, 30*time.Second); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q does not mention %q", tc.name, got, tc.want)
		}
	}
}
