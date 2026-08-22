// internal/ui/terminal_paste.go
// terminal_paste.go - Paste handling for NativeTerminalWidget.
//
// Both paste entry points - the Ctrl+V / Cmd+V shortcut (see TypedShortcut in
// terminal_widget.go) and the right-click context menu (TappedSecondary below) -
// funnel through pasteText(). That single chokepoint is where paste behavior lives,
// so pacing, line-splitting, and abort logic only have to exist in one place.
//
// Multi-line pastes can be paced line-by-line via Settings -> Paste Line Delay so
// that slow CLI parsers on network gear (or slow links / jump hosts) don't drop
// lines when a config block is pasted in one shot. Single-line pastes and a zero
// delay take the original fast path: one write, no goroutine.
package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// Compile-time assertion: the terminal offers a right-click context menu.
var _ fyne.SecondaryTappable = (*NativeTerminalWidget)(nil)

// pasteText is the single entry point for all paste operations.
//
// It cancels any paste still in flight, then chooses between a fast single-shot
// write and a paced, abortable, line-by-line write based on the content and the
// configured paste line delay.
func (t *NativeTerminalWidget) pasteText(content string) {
	if content == "" {
		return
	}

	// Normalize BEFORE anything else, so every branch below sends the bytes a
	// person typing the same text would send, and so the line count in the
	// confirmation is counted against the endings actually going on the wire
	// rather than whatever flavour the clipboard happened to hold.
	content = normalizeNewlines(content)

	// confirmPaste returns true when it has taken responsibility: the dialog
	// is up and it will call writePaste itself if the answer is yes.
	if t.confirmPaste(content) {
		return
	}

	t.writePaste(content, t.pasteLineDelayMs, t.pasteConsoleBaud)
}

// writePaste puts content on the wire with the pacing it is given.
//
// The pacing arrives as arguments rather than being read from the widget
// because the confirmation dialog can override it for one paste. Passing it in
// means there is no window in which a remembered override and an in-flight
// paste disagree about which values are current.
func (t *NativeTerminalWidget) writePaste(content string, lineDelayMs, consoleBaud int) {
	if content == "" {
		return
	}

	// A new paste supersedes any paste still draining on this terminal.
	//
	// This lives HERE and not in pasteText on purpose: cancelling in the
	// entry point would mean declining a second paste aborts a first paste
	// that is still draining, i.e. answering "no" destroys work nobody asked
	// to abort.
	t.cancelActivePaste()

	lineDelay := time.Duration(lineDelayMs) * time.Millisecond
	if lineDelay < 0 {
		lineDelay = 0
	}
	chunk, gap := pastePacing(consoleBaud)

	// Bracketed paste: if the remote app enabled DEC mode 2004 (vim, bash
	// readline, less, ...), wrap the content in start/end markers. This tells
	// the app the bytes were pasted, so it suspends auto-indent and won't run
	// multi-line input line by line.
	bracketed := t.bracketedPaste.Load()
	if bracketed {
		content = "\x1b[200~" + content + "\x1b[201~"
		// The markers exist to keep the paste together, so nothing is
		// inserted BETWEEN its lines. Byte pacing still applies: it does
		// not split the paste, it only slows the bytes down, and a console
		// line overruns whether or not an editor asked for markers.
		lineDelay = 0
	}

	// Fast path: nothing to pace. One write, no goroutine, which is what the
	// overwhelming majority of pastes get.
	if chunk <= 0 && (lineDelay <= 0 || !strings.ContainsAny(content, "\r\n")) {
		_ = t.sendInput([]byte(content))
		t.updatePending.Store(true)
		return
	}

	// Paced path. Deriving the paste context from t.ctx means a tab close or
	// disconnect (both of which call t.cancel()) aborts an in-flight paste for
	// free, in addition to the explicit Ctrl+C abort wired in TypedShortcut.
	pasteCtx, cancel := context.WithCancel(t.ctx)
	t.pasteMutex.Lock()
	t.pasteCancel = cancel
	t.pasteMutex.Unlock()

	lines := splitLinesKeepEOL(content)

	go func() {
		defer cancel()
		for _, line := range lines {
			if !t.sendPaced(pasteCtx, line, chunk, gap) {
				return
			}

			// Pace only *between* lines - after a line that ended in a newline.
			// The final partial line (no trailing newline) is written without an
			// extra trailing delay.
			if lineDelay > 0 && strings.HasSuffix(line, "\r") {
				select {
				case <-pasteCtx.Done():
					return
				case <-time.After(lineDelay):
				}
			}
		}
	}()
}

