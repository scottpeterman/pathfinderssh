// internal/gopyte/wide_char_screen.go
package gopyte

import (
	"fmt"
	"strings"
	"sync"

	runewidth "github.com/mattn/go-runewidth"
)

// WideCharScreen adds wide character (CJK, emoji) support to HistoryScreen
type WideCharScreen struct {
	*HistoryScreen

	// mu serializes ALL access to the screen model.
	//
	// Locking rule (the whole convention in three lines):
	//   - Exported methods take mu and delegate to an unexported *Locked body.
	//   - Anything called while mu is held uses the *Locked body directly.
	//   - VT handlers (Draw, SetMode, ResetMode, Backspace, and every method
	//     promoted from HistoryScreen/NativeScreen) do NOT lock: they only ever
	//     run inside Stream.Feed, and the CALLER holds mu across that Feed.
	//
	// The write side cannot lock per-method: most VT handlers (CarriageReturn,
	// Linefeed, EraseInLine, ...) are promoted from NativeScreen and are never
	// overridden here, so there is no place to put a lock. Feeding under the
	// caller's lock covers all of them at once.
	mu sync.Mutex

	// Cell widths are NOT owned here. They live on the store rows and are
	// reached through the embedded HistoryScreen, so there is no second grid
	// to drift out of step with the character grid after a resize or an
	// alt-screen swap.

	// Alternate screen state. The alt screen is a separate store with a zero
	// scrollback budget; entering and leaving it is a pointer swap.
	usingAlternate bool

	// applicationCursorKeys is DECCKM (DEC private mode 1). It selects which
	// of the two encodings the arrow and Home/End keys must use on the way
	// OUT to the remote: normal ESC[A, or application ESC O A. The remote
	// chooses - vim sends ESC[?1h on entry (terminfo smkx) and ESC[?1l on
	// exit - and terminfo for xterm-256color declares kcuu1=\EOA, so a
	// terminal that ignores this sends bytes vim does not recognise. vim then
	// reads them one at a time and executes "[D" as its list-definitions
	// command, which is the E388 error.
	//
	// It is INDEPENDENT of usingAlternate. A full-screen application may use
	// either without the other; do not couple them.
	//
	// Written by the VT handlers (SetMode/ResetMode) with no lock of their
	// own - they run inside Stream.Feed under the caller's mu - and read
	// through IsApplicationCursorKeys, which takes mu. Same arrangement as
	// usingAlternate.
	applicationCursorKeys bool
	altStore              *RowStore
	mainStore             *RowStore
	altCursor             Cursor
	mainCursor            Cursor
	HistoryPos            int

	// Virtual scrolling optimization
	virtualScrolling   bool
	viewportStart      int            // First visible line index (in total content)
	viewportEnd        int            // Last visible line index (in total content)
	lastRequestedLines int            // Track what was last requested
	displayCache       []string       // Cache for rendered display
	attributeCache     [][]Attributes // Cache for attributes

	// The cache is keyed on the store it was built from and that store's
	// mutation counter. Anything that changes visible content bumps the
	// counter, so staleness is detected here rather than by remembering to
	// call InvalidateCache at every call site that might have changed
	// something.
	cacheStore        *RowStore
	cacheGen          uint64
	totalContentLines int // Total lines available (History + current)
}

// NewWideCharScreen creates a screen with wide character support and History
func NewWideCharScreen(columns, lines, maxHistory int) *WideCharScreen {
	HistoryScreen := NewHistoryScreen(columns, lines, maxHistory)

	w := &WideCharScreen{
		HistoryScreen:  HistoryScreen,
		usingAlternate: false,

		// Initialize virtual scrolling
		virtualScrolling:   true, // Enable by default for better performance
		viewportStart:      0,
		viewportEnd:        0,
		lastRequestedLines: 0,
		displayCache:       nil,
		attributeCache:     nil,
		totalContentLines:  0,
	}

	// The alternate screen retains no scrollback, which is what a zero
	// budget means to the store.
	w.altStore = NewRowStore(columns, lines, 0)
	w.mainStore = w.HistoryScreen.Store()

	return w
}

// IsUsingAlternate returns true if in alternate screen mode
// Snapshot is a consistent, point-in-time copy of everything a renderer needs.
// Taking it in ONE lock acquisition is the point: reading the same fields via
// ten separate exported getters lets a feed land between them, so the cursor
// could describe a screen the lines no longer show.
type Snapshot struct {
	Lines             []string
	Attrs             [][]Attributes
	CursorX, CursorY  int
	UsingAlternate    bool
	ViewingHistory    bool
	HistoryPos        int
	MaxHistoryPos     int
	ViewportStart     int
	TotalContentLines int
}

