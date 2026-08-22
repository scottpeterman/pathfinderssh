// internal/ui/terminal_selection.go
package ui

import (
	"strings"
	"time"

	"github.com/scottpeterman/pathfinderssh/internal/gopyte"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
)

// SelectionManager tracks a text selection in ABSOLUTE virtual-buffer
// coordinates (an absolute line index into history+screen, plus a column),
// rather than viewport-relative pixels. Anchoring to absolute lines is what
// lets a selection survive scrolling: as the display window scrolls, the same
// anchor/focus lines stay pinned to their text, and the highlight is re-clipped
// to whatever is currently visible on each redraw.
//
// It also drives auto-scroll: while the primary button is held and the pointer
// is dragged above the top edge or below the bottom edge of the terminal, the
// history scrolls in that direction on a timer and the selection's focus is
// continuously extended to the freshly revealed edge line - the standard
// "drag past the edge to keep selecting" behavior.
type SelectionManager struct {
	terminal *NativeTerminalWidget

	// Selection endpoints in absolute virtual-buffer coordinates.
	// anchor is fixed at mouse-down; focus follows the pointer / auto-scroll.
	anchorAbsLine int
	anchorCol     int
	focusAbsLine  int
	focusCol      int

	isSelecting  bool
	hasSelection bool

	// Last drag position, kept in BOTH spaces: the canvas-absolute one drives
	// the hit test (see gridCellAtAbs), the widget-local one drives the
	// past-the-edge test. The auto-scroll timer reuses both to recompute the
	// focus column/edge after each scroll step.
	lastDragPos   fyne.Position
	lastDragLocal fyne.Position

	// Auto-scroll state. autoScrollDir is -1 (up/into history), +1 (down/toward
	// present), or 0 (off). The loop goroutine is stopped by closing stopCh.
	autoScrollActive bool
	autoScrollDir    int
	stopCh           chan struct{}

	// Cached copy of the last selection text.
	selectedText string
}

func NewSelectionManager(terminal *NativeTerminalWidget) *SelectionManager {
	return &SelectionManager{
		terminal: terminal,
	}
}

func (sm *SelectionManager) HandleMouseDown(event *desktop.MouseEvent) bool {
	if event.Button != desktop.MouseButtonPrimary {
		return false
	}

	sm.Clear()
	absLine, col := sm.posToAbs(event.AbsolutePosition, event.Position)
	sm.anchorAbsLine, sm.anchorCol = absLine, col
	sm.focusAbsLine, sm.focusCol = absLine, col
	sm.lastDragPos = event.AbsolutePosition
	sm.lastDragLocal = event.Position
	sm.isSelecting = true
	sm.hasSelection = false

	sm.terminal.updatePending.Store(true)
	return true
}

func (sm *SelectionManager) HandleMouseUp(event *desktop.MouseEvent) bool {
	if event.Button != desktop.MouseButtonPrimary || !sm.isSelecting {
		return false
	}

	sm.isSelecting = false
	sm.stopAutoScroll()

	// Real selection only if anchor and focus differ.
	if sm.anchorAbsLine != sm.focusAbsLine || sm.anchorCol != sm.focusCol {
		sm.hasSelection = true
		// Do NOT auto-copy on mouse-up: Win32 clipboard SetContent can block
		// the UI thread, and micro-drags on ordinary clicks triggered it.
		// Copy is Ctrl+C / shortcut / explicit CopyToClipboard.
		sm.terminal.updatePending.Store(true)
	} else {
		// Plain click - drop any prior selection.
		sm.Clear()
	}

	return true
}

func (sm *SelectionManager) HandleDrag(abs, local fyne.Position) bool {
	if !sm.isSelecting {
		return false
	}

	sm.lastDragPos = abs
	sm.lastDragLocal = local
	sm.updateFocusFromPos(abs, local)
	sm.hasSelection = true
	sm.updateAutoScroll(local)
	sm.terminal.updatePending.Store(true)
	return true
}

// updateFocusFromPos moves the selection focus to the cell under the pointer
// (clamped into the visible viewport), translated to an absolute line.
func (sm *SelectionManager) updateFocusFromPos(abs, local fyne.Position) {
	sm.focusAbsLine, sm.focusCol = sm.posToAbs(abs, local)
}

// updateAutoScroll starts or stops the auto-scroll loop based on whether the
// pointer is currently outside the vertical bounds of the terminal viewport.
func (sm *SelectionManager) updateAutoScroll(pos fyne.Position) {
	t := sm.terminal

	// No history scrolling in full-screen apps (vim/htop): everything visible.
	if t.screen != nil && t.screen.IsUsingAlternate() {
		sm.stopAutoScroll()
		return
	}

	vp, _ := sm.viewportInfo()
	// Same metric the hit test divides by, or the "pointer is past the bottom
	// edge" test fires a row early or late.
	_, cellH := t.gridCellSize()
	heightPx := float32(vp.visibleLines) * cellH

	switch {
	case pos.Y < 0:
		sm.autoScrollDir = -1
		sm.startAutoScroll()
	case pos.Y > heightPx:
		sm.autoScrollDir = 1
		sm.startAutoScroll()
	default:
		sm.stopAutoScroll()
	}
}

