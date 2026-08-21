// internal/netexec/session.go
// The reachssh behavior layer: an interactive PTY shell driven by prompt
// detection, for network gear whose CLI does not support exec channels.
//
// This is the Go port of the Python reachssh execution model (invoke_shell,
// wait for prompt, disable paging, then send one command / read to prompt),
// built on sshcore instead of Paramiko. Three deliberate departures from the
// Python version:
//   - Run() sends EXACTLY the string it is given as one command. The old
//     comma-splitting behavior (a carryover from a CLI helper) is gone;
//     batching is the caller's loop.
//   - No terminal emulation. Output is a raw byte stream with ANSI escape
//     and CR normalization applied before prompt matching — gopyte stays
//     out of this stack entirely.
//   - Output is bounded and every wait takes a context. Both exist because
//     of what comes next: a neighbor table is kilobytes, a running-config is
//     megabytes, and a show tech-support is not bounded by anything the
//     caller knows in advance. See MaxOutputBytes below.
package netexec

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/scottpeterman/pathfinderssh/internal/sshcore"
)

// DefaultPromptRegex matches the trailing prompt line of most network
// operating systems and Unix shells: a non-whitespace run ending in
// # > $ or %, e.g. "lab-r1#", "lab-r1(config)#", "user@lab-host:~$".
//
// The second alternative exists for one specific known real-world shape:
// ExtremeXOS on a stack prefixes its prompt with the active member marker,
// e.g. "* Slot-1 lab-exos1.1 #" — a prompt built from multiple
// space-separated words, which the single-token first alternative cannot
// match at all.
//
// This is deliberately anchored on the literal "* " marker rather than
// "any line with a space before the terminal character": TestPromptDetection
// already carries `{"lab-fw1 %", false}` for a real platform that emits a
// non-prompt line shaped exactly like that, so "space before the terminal
// char" is proven, not just suspected, to be an unsafe general signal.
// Requiring the "* " prefix keeps the match narrow to the one shape it
// exists for, rather than reopening the "mid-output line looks prompt-ish"
// failure mode endsAtPrompt's single-line matching exists to avoid.
const DefaultPromptRegex = `(?m)^(?:[^\s]{1,80}|\*\s.{1,77})[#>\$%]\s*$`

// DefaultMaxOutputBytes bounds one command's output. Sized to hold any
// running-config comfortably and to refuse a show tech-support, because
// those are the two sides of the decision: the first is what this is for,
// the second is what would otherwise sit in memory once per concurrent
// device.
const DefaultMaxOutputBytes = 4 << 20 // 4 MiB

// tailKeep is how much of an over-limit stream stays buffered. Past the
// limit the content is already unusable, but the read has to keep going and
// the prompt still has to be recognized when it finally arrives — otherwise
// the command ends by timeout, the device is left mid-write, and the
// session cannot be reused. Prompt matching only reads the last line, so
// this is generous.
const tailKeep = 8 << 10

// ErrOutputTooLarge reports that a command produced more than
// Options.MaxOutputBytes. The output is NOT returned truncated: a partial
// running-config that looks like a whole one is worse than no capture at
// all, so callers get an error and decide. Test with errors.Is.
var ErrOutputTooLarge = errors.New("command output exceeded the configured limit")

// Options configures a Session. Zero values get sensible defaults.
type Options struct {
	// PromptRegex matches the device prompt on its own line. Defaults to
	// DefaultPromptRegex. This is the direct analog of reachssh's
	// prompt_regex platform hint.
	PromptRegex string

	// PagingDisable is sent once after the first prompt (e.g.
	// "terminal length 0"). Empty skips the step.
	PagingDisable string

	// CommandTimeout bounds each Run/expect. Default 30s.
	CommandTimeout time.Duration

	// ConnectTimeout bounds the wait for the FIRST prompt after the shell
	// opens (banners, MOTD, slow control planes). Default = CommandTimeout.
	ConnectTimeout time.Duration

	// MaxOutputBytes bounds a single command's output. Default
	// DefaultMaxOutputBytes; a negative value disables the bound, which is
	// only reasonable for a single-device tool that owns the machine.
	//
	// The bound is per command and per session, so the memory a run can
	// reach is roughly concurrency x this. That product, not this number,
	// is the thing to size against.
	MaxOutputBytes int
}