// Snapshot captures the render read-set atomically.
func (w *WideCharScreen) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()

	snap := Snapshot{
		Lines:             w.getDisplayLocked(),
		UsingAlternate:    w.usingAlternate,
		ViewingHistory:    w.isViewingHistoryLocked(),
		TotalContentLines: w.totalContentLinesLocked(),
	}
	// After getDisplayLocked, so viewportStart reflects the window just built.
	snap.Attrs = w.getAttributesLocked()
	if w.usingAlternate {
		snap.ViewportStart = 0
	} else {
		snap.ViewportStart = w.viewportStart
	}
	if w.HistoryScreen != nil {
		snap.HistoryPos = w.HistoryScreen.GetHistoryPos()
		snap.MaxHistoryPos = w.HistoryScreen.GetMaxHistoryPos()
	}
	snap.CursorX, snap.CursorY = w.cursor.X, w.cursor.Y
	return snap
}

// Lock and Unlock expose the model lock so a caller can hold it across a whole
// Stream.Feed. This is REQUIRED on every feed path: the VT handlers that Feed
// dispatches to are mostly promoted from NativeScreen and take no lock of their
// own, so the caller's lock is the only thing serializing writes against the
// exported readers below.
//
//	screen.Lock()
//	stream.Feed(chunk)
//	screen.Unlock()
func (w *WideCharScreen) Lock()   { w.mu.Lock() }
func (w *WideCharScreen) Unlock() { w.mu.Unlock() }

// IsApplicationCursorKeys reports whether DECCKM is set, i.e. whether the
// cursor keys must be sent as SS3 (ESC O A) rather than CSI (ESC [ A).
func (w *WideCharScreen) IsApplicationCursorKeys() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.applicationCursorKeys
}

func (w *WideCharScreen) IsUsingAlternate() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.usingAlternate
}

// Override Draw to handle wide characters and emojis
func (w *WideCharScreen) Draw(text string) {
	// Invalidate cache when new content arrives
	w.invalidateCache()

	// Exit History mode if in main screen and viewing History
	if !w.usingAlternate && w.isViewingHistoryLocked() {
		w.scrollToBottomLocked()
	}

	// Process each character with width awareness
	for _, ch := range text {
		w.drawChar(ch)
	}
}

// drawChar handles a single character with width calculation
func (w *WideCharScreen) drawChar(ch rune) {
	// Get the display width of the character
	charWidth := runewidth.RuneWidth(ch)

	// Handle zero-width characters (combining marks, etc.)
	if charWidth == 0 {
		w.handleZeroWidth(ch)
		return
	}

	// Check if the character fits at current position
	if w.cursor.X+charWidth > w.columns {
		if w.autoWrap {
			// Autowrap moves to the next line, and at the bottom of the ACTIVE
			// SCROLL REGION it scrolls that region -- not the whole screen.
			// Index() is exactly that step, and routing through it means the
			// wrap path and the newline path can no longer disagree about
			// either the push or the region. The previous code compared
			// against w.lines and scrolled the whole window, so text wrapping
			// at the bottom of a region dragged the rows below it (vim's
			// status line) up into the region.
			w.cursor.X = 0
			w.HistoryScreen.Index()
		} else {
			// Can't place character at edge without wrapping
			return
		}
	}

	// CRITICAL FIX: Bounds check against actual buffer sizes, not just w.lines/w.columns
	// After resize, buffers might not match the new dimensions yet
	if w.cursor.Y < 0 || w.cursor.X < 0 {
		return
	}
	if w.cursor.Y >= len(w.buffer) || w.cursor.Y >= len(w.cellWidths) || w.cursor.Y >= len(w.attrs) {
		return
	}
	if w.cursor.X >= len(w.buffer[w.cursor.Y]) || w.cursor.X >= len(w.cellWidths[w.cursor.Y]) || w.cursor.X >= len(w.attrs[w.cursor.Y]) {
		return
	}

	// Clear any wide character we're overwriting
	w.clearCellAt(w.cursor.Y, w.cursor.X)

	w.buffer[w.cursor.Y][w.cursor.X] = ch
	w.attrs[w.cursor.Y][w.cursor.X] = w.cursor.Attrs
	w.cellWidths[w.cursor.Y][w.cursor.X] = charWidth

	if charWidth == 2 {
		// Mark the next cell as continuation - with bounds check
		nextX := w.cursor.X + 1
		if nextX < len(w.buffer[w.cursor.Y]) && nextX < len(w.cellWidths[w.cursor.Y]) && nextX < len(w.attrs[w.cursor.Y]) {
			w.buffer[w.cursor.Y][nextX] = 0 // Null char for continuation
			w.attrs[w.cursor.Y][nextX] = w.cursor.Attrs
			w.cellWidths[w.cursor.Y][nextX] = 0 // Continuation marker
		}
	}

	w.cursor.X += charWidth
}

