// internal/ui/session.go
// The transport-facing half of the terminal widget.
//
// This is the port of TetherSSH's ssh_backend.go, reduced to what the new
// interfaces leave behind. Three things collapsed on the way over:
//
//   - Connecting, authenticating, host-key confirmation and credential
//     laddering are gone. They happen before a Transport exists, in sshcore
//     and credres. This file receives a live Transport and never learns how
//     it was obtained.
//   - The transport-kind fields (sshBackend / telnetBackend, kept only so a
//     read error could be classified by which transport produced it) are
//     gone. term.Transport reports why a session ended through Err(), so
//     there is nothing left to infer from error text.
//   - intentionalClose is gone for the same reason. Both transports honour
//     the contract that a local Close reports a nil Err, so "did the operator
//     do this" no longer has to be tracked out of band.
//
// What did NOT collapse is the read path. Every line of it is a field
// finding, and the comments there say which.
package ui

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"fyne.io/fyne/v2"

	"github.com/scottpeterman/pathfinderssh/internal/gopyte"
	"github.com/scottpeterman/pathfinderssh/internal/term"
)

// ConnectionState is the UI's vocabulary for session status. The connecting
// and authenticating states are emitted by the dial path, not by this file:
// a Session only ever exists in the connected state or past it.
type ConnectionState int

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateAuthenticating
	StateConnected
	StateError
	StateReconnecting
)

func (s ConnectionState) String() string {
	switch s {
	case StateConnecting:
		return "Connecting"
	case StateAuthenticating:
		return "Authenticating"
	case StateConnected:
		return "Connected"
	case StateError:
		return "Error"
	case StateReconnecting:
		return "Reconnecting"
	default:
		return "Disconnected"
	}
}

// readChunk is the read buffer size. 64 KiB is large enough that a burst of
// output from a "show run" arrives in a handful of reads rather than hundreds.
const readChunk = 64 * 1024

// emptyReadPause is how long the read loop waits after a read that returned no
// bytes and no error, and idleReadWarn is how many of those in a row it takes
// before saying so.
//
// A blocking transport never does this: it either returns bytes or fails. But a
// serial port opened with a positive ReadTimeout returns (0, nil) on every
// timeout by design, and a port whose USB device has stopped responding can do
// it forever. Without a pause the loop spins as fast as the CPU allows -- an
// observed case burned eight minutes of CPU on a session that looked merely
// idle. The pause costs at most a few milliseconds of latency on the first byte
// after a quiet period, and only for a transport that was returning empties
// anyway.
const (
	emptyReadPause = 5 * time.Millisecond
	idleReadWarn   = 2000
)

// Session is a terminal widget bound to one interactive transport. It embeds
// the render widget and adds exactly one thing: bytes move.
type Session struct {
	*NativeTerminalWidget

	// tp is the active transport, set by Attach and cleared by Close. tpMu
	// guards it because those two happen on different goroutines: Close runs
	// on the UI goroutine (a window closing) while the read loop is sitting in
	// tp.Read on its own. Unguarded, that is a data race, and the read loop
	// could dereference a nil tp on its next pass.
	tpMu   sync.RWMutex
	tp     term.Transport
	stream *gopyte.Stream

	// writeMu serialises the writer loop's transport.Write calls
	// (x/crypto/ssh channels and serial ports are not safe for concurrent writers).
	writeMu sync.Mutex

	// writeCh carries outbound bytes off the UI thread. TypedKey/TypedRune
	// run on Fyne's UI goroutine; a blocking SSH Write there freezes the
	// whole window (including the terminal paint). The writer loop owns the
	// actual transport.Write.
	writeCh     chan []byte
	cancelWrite context.CancelFunc

	// cancelRead stops the read loop on a local teardown.
	cancelRead context.CancelFunc

	onStateChange func(ConnectionState)
	onError       func(error)

	// onReconnectRequest is set by the session manager to raise the
	// reconnect prompt; reconnectPromptOpen de-dupes it so typing a burst of
	// keys at a dead session raises exactly one dialog.
	onReconnectRequest  func()
	reconnectPromptOpen atomic.Bool

	// --- Anti-idle (see antiidle.go) ---
	antiIdle       AntiIdleConfig
	lastUserInput  atomic.Int64
	cancelAntiIdle context.CancelFunc

	// logger is the session transcript, nil when off. Swapped atomically
	// because the read goroutine and the UI toggle both touch it.
	logger atomic.Pointer[sessionLogger]

	// name labels the session in logs and in the transcript filename. It
	// replaces sshConfig.SessionName/Host, which went with the SSH-shaped
	// config; the layer that dialled knows what to call this and sets it.
	name string

	// sessionLogEnabled is resolved from settings + the session definition
	// before Attach. It replaces the old sshConfig.LogEnabled, which is gone
	// with the rest of the SSH-shaped config.
	sessionLogEnabled bool
}