// Session is an interactive shell on an established sshcore.Client.
type Session struct {
	sess   *ssh.Session
	stdin  interface{ Write([]byte) (int, error) }
	prompt *regexp.Regexp
	opt    Options
	// baseLimit is the session default; limit is the one in force for the
	// command currently in flight, since a spec can widen or narrow it
	// per command.
	baseLimit int

	mu     sync.Mutex
	buf    []byte
	notify chan struct{}
	err    error
	closed bool
	// received counts every byte of the command in flight, including the
	// bytes dropped after the limit was passed. It is what the overflow
	// error reports, so "how far over" is answerable.
	received int64
	overflow bool
	limit    int
	// lastPrompt is the prompt line the most recent read ended on. The
	// device's own name is in there, and on a device reached by address
	// that is the only place it appears.
	lastPrompt string
}

// Prompt returns the prompt line the last read ended at, verbatim. Empty
// before the first prompt has been seen.
func (s *Session) Prompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPrompt
}

// Open starts a PTY shell on client, waits for the first prompt, and runs
// the paging-disable command when configured.
//
// ctx cancels the banner wait and the paging command. That matters more
// than it looks: ConnectTimeout defaults to 30 seconds, so without a
// context a stop request against a fleet waits out one full banner timeout
// per in-flight device before anything visibly happens.
func Open(ctx context.Context, client *sshcore.Client, opt Options) (*Session, error) {
	if opt.PromptRegex == "" {
		opt.PromptRegex = DefaultPromptRegex
	}
	if opt.CommandTimeout == 0 {
		opt.CommandTimeout = 30 * time.Second
	}
	if opt.ConnectTimeout == 0 {
		opt.ConnectTimeout = opt.CommandTimeout
	}
	limit := opt.MaxOutputBytes
	if limit == 0 {
		limit = DefaultMaxOutputBytes
	}
	if limit < 0 {
		limit = 0 // unbounded, by explicit request
	}
	prompt, err := regexp.Compile(opt.PromptRegex)
	if err != nil {
		return nil, fmt.Errorf("prompt regex: %w", err)
	}

	sess, err := client.SSH().NewSession()
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}

	// A dumb, very wide PTY: no line wrapping surprises in captured
	// output, and "vt100"/dumb-adjacent terms keep gear from emitting
	// fancy escapes we would only strip anyway.
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 115200,
		ssh.TTY_OP_OSPEED: 115200,
	}
	if err := sess.RequestPty("vt100", 60, 511, modes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("request pty: %w", err)
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := sess.Shell(); err != nil {
		sess.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}

	s := &Session{
		sess:      sess,
		stdin:     stdin,
		prompt:    prompt,
		opt:       opt,
		baseLimit: limit,
		limit:     limit,
		notify:    make(chan struct{}, 1),
	}
	go s.readLoop(stdout)
	go s.readLoop(stderr)

	// Login banner + first prompt.
	if _, err := s.expect(ctx, opt.ConnectTimeout); err != nil {
		s.Close()
		return nil, fmt.Errorf("waiting for initial prompt: %w", err)
	}

	if opt.PagingDisable != "" {
		if _, err := s.Run(ctx, opt.PagingDisable); err != nil {
			s.Close()
			return nil, fmt.Errorf("paging disable %q: %w", opt.PagingDisable, err)
		}
	}
	return s, nil
}

func (s *Session) readLoop(r interface{ Read([]byte) (int, error) }) {
	chunk := make([]byte, 32*1024)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			s.mu.Lock()
			s.appendLocked(chunk[:n])
			s.mu.Unlock()
			select {
			case s.notify <- struct{}{}:
			default:
			}
		}
		if err != nil {
			s.mu.Lock()
			if s.err == nil {
				s.err = err
			}
			s.mu.Unlock()
			select {
			case s.notify <- struct{}{}:
			default:
			}
			return
		}
	}
}

// appendLocked adds one arrival to the buffer, enforcing the limit.
//
// Past the limit the read deliberately continues. Stopping would leave the
// device blocked on a full window mid-config with a channel nobody is
// draining, which is the one thing this stack is not allowed to do to
// production gear; and the prompt, when it finally arrives, is what lets
// the session be reused instead of torn down.
func (s *Session) appendLocked(p []byte) {
	s.received += int64(len(p))
	s.buf = append(s.buf, p...)
	if s.limit <= 0 {
		return
	}
	if !s.overflow && len(s.buf) <= s.limit {
		return
	}
	s.overflow = true
	keep := tailKeep
	if keep > s.limit {
		keep = s.limit
	}
	if len(s.buf) > keep {
		n := copy(s.buf, s.buf[len(s.buf)-keep:])
		s.buf = s.buf[:n]
	}
}

