// internal/ui/terminal_events.go
// Fixed terminal_events.go - Mouse wheel scrolling with WideCharScreen integration
// FIXES: SSH resize propagation and dimension calculation
package ui

import (
	"log"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
)

// KEYBOARD EVENT HANDLING
func (t *NativeTerminalWidget) TypedKey(key *fyne.KeyEvent) {
	dprintf("========== TypedKey ENTRY ==========\n")
	dprintf("TypedKey: Key pressed: %s\n", key.Name)
	dprintf("TypedKey: writeOverride is nil: %v\n", t.writeOverride == nil)

	if !t.canSend() {
		dprintf("TypedKey: no transport attached, ignoring\n")
		dprintf("========== TypedKey EXIT (detached) ==========\n")
		return
	}

	var data []byte

	// Handle keys - Page Up/Down always go to applications
	switch key.Name {
	case fyne.KeyPageUp:
		// Always send to application (vim, less, etc.)
		data = []byte("\x1b[5~")

	case fyne.KeyPageDown:
		// Always send to application (vim, less, etc.)
		data = []byte("\x1b[6~")
	case fyne.KeyBackspace:
		data = []byte("\x7f")
		// Coalesce via updatePending — do NOT call performRedrawDirect on the
		// UI path. That bypassed the in-flight paint guard and (when debug
		// logging was heavy) froze the window after a few keypresses.
	case fyne.KeyReturn:
		// Exit history mode on Enter in normal mode
		if t.screen != nil && !t.screen.IsUsingAlternate() && t.screen.IsViewingHistory() {
			dprintf("TypedKey: Enter pressed, exiting history mode\n")
			t.exitHistoryMode()
		}
		data = []byte("\r")

	case fyne.KeyTab:
		data = []byte("\t")

	case fyne.KeyDelete:
		data = []byte("\x1b[3~")

	// The six keys that have TWO encodings, chosen by the remote through
	// DECCKM. See cursorKey.
	case fyne.KeyUp:
		data = cursorKey(t.applicationCursorKeys(), 'A')

	case fyne.KeyDown:
		data = cursorKey(t.applicationCursorKeys(), 'B')

	case fyne.KeyLeft:
		data = cursorKey(t.applicationCursorKeys(), 'D')

	case fyne.KeyRight:
		data = cursorKey(t.applicationCursorKeys(), 'C')

	case fyne.KeyHome:
		data = cursorKey(t.applicationCursorKeys(), 'H')

	case fyne.KeyEnd:
		data = cursorKey(t.applicationCursorKeys(), 'F')

	case fyne.KeyEscape:
		data = []byte("\x1b")

	case fyne.KeyF1:
		data = []byte("\x1b[11~")
	case fyne.KeyF2:
		data = []byte("\x1b[12~")
	case fyne.KeyF3:
		data = []byte("\x1b[13~")
	case fyne.KeyF4:
		data = []byte("\x1b[14~")
	case fyne.KeyF5:
		data = []byte("\x1b[15~")
	case fyne.KeyF6:
		data = []byte("\x1b[17~")
	case fyne.KeyF7:
		data = []byte("\x1b[18~")
	case fyne.KeyF8:
		data = []byte("\x1b[19~")
	case fyne.KeyF9:
		data = []byte("\x1b[20~")
	case fyne.KeyF10:
		data = []byte("\x1b[21~")
	case fyne.KeyF11:
		data = []byte("\x1b[23~")
	case fyne.KeyF12:
		data = []byte("\x1b[24~")
	}

	if len(data) > 0 {
		dprintf("TypedKey: Calling sendInput with %d bytes: %v\n", len(data), data)
		err := t.sendInput(data)
		if err != nil {
			dprintf("TypedKey: sendInput error: %v\n", err)
		} else {
			dprintf("TypedKey: sendInput succeeded\n")
		}
		t.updatePending.Store(true)
	} else {
		dprintf("TypedKey: No data to send for key: %s\n", key.Name)
	}
	dprintf("========== TypedKey EXIT ==========\n")
}

