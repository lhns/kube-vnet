package main

import (
	"net/http/httptest"
	"slices"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

type recordedProbes struct {
	healthz, readyz map[string]healthz.Checker
}

func (r *recordedProbes) AddHealthzCheck(name string, c healthz.Checker) error {
	r.healthz[name] = c
	return nil
}

func (r *recordedProbes) AddReadyzCheck(name string, c healthz.Checker) error {
	r.readyz[name] = c
	return nil
}

func sortedNames(m map[string]healthz.Checker) []string {
	var names []string
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Readiness waits for the webhook server only when the webhook is enabled;
// liveness is a ping either way, so a webhook that cannot serve pulls the
// replica out of the Service instead of restarting it.
func TestAddProbes(t *testing.T) {
	req := httptest.NewRequest("GET", "/readyz", nil)
	for _, tc := range []struct {
		name       string
		webhook    bool
		wantReadyz []string
		wantReady  bool
	}{
		{name: "webhook disabled", webhook: false, wantReadyz: []string{"readyz"}, wantReady: true},
		{name: "webhook enabled, server not started", webhook: true, wantReadyz: []string{"readyz", "webhook"}, wantReady: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var started healthz.Checker
			if tc.webhook {
				started = webhook.NewServer(webhook.Options{Port: 9443}).StartedChecker()
			}
			r := &recordedProbes{healthz: map[string]healthz.Checker{}, readyz: map[string]healthz.Checker{}}
			if err := addProbes(r, started); err != nil {
				t.Fatalf("addProbes: %v", err)
			}
			if got := sortedNames(r.healthz); !slices.Equal(got, []string{"healthz"}) {
				t.Errorf("healthz checks = %v, want [healthz]", got)
			}
			if err := r.healthz["healthz"](req); err != nil {
				t.Errorf("healthz failed: %v", err)
			}
			if got := sortedNames(r.readyz); !slices.Equal(got, tc.wantReadyz) {
				t.Errorf("readyz checks = %v, want %v", got, tc.wantReadyz)
			}
			ready := true
			for name, check := range r.readyz {
				if err := check(req); err != nil {
					t.Logf("readyz %s: %v", name, err)
					ready = false
				}
			}
			if ready != tc.wantReady {
				t.Errorf("ready = %v, want %v", ready, tc.wantReady)
			}
		})
	}
}
