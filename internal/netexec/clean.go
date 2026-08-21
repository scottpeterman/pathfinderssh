// internal/netexec/clean.go
// Output normalization and deterministic cleaning.
//
// Port of reachssh's _strip_echo_and_prompt: remove the command echo from
// the first line and the trailing prompt from the last, so captured device
// output diffs cleanly — no phantom first/last-line changes between
// snapshots. Normalize additionally flattens the escape-sequence and CR
// noise that raw PTY streams carry, since this stack deliberately has no
// terminal emulator to absorb it.
package netexec

import (
	"regexp"
	"strings"
)

// ansiRe matches CSI sequences (colors, cursor moves), OSC sequences
// (window titles), and stray single-character escapes.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-_]`)

// cursorToColumnOneRe matches a CSI cursor-position escape that moves to
// column 0 or 1 of some row, e.g. "\x1b[60;1H".
//
// Confirmed live against an ArubaOS-Switch 3810M: it redraws its prompt at
// a fixed screen row using absolute cursor addressing instead of a
// trailing newline. A full terminal emulator like PuTTY renders that
// cursor jump as a fresh line; ansiRe's blanket escape-stripping would
// otherwise collapse the preceding text and the redrawn prompt together
// with nothing between them, e.g. the echoed command "no page" landing
// directly against the next prompt as "no pagelab-sw01#" -- which then
// matches no prompt regex at all and times out the caller.
//
// Recognizing this one specific sequence shape as a line break, before
// ansiRe strips it away, recovers the line boundary these devices convey
// through cursor position rather than through bytes. Deliberately narrow:
// column 11, 18, etc. (repositioning within the same line to place the
// cursor after echoed input) do not match, only a move all the way back to
// the start of a row does -- that is the distinction between "the device
// is repositioning within what it just drew" and "the device is starting
// a new line."
var cursorToColumnOneRe = regexp.MustCompile(`\x1b\[\d+;[01]H`)

// crlfRunRe matches one or more CRs immediately followed by an LF, e.g.
// "\r\n" or "\r\r\n".
//
// Confirmed live (2026-08-21) against a real Comware 7 switch (HP
// 5900AF): every line of its output ends in "\r\r\n" rather than "\r\n" --
// a doubled carriage return before the line feed. A plain "\r\n" -> "\n"
// replacement leaves the first of those two CRs behind, and the bare-CR
// overwrite handling immediately below then reads it as "discard
// everything on this line before me" -- the same rule that correctly
// collapses a progress bar's "10%\rdone" down to "done". Against this
// device it instead discarded EVERY line's actual content, turning real
// output ("HP Comware Software, Version 7.1.045...") into nothing but
// blank lines, which is why display version's real text never reached the
// classifier at all. Matching a whole run of CRs before the LF, rather
// than just one, collapses the doubled terminator to a single newline
// before the overwrite logic ever sees it, without changing behavior for
// a normal single "\r\n" or a genuine mid-line progress-bar CR (which is
// never immediately followed by "\n" -- see clean_test.go).
var crlfRunRe = regexp.MustCompile(`\r+\n`)

// Normalize strips ANSI escape sequences, resolves CR handling (a run of
// CRs immediately before an LF collapses to one LF, then a remaining lone
// CR keeps only the text after it — the overwrite semantics pagers and
// progress lines rely on), and drops NUL/BEL bytes.
func Normalize(s string) string {
	s = cursorToColumnOneRe.ReplaceAllString(s, "\n")
	s = ansiRe.ReplaceAllString(s, "")
	s = crlfRunRe.ReplaceAllString(s, "\n")
	if strings.ContainsRune(s, '\r') {
		lines := strings.Split(s, "\n")
		for i, line := range lines {
			if j := strings.LastIndexByte(line, '\r'); j >= 0 {
				lines[i] = line[j+1:]
			}
		}
		s = strings.Join(lines, "\n")
	}
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, "\x07", "")
	return s
}

// StripEchoAndPrompt removes the echoed command from the head of output and
// the prompt line from its tail. Conservative on the echo side: the first
// line is dropped only when it actually corresponds to the sent command
// (equal, or ends with it after the device re-wrapped/prefixed it).
func StripEchoAndPrompt(raw, cmd string, prompt *regexp.Regexp) string {
	lines := strings.Split(raw, "\n")

	// Trailing prompt.
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" || prompt.MatchString(last) {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}

	// Leading echo. Devices may echo "cmd", "prompt cmd", or a wrapped
	// form; treat a first line that equals or ends with the command as
	// echo. Some stacks emit a leading blank line first — skip those too.
	cmdTrim := strings.TrimSpace(cmd)
	for len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		if first == "" {
			lines = lines[1:]
			continue
		}
		if first == cmdTrim || strings.HasSuffix(first, cmdTrim) {
			lines = lines[1:]
		}
		break
	}

	return strings.TrimRight(strings.Join(lines, "\n"), " \t\n")
}