func (t *NativeTerminalWidget) TypedRune(r rune) {
	dprintf("========== TypedRune ENTRY ==========\n")
	dprintf("TypedRune: Character typed: %c (0x%04X)\n", r, r)
	dprintf("TypedRune: writeOverride is nil: %v\n", t.writeOverride == nil)

	if !t.canSend() {
		dprintf("TypedRune: no transport attached, ignoring\n")
		dprintf("========== TypedRune EXIT (detached) ==========\n")
		return
	}

	// Exit history mode on any typing in normal mode
	if t.screen != nil && !t.screen.IsUsingAlternate() && t.screen.IsViewingHistory() {
		dprintf("TypedRune: Exiting history mode on character input\n")
		t.exitHistoryMode()
	}

	var data []byte

	// Handle control characters (0x01-0x1F)
	if r >= 1 && r <= 31 {
		dprintf("TypedRune: Control character detected: Ctrl+%c (0x%02X)\n", r+64, r)
		data = []byte{byte(r)}
	} else if r < 32 {
		// Other control characters
		dprintf("TypedRune: Special control character: 0x%02X\n", r)
		data = []byte{byte(r)}
	} else {
		// Regular printable character
		data = []byte(string(r))
	}

	dprintf("TypedRune: Calling sendInput with %d bytes: %v (%q)\n", len(data), data, string(data))

	err := t.sendInput(data)
	if err != nil {
		dprintf("TypedRune: sendInput error: %v\n", err)
	} else {
		dprintf("TypedRune: sendInput succeeded\n")
	}

	t.updatePending.Store(true)
	dprintf("========== TypedRune EXIT ==========\n")
}

// Fix 4: Immediate redraw trigger — only mark dirty; the update processor
// paints. Calling performRedrawDirect here stacked ungarded full paints on
// the UI queue and made the terminal unresponsive.
func (t *NativeTerminalWidget) triggerImmediateRedraw() {
	t.updatePending.Store(true)
}

// Fix 7: Enhanced focus handling
func (t *NativeTerminalWidget) FocusGained() {
	dprintf("FocusGained: Terminal widget gained focus\n")
	t.hasFocus = true

	// No re-focus call here. FocusGained is a NOTIFICATION that the canvas has
	// already focused us - asking it to focus again is at best a no-op, and the
	// call that used to live here passed the inner widget, which never matched
	// the tree object anyway. Focus requests belong in GrabFocus.
}

// MOUSE EVENT HANDLING - Implements desktop.Mouseable
func (t *NativeTerminalWidget) MouseDown(event *desktop.MouseEvent) {
	dprintf("MouseDown: position=%v, button=%v\n", event.Position, event.Button)

	// Request focus on click. This has to go through GrabFocus: focusing the
	// inner widget directly is a silent no-op for SSH/telnet/serial tabs, where
	// the object in the tree is the *Session wrapper. That is why
	// clicking the pane could not take focus back from the find bar.
	t.GrabFocus()
}

func (t *NativeTerminalWidget) MouseUp(event *desktop.MouseEvent) {
	dprintf("MouseUp: position=%v\n", event.Position)
}

// SCROLL HANDLING - Implements fyne.Scrollable
func (t *NativeTerminalWidget) Scrolled(event *fyne.ScrollEvent) {
	dprintf("Scrolled event: DY=%.2f\n", event.Scrolled.DY)
	t.handleScrollEvent(event)
}

// handleScrollEvent processes mouse wheel events
func (t *NativeTerminalWidget) handleScrollEvent(event *fyne.ScrollEvent) bool {
	if t.screen == nil {
		return false
	}

	// A manual wheel scroll drops any settled selection. This avoids the
	// "highlight frozen on screen while text scrolls behind it" problem
	// entirely: a selection can't outlive the scroll that would strand it.
	// Drag auto-scroll is exempt - it keeps isSelecting true throughout and
	// calls the screen scroll methods directly, never this handler - so
	// extending a selection past the edge still works.
	if t.selection != nil && t.selection.HasSelection() && !t.selection.IsSelecting() {
		t.selection.Clear()
	}

	now := time.Now()

	// Debounce rapid scroll events
	if now.Sub(t.lastScrollTime) < 50*time.Millisecond {
		return true
	}
	t.lastScrollTime = now

	dprintf("handleScrollEvent: DY=%.2f, IsUsingAlternate=%v\n",
		event.Scrolled.DY, t.screen.IsUsingAlternate())

	// DEBUG: Log state before scroll
	t.debugScrollEvent("BEFORE", 0)

	// In alternate screen (vim), don't handle scroll
	if t.screen.IsUsingAlternate() {
		dprintf("handleScrollEvent: in alternate screen, letting application handle\n")
		return false
	}

	// Normal mode: handle mouse wheel scrolling for history
	scrollLines := 3
	if absFloat32(event.Scrolled.DY) > 5 {
		scrollLines = 5 // Faster scroll for larger movements
	}

	if event.Scrolled.DY > 0.1 {
		// Scroll up (into history) - use WideCharScreen directly
		dprintf("handleScrollEvent: MOUSE WHEEL UP by %d lines\n", scrollLines)

		// DEBUG: Check before scroll up
		beforePos := t.screen.GetHistoryPos()
		beforeMax := t.screen.GetHistorySize()
		dprintf("Before scroll up: %d/%d\n", beforePos, beforeMax)

		t.screen.ScrollUp(scrollLines)
		t.updatePending.Store(true)

		// DEBUG: Check after scroll up and log what happened
		t.debugScrollEvent("UP", scrollLines)

		return true
	} else if event.Scrolled.DY < -0.1 {
		// Scroll down (towards current) - use WideCharScreen directly
		dprintf("handleScrollEvent: MOUSE WHEEL DOWN by %d lines\n", scrollLines)

		// DEBUG: Check before scroll down
		beforePos := t.screen.GetHistoryPos()
		beforeMax := t.screen.GetHistorySize()
		dprintf("Before scroll down: %d/%d\n", beforePos, beforeMax)

		t.screen.ScrollDown(scrollLines)
		t.updatePending.Store(true)

		// DEBUG: Check after scroll down and log what happened
		t.debugScrollEvent("DOWN", scrollLines)

		return true
	}

	return false
}