// confirmPaste raises the multi-line paste question and reports whether it took
// responsibility for the paste. A false return means no question was needed and
// the caller should write immediately.
//
// Every decline is logged, because "the dialog did not appear" has four
// possible causes and none of them are distinguishable from the outside.
func (t *NativeTerminalWidget) confirmPaste(content string) bool {
	t.mutex.RLock()
	threshold := t.pasteWarnLines
	delayMs := t.pasteLineDelayMs
	baud := t.pasteConsoleBaud
	remember := t.pasteRemember
	t.mutex.RUnlock()

	if threshold <= 0 {
		dprintf("PASTE: confirmation is off (paste_warn_lines=%d)", threshold)
		return false
	}
	lines := countPasteLines(content)
	if lines < threshold {
		dprintf("PASTE: %d line(s), below the warn threshold of %d", lines, threshold)
		return false
	}

	// The terminal's OWN window. A session detached into a window of its own
	// would otherwise raise the question on the main window, where the person
	// who pressed the keys is not looking.
	win := t.HostWindow()
	if win == nil {
		// Refusing the paste here would read as "paste is broken", and a
		// terminal that cannot find its own window has a bigger problem
		// than this one paste. Logged UNGATED for that reason.
		fyne.LogError("paste confirmation skipped: no window to show it in", nil)
		return false
	}

	if delayMs < 0 {
		delayMs = 0
	}
	if baud < 0 {
		baud = 0
	}
	bracketed := t.bracketedPaste.Load()

	// --- the review pane -------------------------------------------------
	//
	// A TextGrid rather than an Entry: it is monospaced, so an indented
	// config block still lines up, and it is not editable by construction --
	// no Disable() greying the very text somebody opened the dialog to read.
	// Fyne has no read-only Entry.
	//
	// It goes inside a Scroll because a TextGrid's MinSize is its whole
	// content: without the scroll a 200-line block asks for a dialog taller
	// than the screen, which is the original reason a preview was capped at
	// four lines.
	body, truncated := pasteReviewText(content)
	grid := widget.NewTextGridFromString(body)
	review := container.NewScroll(grid)

	head := widget.NewLabel(pasteConfirmHeadline(t.TargetLabel(), lines))
	head.TextStyle = fyne.TextStyle{Bold: true}

	// --- the two pacing selectors ----------------------------------------
	delayOpts, delaySel := pasteDelayChoices(delayMs)
	baudOpts, baudSel := pasteBaudChoices(baud)

	summary := widget.NewLabel("")
	delaySelect := widget.NewSelect(delayOpts, nil)
	baudSelect := widget.NewSelect(baudOpts, nil)

	// Read the SELECTED STRING, never the index. An index is silently wrong
	// the first time an option is inserted, and the fold-in above inserts one
	// whenever the session carries a value the shipped list does not have.
	current := func() (int, int) {
		return pasteDelayFromLabel(delaySelect.Selected), pasteBaudFromLabel(baudSelect.Selected)
	}
	refresh := func(string) {
		d, b := current()
		summary.SetText(pastePacingSummary(content, d, b, bracketed))
	}
	delaySelect.OnChanged = refresh
	baudSelect.OnChanged = refresh
	delaySelect.SetSelected(delaySel)
	baudSelect.SetSelected(baudSel)
	refresh("")

	controls := container.NewGridWithColumns(2,
		container.NewBorder(nil, nil, widget.NewLabel("Line delay"), nil, delaySelect),
		container.NewBorder(nil, nil, widget.NewLabel("Console speed"), nil, baudSelect),
	)

	// The checkbox appears only when something can actually keep the promise.
	// With no host hook there is no inventory to write to, and an offer to
	// remember that silently forgets is worse than no offer.
	var rememberBox *widget.Check
	footer := []fyne.CanvasObject{summary}
	if remember != nil {
		rememberBox = widget.NewCheck("Remember for this session", nil)
		footer = append(footer, rememberBox)
	}
	if truncated {
		footer = append(footer, widget.NewLabel(fmt.Sprintf(
			"Showing the first %d lines of %d — all %d will be sent.",
			pasteReviewMaxLines, lines, lines)))
	}

	content_ := container.NewBorder(
		container.NewVBox(head),
		container.NewVBox(append([]fyne.CanvasObject{controls}, footer...)...),
		nil, nil,
		review,
	)

	d := dialog.NewCustomConfirm("Multi-line Paste", "Paste", "Cancel", content_,
		func(ok bool) {
			if !ok {
				dprintf("PASTE: %d lines cancelled", lines)
				return
			}
			useDelay, useBaud := current()
			if rememberBox != nil && rememberBox.Checked {
				// Persist first, then apply to this widget, so a tab that
				// is already open behaves like the saved session
				// immediately rather than at the next reconnect.
				remember(useDelay, useBaud)
				t.SetPastePacing(useDelay, useBaud)
			}
			t.writePaste(content, useDelay, useBaud)
		}, win)
	// A dialog sizes itself from its content's MinSize, and the Scroll above
	// deliberately has almost none. This is where the review pane gets its
	// room.
	d.Resize(fyne.NewSize(pasteDialogWidth, pasteDialogHeight(lines)))
	d.Show()
	return true
}

