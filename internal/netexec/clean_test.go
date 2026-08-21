// internal/netexec/clean_test.go
// Unit tests for Normalize, StripEchoAndPrompt, and prompt detection.
package netexec

import (
	"regexp"
	"strings"
	"testing"
)

var prompt = regexp.MustCompile(DefaultPromptRegex)

// Captured live (2026-08-21) from an ArubaOS-Switch 3810M's response to
// "no page": the switch redraws its prompt at a fixed screen row via
// absolute cursor addressing rather than a trailing newline. Before
// cursorToColumnOneRe existed, Normalize collapsed this into
// "no pagelab-sw01#" as one line, which matched no prompt regex and timed
// out the caller.
const arubaSwitchNoPageResponse = "\x1b[60;11Hno page\x1b[60;11H\x1b[?25h\x1b[60;18H\x1b[1;0H\x1b[1M\x1b[60;1H\x1b[1L\x1b[60;18H\x1b[60;1H\x1b[2K\x1b[60;1H\x1b[?25h\x1b[60;1H\x1b[1;60r\x1b[60;1H\x1b[1;60r\x1b[60;1H\x1b[60;1H\x1b[2K\x1b[60;1H\x1b[?25h\x1b[60;1H\x1b[60;1Hlab-sw01# \x1b[60;1H\x1b[60;11H\x1b[60;1H\x1b[?25h\x1b[60;11H"

func TestNormalizeSeparatesCursorAddressedPrompt(t *testing.T) {
	got := Normalize(arubaSwitchNoPageResponse)
	if strings.Contains(got, "pagelab-sw01") {
		t.Fatalf("echo and prompt still glued together: %q", got)
	}
	if !endsAtPrompt(got, prompt) {
		t.Errorf("endsAtPrompt(%q) = false, want true", got)
	}
	if got := lastLine(got); got != "lab-sw01#" {
		t.Errorf("lastLine = %q, want %q", got, "lab-sw01#")
	}
}

// The column-11/18 repositioning within the echoed line must NOT be read
// as a line break -- only a move all the way back to column 0/1 should be.
// If it were, this same fixture would already have separated "no" from
// "page" (the cursor revisits column 11 mid-echo), which it must not.
func TestNormalizeKeepsEchoedCommandIntact(t *testing.T) {
	got := Normalize(arubaSwitchNoPageResponse)
	if !strings.Contains(got, "no page") {
		t.Errorf("echoed command was split or lost: %q", got)
	}
}

// Captured live (2026-08-21) from a real Comware 7 switch's response to
// "display version": every line ends in "\r\r\n" (a doubled CR) rather than
// "\r\n". Before crlfRunRe existed, the bare-CR overwrite handling read the
// leftover CR as "discard everything before me on this line", which wiped
// out every line's actual content -- the classifier saw nothing but blank
// lines where "HP Comware Software, Version 7.1.045..." should have been,
// and Fingerprint reported "unknown" for a device that was answering fine.
const comware7DisplayVersionResponse = "display version\r\r\nHP Comware Software, Version 7.1.045, Release 2311P05\r\r\nCopyright (c) 2010-2014 Hewlett-Packard Development Company, L.P.\r\r\nHP 5900AF-48XG-4QSFP+ Switch uptime is 0 weeks, 0 days, 0 hours, 32 minutes\r\r\nLast reboot reason : Power on\r\r\n\r\r\nBoot image: flash:/5900_5920-cmw710-boot-r2311p05.bin\r\r\nBoot image version: 7.1.045P21, Release 2311P05\r\r\n  Compiled Dec 29 2014 16:29:56\r\r\nSystem image: flash:/5900_5920-cmw710-system-r2311p05.bin\r\r\nSystem image version: 7.1.045, Release 2311P05\r\r\n  Compiled Dec 29 2014 16:30:10\r\r\n\r\r\nSlot 1\r\r\nHP 5900AF-48XG-4QSFP+ Switch with 2 Processors\r\r\nLast reboot reason : Power on\r\r\n2048M   bytes SDRAM\r\r\n4M      bytes Nor Flash Memory\r\r\n512M    bytes Nand Flash Memory\r\r\nConfig Register points to Nand Flash\r\r\n\r\r\nHardware Version is Ver.B\r\r\nCPLDA Version is 002, CPLDB Version is 002\r\r\nBootRom Version is 132\r\r\n---- More ----"