// SIMPLE HISTORY MODE EXIT

func (t *NativeTerminalWidget) exitHistoryMode() {
	if t.screen != nil {
		log.Printf("Exiting history mode - returning to current output")
		// Same rationale as the wheel-scroll gate: snapping back to the bottom
		// would strand a selection made while scrolled up, leaving its blue
		// block frozen as the view jumps. Drop it. Clear() no-ops if empty.
		if t.selection != nil {
			t.selection.Clear()
		}
		t.screen.ScrollToBottom()
		t.updatePending.Store(true)
	}
}

// RESIZE HANDLING

func (t *NativeTerminalWidget) handleResize(width, height float32) {
	t.resizeMutex.Lock()
	defer t.resizeMutex.Unlock()

	if width == t.lastWidth && height == t.lastHeight {
		return
	}

	t.lastWidth = width
	t.lastHeight = height

	if t.resizeTimer != nil {
		t.resizeTimer.Stop()
	}

	t.resizeTimer = time.AfterFunc(250*time.Millisecond, func() {
		t.performResize(width, height)
	})
}

func (t *NativeTerminalWidget) performResize(width, height float32) {
	newCols, newRows := t.CalculateTerminalSize(width, height)

	t.mutex.Lock()
	currentCols, currentRows := t.cols, t.rows
	needsResize := newCols != currentCols || newRows != currentRows

	if needsResize {
		dlogf("performResize: from %dx%d to %dx%d (widget: %.1fx%.1f)",
			currentCols, currentRows, newCols, newRows, width, height)

		// Update terminal dimensions
		t.cols = newCols
		t.rows = newRows

		// Update virtual scroll viewport size if it exists
		if hasVirtualScroll(t) {
			t.virtualScroll.visibleLines = newRows
		}

		// Resize the underlying terminal screen (gopyte)
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Error resizing screen: %v", r)
				}
			}()
			t.screen.Resize(newCols, newRows)
		}()
	}
	t.mutex.Unlock()

	if needsResize {
		// Coalesce remote WindowChange onto one worker. Spawning a goroutine
		// per debounce could pile up blocked SSH writes and freeze the UI
		// when the user dragged the split with several sessions open.
		if t.onResizeCallback != nil {
			t.scheduleRemoteResize(newCols, newRows)
		}
		t.updatePending.Store(true)
	}
}

// scheduleRemoteResize queues cols/rows for the far end and ensures a single
// worker is draining the latest size (SSH WindowChange can block).
func (t *NativeTerminalWidget) scheduleRemoteResize(cols, rows int) {
	t.remoteCols.Store(int32(cols))
	t.remoteRows.Store(int32(rows))
	if !t.remoteResizeOn.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer t.remoteResizeOn.Store(false)
		for {
			c := int(t.remoteCols.Load())
			r := int(t.remoteRows.Load())
			cb := t.onResizeCallback
			if cb != nil {
				cb(c, r)
			}
			if int(t.remoteCols.Load()) == c && int(t.remoteRows.Load()) == r {
				return
			}
		}
	}()
}

// SIZING AND UTILITY METHODS

