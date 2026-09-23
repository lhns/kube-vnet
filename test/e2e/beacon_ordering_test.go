//go:build e2e_experiment

package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The beacon-ordering experiment (ADR 0045), run by the manually triggered
// e2e-experiment workflow. It asks whether, when a node's beacon first
// accepts a new pod, that node has also applied the pod's vnet rules. kind
// never reproduces the startup race, so a first-connection test can't answer
// it; this measures the order directly.
//
// Each probe pod is a member of vnet net1 on the worker node, without the
// network-wait annotation. Its first container is a native sidecar running
// test/e2e/probe, which starts right after the pod's network is set up and
// dials, every interval, three targets until each accepts:
//
//   - beacon: the beacon on the control-plane node, the wait's witness there;
//   - vnet: a member of net1 on the control-plane node, reachable only
//     through net1's membership policy;
//   - local: the beacon on the pod's own node, for reference only.
//
// delta = vnet - beacon, both the send time of the first successful attempt.
// The witness holds for a pod if delta <= one interval. The test fails only
// on infrastructure errors, unless E2E_EXPERIMENT_ASSERT=true.
//
// Knobs (environment): E2E_EXPERIMENT_PROBES (probe pods, default 20),
// E2E_EXPERIMENT_BURST (pods created at once, default 5),
// E2E_EXPERIMENT_POLICIES (extra NetworkPolicies created first, default 0),
// E2E_EXPERIMENT_CHURN (extra NetworkPolicies created and deleted per second
// while the probes run, default 0), E2E_EXPERIMENT_INTERVAL (default 20ms),
// E2E_EXPERIMENT_TIMEOUT (one attempt, default 200ms), E2E_EXPERIMENT_IMAGE
// (default kube-vnet-probe:e2e, loaded into kind), E2E_EXPERIMENT_OUT
// (directory for raw.json, summary.md and row.md; none if empty).
func TestExperiment_BeaconOrdering(t *testing.T) {
	cfg := experimentConfig{
		Probes:   envInt(t, "E2E_EXPERIMENT_PROBES", 20),
		Burst:    envInt(t, "E2E_EXPERIMENT_BURST", 5),
		Policies: envInt(t, "E2E_EXPERIMENT_POLICIES", 0),
		Churn:    envInt(t, "E2E_EXPERIMENT_CHURN", 0),
		Interval: envDuration(t, "E2E_EXPERIMENT_INTERVAL", 20*time.Millisecond),
		Timeout:  envDuration(t, "E2E_EXPERIMENT_TIMEOUT", 200*time.Millisecond),
		Image:    envString("E2E_EXPERIMENT_IMAGE", "kube-vnet-probe:e2e"),
		CNI:      cni(),
	}
	out := os.Getenv("E2E_EXPERIMENT_OUT")
	assert := os.Getenv("E2E_EXPERIMENT_ASSERT") == "true"
	if cfg.Burst < 1 {
		cfg.Burst = 1
	}
	t.Logf("config: %+v", cfg)

	ns := uniqueNS(t, "order")
	ensureNamespace(t, ns, nil)
	t.Cleanup(func() { cleanupNamespace(t, ns) })
	applyYAML(t, vnetSpec("net1", ns, ""))

	applyYAML(t, pinnedPod(ns, "server", `
  nodeSelector:
    node-role.kubernetes.io/control-plane: ""
  tolerations:
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule
  containers:
    - name: web
      image: `+testImage+`
      args: ["netexec", "--http-port=80"]`, ""))
	waitForPod(t, ns, "server", 2*time.Minute)
	server := net.JoinHostPort(podIP(t, ns, "server"), "80")

	worker := workerNode(t)
	var local, remote string
	for node, ip := range beaconsByNode(t) {
		if node == worker {
			local = net.JoinHostPort(ip, "9444")
		} else {
			remote = net.JoinHostPort(ip, "9444")
		}
	}
	if local == "" || remote == "" {
		t.Fatalf("want a beacon on %s and one on another node, got local %q remote %q", worker, local, remote)
	}

	// Load: policies selecting every pod in the namespace, each with its own
	// peer selector that matches every pod, on a port nothing listens on. So
	// every new pod joins each policy's peer set (Calico's IP sets, Cilium's
	// selector cache, kube-router's ipsets) on every node, and nothing opens
	// the server's port 80 or the beacons. The policies share loadPorts ports:
	// Cilium keeps one policy map entry per peer identity and port for each
	// endpoint, and a distinct port per policy overflows that map (16k
	// entries) at about 1000 policies.
	for i := 0; i < cfg.Policies; i += 100 {
		var docs []string
		for j := i; j < min(i+100, cfg.Policies); j++ {
			docs = append(docs, loadPolicy(ns, fmt.Sprintf("load-%d", j), j))
		}
		applyYAML(t, strings.Join(docs, "---\n"))
	}
	stopChurn := startChurn(t, ns, cfg.Churn)

	var results []podResult
	for b := 0; b < cfg.Probes; b += cfg.Burst {
		var names, docs []string
		for i := b; i < min(b+cfg.Burst, cfg.Probes); i++ {
			name := fmt.Sprintf("probe-%d", i)
			names = append(names, name)
			docs = append(docs, probePod(ns, name, worker, cfg, remote, server, local))
		}
		applyYAML(t, strings.Join(docs, "---\n"))
		deadline := time.Now().Add(5 * time.Minute)
		for _, name := range names {
			results = append(results, awaitProbe(t, ns, name, deadline))
		}
	}
	churned := stopChurn()

	s := summarize(cfg, results)
	s.Churned = churned
	t.Log("\n" + s.text())
	if out != "" {
		writeExperiment(t, out, cfg, s, results)
	}
	if s.Missing > 0 {
		t.Errorf("%d probe pods never reached both the beacon and the vnet server", s.Missing)
	}
	if assert && s.Lagged > 0 {
		t.Errorf("on %s the vnet rule lagged the beacon by more than %s for %d of %d pods",
			cfg.CNI, cfg.Interval, s.Lagged, s.N)
	}
}