// startAutoScroll launches the repeating scroll timer (idempotent). A timer is
// required because Fyne only emits Dragged events while the pointer actually
// moves; holding it still past the edge must keep scrolling on its own.
func (sm *SelectionManager) startAutoScroll() {
	if sm.autoScrollActive {
		return
	}
	sm.autoScrollActive = true
	stop := make(chan struct{})
	sm.stopCh = stop

	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				fyne.Do(func() { sm.autoScrollStep() })
			}
		}
	}()
}

func (sm *SelectionManager) stopAutoScroll() {
	if !sm.autoScrollActive {
		return
	}
	sm.autoScrollActive = false
	sm.autoScrollDir = 0
	if sm.stopCh != nil {
		close(sm.stopCh)
		sm.stopCh = nil
	}
}

// autoScrollStep runs on the UI thread (via fyne.Do). It scrolls the history a
// little in the active direction, then re-derives the focus from the last drag
// position so the selection extends to the newly revealed edge line.
func (sm *SelectionManager) autoScrollStep() {
	if !sm.isSelecting || sm.autoScrollDir == 0 {
		return
	}
	t := sm.terminal
	if t.screen == nil || t.screen.IsUsingAlternate() {
		sm.stopAutoScroll()
		return
	}

	const linesPerStep = 2
	if sm.autoScrollDir < 0 {
		t.screen.ScrollUp(linesPerStep)
	} else {
		t.screen.ScrollDown(linesPerStep)
	}

	// Extend the selection focus to the (clamped) pointer position against the
	// now-scrolled viewport.
	sm.updateFocusFromPos(sm.lastDragPos, sm.lastDragLocal)
	sm.hasSelection = true
	t.updatePending.Store(true)
}

func (sm *SelectionManager) GetSelectedText() string {
	if !sm.hasSelection && !sm.isSelecting {
		return ""
	}

	sRow, sCol, eRow, eCol := sm.normalized()

	// Pull exactly the spanned absolute lines, even if they're outside the
	// current display window (e.g. the user dragged across several screens).
	lines := sm.terminal.screen.GetLinesInRange(sRow, eRow+1)
	if len(lines) == 0 {
		return ""
	}

	var b strings.Builder

	if sRow == eRow {
		runes := []rune(lines[0])
		if sCol < len(runes) {
			end := eCol
			if end > len(runes) {
				end = len(runes)
			}
			if end > sCol {
				b.WriteString(strings.TrimRight(string(runes[sCol:end]), " "))
			}
		}
		// Columns above are grid columns, so the slice may carry the
		// continuation spacer of a double-width glyph. Drop it here: what
		// the user copies is text, not a grid row.
		return gopyte.StripContinuations(b.String())
	}

	for i := 0; i < len(lines); i++ {
		row := sRow + i
		runes := []rune(lines[i])

		switch {
		case row == sRow:
			if sCol < len(runes) {
				b.WriteString(strings.TrimRight(string(runes[sCol:]), " "))
			}
			b.WriteString("\r\n")
		case row == eRow:
			end := eCol
			if end > len(runes) {
				end = len(runes)
			}
			if end > 0 {
				b.WriteString(strings.TrimRight(string(runes[:end]), " "))
			}
		default:
			b.WriteString(strings.TrimRight(lines[i], " "))
			b.WriteString("\r\n")
		}
	}

	return gopyte.StripContinuations(b.String())
}

func (sm *SelectionManager) CopyToClipboard() {
	text := sm.GetSelectedText()
	if text == "" {
		return
	}

	sm.selectedText = text

	windows := fyne.CurrentApp().Driver().AllWindows()
	if len(windows) == 0 {
		return
	}
	windows[0].Clipboard().SetContent(text)
}

func (sm *SelectionManager) Clear() {
	sm.stopAutoScroll()
	sm.hasSelection = false
	sm.isSelecting = false
	sm.selectedText = ""
	sm.terminal.updatePending.Store(true)
}

func (sm *SelectionManager) HasSelection() bool {
	return sm.hasSelection
}

func (sm *SelectionManager) IsSelecting() bool {
	return sm.isSelecting
}

// SetSelection lets word/line double/triple-click helpers (or any caller that
// computes its own absolute range) drive the selection directly. endCol is
// exclusive on endLine.
func (sm *SelectionManager) SetSelection(startLine, startCol, endLine, endCol int) {
	sm.anchorAbsLine, sm.anchorCol = startLine, startCol
	sm.focusAbsLine, sm.focusCol = endLine, endCol
	sm.hasSelection = true
	sm.isSelecting = false
	sm.terminal.updatePending.Store(true)
}

// SelectAll selects the entire virtual buffer: all of scrollback plus the live
// screen. The range is absolute, so it does not depend on the current scroll
// position or on how much of the buffer happens to be rendered.
//
// Trailing blank rows below the cursor are excluded, so a select-all on a
// mostly-empty screen does not copy a run of empty lines.
func (sm *SelectionManager) SelectAll() {
	total := sm.terminal.screen.GetTotalContentLines()
	if total <= 0 {
		return
	}

	last := total - 1
	if lines := sm.terminal.screen.GetLinesInRange(0, total); len(lines) > 0 {
		for last = len(lines) - 1; last > 0; last-- {
			if strings.TrimSpace(lines[last]) != "" {
				break
			}
		}
	}

	// endCol is exclusive and clipped against the real line length by
	// GetSelectedText, so the full width is safe here.
	sm.SetSelection(0, 0, last, sm.terminal.cols)
}