func (t *NativeTerminalWidget) CalculateTerminalSize(width, height float32) (int, int) {
	// Ensure we have valid character dimensions
	if t.charWidth <= 0 || t.charHeight <= 0 {
		log.Printf("CalculateTerminalSize: invalid char dimensions (%.2f x %.2f), using defaults",
			t.charWidth, t.charHeight)
		return 80, 24
	}

	// Account for padding/margins in the terminal widget
	// Fyne TextGrid typically has some internal padding
	const horizontalPadding float32 = 4.0 // Left + right padding
	const verticalPadding float32 = 2.0   // Top + bottom padding (minimal)

	usableWidth := width - horizontalPadding
	usableHeight := height - verticalPadding

	// Ensure we don't go negative
	if usableWidth < 0 {
		usableWidth = width
	}
	if usableHeight < 2 {
		usableHeight = height - 2
	}
	// Calculate columns and rows

	// Measure the cell from the grid's ACTUAL rendered size rather than the
	// fontSize-derived charWidth/charHeight. The visible glyphs are a TextGrid
	// that renders at the active theme's text size, so deriving the PTY row/col
	// count from the same measured metric the overlay and hit-testing use keeps
	// the remote's row count matched to what's actually visible (no blank band
	// at the bottom, no clipped last rows). It also future-proofs a
	// user-configurable terminal font: change the grid's render size and rows,
	// cols, selection, and scrollback all recompute from this one measurement.
	// gridCellSize falls back to charWidth/charHeight before the grid has
	// content, so the very first sizing still works.
	cw, ch := t.gridCellSize()
	if cw <= 0 {
		cw = t.charWidth
	}
	if ch <= 0 {
		ch = t.charHeight
	}

	cols := int(usableWidth / cw)
	rows := int(usableHeight / ch)
	settings := CurrentSettings()
	rows = rows - settings.RowOffset
	cols = cols - settings.ColOffset
	// Apply reasonable limits
	if cols < 10 {
		cols = 10
	} else if cols > 500 {
		cols = 500
	}

	if rows < 3 {
		rows = 3
	} else if rows > 200 {
		rows = 200
	}

	dlogf("CalculateTerminalSize: window=%.1fx%.1f, measuredCell=%.2fx%.2f (fontSizeCell=%.2fx%.2f) -> %dx%d",
		width, height, cw, ch, t.charWidth, t.charHeight, cols, rows)
	return cols, rows
}

// HELPER FUNCTIONS

// hasVirtualScroll checks if virtual scroll field exists
func hasVirtualScroll(t *NativeTerminalWidget) bool {
	defer func() {
		recover() // Ignore if field doesn't exist
	}()
	return t.virtualScroll != (VirtualScrollState{})
}

// UTILITY FUNCTIONS

func absFloat32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

// *** NEW: SetResizeCallback allows SSH widgets to receive resize events ***
func (t *NativeTerminalWidget) SetResizeCallback(callback func(cols, rows int)) {
	t.onResizeCallback = callback
	dlogf("SetResizeCallback: resize callback registered")
}

// cursorKey renders one of the six keys that have two encodings.
//
// # Why there are two, and why guessing is not an option
//
// A cursor key can be sent as CSI (ESC [ A) or as SS3 (ESC O A), and the
// REMOTE picks by setting DECCKM -- private mode 1. Full-screen applications
// set it on entry because that is what their terminfo entry promises: TERM is
// xterm-256color, whose kcuu1 is \EOA, so vim reads SS3 and nothing else.
//
// Send the wrong one and there is no error anywhere. vim does not recognise
// the sequence, falls back to reading the bytes one at a time, and executes
// the two after the ESC -- "[D" is its list-definitions command, which is why
// the symptom of this bug is the words
//
//	E388: Could not find definition
//
// appearing when somebody presses the left arrow.
//
// Home and End take the same pair, which is why they are here. PageUp,
// PageDown, Delete and the F-keys have ONE encoding each and deliberately do
// not route through this -- a test pins that, because "route everything
// through the mode" is the obvious wrong generalisation.
func cursorKey(application bool, final byte) []byte {
	if application {
		return []byte{0x1b, 'O', final}
	}
	return []byte{0x1b, '[', final}
}

// applicationCursorKeys reports whether the remote asked for SS3 encoding.
//
// A nil screen answers false: a widget with no screen yet has had nothing
// tell it otherwise, and CSI is the unset state of DECCKM.
func (t *NativeTerminalWidget) applicationCursorKeys() bool {
	if t.screen == nil {
		return false
	}
	return t.screen.IsApplicationCursorKeys()
}
