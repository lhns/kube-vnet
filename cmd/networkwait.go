package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/lhns/kube-vnet/internal/networkwait"
)

// Subcommands of the operator binary, so the network wait needs no image of
// its own (ADR 0045):
//
//	network-wait    the init container the webhook injects into opted-in pods
//	network-beacon  the per-node listener the chart's DaemonSet runs
func runSubcommand(args []string) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "network-wait":
		return true, runNetworkWait(args[1:])
	case "network-beacon":
		return true, runNetworkBeacon(args[1:])
	}
	return false, 0
}

func runNetworkWait(args []string) int {
	fs := flag.NewFlagSet("network-wait", flag.ContinueOnError)
	beacons := fs.String("beacons", "", "beacon Service address, host:port")
	maxWait := fs.Duration("max-wait", 30*time.Second, "longest to hold the pod")
	parseErr := fs.Parse(args)
	host, port, err := net.SplitHostPort(*beacons)
	if parseErr != nil || err != nil || *maxWait <= 0 {
		// Never block a pod over a bad argument: the wait is a convenience.
		fmt.Fprintf(os.Stderr, "network-wait: invalid arguments (beacons %q, max-wait %v); not waiting\n", *beacons, *maxWait)
		return 0
	}

	res := networkwait.Wait(context.Background(), networkwait.Config{Host: host, Port: port, MaxWait: *maxWait})
	for _, ip := range sortedKeys(res.Accepted) {
		fmt.Printf("network-wait: beacon %s accepted after %v\n", ip, res.Accepted[ip].Round(time.Millisecond))
	}
	if res.Released {
		fmt.Printf("network-wait: all %d beacons accepted after %v; starting\n",
			len(res.Accepted), res.Elapsed.Round(time.Millisecond))
	} else {
		fmt.Printf("network-wait: max wait %v reached; starting anyway. Beacons that never accepted: %v\n",
			*maxWait, res.Pending)
	}
	return 0
}

func runNetworkBeacon(args []string) int {
	fs := flag.NewFlagSet("network-beacon", flag.ContinueOnError)
	port := fs.Int("port", 9444, "TCP port to listen on")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("network-beacon: listening on :%d\n", *port)
	if err := networkwait.Serve(ctx, fmt.Sprintf(":%d", *port)); err != nil {
		fmt.Fprintf(os.Stderr, "network-beacon: %v\n", err)
		return 1
	}
	return 0
}

func sortedKeys(m map[string]time.Duration) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