// expect blocks until the normalized buffer ends at a prompt line, then
// drains and returns the buffer contents (normalized, prompt included).
//
// A cancelled context ends the wait immediately. The session is left as it
// is rather than closed: the caller owns it, and Close from two places is
// how a shutdown path grows a race.
func (s *Session) expect(ctx context.Context, timeout time.Duration) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		text := Normalize(string(s.buf))
		readErr := s.err
		over, count := s.overflow, s.received
		if endsAtPrompt(text, s.prompt) {
			s.lastPrompt = lastLine(text)
			s.buf = s.buf[:0]
			s.mu.Unlock()
			if over {
				return "", s.tooLarge(count, "")
			}
			return text, nil
		}
		s.mu.Unlock()

		// "Any key" genuinely means any key, so a single space is what
		// gets sent -- unlike Enter or a letter key, it cannot be read as
		// an answer to a yes/no question or a menu selection. This can
		// send a few redundant spaces if the device is slow to react and
		// more read chunks arrive before it does; a handful of leading
		// spaces on an otherwise-empty input line ahead of the real
		// prompt is harmless, and simpler than tracking which exact
		// occurrence has already been acknowledged.
		if looksLikeContinuePrompt(text) {
			if err := s.send(" "); err != nil {
				return text, fmt.Errorf("acknowledging continue prompt: %w", err)
			}
		}

		if readErr != nil {
			if over {
				return "", s.tooLarge(count, fmt.Sprintf("session then closed: %v", readErr))
			}
			return text, fmt.Errorf("session closed while waiting for prompt: %w", readErr)
		}
		select {
		case <-s.notify:
		case <-ctx.Done():
			return text, fmt.Errorf("cancelled waiting for prompt: %w", ctx.Err())
		case <-deadline.C:
			if over {
				return "", s.tooLarge(count, fmt.Sprintf("no prompt within %s", timeout))
			}
			return text, fmt.Errorf("timeout (%s) waiting for prompt; last output: %q",
				timeout, lastLine(text))
		}
	}
}

// tooLarge builds the overflow error. It always wins over a timeout or a
// closed session, because the limit is the thing the operator can act on
// and the other two are its consequences.
func (s *Session) tooLarge(count int64, note string) error {
	if note != "" {
		return fmt.Errorf("%w: read %d bytes, limit %d (%s)",
			ErrOutputTooLarge, count, s.limit, note)
	}
	return fmt.Errorf("%w: read %d bytes, limit %d", ErrOutputTooLarge, count, s.limit)
}

// continuePromptRe matches an interactive "send any key to proceed" gate: a
// login banner/legal-notice acknowledgment (ArubaOS-Switch: "Press any key
// to continue"), or a device's own output pager caught before any paging
// command has had a chance to run or disable it at all. Two pager phrasings
// are confirmed live: ExtremeXOS ("Press <SPACE> to continue or <Q> to
// quit:") and ArubaOS-CX ("-- MORE --, next page: Space, next line: Enter,
// quit: q") -- confirmed the hard way, since ArubaOS-CX has no documented
// paging-disable command at all (see the aruba_cx fingerprint probe), so
// this generic gate is the only thing standing between a long `show`
// command on that platform and a 30-second timeout.
//
// Bounded to a short trailing line for the same reason LooksLikeRejection
// in fingerprint.go is: real device output -- a running-config's own
// banner motd line, for instance -- can legitimately CONTAIN this phrase as
// configured text, and that is not the same thing as the device actually
// being blocked on a keystroke right now. Checking only the buffer's
// current trailing line, the same restriction endsAtPrompt already
// applies, means this can only match where a live device would genuinely
// be stuck: at the very end of whatever has arrived so far.
var continuePromptRe = regexp.MustCompile(`(?i)press\s+\S.{0,30}?to\s+(continue|quit|proceed|start)|--\s*more\s*--`)

// looksLikeContinuePrompt reports whether the final line of text is a
// device waiting on an acknowledging keystroke rather than a real prompt or
// a completed read.
func looksLikeContinuePrompt(text string) bool {
	line := lastLine(text)
	if line == "" || len(line) > 200 {
		return false
	}
	return continuePromptRe.MatchString(line)
}

// endsAtPrompt reports whether the final non-empty line of text matches the
// prompt regex. Matching only the LAST line keeps mid-output lines that
// merely look prompt-ish from ending a read early.
func endsAtPrompt(text string, prompt *regexp.Regexp) bool {
	line := lastLine(text)
	if line == "" {
		return false
	}
	return prompt.MatchString(line)
}