// Backspace moves the cursor one column to the left WITHOUT erasing.
//
// BS (0x08) is a non-destructive cursor movement in a VT terminal. The host
// (readline, bash line editing, etc.) walks the cursor left with runs of BS
// for Ctrl-A, the left arrow, and mid-line insert/delete, and expects the
// cells it passes over to stay intact; the actual erasing is always done
// explicitly via a space-overwrite or an EL/DCH sequence. The previous
// implementation cleared the destination cell, so every leftward move wiped
// the character under it (Ctrl-A erased the whole line, left arrow ate a
// character, mid-line edits corrupted the display).
//
// Wide-character awareness is preserved: if the new column lands on the
// continuation half of a wide glyph (width 0), step one more column left so
// the cursor sits on the glyph's start cell.
func (w *WideCharScreen) Backspace() {
	if w.cursor.X <= 0 {
		return
	}
	w.cursor.X--
	if w.cursor.Y >= 0 && w.cursor.Y < len(w.cellWidths) &&
		w.cursor.X > 0 && w.cursor.X < len(w.cellWidths[w.cursor.Y]) &&
		w.cellWidths[w.cursor.Y][w.cursor.X] == 0 {
		w.cursor.X--
	}
}

// Enhanced GetDisplay with virtual scrolling

func (w *WideCharScreen) GetDisplay() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getDisplayLocked()
}

func (w *WideCharScreen) getDisplayLocked() []string {
	dprintf("WideCharScreen.GetDisplay: START\n")

	// In alternate screen mode, no virtual scrolling needed
	if w.usingAlternate {
		dprintf("WideCharScreen.GetDisplay: using alternate screen, rendering current content\n")
		w.invalidateCache()
		result := w.renderCurrentScreenContent()
		dprintf("WideCharScreen.GetDisplay: alternate screen returned %d lines\n", len(result))
		return result
	}

	// Calculate total available content with detailed buffer information
	HistorySize := w.getHistorySizeLocked()
	actualBufferLines := 0
	if w.HistoryScreen != nil {
		actualBufferLines = w.HistoryScreen.Store().Scrollback()
	}
	currentScreenLines := w.lines
	totalLines := HistorySize + currentScreenLines
	w.totalContentLines = totalLines

	dprintf("WideCharScreen.GetDisplay: BUFFER ANALYSIS:\n")
	dprintf("  - actualBufferLines (linked list): %d\n", actualBufferLines)
	dprintf("  - HistorySize (reported): %d\n", HistorySize)
	dprintf("  - currentScreenLines: %d\n", currentScreenLines)
	dprintf("  - virtualTotalLines: %d\n", totalLines)
	dprintf("  - viewportLines (terminal rows): %d\n", w.lines)

	// Add debug info about History state
	if w.HistoryScreen != nil {
		dprintf("WideCharScreen.GetDisplay: ViewingHistory=%v, HistoryPos=%d/%d\n",
			w.HistoryScreen.IsViewingHistory(), w.HistoryScreen.GetHistoryPos(), HistorySize)
	}

	// If we have virtual scrolling enabled and cache is valid, use it
	if w.virtualScrolling && w.cacheFresh() && w.displayCache != nil {
		dprintf("WideCharScreen.GetDisplay: using cached result (%d lines)\n", len(w.displayCache))
		return w.displayCache
	}

	var linesToRender []string

	if w.isViewingHistoryLocked() {
		dprintf("WideCharScreen.GetDisplay: viewing History, calling getProgressiveHistoryContent\n")
		linesToRender = w.getProgressiveHistoryContent()
	} else {
		dprintf("WideCharScreen.GetDisplay: normal mode, calling getRecentContext\n")
		linesToRender = w.getRecentContext()
	}

	dprintf("getProgressiveHistoryContent: got %d lines to render\n", len(linesToRender))

	// ENHANCED DEBUG: Show buffer analysis and content verification
	if len(linesToRender) > 0 {
		dprintf("WideCharScreen.GetDisplay: CONTENT VERIFICATION:\n")
		dprintf("  - returned %d lines for viewport\n", len(linesToRender))
		dprintf("  - viewport covers virtual lines [%d-%d] of %d total\n",
			w.viewportStart, w.viewportEnd-1, totalLines)

		// Show first line content to verify we're getting the right data
		firstLine := linesToRender[0]
		if len(firstLine) > 60 {
			firstLine = firstLine[:60] + "..."
		}
		dprintf("  - first line: %q\n", firstLine)

		if len(linesToRender) > 1 {
			lastIdx := len(linesToRender) - 1
			lastLine := linesToRender[lastIdx]
			if len(lastLine) > 60 {
				lastLine = lastLine[:60] + "..."
			}
			dprintf("  - last line: %q\n", lastLine)
		}

		// Show what range of actual History we're accessing
		if w.viewportStart < HistorySize {
			HistoryLines := min(w.viewportEnd, HistorySize) - w.viewportStart
			dprintf("  - showing %d History lines + %d current screen lines\n",
				HistoryLines, len(linesToRender)-HistoryLines)
		} else {
			dprintf("  - showing %d current screen lines only\n", len(linesToRender))
		}
	}

	// Cache the result
	w.displayCache = linesToRender
	w.attributeCache = w.getAttributesForLines(linesToRender)
	w.markCacheFresh()

	dprintf("WideCharScreen.GetDisplay: END, returning %d lines\n", len(linesToRender))
	return linesToRender
}