func TestNormalizeSurvivesDoubledCRLineEndings(t *testing.T) {
	got := Normalize(comware7DisplayVersionResponse)
	if !strings.Contains(got, "HP Comware Software, Version 7.1.045") {
		t.Fatalf("real content was wiped out by the doubled CR: %q", got)
	}
	wantLines := []string{
		"display version",
		"HP Comware Software, Version 7.1.045, Release 2311P05",
		"Copyright (c) 2010-2014 Hewlett-Packard Development Company, L.P.",
		"Hardware Version is Ver.B",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("expected line %q missing from normalized output: %q", line, got)
		}
	}
}

func TestNormalizeStripsAnsiAndCR(t *testing.T) {
	in := "\x1b[32mlab-r1#\x1b[0m\r\nline1\r\nprogress 10%\rprogress done\r\n"
	want := "lab-r1#\nline1\nprogress done\n"
	if got := Normalize(in); got != want {
		t.Fatalf("Normalize:\n got %q\nwant %q", got, want)
	}
}

func TestStripEchoAndPrompt(t *testing.T) {
	raw := "show version\nIOS XE 17.9.4a\nuptime 12 weeks\nlab-r1#"
	got := StripEchoAndPrompt(raw, "show version", prompt)
	want := "IOS XE 17.9.4a\nuptime 12 weeks"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestStripEchoWithPromptPrefix(t *testing.T) {
	// Some stacks echo "prompt command" on one line.
	raw := "lab-r1# show clock\n10:15:03.201 UTC Tue Jul 28 2026\nlab-r1#"
	got := StripEchoAndPrompt(raw, "show clock", prompt)
	want := "10:15:03.201 UTC Tue Jul 28 2026"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestStripKeepsNonEchoFirstLine(t *testing.T) {
	// First line is real output, not echo — must survive.
	raw := "hostname lab-r1\nlab-r1#"
	got := StripEchoAndPrompt(raw, "show run | include hostname", prompt)
	want := "hostname lab-r1"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Both real-world examples this regex was built from, confirmed live:
// ArubaOS-Switch's login banner ("Press any key to continue") and
// ExtremeXOS's own pager caught before paging was disabled ("Press <SPACE>
// to continue or <Q> to quit:").
func TestLooksLikeContinuePrompt(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"Press any key to continue", true},
		{"Press <SPACE> to continue or <Q> to quit:", true},
		{"-- MORE --, next page: Space, next line: Enter, quit: q", true}, // ArubaOS-CX pager, confirmed live
		{"--More--", true},
		{"banner line\nPress any key to continue", true}, // only the last line matters
		{"lab-r1#", false},
		{"building configuration...", false},
		{"", false},
		{"Press any key to continue\nmore output after it", false}, // no longer the last line
	}
	for _, c := range cases {
		if got := looksLikeContinuePrompt(c.text); got != c.want {
			t.Errorf("looksLikeContinuePrompt(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestPromptDetection(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"lab-r1#", true},
		{"lab-r1>", true},
		{"lab-r1(config)#", true},
		{"user@lab-host:~$", true},
		{"lab-fw1 %", false},                  // space before % — not a prompt tail
		{"lab-sw2% ", true},                   // trailing space is fine
		{"* Slot-1 lab-exos1.1 #", true},  // EXOS stack: "* " member marker
		{"no star prefix here %", false},       // still rejected without the "* " marker
		{"building configuration...", false},
		{"", false},
		{"output line\nlab-r1#", true},
		{"lab-r1#\nmore output still streaming", false},
	}
	for _, c := range cases {
		if got := endsAtPrompt(c.text, prompt); got != c.want {
			t.Errorf("endsAtPrompt(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
