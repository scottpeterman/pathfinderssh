// internal/netexec/live_test.go
//
// netexec driven against a real SSH server that behaves like a device.
//
// The existing tests in this package are string tests: they check that
// Normalize and classify do the right thing to text handed to them
// directly. Everything between opening a shell and getting clean output
// back — the first-prompt wait, paging disable, echo stripping across
// chunk boundaries, the probe ladder in Fingerprint — had never executed
// in a test at all. This file is that half.
//
// External test package on purpose: these exercise the exported surface a
// capture engine will use, and fakedev imports netexec, so an internal
// test package here would be an import cycle.
package netexec_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scottpeterman/pathfinderssh/internal/fakedev"
	"github.com/scottpeterman/pathfinderssh/internal/netexec"
)

func session(t *testing.T, cfg fakedev.Config, opt netexec.Options) (*fakedev.Server, *netexec.Session) {
	t.Helper()
	srv, err := fakedev.Start(cfg)
	if err != nil {
		t.Fatalf("start device: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	client, err := srv.Dial("lab", "lab")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	if opt.CommandTimeout == 0 {
		opt.CommandTimeout = 5 * time.Second
	}
	sess, err := netexec.Open(context.Background(), client, opt)
	if err != nil {
		t.Fatalf("open shell: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return srv, sess
}

// The probe ladder, end to end, per platform. Junos is the interesting one:
// it has to fail the first probe on the device's own rejection text before
// the second probe can land, which is exactly the behavior that a
// too-polite fixture would break.
func TestFingerprintIdentifiesEachPlatform(t *testing.T) {
	cases := []struct {
		name       string
		cfg        fakedev.Config
		wantName   string
		wantPaging string
	}{
		{"ios", fakedev.IOS("lab-r1"), "cisco_ios", "terminal length 0"},
		{"eos", fakedev.EOS("lab-spine-1"), "arista_eos", "terminal length 0"},
		{"nxos", fakedev.NXOS("lab-sw1"), "cisco_nxos", "terminal length 0"},
		{"junos", fakedev.Junos("lab-edge-1"), "juniper_junos", "set cli screen-length 0"},
		{"linux", fakedev.Linux("lab-host"), "linux", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sess := session(t, tc.cfg, netexec.Options{})

			p, err := netexec.Fingerprint(context.Background(), sess)
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			if p.Name != tc.wantName {
				t.Errorf("Name = %q, want %q (version output: %q)",
					p.Name, tc.wantName, p.VersionOutput)
			}
			if p.PagingDisable != tc.wantPaging {
				t.Errorf("PagingDisable = %q, want %q", p.PagingDisable, tc.wantPaging)
			}
		})
	}
}

// Comware's "screen-length disable" reports its own already-applied state
// with a leading '%', which the generic isCLIError check reads as a
// rejection -- confirmed live (2026-08-21) against a real Comware 5
// switch, where this caused Fingerprint to skip straight past a working
// "display version" and report "unknown". probe.pagingBenign exists to
// stop that specific message from being read as "wrong platform."
func TestFingerprintAcceptsComwaresBenignPagingNotice(t *testing.T) {
	cfg := fakedev.Config{
		Prompt:            "<lab-core1>",
		AcceptAnyPassword: true,
		Commands: map[string]string{
			"screen-length disable": "% Screen-length configuration is disabled for current user.",
			"display version":       "HPE Comware Platform Software\nComware Software, Version 5.20.99, Release 5501P36",
		},
		Unknown: "% Unrecognized command found at '^' position.",
	}
	_, sess := session(t, cfg, netexec.Options{})

	p, err := netexec.Fingerprint(context.Background(), sess)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if p.Name != "hp_comware" {
		t.Errorf("Name = %q, want %q (version output: %q)", p.Name, "hp_comware", p.VersionOutput)
	}
}

// "no page" disables paging on ArubaOS-CX, confirmed live (2026-08-21) --
// the same command ArubaOS-Switch uses, apparently carried over as a
// legacy-compatible alias. This probe originally shipped with no paging
// step at all on the wrong assumption that ArubaOS-CX had none to offer;
// there was no end-to-end fingerprint test for this platform before now,
// only the regex-only classification case in TestClassifyVersions, which
// would not have caught the difference between "no page" being sent and
// not being sent at all.
func TestFingerprintDisablesPagingOnArubaCX(t *testing.T) {
	cfg := fakedev.Config{
		Prompt:            "lab-cx1#",
		AcceptAnyPassword: true,
		Commands: map[string]string{
			"no page":     "",
			"show system": "Vendor             : Aruba\nArubaOS-CX Version : GL.10.13.1050",
		},
		Unknown: "% Unrecognized command found at '^' position.",
	}
	_, sess := session(t, cfg, netexec.Options{})

	p, err := netexec.Fingerprint(context.Background(), sess)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if p.Name != "aruba_cx" {
		t.Errorf("Name = %q, want %q (version output: %q)", p.Name, "aruba_cx", p.VersionOutput)
	}
	if p.PagingDisable != "no page" {
		t.Errorf("PagingDisable = %q, want %q", p.PagingDisable, "no page")
	}
}

// A device nobody has a probe for must come back "unknown" with a nil
// error, not an error. The crawler treats those differently and the
// distinction has never been exercised against a live session.
func TestFingerprintReportsUnknownRatherThanFailing(t *testing.T) {
	cfg := fakedev.Config{
		Prompt:            "lab-widget>",
		AcceptAnyPassword: true,
		Unknown:           "% unrecognized command",
	}
	_, sess := session(t, cfg, netexec.Options{})

	p, err := netexec.Fingerprint(context.Background(), sess)
	if err != nil {
		t.Fatalf("fingerprint returned an error for an unknown device: %v", err)
	}
	if p.Name != "unknown" {
		t.Errorf("Name = %q, want %q", p.Name, "unknown")
	}
}

// Fingerprint's contract says the accepted paging command has already been
// applied when it returns, so the session is ready for long output. That
// is a claim about the wire, and this is the only way to check it.
func TestFingerprintLeavesPagingApplied(t *testing.T) {
	srv, sess := session(t, fakedev.IOS("lab-r1"), netexec.Options{})

	if _, err := netexec.Fingerprint(context.Background(), sess); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	var sawPaging bool
	for _, cmd := range srv.Asked() {
		if cmd == "terminal length 0" {
			sawPaging = true
		}
	}
	if !sawPaging {
		t.Errorf("paging was never disabled on the wire; asked: %v", srv.Asked())
	}
}

// Fingerprinting is not free. Every probe that misses costs a round trip,
// and against a device at the bottom of the ladder that is most of the
// list. Pinning the count makes reordering the probes a visible change.
func TestFingerprintCostIsBoundedForTheWorstCase(t *testing.T) {
	srv, sess := session(t, fakedev.Linux("lab-host"), netexec.Options{})

	if _, err := netexec.Fingerprint(context.Background(), sess); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	// Measured: 9 (was 7 before the aruba_cx and extreme_exos probes were
	// added). A device at the bottom of the ladder pays for every probe
	// above it, and each one is a round trip — on a link with 300ms of
	// latency that is over two seconds spent deciding the box is a
	// server. Set tight so reordering or extending the probes shows up
	// here rather than as an unexplained slow crawl.
	if n := len(srv.Asked()); n > 10 {
		t.Errorf("fingerprinting a Linux host took %d commands: %v", n, srv.Asked())
	}
}

func TestOpenAppliesPagingBeforeReturning(t *testing.T) {
	srv, _ := session(t, fakedev.IOS("lab-r1"), netexec.Options{
		PagingDisable: "terminal length 0",
	})

	got := srv.Asked()
	if len(got) != 1 || got[0] != "terminal length 0" {
		t.Errorf("Open sent %v, want just the paging command", got)
	}
}

// A banner is the normal case on real gear and it arrives before the first
// prompt, so it is the thing most likely to break a naive prompt wait.
func TestOpenWaitsPastTheBanner(t *testing.T) {
	cfg := fakedev.IOS("lab-r1")
	cfg.Banner = "Unauthorized access prohibited.\nThis system is monitored.\nlab notice"
	_, sess := session(t, cfg, netexec.Options{PagingDisable: "terminal length 0"})

	out, err := sess.Run(context.Background(), "show version")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out, "Unauthorized access") {
		t.Errorf("banner leaked into command output: %q", out)
	}
}

// Slow output split across many reads is the normal case on a loaded
// device, and prompt matching that works on one arrival can fail across
// several.
func TestOutputArrivingInPiecesIsAssembled(t *testing.T) {
	cfg := fakedev.IOS("lab-r1")
	cfg.ChunkSize = 3
	cfg.ChunkDelay = 500 * time.Microsecond
	_, sess := session(t, cfg, netexec.Options{PagingDisable: "terminal length 0"})

	out, err := sess.Run(context.Background(), "show running-config")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"hostname lab-r1", "router bgp 65001", "end"} {
		if !strings.Contains(out, want) {
			t.Errorf("chunked output missing %q; got %q", want, out)
		}
	}
}

// Run takes exactly one command. The multi-command form was removed
// deliberately, and this proves the guard fires before anything reaches
// the device rather than after.
func TestRunRefusesMultipleCommandsWithoutTouchingTheDevice(t *testing.T) {
	srv, sess := session(t, fakedev.IOS("lab-r1"), netexec.Options{})

	if _, err := sess.Run(context.Background(), "show version\nshow running-config"); err == nil {
		t.Fatal("multi-command Run was accepted")
	}
	if got := srv.Asked(); len(got) != 0 {
		t.Errorf("device saw %v; the guard should fire before the write", got)
	}
}

func TestRunAfterCloseFails(t *testing.T) {
	_, sess := session(t, fakedev.IOS("lab-r1"), netexec.Options{})

	sess.Close()
	if _, err := sess.Run(context.Background(), "show version"); err == nil {
		t.Fatal("Run on a closed session returned no error")
	}
}