// Enhanced GetAttributes with caching
func (w *WideCharScreen) GetAttributes() [][]Attributes {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getAttributesLocked()
}

func (w *WideCharScreen) getAttributesLocked() [][]Attributes {
	// In alternate screen mode, no virtual scrolling needed
	if w.usingAlternate {
		return w.extractCurrentScreenAttributes()
	}

	// If we have cached attributes and they're valid, use them
	if w.virtualScrolling && w.cacheFresh() && w.attributeCache != nil {
		return w.attributeCache
	}

	// This will trigger GetDisplay() which will populate attributeCache
	_ = w.getDisplayLocked()

	return w.attributeCache
}

func (w *WideCharScreen) getProgressiveHistoryContent() []string {
	dprintf("getProgressiveHistoryContent: START\n")

	HistorySize := w.getHistorySizeLocked()
	actualBufferLines := 0
	if w.HistoryScreen != nil {
		actualBufferLines = w.HistoryScreen.Store().Scrollback()
	}
	virtualTotalLines := HistorySize + w.lines
	viewableScreenLines := w.lines

	dprintf("getProgressiveHistoryContent: BUFFER DEBUG - actualBufferLines=%d, HistorySize=%d, screenLines=%d, virtualTotal=%d\n",
		actualBufferLines, HistorySize, viewableScreenLines, virtualTotalLines)

	if HistorySize == 0 {
		dprintf("getProgressiveHistoryContent: no History, rendering current screen\n")
		return w.renderCurrentScreenContent()
	}

	currentHistoryPos := w.HistoryScreen.GetHistoryPos()
	dprintf("getProgressiveHistoryContent: HistorySize=%d, currentHistoryPos=%d\n", HistorySize, currentHistoryPos)

	// SIMPLE APPROACH: Map History position directly to content position
	// When currentHistoryPos = 0: show recent content (bottom of History + current screen)
	// When currentHistoryPos = HistorySize: show oldest content (top of History + some context)

	totalAvailableLines := HistorySize + w.lines

	// Calculate how much content to show
	displayLines := w.lines * 3 // Show 3 screens worth for context
	if displayLines > totalAvailableLines {
		displayLines = totalAvailableLines
	}

	// Map History position to start position
	// currentHistoryPos=0 -> show end of content (recent)
	// currentHistoryPos=max -> show beginning of content (oldest)

	var startPos int
	if currentHistoryPos == 0 {
		// At bottom - show recent content
		startPos = totalAvailableLines - displayLines
		if startPos < 0 {
			startPos = 0
		}
	} else {
		// In History - map position directly
		// Higher currentHistoryPos = show older content (lower startPos)
		maxPos := HistorySize
		scrollRatio := float64(currentHistoryPos) / float64(maxPos)

		// When scrollRatio = 1.0 (at top of History), startPos = 0
		// When scrollRatio = 0.0 (at bottom), startPos = totalAvailableLines - displayLines
		maxStartPos := totalAvailableLines - displayLines
		if maxStartPos < 0 {
			maxStartPos = 0
		}

		startPos = maxStartPos - int(scrollRatio*float64(maxStartPos))

		// Ensure we don't go negative
		if startPos < 0 {
			startPos = 0
		}
	}

	endPos := startPos + displayLines
	if endPos > totalAvailableLines {
		endPos = totalAvailableLines
		startPos = endPos - displayLines
		if startPos < 0 {
			startPos = 0
		}
	}

	w.viewportStart = startPos
	w.viewportEnd = endPos

	dprintf("getProgressiveHistoryContent: SIMPLIFIED - HistoryPos=%d/%d maps to lines [%d-%d] of %d total\n",
		currentHistoryPos, HistorySize, startPos, endPos-1, totalAvailableLines)

	result := w.renderLinesInRange(startPos, endPos)
	dprintf("getProgressiveHistoryContent: END, returning %d lines\n", len(result))
	return result
}

// Get recent context for normal typing mode (live edge, not scrolled into history).
//
// Only the current screen is rendered here. An earlier version also rebuilt up
// to 200 scrollback lines on every paint so a tiny flick of the wheel felt
// instant — but every Feed (Enter echo, command output) invalidates the cache,
// so that path ran on every keystroke and made Return feel like it hung for a
// beat. Wheel scroll enters history mode and uses getProgressiveHistoryContent.
func (w *WideCharScreen) getRecentContext() []string {
	HistorySize := w.getHistorySizeLocked()
	w.viewportStart = HistorySize
	w.viewportEnd = HistorySize + w.lines
	return w.renderLinesInRange(HistorySize, HistorySize+w.lines)
}