func lastLine(text string) string {
	text = strings.TrimRight(text, " \t\r\n")
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		return strings.TrimSpace(text[i+1:])
	}
	return strings.TrimSpace(text)
}

// RunOptions overrides the session defaults for one command.
//
// It exists because a capture spec knows things the session does not: a
// running-config and a tech-support come off the same session and want very
// different bounds, and the alternative — sizing the session for its largest
// command — removes the bound exactly where it matters. The other alternative,
// a session per capture type, buys a second login per type against every
// device, which is the cost this stack has spent the most effort avoiding.
type RunOptions struct {
	// MaxBytes overrides Options.MaxOutputBytes for this command only.
	// Zero keeps the session's value; negative disables the bound.
	MaxBytes int

	// Timeout overrides Options.CommandTimeout for this command only.
	// Zero keeps the session's value.
	Timeout time.Duration
}

// Run sends exactly one command and returns its cleaned output: command
// echo stripped from the top, prompt line stripped from the bottom, ANSI
// and CR noise normalized. The command string is sent verbatim — no
// splitting of any kind. To run several commands, call Run several times.
//
// ctx cancels the wait for output. It does not stop the device: the command
// is already running out there, so a cancelled Run leaves the session with
// output still arriving and the honest thing to do with it is Close.
func (s *Session) Run(ctx context.Context, cmd string) (string, error) {
	return s.RunWith(ctx, cmd, RunOptions{})
}

// RunWith is Run with per-command bounds.
func (s *Session) RunWith(ctx context.Context, cmd string, o RunOptions) (string, error) {
	if strings.ContainsAny(cmd, "\r\n") {
		return "", fmt.Errorf("Run takes a single command; %q contains a line break (loop over commands instead)", cmd)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("cancelled before sending %q: %w", cmd, err)
	}
	s.beginCommand()
	if o.MaxBytes != 0 {
		s.setLimit(o.MaxBytes)
	}
	timeout := s.opt.CommandTimeout
	if o.Timeout > 0 {
		timeout = o.Timeout
	}
	if err := s.send(cmd + "\n"); err != nil {
		return "", err
	}
	raw, err := s.expect(ctx, timeout)
	if err != nil {
		return raw, err
	}
	return StripEchoAndPrompt(raw, cmd, s.prompt), nil
}

// setLimit applies a per-command bound. A negative value disables it, which
// is how a spec says "this one is genuinely unbounded and I accept that".
func (s *Session) setLimit(n int) {
	s.mu.Lock()
	if n < 0 {
		s.limit = 0
	} else {
		s.limit = n
	}
	s.mu.Unlock()
}

// beginCommand resets the per-command accounting. The buffer itself is left
// alone — expect drains it on success, and whatever is in there after a
// failure is the evidence of that failure.
func (s *Session) beginCommand() {
	s.mu.Lock()
	s.received = 0
	s.overflow = false
	s.limit = s.baseLimit
	s.mu.Unlock()
}

// Enable enters privileged mode: sends enableCmd (default "enable"),
// answers the password challenge, and waits for the elevated prompt.
func (s *Session) Enable(ctx context.Context, enableCmd, password string) error {
	if enableCmd == "" {
		enableCmd = "enable"
	}
	s.beginCommand()
	if err := s.send(enableCmd + "\n"); err != nil {
		return err
	}
	// The device answers with either a Password: challenge or (already
	// privileged) the prompt itself. Wait briefly for the challenge.
	challenge := regexp.MustCompile(`(?i)password[^\n]*[:]\s*$`)
	deadline := time.NewTimer(s.opt.CommandTimeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		text := Normalize(string(s.buf))
		if challenge.MatchString(lastLine(text)) {
			s.buf = s.buf[:0]
			s.mu.Unlock()
			if err := s.send(password + "\n"); err != nil {
				return err
			}
			_, err := s.expect(ctx, s.opt.CommandTimeout)
			return err
		}
		if endsAtPrompt(text, s.prompt) {
			s.buf = s.buf[:0]
			s.mu.Unlock()
			return nil // no challenge — already privileged
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-ctx.Done():
			return fmt.Errorf("cancelled waiting for enable password prompt: %w", ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for enable password prompt")
		}
	}
}

func (s *Session) send(data string) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("session is closed")
	}
	_, err := s.stdin.Write([]byte(data))
	return err
}

// Close tears down the shell session (the sshcore.Client stays open and is
// closed by its owner).
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.sess.Close()
}
