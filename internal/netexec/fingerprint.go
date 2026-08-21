// internal/netexec/fingerprint.go
// Platform fingerprinting: identify the NOS by combining paging-disable
// probes with a version command.
//
// The trick that makes this cheap: paging-disable commands are a clean
// binary signal. The right one for the platform produces no output; the
// wrong one produces an error marker ("% Invalid input", "syntax error",
// "Unrecognized command", ...). One probe therefore narrows to a family,
// and the family's version command refines to the exact platform.
//
// HARD CONSTRAINT on the probe table: every command in it must be
// exec-level, read-only, and side-effect-free on every platform it might
// be blindly thrown at. Paging changes are session-local on all listed
// platforms. Never add a probe that could touch config.
package netexec

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Platform is the result of a successful (or best-effort) fingerprint.
type Platform struct {
	// Name is the detected platform, e.g. "arista_eos", "cisco_iosxe",
	// "juniper_junos". "unknown" when nothing matched.
	Name string
	// PagingDisable is the paging command this platform accepted ("" for
	// platforms without paging, e.g. mikrotik, linux).
	PagingDisable string
	// VersionCommand is the command whose output identified the platform.
	VersionCommand string
	// VersionOutput is that command's cleaned output, kept so callers can
	// mine model/version details without a second round trip.
	VersionOutput string
}

// cliErrorRe matches the error markers network CLIs emit for an
// unrecognized command.
var cliErrorRe = regexp.MustCompile(`(?im)^\s*(%|\^|error[:\s]|invalid input|syntax error|unknown command|unrecognized command|bad command|incomplete command|expecting|failure:)|command not found`)

// isCLIError reports whether output looks like a command rejection rather
// than real output.
func isCLIError(out string) bool {
	return cliErrorRe.MatchString(out)
}

// Bounds on what LooksLikeRejection will consider. A refusal is one or two
// lines; anything longer that happens to contain a marker is real output that
// merely looks alarming.
const (
	rejectionMaxBytes = 240
	rejectionMaxLines = 4
)

// LooksLikeRejection reports whether out is a CLI refusing a command rather
// than answering it.
//
// This exists because a rejection is not an error at the transport layer: the
// device accepted the line, replied, and returned to its prompt, so the read
// succeeds and the caller gets a perfectly valid string that happens to mean
// "no". A caller that stores whatever it reads will file that refusal as
// content.
//
// The size bounds are the whole reason this is separate from isCLIError.
// cliErrorRe is multiline and anchors on things like a leading "%", which real
// captures do contain — a banner line inside a running-config being the
// obvious one. Restricting the question to output too short to BE a capture
// keeps that from turning a config into a false negative, at the cost of
// missing a refusal that arrives with a wall of help text. That trade only
// goes one way on purpose: failing to detect a refusal stores junk that a
// human will spot, while a false positive silently discards a real capture.
func LooksLikeRejection(out string) bool {
	t := strings.TrimSpace(out)
	if t == "" || len(t) > rejectionMaxBytes || strings.Count(t, "\n") >= rejectionMaxLines {
		return false
	}
	return isCLIError(t)
}

// versionClass maps version-command output to a platform name.
type versionClass struct {
	name  string
	match *regexp.Regexp
}

// probe is one fingerprint attempt: a paging command to try (optional) and
// a version command whose output is classified.
type probe struct {
	paging     string
	versionCmd string
	classes    []versionClass

	// pagingBenign, when set, additionally treats a paging response that
	// matches this pattern as accepted rather than rejected, even though
	// the generic isCLIError check flags it as a rejection.
	//
	// Exists for Comware, confirmed live (2026-08-21): its
	// "screen-length disable" toggle reports its own already-applied
	// state as "% Screen-length configuration is disabled for current
	// user." -- an informational notice, not a rejection -- but it
	// starts with '%', the same marker Cisco/Juniper use for genuine
	// command errors. Loosening the shared isCLIError check to stop
	// treating a leading '%' as an error would reopen exactly the
	// rejection detection it exists for on those platforms, so this
	// keeps the exception scoped to the one probe that needs it instead.
	pagingBenign *regexp.Regexp
}