func (w *WideCharScreen) renderLinesInRange(start, end int) []string {
	// Absolute virtual-buffer line i is store row (origin + i) for every i in
	// [0, scrollback+lines): scrollback rows come first and the live screen
	// begins at base, which is origin+scrollback. So the whole range is one
	// contiguous walk. The old two-branch version fused a history slice with
	// a separately rendered screen slice, which is where a selection dragged
	// across the boundary could pick up a duplicated or dropped line.
	if start < 0 {
		start = 0
	}
	st := w.HistoryScreen.Store()
	total := st.Scrollback() + w.lines
	if end > total {
		end = total
	}
	if start >= end {
		return []string{}
	}

	rows := st.Range(st.Origin()+start, st.Origin()+end)
	result := make([]string, 0, len(rows))
	for i := range rows {
		result = append(result, strings.TrimRight(renderRow(rows[i].Chars, rows[i].Widths), " "))
	}
	return result
}

// Get attributes for the specific rendered lines
func (w *WideCharScreen) getAttributesForLines(lines []string) [][]Attributes {
	result := make([][]Attributes, len(lines))

	HistorySize := w.getHistorySizeLocked()

	// For each line, determine if it's from History or current screen
	for i := 0; i < len(lines); i++ {
		if w.viewportStart+i < HistorySize {
			// History line
			result[i] = w.getHistoryAttributesForLine(w.viewportStart + i)
		} else {
			// Current screen line
			screenLine := (w.viewportStart + i) - HistorySize
			if screenLine < w.lines {
				result[i] = w.extractCurrentLineAttributes(screenLine)
			} else {
				result[i] = []Attributes{}
			}
		}
	}

	return result
}

// Get attributes for a specific History line
func (w *WideCharScreen) getHistoryAttributesForLine(HistoryIndex int) []Attributes {
	if w.HistoryScreen == nil {
		return []Attributes{}
	}

	st := w.HistoryScreen.Store()
	r := st.At(st.Origin() + HistoryIndex)
	if r == nil || HistoryIndex >= st.Scrollback() {
		return []Attributes{}
	}
	return extractRowAttributes(r.Attrs, r.Widths)
}

// handleZeroWidth handles zero-width combining characters
func (w *WideCharScreen) handleZeroWidth(ch rune) {
	// Combining characters attach to the previous character
	if w.cursor.X > 0 {
		// Combine with previous character
		prevX := w.cursor.X - 1
		if w.cellWidths[w.cursor.Y][prevX] == 2 && prevX > 0 {
			// Previous is a wide character, combine with its start
			prevX--
		}

		// Append the combining character
		existing := w.buffer[w.cursor.Y][prevX]
		if existing != 0 && existing != ' ' {
			// In a real implementation, we'd normalize the combination
			// For now, we'll just store the base character
		}
	} else if w.cursor.Y > 0 {
		// Combine with last character of previous line
		prevY := w.cursor.Y - 1
		prevX := w.columns - 1

		// Find the last actual character
		for prevX >= 0 && w.cellWidths[prevY][prevX] == 0 {
			prevX--
		}

		if prevX >= 0 && w.buffer[prevY][prevX] != ' ' {
			// Would combine here in full implementation
		}
	}
}

// clearCellAt clears a cell, handling wide characters properly
func (w *WideCharScreen) clearCellAt(y, x int) {
	// Bounds check against actual array sizes, not just w.lines/w.columns
	if y < 0 || x < 0 {
		return
	}
	if y >= len(w.buffer) || y >= len(w.cellWidths) || y >= len(w.attrs) {
		return
	}
	if x >= len(w.buffer[y]) || x >= len(w.cellWidths[y]) || x >= len(w.attrs[y]) {
		return
	}

	width := w.cellWidths[y][x]

	// If this is a continuation cell, clear the start cell too
	if width == 0 && x > 0 {
		w.clearCellAt(y, x-1)
		return
	}

	// Clear this cell
	w.buffer[y][x] = ' '
	w.attrs[y][x] = Attributes{Fg: "default", Bg: "default"}
	w.cellWidths[y][x] = 1

	// If this was a wide character, clear its continuation
	if width == 2 && x+1 < len(w.buffer[y]) && x+1 < len(w.cellWidths[y]) && x+1 < len(w.attrs[y]) {
		w.buffer[y][x+1] = ' '
		w.attrs[y][x+1] = Attributes{Fg: "default", Bg: "default"}
		w.cellWidths[y][x+1] = 1
	}
}

// screenRow returns the character and width slices for live-screen row y,
// padded to w.columns. The backing buffer can lag w.columns for a beat after
// a resize, so short rows are padded rather than allowed to shorten the
// rendered row and break the column mapping.
func (w *WideCharScreen) screenRow(y int) (chars []rune, widths []int) {
	chars = make([]rune, w.columns)
	widths = make([]int, w.columns)
	for x := 0; x < w.columns; x++ {
		chars[x] = ' '
		widths[x] = 1
		if y >= 0 && y < len(w.buffer) && x < len(w.buffer[y]) {
			chars[x] = w.buffer[y][x]
		}
		if y >= 0 && y < len(w.cellWidths) && x < len(w.cellWidths[y]) {
			widths[x] = w.cellWidths[y][x]
		}
	}
	return chars, widths
}

