package controller

import "testing"

func TestPluralize(t *testing.T) {
	cases := []struct {
		name     string
		n        int
		singular string
		plural   string
		want     string
	}{
		{"one_no_format", 1, "1 namespace", "%d namespaces", "1 namespace"},
		{"zero_uses_plural", 0, "1 NetworkPolicy", "%d NetworkPolicies", "0 NetworkPolicies"},
		{"two_uses_plural", 2, "1 namespace", "%d namespaces", "2 namespaces"},
		{"large_uses_plural", 47, "1 pod", "%d pods", "47 pods"},
		// The singular form is passed through verbatim. No formatting.
		{"singular_form_literal", 1, "exactly one", "%d", "exactly one"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pluralize(c.n, c.singular, c.plural); got != c.want {
				t.Errorf("pluralize(%d, %q, %q) = %q, want %q", c.n, c.singular, c.plural, got, c.want)
			}
		})
	}
}