// NewSession creates a terminal widget with no transport attached. Input typed
// before Attach raises the reconnect prompt rather than being dropped.
func NewSession() *Session {
	s := &Session{NativeTerminalWidget: NewNativeTerminalWidget()}

	// This wrapper, not the embedded terminal, is the object in the canvas
	// tree, so it is the object Fyne must be asked to focus.
	s.NativeTerminalWidget.SetFocusHost(s)

	s.NativeTerminalWidget.writeOverride = func(data []byte) {
		if !s.Connected() {
			// Disconnected, but the user is typing into the pane. Offer to
			// reconnect rather than silently swallowing the bytes.
			s.requestReconnect()
			return
		}
		// Real user input resets the anti-idle clock. Anti-idle's own
		// keystrokes go through SendRaw and never reach here, so they
		// cannot hold the session open by themselves.
		s.noteUserInput()
		s.write(data)
	}

	s.NativeTerminalWidget.SetResizeCallback(func(cols, rows int) {
		s.ResizeTerminal(cols, rows)
	})

	s.NativeTerminalWidget.isLoggingFn = s.IsLogging
	s.NativeTerminalWidget.toggleLoggingFn = s.ToggleLogging

	return s
}

// Attach binds a live transport to the widget and starts moving bytes. The
// caller has already dialled, authenticated and opened the interactive
// channel; Attach neither knows nor cares which transport it is handed.
func (s *Session) Attach(tp term.Transport) error {
	if s.screen == nil {
		return fmt.Errorf("attach: screen not initialized")
	}

	s.tpMu.Lock()
	s.tp = tp
	s.tpMu.Unlock()
	s.reconnectPromptOpen.Store(false)
	s.noteUserInput() // seed the idle clock so anti-idle waits a full interval

	// A session that dropped while vim had DEC 2004 on would otherwise
	// reconnect to a shell that never asked for it, and the first paste would
	// arrive wrapped in markers the shell prints literally.
	s.resetBracketedPaste()

	// false = parse ANSI rather than passing it through.
	s.stream = gopyte.NewStream(s.screen, false)

	if s.sessionLogEnabled {
		if path, err := s.startLogger(); err != nil {
			log.Printf("session logging disabled: %v", err)
		} else {
			log.Printf("session logging to %s", path)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancelRead = cancel

	writeCtx, cancelWrite := context.WithCancel(context.Background())
	s.cancelWrite = cancelWrite
	s.writeCh = make(chan []byte, 256)
	go s.writeLoop(writeCtx)

	// Push the grid size at the far end before any output arrives. The PTY was
	// opened at whatever size the dialler asked for, which is term.DefaultSize
	// unless it knew better -- and it does not, because the widget owns that
	// number. Nothing else will correct it either: performResize only fires its
	// callback when the size CHANGES, and layout has usually finished while the
	// dial was still in flight, so the widget is already at its final size and
	// no resize event is coming. Without this the session runs at 80x24 until
	// the user happens to drag the window.
	s.syncSize()

	// Anti-idle is scoped to the read loop's lifetime, so it can never
	// outlive the connection or leak across a reconnect.
	s.startAntiIdle(ctx)

	go s.readLoop(ctx)
	go s.watchDone()

	s.emitState(StateConnected)
	return nil
}

// transport returns the active transport, or nil once Close has run. Callers
// must hold the returned value rather than re-reading s.tp, so a teardown
// mid-operation cannot turn a valid transport into a nil one underneath them.
func (s *Session) transport() term.Transport {
	s.tpMu.RLock()
	defer s.tpMu.RUnlock()
	return s.tp
}

// syncSize pushes the widget's current grid size to the transport. It is a
// no-op before layout has run, when the size is not yet meaningful; the resize
// event that follows layout covers that case.
func (s *Session) syncSize() {
	s.mutex.RLock()
	size := term.Size{Cols: s.cols, Rows: s.rows}
	s.mutex.RUnlock()

	tp := s.transport()
	if !size.Valid() || tp == nil {
		return
	}
	if err := tp.Resize(size); err != nil {
		log.Printf("initial resize to %dx%d: %v", size.Cols, size.Rows, err)
	}
}

// watchDone is the single place a session's end is reported. The read loop
// exits quietly on any read error and leaves the explaining to this: the
// transport already knows whether the end was a failure, and duplicating that
// judgement in two goroutines is how the old code ended up matching on error
// text.
func (s *Session) watchDone() {
	tp := s.transport()
	if tp == nil {
		return
	}
	<-tp.Done()
	s.stopAntiIdle()

	if err := tp.Err(); err != nil {
		// Non-nil means the far end went away on its own: a reset, a broken
		// pipe, or an unplugged console cable. A local Close and a clean
		// remote exit both report nil and stay silent.
		log.Printf("session ended: %v", err)
		if s.onError != nil {
			e := err
			fyne.Do(func() { s.onError(e) })
		}
	}
	s.emitState(StateDisconnected)
}

// readLoop moves bytes from the transport into the VT engine until the session
// ends or the widget is torn down.
func (s *Session) readLoop(ctx context.Context) {
	tp := s.transport()
	if tp == nil {
		return
	}
	buf := make([]byte, readChunk)

	// utf8tail carries an incomplete trailing UTF-8 sequence across reads.
	// Braille runes (U+28xx, 3 bytes) are the case that exposed this: a read
	// can split one across a chunk boundary, and feeding the halves
	// separately decodes them as garbage.
	var utf8tail []byte

	// Consecutive reads that returned nothing at all. See emptyReadPause.
	var empties int

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := tp.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			// Tee the raw bytes to the transcript: exact stream, no framing
			// assumptions.
			s.logger.Load().Write(data)

			chunk := append(utf8tail, data...)
			cut := completePrefixLen(chunk)
			complete := chunk[:cut]
			utf8tail = append([]byte(nil), chunk[cut:]...)

			if len(complete) > 0 {
				str := string(complete)
				s.feed(str)
				// Bracketed paste (mode 2004) is tracked from the output
				// stream here because this path does not go through the
				// escape-sequence handler; without it the flag would never
				// be set and a paste could never be wrapped.
				s.updateBracketedPasteState(str)
			}

			// ScrollToBottom is model state, not UI state, so it runs inline
			// on this goroutine under the screen's own lock. Queueing it
			// through fyne.Do let it land after the paint that was meant to
			// show the new tail, and it never re-armed the dirty flag, so
			// the scroll for the last chunk of a burst could be dropped.
			if !s.screen.IsUsingAlternate() && !s.screen.IsViewingHistory() {
				s.screen.ScrollToBottom()
			}
			s.updatePending.Store(true)
		}

		if err != nil {
			// No classification. Whether this was a teardown or a dropped
			// link is watchDone's business, and it reads the answer off the
			// transport instead of guessing from the error string.
			return
		}

		if n > 0 {
			empties = 0
			continue
		}

		// Nothing read and nothing wrong. Yield rather than spin.
		empties++
		if empties == idleReadWarn {
			log.Printf("readLoop: %d consecutive empty reads; transport may be wedged", empties)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(emptyReadPause):
		}
	}
}