// normalized returns the selection as an ordered absolute range with the start
// before the end. endCol is exclusive on the end row.
func (sm *SelectionManager) normalized() (sRow, sCol, eRow, eCol int) {
	sRow, sCol = sm.anchorAbsLine, sm.anchorCol
	eRow, eCol = sm.focusAbsLine, sm.focusCol
	if sRow > eRow || (sRow == eRow && sCol > eCol) {
		sRow, eRow = eRow, sRow
		sCol, eCol = eCol, sCol
	}
	return
}

// viewportInfo returns the current virtual-scroll viewport plus the absolute
// virtual-buffer line index of its top visible row. topAbs ties viewport-local
// rows to absolute lines: visible row r == absolute line (topAbs + r).
//
// Intentionally avoids GetDisplay(): rebuilding the full buffer on every
// mouse-down contended with the feed path on screen.mu and froze the UI.
func (sm *SelectionManager) viewportInfo() (VirtualScrollState, int) {
	t := sm.terminal
	totalLines := 0
	if t.screen != nil {
		totalLines = t.screen.GetTotalContentLines()
	}
	if totalLines <= 0 {
		totalLines = t.rows
	}
	var viewingHist bool
	var pos, maxPos int
	if t.screen != nil {
		viewingHist = t.screen.IsViewingHistory()
		if viewingHist {
			pos, maxPos = t.screen.GetHistoryPos(), t.screen.GetMaxHistoryPos()
		}
	}
	vp := t.calculateViewport(totalLines, viewingHist, pos, maxPos)
	topAbs := 0
	if t.screen != nil {
		topAbs = t.screen.GetViewportStart() + vp.scrollOffset
	}
	return vp, topAbs
}

// posToAbs converts a viewport-pixel position into an absolute (line, col).
// The pixel->cell mapping matches the rest of the widget; the row is clamped
// into the visible window (so dragging past an edge pins to that edge), then
// offset by the viewport's absolute top to yield a stable line index.
func (sm *SelectionManager) posToAbs(abs, local fyne.Position) (absLine, col int) {
	t := sm.terminal
	vp, topAbs := sm.viewportInfo()

	// Ask the TextGrid where this position lands -- it owns the cell size and
	// the origin, and a second opinion computed here can only agree with it or
	// be wrong about it. See NativeTerminalWidget.gridCellAt.
	//
	// The fallback is the old arithmetic, for a widget with no canvas yet.
	row, col, ok := t.gridCellAtAbs(abs)
	if !ok {
		// Fallback for a widget with no canvas (tests) or a lookup that did not
		// resolve: the widget-local position with plain arithmetic.
		cw, ch := t.gridCellSize()
		row = 0
		if ch > 0 {
			row = int(local.Y / ch)
		}
		col = 0
		if cw > 0 {
			col = int(local.X / cw)
		}
	}
	if row < 0 {
		row = 0
	}
	if vp.visibleLines > 0 && row > vp.visibleLines-1 {
		row = vp.visibleLines - 1
	}

	if col < 0 {
		col = 0
	}
	if col > t.cols {
		col = t.cols
	}

	absLine = topAbs + row
	if total := t.screen.GetTotalContentLines(); total > 0 && absLine > total-1 {
		absLine = total - 1
	}
	if absLine < 0 {
		absLine = 0
	}
	return absLine, col
}

// toRange clips the absolute selection to the currently visible window and
// returns it in viewport-local cell coordinates for the background overlay, or
// nil if there is nothing visible to draw. topAbs is the absolute line index of
// the top visible row (visible row r == absolute line topAbs+r). When the
// selection starts above the window the top row is highlighted full-width from
// column 0; when it ends below the window the bottom row is highlighted through
// the last column.
func (sm *SelectionManager) toRange(viewport VirtualScrollState, topAbs int) *selRange {
	if !sm.hasSelection && !sm.isSelecting {
		return nil
	}

	sRow, sCol, eRow, eCol := sm.normalized()
	if sRow == eRow && sCol == eCol {
		return nil
	}

	bottomAbs := topAbs + viewport.visibleLines - 1
	if eRow < topAbs || sRow > bottomAbs {
		return nil // selection entirely off-screen
	}

	vStartRow := sRow - topAbs
	vStartCol := sCol
	if vStartRow < 0 {
		vStartRow = 0
		vStartCol = 0
	}

	vEndRow := eRow - topAbs
	vEndCol := eCol
	if vEndRow > viewport.visibleLines-1 {
		vEndRow = viewport.visibleLines - 1
		vEndCol = sm.terminal.cols
	}

	return &selRange{
		startRow: vStartRow,
		startCol: vStartCol,
		endRow:   vEndRow,
		endCol:   vEndCol,
	}
}