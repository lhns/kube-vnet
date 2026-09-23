package networkwait

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeNet scripts DNS answers and which beacons accept, per round.
type fakeNet struct {
	mu      sync.Mutex
	addrs   []string
	accepts map[string]int // dials refused before a beacon starts accepting; <0 never
	dials   map[string]int
}

func (f *fakeNet) resolve(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.addrs...), nil
}

func (f *fakeNet) dial(_ context.Context, addr string) error {
	ip, _, _ := net.SplitHostPort(addr)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials[ip]++
	n, ok := f.accepts[ip]
	if !ok || n < 0 || f.dials[ip] <= n {
		return errors.New("connection refused")
	}
	return nil
}

func (f *fakeNet) set(addrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrs = addrs
}

func newFake(accepts map[string]int, addrs ...string) *fakeNet {
	return &fakeNet{addrs: addrs, accepts: accepts, dials: map[string]int{}}
}

func run(f *fakeNet, maxWait time.Duration) Result {
	return Wait(context.Background(), Config{
		Host: "beacons", Port: "9444", MaxWait: maxWait,
		Interval: time.Millisecond, DialTimeout: 10 * time.Millisecond,
		Resolve: f.resolve, Dial: f.dial,
	})
}

func TestWait_ReleasesWhenEveryBeaconAccepts(t *testing.T) {
	f := newFake(map[string]int{"10.0.0.1": 0, "10.0.0.2": 0}, "10.0.0.1", "10.0.0.2")
	res := run(f, time.Second)
	if !res.Released || len(res.Accepted) != 2 || len(res.Pending) != 0 {
		t.Fatalf("want released with 2 accepted, got %+v", res)
	}
}

// A node that hasn't applied the pod yet refuses; the wait keeps trying it
// and releases once it accepts.
func TestWait_RetriesARefusingBeacon(t *testing.T) {
	f := newFake(map[string]int{"10.0.0.1": 0, "10.0.0.2": 5}, "10.0.0.1", "10.0.0.2")
	res := run(f, time.Second)
	if !res.Released {
		t.Fatalf("want released after retries, got %+v", res)
	}
	if f.dials["10.0.0.2"] != 6 {
		t.Errorf("refusing beacon dialled %d times, want 6", f.dials["10.0.0.2"])
	}
	if f.dials["10.0.0.1"] != 1 {
		t.Errorf("an accepted beacon was dialled again (%d times)", f.dials["10.0.0.1"])
	}
}

// Timeout-based by design: a beacon that never accepts delays the pod by at
// most MaxWait, then lets it start.
func TestWait_ReleasesAtMaxWait(t *testing.T) {
	f := newFake(map[string]int{"10.0.0.1": 0, "10.0.0.2": -1}, "10.0.0.1", "10.0.0.2")
	start := time.Now()
	res := run(f, 50*time.Millisecond)
	if res.Released {
		t.Fatalf("released although a beacon never accepted: %+v", res)
	}
	if !reflect.DeepEqual(res.Pending, []string{"10.0.0.2"}) {
		t.Errorf("pending = %v, want [10.0.0.2]", res.Pending)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("wait overran its maximum: %v", took)
	}
}

// No beacons resolved yet (DNS not ready, or the DaemonSet not up) is not
// success: the wait keeps going until some appear.
func TestWait_EmptyDNSIsNotSuccess(t *testing.T) {
	f := newFake(map[string]int{"10.0.0.1": 0})
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.set("10.0.0.1")
	}()
	res := run(f, time.Second)
	if !res.Released || len(res.Accepted) != 1 {
		t.Fatalf("want release once the beacon appeared, got %+v", res)
	}
}

// Addresses are re-resolved each round: a beacon that disappears (its node
// went down) stops being waited on.
func TestWait_FollowsAddressChanges(t *testing.T) {
	f := newFake(map[string]int{"10.0.0.1": 0, "10.0.0.2": -1}, "10.0.0.1", "10.0.0.2")
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.set("10.0.0.1")
	}()
	res := run(f, time.Second)
	if !res.Released || !reflect.DeepEqual(res.Accepted, map[string]time.Duration{"10.0.0.1": res.Accepted["10.0.0.1"]}) {
		t.Fatalf("want release on the remaining beacon only, got %+v", res)
	}
}

func TestWait_StopsOnCancel(t *testing.T) {
	f := newFake(map[string]int{"10.0.0.1": -1}, "10.0.0.1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := Wait(ctx, Config{Host: "beacons", Port: "9444", MaxWait: time.Minute,
		Interval: time.Millisecond, Resolve: f.resolve, Dial: f.dial})
	if res.Released {
		t.Fatalf("released on a cancelled context: %+v", res)
	}
}

// The real beacon and the real dialer, over loopback.
func TestServe_AcceptsAndWaitReleases(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr) }()

	host, port, _ := net.SplitHostPort(addr)
	res := Wait(context.Background(), Config{
		Host: host, Port: port, MaxWait: 5 * time.Second,
		Resolve: func(context.Context, string) ([]string, error) { return []string{host}, nil },
	})
	if !res.Released {
		t.Fatalf("real beacon never accepted: %+v", res)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop on cancel")
	}
}

// failingListener fails every Accept, counting the calls.
type failingListener struct {
	net.Listener
	mu    sync.Mutex
	calls int
}

func (l *failingListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return nil, errors.New("accept: too many open files")
}

func (l *failingListener) Close() error { return nil }

// A persistent Accept error backs off instead of spinning.
func TestServe_BacksOffOnAcceptError(t *testing.T) {
	ln := &failingListener{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := serve(ctx, ln); err != nil {
		t.Fatalf("serve: %v", err)
	}
	// 5+10+20+40ms fit in 100ms; a spin would make millions of calls.
	ln.mu.Lock()
	defer ln.mu.Unlock()
	if ln.calls > 10 {
		t.Errorf("%d Accept calls in 100ms; want a backoff", ln.calls)
	}
}