// renderCurrentScreenContent renders the live screen as column-indexed rows.
// It shares renderRow with the history path so the two can no longer disagree
// about how a wide glyph is serialized.
func (w *WideCharScreen) renderCurrentScreenContent() []string {
	currentLines := make([]string, w.lines)
	for y := 0; y < w.lines; y++ {
		currentLines[y] = renderRow(w.screenRow(y))
	}
	return currentLines
}

// extractCurrentScreenAttributes extracts attributes respecting wide characters
func (w *WideCharScreen) extractCurrentScreenAttributes() [][]Attributes {
	currentAttrs := make([][]Attributes, w.lines)
	for y := 0; y < w.lines; y++ {
		currentAttrs[y] = w.extractCurrentLineAttributes(y)
	}
	return currentAttrs
}

// extractCurrentLineAttributes returns one attribute entry per column for the
// live-screen line y, continuation cells included, so attribute index and
// column index agree with the string returned by renderCurrentScreenContent.
func (w *WideCharScreen) extractCurrentLineAttributes(y int) []Attributes {
	result := make([]Attributes, w.columns)
	for x := 0; x < w.columns; x++ {
		result[x] = Attributes{Fg: "default", Bg: "default"}
		if y >= 0 && y < len(w.attrs) && x < len(w.attrs[y]) {
			result[x] = w.attrs[y][x]
		}
	}
	return result
}

// Alternate screen handling
func (w *WideCharScreen) EnterAlternateScreen() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enterAlternateScreenLocked()
}

func (w *WideCharScreen) enterAlternateScreenLocked() {
	if w.usingAlternate {
		return
	}
	w.switchToAlternate()
}

func (w *WideCharScreen) ExitAlternateScreen() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.exitAlternateScreenLocked()
}

func (w *WideCharScreen) exitAlternateScreenLocked() {
	if !w.usingAlternate {
		return
	}
	w.switchToMain()
}

func (w *WideCharScreen) switchToAlternate() {
	w.invalidateCache()

	// Keep the alt store's geometry current; it may have missed a resize.
	w.altStore.Resize(w.columns, w.lines)
	w.altStore.Reset()

	w.mainCursor = w.cursor
	w.mainStore = w.HistoryScreen.SwapStore(w.altStore)
	w.cursor = w.altCursor
	w.usingAlternate = true
}

func (w *WideCharScreen) switchToMain() {
	w.invalidateCache()

	w.altCursor = w.cursor
	w.altStore = w.HistoryScreen.SwapStore(w.mainStore)
	w.cursor = w.mainCursor
	w.usingAlternate = false
}

// Terminal mode handling
func (w *WideCharScreen) SetMode(modes []int, private bool) {
	for _, mode := range modes {
		if private {
			switch mode {
			case 1: // DECCKM - application cursor keys
				dlogf("WideCharScreen: application cursor keys ON")
				w.applicationCursorKeys = true
			case 1049: // Alternate screen buffer
				dlogf("WideCharScreen: Entering alternate screen mode")
				w.enterAlternateScreenLocked()
			default:
				if w.HistoryScreen != nil {
					w.HistoryScreen.SetMode(modes, private)
				}
			}
		} else {
			if w.HistoryScreen != nil {
				w.HistoryScreen.SetMode(modes, private)
			}
		}
	}
}

func (w *WideCharScreen) ResetMode(modes []int, private bool) {
	for _, mode := range modes {
		if private {
			switch mode {
			case 1: // DECCKM - normal cursor keys
				dlogf("WideCharScreen: application cursor keys OFF")
				w.applicationCursorKeys = false
			case 1049: // Exit alternate screen buffer
				dlogf("WideCharScreen: Exiting alternate screen mode")
				w.exitAlternateScreenLocked()
			default:
				if w.HistoryScreen != nil {
					w.HistoryScreen.ResetMode(modes, private)
				}
			}
		} else {
			if w.HistoryScreen != nil {
				w.HistoryScreen.ResetMode(modes, private)
			}
		}
	}
}

// Utility methods
func (w *WideCharScreen) GetCursor() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cursor.X, w.cursor.Y
}

func (w *WideCharScreen) GetBuffer() [][]rune {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer
}

func (w *WideCharScreen) GetHistorySize() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getHistorySizeLocked()
}

func (w *WideCharScreen) getHistorySizeLocked() int {
	if w.HistoryScreen != nil {
		return w.HistoryScreen.GetHistorySize()
	}
	return 0
}

func (w *WideCharScreen) IsViewingHistory() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.isViewingHistoryLocked()
}

func (w *WideCharScreen) isViewingHistoryLocked() bool {
	if w.HistoryScreen != nil {
		return w.HistoryScreen.IsViewingHistory()
	}
	return false
}

