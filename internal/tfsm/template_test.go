// internal/tfsm/template_test.go
//
// A template that fails to compile is not a parsing edge case -- it means
// the collector can never produce a single record for that platform, ever,
// on any input, and nothing short of running a live crawl would reveal it.
// That is exactly what happened with the first draft of the extreme_exos
// template ("Value Filldown Required NAME (...)" instead of the
// comma-joined "Value Required,Filldown NAME (...)" TextFSM actually wants):
// it built clean, every other test passed, and it only surfaced against a
// real device. This test exists so the next syntax slip fails `go test`
// instead of a live crawl.
package tfsm

import "testing"

// TestEveryRegisteredTemplateCompiles calls Parse with empty input for
// every (platform, key) pair in the selection table. Empty input is
// deliberate: a template that finds no records is a legitimate, common
// result and not what this test is checking. What it is checking is that
// gotextfsm's own ParseString accepted the template's Value/Start syntax at
// all -- a compile error there surfaces as Parse returning a non-nil error
// wrapping "template %s: %w", which is what this test looks for.
func TestEveryRegisteredTemplateCompiles(t *testing.T) {
	for platform, cmds := range selection {
		for key := range cmds {
			if _, err := Parse(platform, key, ""); err != nil {
				t.Errorf("%s/%s: template does not compile: %v", platform, key, err)
			}
		}
	}
}