// feedChunk is how much VT input we parse under one screen lock. Holding the
// lock across a full 64 KiB read starves the paint path (which needs the same
// lock to snapshot), so the terminal looks frozen under bursty output.
const feedChunk = 4 * 1024

// feed pushes a complete-rune string into the VT engine.
//
// The model lock is held across each chunk Feed: the handlers it dispatches to
// write the screen with no locking of their own, so this is what serialises
// them against the render and selection readers. Chunks release the lock
// between slices so a redraw can run. The recover is not defensive habit -- a
// malformed or boundary-split sequence that drives a handler into a panic would
// otherwise take down the read goroutine, and with it the app.
func (s *Session) feed(str string) {
	if s.stream == nil {
		return
	}
	for len(str) > 0 {
		n := feedChunk
		if n > len(str) {
			n = len(str)
		}
		// Do not split a UTF-8 sequence across chunks.
		for n < len(str) && n > 0 && !utf8.RuneStart(str[n]) {
			n--
		}
		if n == 0 {
			_, size := utf8.DecodeRuneInString(str)
			n = size
		}
		chunk := str[:n]
		str = str[n:]

		func() {
			s.screen.Lock()
			defer s.screen.Unlock()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("recovered panic feeding stream: %v", r)
				}
			}()
			s.stream.Feed(chunk)
		}()
	}
}

// writeLoop drains writeCh onto the transport. One goroutine only: SSH channels
// and serial ports are not safe for concurrent writers.
func (s *Session) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-s.writeCh:
			s.writeSync(data)
		}
	}
}

// write queues bytes for the writer loop. Never blocks the UI thread on a
// stalled TCP window.
func (s *Session) write(data []byte) {
	if len(data) == 0 || s.writeCh == nil {
		return
	}
	cp := append([]byte(nil), data...)
	select {
	case s.writeCh <- cp:
	default:
		// Queue full (huge paste / stalled link). Park off the UI thread.
		go func() {
			select {
			case s.writeCh <- cp:
			case <-time.After(5 * time.Second):
				log.Printf("transport write queue full; dropped %d bytes", len(cp))
			}
		}()
	}
}

