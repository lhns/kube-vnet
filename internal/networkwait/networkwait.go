// Package networkwait holds a new pod's app until every node's CNI has
// applied the pod's NetworkPolicy rules (ADR 0045).
//
// Each node runs a beacon: a TCP listener that a NetworkPolicy opens to every
// pod. A node's CNI rejects a new pod at that node's beacon until it has
// programmed the pod's IP, so once every beacon accepts, every node has. The
// beacons are a sync witness, not a vnet probe: on kube-router, which rebuilds
// every rule on a node in one pass, the same rebuild that admitted the pod to
// the beacon also put it into its vnet rules.
//
// The wait is opt-in and bounded: it never blocks a pod longer than the
// maximum it was given, and it always lets the pod start.
package networkwait

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
	"time"
)

// Config configures Wait. Resolve and Dial are for tests; nil uses the real
// network.
type Config struct {
	// Host is the beacons' headless Service name; Port their port.
	Host string
	Port string
	// MaxWait bounds the whole wait.
	MaxWait time.Duration

	Interval    time.Duration // between rounds; default 150ms
	DialTimeout time.Duration // per connection attempt; default 500ms

	Resolve func(ctx context.Context, host string) ([]string, error)
	Dial    func(ctx context.Context, addr string) error
}

// Result reports how the wait ended.
type Result struct {
	// Released is true when every beacon accepted, false when MaxWait ran out.
	Released bool
	Elapsed  time.Duration
	// Accepted maps each beacon address to how long it took to accept.
	Accepted map[string]time.Duration
	// Pending lists the beacons that never accepted (on timeout).
	Pending []string
}

// Wait dials every beacon the Service resolves to until all of them have
// accepted a connection, or MaxWait runs out. Addresses are re-resolved every
// round, so beacons that appear or disappear (a node joining or going down)
// are picked up.
func Wait(ctx context.Context, cfg Config) Result {
	cfg = withDefaults(cfg)
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, cfg.MaxWait)
	defer cancel()

	accepted := map[string]time.Duration{}
	var current []string
	for {
		if addrs, err := cfg.Resolve(ctx, cfg.Host); err == nil {
			current = addrs
		}
		pending := notIn(current, accepted)

		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, ip := range pending {
			wg.Add(1)
			go func(ip string) {
				defer wg.Done()
				dctx, dcancel := context.WithTimeout(ctx, cfg.DialTimeout)
				defer dcancel()
				if cfg.Dial(dctx, net.JoinHostPort(ip, cfg.Port)) == nil {
					mu.Lock()
					accepted[ip] = time.Since(start)
					mu.Unlock()
				}
			}(ip)
		}
		wg.Wait()

		pending = notIn(current, accepted)
		if len(current) > 0 && len(pending) == 0 {
			return Result{Released: true, Elapsed: time.Since(start), Accepted: only(current, accepted)}
		}

		select {
		case <-ctx.Done():
			return Result{Elapsed: time.Since(start), Accepted: only(current, accepted), Pending: pending}
		case <-time.After(cfg.Interval):
		}
	}
}

// Serve accepts every TCP connection on addr and closes it at once, until ctx
// is done. The handshake is the whole signal.
func Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(ctx, ln)
}

func serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Back off like net/http does, so an error that persists (such
			// as running out of file descriptors) doesn't spin the CPU.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		_ = conn.Close()
	}
}

func withDefaults(cfg Config) Config {
	if cfg.Interval == 0 {
		cfg.Interval = 150 * time.Millisecond
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 500 * time.Millisecond
	}
	if cfg.Resolve == nil {
		cfg.Resolve = net.DefaultResolver.LookupHost
	}
	if cfg.Dial == nil {
		cfg.Dial = func(ctx context.Context, addr string) error {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				return err
			}
			return conn.Close()
		}
	}
	return cfg
}

// notIn returns the addresses of addrs not yet in accepted, sorted.
func notIn(addrs []string, accepted map[string]time.Duration) []string {
	var out []string
	for _, a := range addrs {
		if _, ok := accepted[a]; !ok {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// only restricts accepted to the addresses still published.
func only(addrs []string, accepted map[string]time.Duration) map[string]time.Duration {
	out := make(map[string]time.Duration, len(addrs))
	for _, a := range addrs {
		if d, ok := accepted[a]; ok {
			out[a] = d
		}
	}
	return out
}