type experimentConfig struct {
	CNI      string        `json:"cni"`
	Probes   int           `json:"probes"`
	Burst    int           `json:"burst"`
	Policies int           `json:"policies"`
	Churn    int           `json:"churnPerSecond"`
	Interval time.Duration `json:"interval"`
	Timeout  time.Duration `json:"timeout"`
	Image    string        `json:"image"`
}

// probeTarget and probeResult mirror test/e2e/probe's output.
type probeTarget struct {
	Addr          string         `json:"addr"`
	FirstMs       float64        `json:"firstMs"`
	Attempts      int            `json:"attempts"`
	Failures      map[string]int `json:"failures"`
	FailuresAfter int            `json:"failuresAfter"`
}

type probeResult struct {
	IntervalMs float64                 `json:"intervalMs"`
	TimeoutMs  float64                 `json:"timeoutMs"`
	StartUnix  int64                   `json:"startUnixNano"`
	PodIPs     []string                `json:"podIPs"`
	Targets    map[string]*probeTarget `json:"targets"`
}

type podResult struct {
	Pod      string       `json:"pod"`
	Probe    *probeResult `json:"probe,omitempty"`
	BeaconMs float64      `json:"beaconMs"`
	VnetMs   float64      `json:"vnetMs"`
	LocalMs  float64      `json:"localMs"`
	DeltaMs  float64      `json:"deltaMs"`
	Complete bool         `json:"complete"`
}

func probePod(ns, name, node string, cfg experimentConfig, beacon, vnet, local string) string {
	return pinnedPod(ns, name, fmt.Sprintf(`
  nodeSelector:
    kubernetes.io/hostname: %s
  terminationGracePeriodSeconds: 1
  initContainers:
    - name: probe
      image: %s
      imagePullPolicy: Never
      restartPolicy: Always
      args: ["-target", "beacon=%s", "-target", "vnet=%s", "-target", "local=%s",
             "-interval", "%s", "-timeout", "%s"]
      resources:
        requests: {cpu: 10m, memory: 16Mi}
  containers:
    - name: main
      image: %s
      imagePullPolicy: Never
      args: ["sleep"]
      resources:
        requests: {cpu: 1m, memory: 8Mi}`, node, cfg.Image, beacon, vnet, local, cfg.Interval, cfg.Timeout, cfg.Image), "")
}