// probes are ordered by prevalence: the terminal-length-0 family first
// (IOS/IOS-XE/NX-OS/EOS all take it), then Junos, ASA, and the long tail.
var probes = []probe{
	{
		paging:     "terminal length 0",
		versionCmd: "show version",
		classes: []versionClass{
			{"arista_eos", regexp.MustCompile(`(?i)\bArista\b`)},
			{"cisco_nxos", regexp.MustCompile(`(?i)NX-OS|Nexus`)},
			{"cisco_iosxe", regexp.MustCompile(`(?i)IOS[ -]XE`)},
			{"cisco_ios", regexp.MustCompile(`(?i)Cisco IOS Software`)},
		},
	},
	{
		paging:     "set cli screen-length 0",
		versionCmd: "show version",
		classes: []versionClass{
			{"juniper_junos", regexp.MustCompile(`(?i)JUNOS|Junos`)},
		},
	},
	{
		paging:     "terminal pager 0",
		versionCmd: "show version",
		classes: []versionClass{
			{"cisco_asa", regexp.MustCompile(`(?i)Adaptive Security Appliance`)},
		},
	},
	{
		paging:       "screen-length disable",
		versionCmd:   "display version",
		pagingBenign: regexp.MustCompile(`(?i)screen-length configuration is disabled`),
		classes: []versionClass{
			{"huawei_vrp", regexp.MustCompile(`(?i)Huawei|VRP`)},
			{"hp_comware", regexp.MustCompile(`(?i)Comware|HPE?`)},
		},
	},
	{
		// ExtremeXOS. "disable clipaging" is a session-scoped toggle
		// (same shape as Cisco's "terminal length 0"), confirmed
		// against the ExtremeXOS User Guide.
		//
		// MUST stay ordered before the aruba_cx probe below: EXOS also
		// happens to accept "show system" as valid syntax (confirmed
		// live against a real X450G2 stack), and unlike aruba_cx this
		// probe gates its version command behind a paging command that
		// EXOS actually understands. Any probe with a real paging step
		// is safe to put first purely because a device that doesn't
		// recognize the paging command never reaches the version
		// command at all — see the aruba_cx comment below for what goes
		// wrong when that gate is missing.
		paging:     "disable clipaging",
		versionCmd: "show version",
		classes: []versionClass{
			{"extreme_exos", regexp.MustCompile(`(?i)ExtremeXOS`)},
		},
	},
	{
		// "no page" is confirmed live (2026-08-21) to disable paging on
		// ArubaOS-CX -- the same command ArubaOS-Switch (ProVision)
		// uses, apparently carried over as a legacy-compatible alias.
		// This was NOT known when this probe was first written: it
		// originally shipped with no paging step at all, on the
		// (wrong) assumption that ArubaOS-CX's own documentation
		// didn't mention a pager toggle because it didn't have one.
		// That assumption caused a real, live-confirmed hang: probed
		// against a real ExtremeXOS stack before the extreme_exos
		// probe above existed, this probe's own "show system" version
		// command turned out to ALSO be valid EXOS syntax, and with no
		// paging step to gate against that, it hung the whole
		// fingerprint attempt on EXOS's "Press <SPACE> to continue"
		// prompt until the connect timeout. Kept ordered after every
		// other paging-gated probe regardless, both for that history
		// and because it must still run before aruba_procurve for the
		// unrelated reason given in that probe's comment.
		//
		// Probed with `show system` rather than `show version`: it is
		// the command this platform's own docs confirm, and its
		// "ArubaOS-CX Version" field is unambiguous, whereas
		// ArubaOS-CX's `show version` output (if any) is unconfirmed
		// and risks colliding with the "Aruba" match just below.
		paging:     "no page",
		versionCmd: "show system",
		classes: []versionClass{
			{"aruba_cx", regexp.MustCompile(`(?i)ArubaOS-CX`)},
		},
	},
	{
		// ArubaOS-Switch (ProVision) — the older, non-CX Aruba/HP
		// ProCurve line. "ProCurve|Aruba|HP" is broad enough to also
		// match ArubaOS-CX's own `show version` output if that command
		// exists there too, which is why the aruba_cx probe above runs
		// first and unconditionally wins on a match.
		//
		// "Image stamp:" is also matched. Confirmed live (2026-08-21)
		// that a real 3810M's `show version` carries no vendor name
		// text at all -- no "Aruba", "ProCurve", or "HP" anywhere,
		// just a build path, a date, and a software revision string
		// like "KB.16.10.0024". "Image stamp:" is the field label
		// that IS present, and it is the thing this probe actually
		// classifies on for that firmware; the vendor-name alternation
		// stays for any build/model that does print one.
		paging:     "no page",
		versionCmd: "show version",
		classes: []versionClass{
			{"aruba_procurve", regexp.MustCompile(`(?i)ProCurve|Aruba|HP|Image stamp:`)},
		},
	},
	{
		// No paging concept on these; version command alone.
		paging:     "",
		versionCmd: "/system resource print",
		classes: []versionClass{
			{"mikrotik_routeros", regexp.MustCompile(`(?i)RouterOS|MikroTik`)},
		},
	},
	{
		paging:     "",
		versionCmd: "uname -a",
		classes: []versionClass{
			{"linux", regexp.MustCompile(`(?i)\bLinux\b`)},
			{"bsd_or_mac", regexp.MustCompile(`(?i)Darwin|BSD`)},
		},
	},
}

// classify returns the platform name for version output, or "".
func classify(out string, classes []versionClass) string {
	for _, c := range classes {
		if c.match.MatchString(out) {
			return c.name
		}
	}
	return ""
}

// Fingerprint identifies the platform of an open session. On success the
// accepted paging-disable command has already been applied, so the session
// is ready for long-output commands. When every probe misses, it returns a
// best-effort Platform{Name: "unknown"} carrying whatever paging command
// stuck and the last usable version output, plus a nil error — callers
// distinguish by Name. A non-nil error means the session itself broke.
func Fingerprint(ctx context.Context, s *Session) (*Platform, error) {
	var (
		bestPaging  string
		bestVersion string
		bestVerCmd  string
	)
	for _, p := range probes {
		if p.paging != "" {
			out, err := s.Run(ctx, p.paging)
			if err != nil {
				return nil, fmt.Errorf("fingerprint probe %q: %w", p.paging, err)
			}
			if isCLIError(out) && (p.pagingBenign == nil || !p.pagingBenign.MatchString(out)) {
				continue // wrong family — next probe
			}
			bestPaging = p.paging
		}
		out, err := s.Run(ctx, p.versionCmd)
		if err != nil {
			return nil, fmt.Errorf("fingerprint probe %q: %w", p.versionCmd, err)
		}
		if isCLIError(out) {
			continue
		}
		if strings.TrimSpace(out) != "" {
			bestVersion, bestVerCmd = out, p.versionCmd
		}
		if name := classify(out, p.classes); name != "" {
			return &Platform{
				Name:           name,
				PagingDisable:  p.paging,
				VersionCommand: p.versionCmd,
				VersionOutput:  out,
			}, nil
		}
	}
	return &Platform{
		Name:           "unknown",
		PagingDisable:  bestPaging,
		VersionCommand: bestVerCmd,
		VersionOutput:  bestVersion,
	}, nil
}