// GetViewportStart returns the absolute virtual-buffer line index of the FIRST
// line in the slice most recently returned by GetDisplay(). The virtual buffer
// is the concatenation of scrollback history (lines 0..HistorySize-1) and the
// current screen (lines HistorySize..HistorySize+rows-1). This index is stable
// across scrolling, so callers (text selection) can anchor to absolute lines
// instead of viewport-relative pixels. In alternate-screen mode there is no
// history, so the window always starts at 0.
func (w *WideCharScreen) GetViewportStart() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.usingAlternate {
		return 0
	}
	return w.viewportStart
}

// GetTotalContentLines returns the number of lines in the virtual buffer:
// scrollback history plus the current screen. Used to clamp absolute line
// indices. In alternate-screen mode only the current screen exists.
func (w *WideCharScreen) GetTotalContentLines() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.totalContentLinesLocked()
}

func (w *WideCharScreen) totalContentLinesLocked() int {
	if w.usingAlternate {
		return w.lines
	}
	return w.getHistorySizeLocked() + w.lines
}

// GetLinesInRange returns rendered lines for the absolute virtual-buffer range
// [start, end) (end exclusive), regardless of where the current GetDisplay()
// window happens to sit. This lets a selection that was dragged across several
// scroll steps be copied in full, even though only a few screens are ever held
// in the live display window. In alternate-screen mode it slices the current
// screen content directly, since there is no history to index into.
func (w *WideCharScreen) GetLinesInRange(start, end int) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getLinesInRangeLocked(start, end)
}

func (w *WideCharScreen) getLinesInRangeLocked(start, end int) []string {
	if w.usingAlternate {
		cur := w.renderCurrentScreenContent()
		if start < 0 {
			start = 0
		}
		if end > len(cur) {
			end = len(cur)
		}
		if start >= end {
			return []string{}
		}
		return cur[start:end]
	}
	return w.renderLinesInRange(start, end)
}

// GetTextInRange is the text-extraction counterpart to GetLinesInRange: the
// same rows with continuation spacers removed, so a wide glyph appears once
// rather than followed by a spacer. Use this for copy, search and logging;
// use GetLinesInRange for anything that maps rune index to grid column.
func (w *WideCharScreen) GetTextInRange(start, end int) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return StripContinuationLines(w.getLinesInRangeLocked(start, end))
}

// GetDisplayText is the text-extraction counterpart to GetDisplay. See
// GetTextInRange for when to prefer which.
func (w *WideCharScreen) GetDisplayText() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return StripContinuationLines(w.getDisplayLocked())
}

// In wide_char_screen.go, replace the ScrollUp and ScrollDown methods:

func (w *WideCharScreen) ScrollUp(lines int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// CRITICAL FIX: Complete no-op in alternate screen mode
	if w.usingAlternate {
		return // No scrolling in alternate screen
	}

	w.invalidateCache()
	if w.HistoryScreen != nil {
		w.HistoryScreen.ScrollUp(lines)
	}
}

func (w *WideCharScreen) ScrollDown(lines int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// CRITICAL FIX: Complete no-op in alternate screen mode
	if w.usingAlternate {
		return // No scrolling in alternate screen
	}

	w.invalidateCache()
	if w.HistoryScreen != nil {
		w.HistoryScreen.ScrollDown(lines)
	}
}

func (w *WideCharScreen) ScrollToBottom() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.scrollToBottomLocked()
}

func (w *WideCharScreen) scrollToBottomLocked() {
	// CRITICAL FIX: Complete no-op in alternate screen mode
	if w.usingAlternate {
		return // No scrolling in alternate screen
	}

	w.invalidateCache()
	if w.HistoryScreen != nil {
		w.HistoryScreen.ScrollToBottom()
	}
}

// ScrollToTop overrides the promoted HistoryScreen method purely to take the
// model lock. Without this override the CLI's scroll handler would mutate the
// view position with no synchronization at all.
func (w *WideCharScreen) ScrollToTop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.usingAlternate {
		return
	}
	w.invalidateCache()
	if w.HistoryScreen != nil {
		w.HistoryScreen.ScrollToTop()
	}
}

// Cache management
// cacheFresh reports whether the cached render still matches the store.
func (w *WideCharScreen) cacheFresh() bool {
	st := w.HistoryScreen.Store()
	return w.cacheStore == st && w.cacheGen == st.Gen()
}

// markCacheFresh records the store and generation the cache was built from.
func (w *WideCharScreen) markCacheFresh() {
	st := w.HistoryScreen.Store()
	w.cacheStore = st
	w.cacheGen = st.Gen()
}

func (w *WideCharScreen) invalidateCache() {
	w.cacheStore = nil
	w.displayCache = nil
	w.attributeCache = nil
}

// InvalidateCache is retained so external callers still compile, but it is
// no longer required: the generation check catches staleness on its own.
//
// Deprecated: mutations invalidate the cache automatically.
func (w *WideCharScreen) InvalidateCache() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cacheStore = nil
	w.displayCache = nil
	w.attributeCache = nil
}
func (w *WideCharScreen) EnableVirtualScrolling() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.virtualScrolling = true
	w.invalidateCache()
}