func loadPolicy(ns, name string, port int) string {
	return fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: %s
  namespace: %s
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchExpressions:
              - {key: %s, operator: DoesNotExist}
      ports:
        - {protocol: TCP, port: %d}
`, name, ns, name, 20000+port%loadPorts)
}

// startChurn creates perSecond policies every second and deletes those from
// two seconds earlier, until the returned func is called; that returns how
// many it created. Errors are logged, not fatal: churn is only load.
func startChurn(t *testing.T, ns string, perSecond int) func() int {
	if perSecond <= 0 {
		return func() int { return 0 }
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	created := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for round := 0; ; round++ {
			var docs []string
			for i := range perSecond {
				n := round*perSecond + i
				docs = append(docs, loadPolicy(ns, fmt.Sprintf("churn-%d", n), 30000+n))
			}
			if err := kubectlStdin(strings.Join(docs, "---\n"), "apply", "-f", "-"); err != nil {
				t.Logf("churn: %v", err)
			} else {
				created += perSecond
			}
			if old := round - 2; old >= 0 {
				var names []string
				for i := range perSecond {
					names = append(names, fmt.Sprintf("churn-%d", old*perSecond+i))
				}
				args := append([]string{"delete", "networkpolicy", "-n", ns, "--ignore-not-found", "--wait=false"}, names...)
				if err := kubectlStdin("", args...); err != nil {
					t.Logf("churn: %v", err)
				}
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	return func() int {
		close(stop)
		wg.Wait()
		return created
	}
}

// kubectlStdin runs kubectl with stdin, for use off the test goroutine.
func kubectlStdin(stdin string, args ...string) error {
	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kubectl %s: %v: %s", args[0], err, out)
	}
	return nil
}

// loadPorts is how many distinct ports the load policies use.
const loadPorts = 20

// awaitProbe waits until deadline for the probe's RESULT line.
func awaitProbe(t *testing.T, ns, pod string, deadline time.Time) podResult {
	t.Helper()
	r := podResult{Pod: pod, BeaconMs: -1, VnetMs: -1, LocalMs: -1, DeltaMs: math.NaN()}
	var logs string
	for ; time.Now().Before(deadline); time.Sleep(time.Second) {
		logs, _ = kubectl(t, "logs", "-n", ns, pod, "-c", "probe")
		for _, line := range strings.Split(logs, "\n") {
			raw, ok := strings.CutPrefix(strings.TrimSpace(line), "RESULT ")
			if !ok {
				continue
			}
			var p probeResult
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				t.Errorf("%s: bad result %q: %v", pod, raw, err)
				return r
			}
			r.Probe = &p
			first := func(name string) float64 {
				if tg := p.Targets[name]; tg != nil {
					return tg.FirstMs
				}
				return -1
			}
			r.BeaconMs, r.VnetMs, r.LocalMs = first("beacon"), first("vnet"), first("local")
			if r.BeaconMs >= 0 && r.VnetMs >= 0 {
				r.Complete = true
				r.DeltaMs = r.VnetMs - r.BeaconMs
			}
			return r
		}
	}
	t.Errorf("%s: no result in time; logs:\n%s", pod, logs)
	return r
}

type experimentSummary struct {
	CNI       string
	Config    experimentConfig
	N         int
	Missing   int
	Lagged    int // delta > interval: the vnet rule came later than the witness
	Ahead     int // delta < -interval: the vnet rule came first
	Churned   int
	Delta     [3]float64 // min, median, max
	Beacon    [2]float64 // median, max
	Vnet      [2]float64
	Local     [2]float64
	Failures  map[string]map[string]int // target -> kind -> count, before the first success
	FlapAfter map[string]int            // target -> failed attempts after the first success
	Results   []podResult
}

func summarize(cfg experimentConfig, results []podResult) experimentSummary {
	s := experimentSummary{CNI: cfg.CNI, Config: cfg, Results: results,
		Failures: map[string]map[string]int{}, FlapAfter: map[string]int{}}
	intervalMs := float64(cfg.Interval) / float64(time.Millisecond)
	var deltas, beacons, vnets, locals []float64
	for _, r := range results {
		if r.Probe != nil {
			for name, tg := range r.Probe.Targets {
				if s.Failures[name] == nil {
					s.Failures[name] = map[string]int{}
				}
				for k, v := range tg.Failures {
					s.Failures[name][k] += v
				}
				s.FlapAfter[name] += tg.FailuresAfter
			}
		}
		if !r.Complete {
			s.Missing++
			continue
		}
		s.N++
		deltas = append(deltas, r.DeltaMs)
		beacons = append(beacons, r.BeaconMs)
		vnets = append(vnets, r.VnetMs)
		if r.LocalMs >= 0 {
			locals = append(locals, r.LocalMs)
		}
		if r.DeltaMs > intervalMs {
			s.Lagged++
		}
		if r.DeltaMs < -intervalMs {
			s.Ahead++
		}
	}
	s.Delta = [3]float64{minOf(deltas), median(deltas), maxOf(deltas)}
	s.Beacon = [2]float64{median(beacons), maxOf(beacons)}
	s.Vnet = [2]float64{median(vnets), maxOf(vnets)}
	s.Local = [2]float64{median(locals), maxOf(locals)}
	return s
}

func (s experimentSummary) load() string {
	return fmt.Sprintf("%d policies, churn %d/s (%d created)", s.Config.Policies, s.Config.Churn, s.Churned)
}

// row is one line of the cross-CNI table; rowHeader is its header.
const rowHeader = "| CNI | pods | load | beacon ms (median / max) | vnet ms (median / max) | delta ms (min / median / max) | vnet > 1 interval after beacon | vnet > 1 interval before beacon |\n" +
	"|---|---|---|---|---|---|---|---|\n"

func (s experimentSummary) row() string {
	return fmt.Sprintf("| %s | %d | %s | %.0f / %.0f | %.0f / %.0f | %.0f / %.0f / %.0f | %d | %d |\n",
		s.CNI, s.N, s.load(), s.Beacon[0], s.Beacon[1], s.Vnet[0], s.Vnet[1],
		s.Delta[0], s.Delta[1], s.Delta[2], s.Lagged, s.Ahead)
}

func (s experimentSummary) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "beacon ordering on %s: %d pods (%d incomplete), interval %s, attempt timeout %s, load %s\n",
		s.CNI, s.N, s.Missing, s.Config.Interval, s.Config.Timeout, s.load())
	fmt.Fprintf(&b, "%-10s %10s %10s %10s %10s\n", "pod", "beacon", "vnet", "local", "delta")
	for _, r := range s.Results {
		fmt.Fprintf(&b, "%-10s %10.1f %10.1f %10.1f %10.1f\n", r.Pod, r.BeaconMs, r.VnetMs, r.LocalMs, r.DeltaMs)
	}
	fmt.Fprintf(&b, "delta ms min/median/max: %.1f / %.1f / %.1f; vnet after beacon by > %s: %d; before by > %s: %d\n",
		s.Delta[0], s.Delta[1], s.Delta[2], s.Config.Interval, s.Lagged, s.Config.Interval, s.Ahead)
	fmt.Fprintf(&b, "first success ms median/max: beacon %.0f/%.0f, vnet %.0f/%.0f, local beacon %.0f/%.0f\n",
		s.Beacon[0], s.Beacon[1], s.Vnet[0], s.Vnet[1], s.Local[0], s.Local[1])
	fmt.Fprintf(&b, "failed attempts before the first success: %v; after it: %v\n", s.Failures, s.FlapAfter)
	return b.String()
}

func (s experimentSummary) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Beacon ordering on %s\n\n", s.CNI)
	fmt.Fprintf(&b, "%d probe pods in bursts of %d (%d incomplete), probe interval %s, attempt timeout %s, load %s. "+
		"Times are ms from the probe's start (≈ the pod's network being set up); delta = vnet − beacon.\n\n",
		s.N, s.Config.Burst, s.Missing, s.Config.Interval, s.Config.Timeout, s.load())
	b.WriteString(rowHeader)
	b.WriteString(s.row())
	fmt.Fprintf(&b, "\nLocal beacon median/max: %.0f / %.0f ms. Failed attempts before the first success: %v; after it: %v.\n\n",
		s.Local[0], s.Local[1], s.Failures, s.FlapAfter)
	b.WriteString("<details><summary>per pod</summary>\n\n| pod | beacon | vnet | local | delta |\n|---|---|---|---|---|\n")
	for _, r := range s.Results {
		fmt.Fprintf(&b, "| %s | %.1f | %.1f | %.1f | %.1f |\n", r.Pod, r.BeaconMs, r.VnetMs, r.LocalMs, r.DeltaMs)
	}
	b.WriteString("\n</details>\n")
	return b.String()
}

func writeExperiment(t *testing.T, dir string, cfg experimentConfig, s experimentSummary, results []podResult) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("output dir: %v", err)
	}
	// NaN (an incomplete pod's delta) is not JSON.
	for i := range results {
		if math.IsNaN(results[i].DeltaMs) {
			results[i].DeltaMs = 0
		}
	}
	raw, err := json.MarshalIndent(map[string]any{"config": cfg, "results": results}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for name, data := range map[string]string{
		"raw.json":   string(raw),
		"summary.md": s.markdown(),
		"row.md":     s.row(),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func envString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

func envDuration(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func minOf(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	return slices.Min(xs)
}

func maxOf(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	return slices.Max(xs)
}