// sendPaced writes one line, in chunks when a console speed is configured.
// It reports whether the whole line went out; false means cancelled or failed,
// and the caller must stop rather than move on to the next line.
func (t *NativeTerminalWidget) sendPaced(ctx context.Context, line string, chunk int, gap time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	default:
	}

	if chunk <= 0 {
		if err := t.sendInput([]byte(line)); err != nil {
			return false
		}
		t.updatePending.Store(true)
		return true
	}

	b := []byte(line)
	for len(b) > 0 {
		n := chunk
		if n > len(b) {
			n = len(b)
		}
		if err := t.sendInput(b[:n]); err != nil {
			return false
		}
		t.updatePending.Store(true)
		b = b[n:]

		// No trailing wait: the pause belongs BETWEEN chunks, and sleeping
		// after the last one only delays the newline that follows it.
		if len(b) == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(gap):
		}
	}
	return true
}

// pastePacing turns a line speed into a chunk size and the pause between
// chunks. A zero chunk means no byte pacing at all.
//
// # Why not one byte at a time
//
// That would be the obvious reading of "send at 9600 baud" and it does not
// work. Each byte would cost a goroutine wakeup and its own TCP segment, and
// the platform's timer granularity is around a millisecond, so the sleep would
// dominate the rate and the paste would run SLOWER than the line it is trying
// to match. Chunks large enough that the pause stays well above that
// granularity keep the average rate honest.
//
// # The arithmetic
//
// A console line is 8N1: eight data bits plus a start and a stop bit, so ten
// bits per character and characters-per-second is the baud rate over ten. The
// derate on top of that is not superstition — the console echoes every
// character back and the device has to service it, usually at a low interrupt
// priority, so the wire rate is a ceiling rather than a budget.
func pastePacing(baud int) (chunk int, gap time.Duration) {
	if baud <= 0 {
		return 0, 0
	}
	cps := baud * consoleDeratePercent / (bitsPerConsoleChar * 100)
	if cps < 1 {
		cps = 1
	}

	chunk = cps * int(pasteChunkInterval/time.Millisecond) / 1000
	if chunk < 1 {
		// Slower than one character per interval: send single characters and
		// stretch the pause instead, or the floor above would silently paste
		// faster than the line can carry.
		return 1, time.Second / time.Duration(cps)
	}
	return chunk, time.Duration(chunk) * time.Second / time.Duration(cps)
}