func (w *WideCharScreen) DisableVirtualScrolling() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.virtualScrolling = false
	w.invalidateCache()
}

// History management
func (w *WideCharScreen) SetMaxHistoryLines(maxLines int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.HistoryScreen != nil {
		// Trimming is the store's job and happens immediately.
		w.HistoryScreen.SetMaxHistory(maxLines)
	}
}

// Resize handling
func (w *WideCharScreen) Resize(newCols, newLines int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if newCols <= 0 || newLines <= 0 {
		return
	}

	dlogf("WideCharScreen.Resize: %dx%d -> %dx%d", w.columns, w.lines, newCols, newLines)

	w.invalidateCache()

	// If viewing History, return to live view first
	if !w.usingAlternate && w.isViewingHistoryLocked() {
		w.scrollToBottomLocked()
	}

	// Let HistoryScreen resize first (this should resize buffer and attrs)
	w.HistoryScreen.Resize(newCols, newLines)

	// Update geometry
	w.columns = newCols
	w.lines = newLines

	// No grids to rebuild or re-point: the store resized its rows and
	// HistoryScreen.Resize already re-synced the visible window.
	if w.usingAlternate {
		w.mainStore.Resize(newCols, newLines)
	} else {
		w.altStore.Resize(newCols, newLines)
	}

	dlogf("WideCharScreen.Resize complete: buffer=%d rows", len(w.buffer))
}

// ensureBufferSizes makes sure all buffers match the expected dimensions

// ADD these delegate methods to your wide_char_screen.go file
// Put them with the other delegate methods around line 400+

// GetHistoryPos delegates to HistoryScreen
func (w *WideCharScreen) GetHistoryPos() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.HistoryScreen != nil {
		return w.HistoryScreen.GetHistoryPos()
	}
	return 0
}

// GetMaxHistoryPos delegates to HistoryScreen
func (w *WideCharScreen) GetMaxHistoryPos() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.HistoryScreen != nil {
		return w.HistoryScreen.GetMaxHistoryPos()
	}
	return 0
}

// IsAtTopOfHistory delegates to HistoryScreen
func (w *WideCharScreen) IsAtTopOfHistory() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.HistoryScreen != nil {
		return w.HistoryScreen.IsAtTopOfHistory()
	}
	return false
}

// IsAtBottomOfHistory delegates to HistoryScreen
func (w *WideCharScreen) IsAtBottomOfHistory() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.HistoryScreen != nil {
		return w.HistoryScreen.IsAtBottomOfHistory()
	}
	return true
}

// ADD this method to the end of your wide_char_screen.go file:

// DebugViewportState provides detailed information about current viewport state
func (w *WideCharScreen) DebugViewportState() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.HistoryScreen == nil {
		dprintf("DebugViewportState: HistoryScreen is nil\n")
		return
	}

	historySize := w.getHistorySizeLocked()
	currentPos := w.HistoryScreen.GetHistoryPos()
	totalContent := historySize + w.lines

	dprintf("=== VIEWPORT DEBUG ===\n")
	dprintf("History: %d lines, position: %d/%d\n", historySize, currentPos, historySize)
	dprintf("Current screen: %d lines\n", w.lines)
	dprintf("Total content: %d lines\n", totalContent)
	dprintf("Viewport: [%d-%d] of %d\n", w.viewportStart, w.viewportEnd-1, totalContent)
	dprintf("In history mode: %v\n", w.isViewingHistoryLocked())

	// Show what content the viewport should be showing
	if currentPos > 0 {
		progress := float64(currentPos) / float64(historySize)
		dprintf("Scroll progress: %.1f%% toward top\n", progress*100)

		if currentPos >= historySize {
			dprintf("Should show: OLDEST history content\n")
		} else {
			dprintf("Should show: Mixed content, %d lines into history\n", currentPos)
		}
	} else {
		dprintf("Should show: RECENT content (bottom)\n")
	}
	dprintf("=====================\n")
}

// DebugScanCells reports cells in the active buffer that are NOT plain width-1
// runes: wide chars (stored width 2), their continuation cells (width 0 / null),
// and Unicode replacement chars (U+FFFD). These are exactly the cells that cause
// width-misalignment corruption when runewidth classifies a glyph differently
// than the remote app assumed. Plain text and width-1 glyphs (e.g. braille) are
// skipped, so the output stays small enough to log per render under a debug gate.
func (w *WideCharScreen) DebugScanCells() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for y := 0; y < len(w.buffer); y++ {
		row := w.buffer[y]
		for x := 0; x < len(row); x++ {
			ch := row[x]
			width := 1
			if w.cellWidths != nil && y < len(w.cellWidths) && x < len(w.cellWidths[y]) {
				width = w.cellWidths[y][x]
			}
			if width == 1 && ch != 0xFFFD {
				continue
			}
			out = append(out, fmt.Sprintf("(%d,%d) U+%04X stored_w=%d runewidth=%d",
				y, x, ch, width, runewidth.RuneWidth(ch)))
		}
	}
	return out
}
