// Command probe measures, from inside a new pod, when each of a few TCP
// targets first accepts a connection from it. The beacon-ordering experiment
// (test/e2e/beacon_ordering_test.go, ADR 0045) runs it as a native sidecar,
// which starts right after the pod's network is set up.
//
// Every interval it starts a new connection attempt to each target that has
// not yet accepted, each with its own timeout, so attempts overlap: a CNI
// that drops the SYN costs that attempt its timeout but does not delay the
// next one. A target's time is when its first successful attempt was sent,
// measured on the process's monotonic clock from its start, so the moment the
// path opened lies within one interval before it.
//
// When every target has accepted (or -max has passed) it keeps probing for
// -settle to count attempts that fail after a success, prints one line
// "RESULT <json>" and idles. "probe sleep" just idles, for the main container.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

type targetFlag []string

func (f *targetFlag) String() string     { return strings.Join(*f, ",") }
func (f *targetFlag) Set(v string) error { *f = append(*f, v); return nil }

// Target is one target's outcome.
type Target struct {
	Addr string `json:"addr"`
	// FirstMs is when the first successful attempt was sent, in ms since the
	// probe started; -1 if none succeeded.
	FirstMs float64 `json:"firstMs"`
	// Attempts counts the attempts sent up to and including the first
	// success, or all of them if none succeeded.
	Attempts int `json:"attempts"`
	// Failures counts the failed attempts before the first success, by kind.
	Failures map[string]int `json:"failures"`
	// FailuresAfter counts the attempts sent after the first success that
	// failed anyway (a flapping path).
	FailuresAfter int `json:"failuresAfter"`
}

// Result is what the probe prints.
type Result struct {
	IntervalMs float64            `json:"intervalMs"`
	TimeoutMs  float64            `json:"timeoutMs"`
	StartUnix  int64              `json:"startUnixNano"`
	PodIPs     []string           `json:"podIPs"`
	Targets    map[string]*Target `json:"targets"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sleep" {
		idle()
	}
	var targets targetFlag
	flag.Var(&targets, "target", "name=host:port to probe (repeatable)")
	interval := flag.Duration("interval", 20*time.Millisecond, "time between attempts per target")
	timeout := flag.Duration("timeout", 200*time.Millisecond, "timeout of one attempt")
	maxWait := flag.Duration("max", 3*time.Minute, "give up on targets that have not accepted by then")
	settle := flag.Duration("settle", time.Second, "keep probing this long after the last first success")
	flag.Parse()

	start := time.Now()
	res := &Result{
		IntervalMs: ms(*interval),
		TimeoutMs:  ms(*timeout),
		StartUnix:  start.UnixNano(),
		PodIPs:     podIPs(),
		Targets:    map[string]*Target{},
	}
	for _, t := range targets {
		name, addr, ok := strings.Cut(t, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "bad -target %q, want name=host:port\n", t)
			os.Exit(2)
		}
		res.Targets[name] = &Target{Addr: addr, FirstMs: -1, Failures: map[string]int{}}
	}

	var mu sync.Mutex
	var firstAt []time.Time // per target, when it first succeeded (zero until then)
	names := make([]string, 0, len(res.Targets))
	for name := range res.Targets {
		names = append(names, name)
		firstAt = append(firstAt, time.Time{})
	}

	var wg sync.WaitGroup
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	deadline := start.Add(*maxWait)
	var doneAt time.Time
	for now := range tick.C {
		mu.Lock()
		all := true
		for i := range names {
			if firstAt[i].IsZero() {
				all = false
			}
		}
		mu.Unlock()
		if all && doneAt.IsZero() {
			doneAt = now
		}
		if (!doneAt.IsZero() && now.Sub(doneAt) >= *settle) || now.After(deadline) {
			break
		}
		for i, name := range names {
			t := res.Targets[name]
			sent := time.Now()
			mu.Lock()
			if firstAt[i].IsZero() {
				t.Attempts++
			}
			n := t.Attempts
			mu.Unlock()
			wg.Add(1)
			go func(i, n int, t *Target, sent time.Time) {
				defer wg.Done()
				err := dial(t.Addr, *timeout)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil && (firstAt[i].IsZero() || sent.Before(firstAt[i])):
					firstAt[i] = sent
					t.FirstMs = ms(sent.Sub(start))
					t.Attempts = n
				case err != nil && !firstAt[i].IsZero() && sent.After(firstAt[i]):
					t.FailuresAfter++
				case err != nil:
					t.Failures[kind(err)]++
				}
			}(i, n, t, sent)
		}
	}
	wg.Wait()
	out, _ := json.Marshal(res)
	fmt.Printf("RESULT %s\n", out)
	idle()
}

func idle() {
	for {
		time.Sleep(time.Hour)
	}
}

func dial(addr string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// kind names a failure: kube-router rejects, Calico and Cilium drop.
func kind(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	default:
		return "other"
	}
}

func podIPs() []string {
	var ips []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() {
			ips = append(ips, n.IP.String())
		}
	}
	return ips
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