const (
	// bitsPerConsoleChar is 8N1 — the framing every console server on a
	// network device is set to.
	bitsPerConsoleChar = 10

	// consoleDeratePercent is how much of the nominal line rate a paste is
	// allowed to use. The remainder is the echo coming back and the device
	// parsing what it has already been sent.
	consoleDeratePercent = 80

	// pasteChunkInterval is the target pause between chunks. Ten milliseconds
	// is far enough above timer granularity that the pause is the pause, and
	// short enough that a chunk is small at every speed worth configuring: at
	// 9600 it is seven bytes, at 115200 about ninety.
	pasteChunkInterval = 10 * time.Millisecond
)

// cancelActivePaste aborts a paced paste currently draining on this terminal, if
// any. Safe to call when no paste is active. Calling a finished cancel func is a
// harmless no-op, so this never needs to know whether the goroutine already exited.
func (t *NativeTerminalWidget) cancelActivePaste() {
	t.pasteMutex.Lock()
	if t.pasteCancel != nil {
		t.pasteCancel()
		t.pasteCancel = nil
	}
	t.pasteMutex.Unlock()
}

// updateBracketedPasteState scans terminal output for DEC private mode 2004 toggles
// and records whether the remote app currently wants bracketed paste. It is called
// from both output paths - the local PTY (handleEscapeSequences) and the SSH read
// loop - because they don't share escape-sequence handling. pasteText reads this
// state to decide whether to wrap pasted content in ESC[200~ / ESC[201~ markers.
// A DEC 2004 sequence is 8 bytes and an SSH read boundary can land anywhere
// inside it. Neither half then contains the string, so the mode never toggles
// and the only symptom is a paste into vim arriving with auto-indent applied —
// which reads as a vim problem. modeTailLen is one less than the sequence, so a
// complete sequence is never re-scanned and cannot toggle twice.
const bracketedSeqLen = len("\x1b[?2004h")
const modeTailLen = bracketedSeqLen - 1

func (t *NativeTerminalWidget) updateBracketedPasteState(data string) {
	t.modeTailMu.Lock()
	scan := t.modeTail + data
	if len(scan) > modeTailLen {
		t.modeTail = scan[len(scan)-modeTailLen:]
	} else {
		t.modeTail = scan
	}
	t.modeTailMu.Unlock()

	// The LAST toggle wins, not "disable if present anywhere". An
	// application that redraws emits both in one burst, and scanning for
	// them in fixed order let an 'l' at the start beat an 'h' that came
	// after it.
	on := strings.LastIndex(scan, "\x1b[?2004h")
	off := strings.LastIndex(scan, "\x1b[?2004l")
	if on < 0 && off < 0 {
		return
	}
	want := on > off
	if t.bracketedPaste.Swap(want) != want {
		dprintf("TERMINAL: bracketed paste mode %v", want)
	}
}

// resetBracketedPaste clears the mode and the carry. Called from Session.Attach:
// a session that dropped while vim had the mode on would otherwise reconnect to
// a shell that never asked for it, and the first paste would arrive wrapped in
// markers the shell prints literally.
func (t *NativeTerminalWidget) resetBracketedPaste() {
	t.bracketedPaste.Store(false)
	t.modeTailMu.Lock()
	t.modeTail = ""
	t.modeTailMu.Unlock()
}