func (s *Session) writeSync(data []byte) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tp := s.transport()
	if tp == nil {
		return
	}
	if _, err := tp.Write(data); err != nil {
		log.Printf("transport write error: %v", err)
	}
}

// SendRaw writes without touching the idle clock. Anti-idle uses it so its own
// keystrokes cannot keep a session alive indefinitely.
func (s *Session) SendRaw(data []byte) {
	if len(data) == 0 || !s.Connected() {
		return
	}
	s.write(data)
}

// ResizeTerminal informs the far end of a new window size. A zero or negative
// dimension is the normal symptom of asking the toolkit for a size before
// layout has run; pushing it at a server yields either an error or a 0x0 PTY
// that renders nothing, so Size.Valid gates it.
func (s *Session) ResizeTerminal(cols, rows int) {
	size := term.Size{Cols: cols, Rows: rows}
	if !size.Valid() {
		return
	}

	if s.screen != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("recovered panic resizing screen: %v", r)
				}
			}()
			s.screen.Resize(cols, rows)
		}()
	}

	// SSH sends a window-change request; a serial console's Resize is a
	// documented no-op. Neither needs a branch here.
	if tp := s.transport(); tp != nil && s.Connected() {
		if err := tp.Resize(size); err != nil {
			log.Printf("transport resize to %dx%d: %v", cols, rows, err)
		}
	}

	s.updatePending.Store(true)
}

// Connected reports whether the transport is still live. It reads the Done
// channel rather than a separate flag, so there is one source of truth about
// whether a session is usable.
func (s *Session) Connected() bool {
	tp := s.transport()
	if tp == nil {
		return false
	}
	select {
	case <-tp.Done():
		return false
	default:
		return true
	}
}

// State reports the session's status in the UI's vocabulary.
func (s *Session) State() ConnectionState {
	if s.Connected() {
		return StateConnected
	}
	return StateDisconnected
}

// Close tears the session down locally. It is safe to call on an already-dead
// session, and reports nil from the transport's Err, so watchDone stays quiet.
func (s *Session) Close() error {
	if s.cancelRead != nil {
		s.cancelRead()
		s.cancelRead = nil
	}
	if s.cancelWrite != nil {
		s.cancelWrite()
		s.cancelWrite = nil
	}
	s.stopLogger()

	s.tpMu.Lock()
	tp := s.tp
	s.tp = nil
	s.tpMu.Unlock()

	if tp == nil {
		return nil
	}
	// Outside the lock: closing a transport whose device has gone away can
	// block in the driver, and holding tpMu through that would stall every
	// reader with it.
	return tp.Close()
}

// SetStateChangeHandler sets the callback for status changes.
func (s *Session) SetStateChangeHandler(fn func(ConnectionState)) { s.onStateChange = fn }

// SetName labels the session for logs and the transcript filename.
func (s *Session) SetName(name string) { s.name = name }

// SetSessionLogEnabled controls whether Attach starts a transcript.
func (s *Session) SetSessionLogEnabled(v bool) { s.sessionLogEnabled = v }

// SetErrorHandler sets the callback for a session that ended in failure.
func (s *Session) SetErrorHandler(fn func(error)) { s.onError = fn }

// SetReconnectRequestHandler sets the callback that raises the reconnect
// prompt when input arrives at a dead session.
func (s *Session) SetReconnectRequestHandler(fn func()) { s.onReconnectRequest = fn }

// emitState notifies the UI on the main thread.
func (s *Session) emitState(st ConnectionState) {
	if s.onStateChange == nil {
		return
	}
	fyne.Do(func() { s.onStateChange(st) })
}

// requestReconnect raises the reconnect prompt at most once per dead session.
func (s *Session) requestReconnect() {
	if s.onReconnectRequest == nil {
		return
	}
	if !s.reconnectPromptOpen.CompareAndSwap(false, true) {
		return
	}
	fyne.Do(s.onReconnectRequest)
}

// completePrefixLen returns the length of the longest prefix of b that ends on
// a UTF-8 rune boundary, excluding any trailing incomplete multi-byte
// sequence. It returns len(b) when b already ends on a complete rune.
func completePrefixLen(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	// Walk back over continuation bytes (10xxxxxx) to the start of the final
	// rune, at most utf8.UTFMax-1 of them.
	i := len(b) - 1
	for i >= 0 && len(b)-i < utf8.UTFMax && b[i]&0xC0 == 0x80 {
		i--
	}
	if i < 0 {
		return len(b) // all continuation bytes (malformed); feed as-is
	}
	if utf8.FullRune(b[i:]) {
		return len(b) // final rune is complete
	}
	return i // hold the incomplete final rune for the next read
}