// splitLinesKeepEOL splits s into chunks that each retain their trailing "\r".
//
// It splits on CR, not LF, because everything reaching it has been through
// normalizeNewlines and CR is the only ending left — the same byte pressing
// Enter sends. A trailing partial line (no final ending) is returned as its own
// chunk.
func splitLinesKeepEOL(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// TappedSecondary is kept for completeness. Note that the terminal widget is not
// itself in the canvas tree (the scroll container holds the textGrid), so Fyne's
// secondary-tap dispatch generally won't reach it - the reliable trigger is
// HybridScrollContainer.MouseUp, which calls ShowContextMenuAt directly.
func (t *NativeTerminalWidget) TappedSecondary(ev *fyne.PointEvent) {
	t.ShowContextMenuAt(ev.AbsolutePosition)
}

// ShowContextMenuAt builds and shows the right-click Copy/Paste menu at the given
// canvas position. The canvas is resolved from the application window rather than
// from the widget, because the terminal widget is not directly in the canvas tree.
//
// Copy copies the current selection to the clipboard and clears it, matching
// the right-click menu's behavior. Exported so the window's Edit menu can drive
// the active terminal without reaching into unexported selection state. No-op
// when there is no selection.
func (t *NativeTerminalWidget) Copy() {
	if t.selection != nil && t.selection.HasSelection() {
		t.selection.CopyToClipboard()
		t.selection.Clear()
	}
}

// Paste writes the current clipboard contents to the PTY via pasteText(), so it
// inherits the same pacing/abort behavior as the keyboard shortcut and the
// right-click menu. No-op when the clipboard is empty/unavailable.
func (t *NativeTerminalWidget) Paste() {
	if cb := currentClipboard(); cb != nil {
		t.pasteText(cb.Content())
	}
}

// SelectAll selects the whole virtual buffer (scrollback plus live screen) and
// leaves it selected, so the highlight is visible and the user can narrow it
// before copying. Exported so both the right-click menu and the window's Edit
// menu can drive the active terminal.
func (t *NativeTerminalWidget) SelectAll() {
	if t.selection != nil {
		t.selection.SelectAll()
	}
}

// CopyAll is SelectAll followed by Copy, for the common case of grabbing the
// entire buffer in one action rather than two round trips through the menu.
func (t *NativeTerminalWidget) CopyAll() {
	if t.selection == nil {
		return
	}
	t.selection.SelectAll()
	t.Copy()
}

// HasSelection reports whether there is an active selection (used to enable or
// disable the Edit > Copy menu item).
func (t *NativeTerminalWidget) HasSelection() bool {
	return t.selection != nil && t.selection.HasSelection()
}

// refocusSoon puts keyboard focus back on this terminal on a later UI pass.
//
// Every exit from the context menu has to go through here, and the delay is the
// point rather than an accident. A menu item runs its action from inside
// Menu.Dismiss, so at the moment the action is called the overlay may still be
// on the canvas -- and while an overlay is up, Canvas.Focus routes to the
// OVERLAY's focus manager, not the content's. Focusing now would put this
// terminal in a manager that is about to be discarded: the call reports
// success, and the keyboard still goes nowhere.
//
// One tick later the overlay is gone, the content manager is the one being
// consulted again, and the same call lands. The shell's focus watchdog would
// eventually catch this too; this makes it immediate rather than something the
// person notices first.
func (t *NativeTerminalWidget) refocusSoon() {
	time.AfterFunc(refocusDelay, func() {
		fyne.Do(func() { t.GrabFocus() })
	})
}

// refocusDelay only has to outlast the teardown of an overlay, which happens on
// the next driver pass.
const refocusDelay = 50 * time.Millisecond

// menuAction wraps a context-menu action so focus comes back to the terminal
// whatever the action was.
//
// Wrapping every item rather than only the clipboard ones is deliberate: the
// bug being fixed is not about the clipboard, it is that dismissing an overlay
// leaves nothing focused. Copy, Paste, Select All and the logging toggle all
// dismiss the same menu, so they all need the same ending.
func (t *NativeTerminalWidget) menuAction(fn func()) func() {
	return func() {
		if fn != nil {
			fn()
		}
		t.refocusSoon()
	}
}

// Copy reuses the existing SelectionManager.CopyToClipboard(); Paste routes through
// pasteText() so it gets the same pacing/abort behavior as the keyboard shortcut.
func (t *NativeTerminalWidget) ShowContextMenuAt(pos fyne.Position) {
	// The terminal's OWN window, not AllWindows()[0]. A session detached
	// into a window of its own would otherwise raise its context menu on
	// the main window, where the pointer is not.
	win := t.HostWindow()
	if win == nil {
		return
	}
	canvas := win.Canvas()
	if canvas == nil {
		return
	}

	hasSelection := t.HasSelection()

	copyItem := fyne.NewMenuItem("Copy", t.menuAction(t.Copy))
	copyItem.Disabled = !hasSelection

	pasteItem := fyne.NewMenuItem("Paste", t.menuAction(t.Paste))

	selectAllItem := fyne.NewMenuItem("Select All", t.menuAction(t.SelectAll))

	copyAllItem := fyne.NewMenuItem("Copy All", t.menuAction(t.CopyAll))

	// Nothing in the buffer yet -- keep both disabled rather than letting them
	// produce an empty selection.
	if t.screen == nil || t.screen.GetTotalContentLines() <= 0 {
		selectAllItem.Disabled = true
		copyAllItem.Disabled = true
	}

	items := []*fyne.MenuItem{copyItem, pasteItem, fyne.NewMenuItemSeparator(), selectAllItem, copyAllItem}

	saveScroll := fyne.NewMenuItem("Save Scrollback…", t.menuAction(t.promptSaveScrollback))
	if t.screen == nil || t.screen.GetTotalContentLines() <= 0 {
		saveScroll.Disabled = true
	}
	items = append(items, saveScroll)

	// Live logging toggle (only when the SSH widget injected the hooks)
	if t.isLoggingFn != nil && t.toggleLoggingFn != nil {
		label := "Start Capture"
		if t.isLoggingFn() {
			label = "Stop Capture"
		}
		logItem := fyne.NewMenuItem(label, t.menuAction(func() {
			on, msg := t.toggleLoggingFn()
			if msg != "" {
				title := "Session Capture"
				if on {
					dialog.ShowInformation(title, msg, win)
				} else {
					dialog.ShowInformation(title, msg, win)
				}
			}
		}))
		items = append(items, fyne.NewMenuItemSeparator(), logItem)
	}

	// Tab management (only when hosted in the tabbed session manager). Mirrors
	// the "close other / close all" entries other terminals put on a tab's
	// right-click menu; Fyne's DocTabs has no header-level secondary-tap hook,
	// so they live on the terminal body's menu instead.
	if t.closeAllTabsFn != nil {
		closeOthers := fyne.NewMenuItem("Close Other Tabs", t.menuAction(func() {
			if t.closeOtherTabsFn != nil {
				t.closeOtherTabsFn()
			}
		}))
		// Nothing to close when this is the only tab.
		if t.tabCountFn != nil && t.tabCountFn() <= 1 {
			closeOthers.Disabled = true
		}

		// No refocus wrapper on this one: after it runs there is no
		// terminal left to focus, and this one has just been closed.
		closeAll := fyne.NewMenuItem("Close All Tabs", func() {
			t.closeAllTabsFn()
		})

		items = append(items, fyne.NewMenuItemSeparator(), closeOthers, closeAll)
	}

	menu := fyne.NewMenu("", items...)
	popup := widget.NewPopUpMenu(menu, canvas)

	// Dismissing without choosing anything -- Escape, or a click outside the
	// menu -- has to end the same way as choosing something. NewPopUpMenu
	// installs its own OnDismiss to hide itself, so keep that call and add the
	// refocus after it rather than replacing it, or the menu stays up.
	dismiss := popup.OnDismiss
	popup.OnDismiss = func() {
		if dismiss != nil {
			dismiss()
		}
		t.refocusSoon()
	}

	popup.ShowAtPosition(pos)
}

// currentClipboard returns the clipboard of the first application window.
//
// Any window will do here and that is not an accident: the clipboard is the
// system's, not the window's, so every window returns the same one. This is the
// one place AllWindows()[0] is still correct now that a session can be detached
// into a window of its own -- ShowContextMenuAt, which genuinely needed the
// terminal's own window, uses HostWindow instead.
func currentClipboard() fyne.Clipboard {
	windows := fyne.CurrentApp().Driver().AllWindows()
	if len(windows) == 0 {
		return nil
	}
	return windows[0].Clipboard()
}
