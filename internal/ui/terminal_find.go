// internal/ui/terminal_find.go
// cli/terminal_find.go - Find in scrollback (Ctrl+Shift+F).
//
// Search runs over the virtual buffer (history + live screen) in ABSOLUTE line
// coordinates - the same coordinate space the selection manager anchors to - so
// a hit can be highlighted with SetSelection and stays pinned to its text while
// the view scrolls. That reuse is the whole design: matches are drawn by the
// existing background overlay, no second highlight mechanism.
//
// The bar lives in the free `top` slot of the terminal's Border and is hidden
// until opened, so it costs nothing when unused. It is deliberately NOT a
// dialog: a modal would dim the pane you are trying to read.
//
// Alternate-screen mode (vim, htop, btop) has no scrollback to search - those
// apps own the screen - so the bar refuses to open there and says why.
package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/scottpeterman/pathfinderssh/internal/gopyte"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// findMatch is one hit, in absolute virtual-buffer coordinates. col is a rune
// index (not a byte offset) because the selection and the grid are both
// rune-indexed; a match inside a line with multibyte glyphs would otherwise
// highlight the wrong span.
type findMatch struct {
	line int
	col  int
	n    int // match length in runes
}

// findEntry is the search box. It exists purely to give the bar an Escape key:
// widget.Entry has no OnEscape hook, and Escape must NOT be handled higher up -
// the terminal has to keep forwarding it to the remote shell (vi, and every
// network CLI that uses it to abort a prompt).
type findEntry struct {
	widget.Entry
	onEscape func()
}

func newFindEntry(onEscape func()) *findEntry {
	e := &findEntry{onEscape: onEscape}
	e.ExtendBaseWidget(e)
	return e
}

// TypedKey consumes Escape and delegates everything else to the normal entry.
func (e *findEntry) TypedKey(ev *fyne.KeyEvent) {
	if ev != nil && ev.Name == fyne.KeyEscape {
		if e.onEscape != nil {
			e.onEscape()
		}
		return
	}
	e.Entry.TypedKey(ev)
}

// findController owns the search bar and the current result set.
type findController struct {
	term *NativeTerminalWidget

	bar        *fyne.Container
	entry      *findEntry
	status     *widget.Label
	matchCase  *widget.Check
	matches    []findMatch
	current    int    // index into matches; -1 when there are none
	lastQuery  string // query the current match set was built from
	lastCasing bool
}

// newFindController builds the (hidden) find bar for a terminal.
func newFindController(t *NativeTerminalWidget) *findController {
	f := &findController{term: t, current: -1}

	f.entry = newFindEntry(f.Close)
	f.entry.SetPlaceHolder("Find in scrollback")
	f.entry.OnChanged = func(string) { f.research() }
	f.entry.OnSubmitted = func(string) { f.next() }

	f.status = widget.NewLabel("")
	f.status.Importance = widget.LowImportance

	f.matchCase = widget.NewCheck("Aa", func(bool) { f.research() })

	// The step buttons hand focus straight back to the entry: a Fyne Button is
	// itself focusable, so without this a click on prev/next parks focus on the
	// button, where Escape no longer closes the bar and typing no longer
	// refines the query.
	prev := widget.NewButtonWithIcon("", theme.MediaSkipPreviousIcon(), func() {
		f.prev()
		f.focusEntry()
	})
	prev.Importance = widget.LowImportance
	nextBtn := widget.NewButtonWithIcon("", theme.MediaSkipNextIcon(), func() {
		f.next()
		f.focusEntry()
	})
	nextBtn.Importance = widget.LowImportance
	closeBtn := widget.NewButtonWithIcon("", theme.CancelIcon(), f.Close)
	closeBtn.Importance = widget.LowImportance

	right := container.NewHBox(f.status, f.matchCase, prev, nextBtn, closeBtn)
	f.bar = container.NewBorder(nil, nil, nil, right, f.entry)
	f.bar.Hide()
	return f
}

// Open shows the bar and focuses the entry. Re-opening with text already in the
// box re-runs the search, so Ctrl+Shift+F twice is "search again".
func (f *findController) Open() {
	if f.term.screen != nil && f.term.screen.IsUsingAlternate() {
		f.term.showTransientNotice("Find is unavailable in full-screen apps (no scrollback)")
		return
	}
	f.bar.Show()
	f.bar.Refresh()
	if c := f.term.FocusCanvas(); c != nil {
		c.Focus(f.entry)
	}
	if strings.TrimSpace(f.entry.Text) != "" {
		f.lastQuery = "" // force a rebuild against current buffer contents
		f.research()
	}
}

// Close hides the bar, drops the highlight, and hands keyboard focus back to
// the terminal.
//
// Focus goes through NativeTerminalWidget.GrabFocus, which targets the object
// that is actually in the canvas tree. Focusing the inner terminal widget
// directly fails silently (Fyne matches focus targets by object identity), which
// is what left the keyboard stuck in the search box.
//
// It is done BEFORE the bar is hidden, and re-asserted a beat later:
//
//   - FocusManager.Focus() walks for a hidden ancestor and, when it finds one,
//     returns true WITHOUT focusing anything. Once bar.Hide() has run, a focus
//     call issued in that same tick can land in that no-op path.
//   - A click on the close button is still mid-dispatch here, and
//     processMouseClicked can call canvas.Unfocus() around the tap, undoing a
//     focus set during Tapped.
func (f *findController) Close() {
	f.term.GrabFocus()

	f.bar.Hide()
	f.bar.Refresh()
	f.matches = nil
	f.current = -1
	f.lastQuery = ""
	if f.term.selection != nil {
		f.term.selection.Clear()
	}
	f.term.updatePending.Store(true)

	f.reassertTerminalFocus()
}

// reassertTerminalFocus re-focuses the terminal shortly after Close, but only if
// focus was actually lost (nil) or is stranded on the now-hidden search box. The
// guard is what keeps this from stealing focus back from wherever the user
// legitimately clicked next.
func (f *findController) reassertTerminalFocus() {
	go func() {
		// Long enough for the in-flight tap/key dispatch to finish, short enough
		// that a fast typist never notices.
		time.Sleep(50 * time.Millisecond)
		fyne.Do(func() {
			c := f.term.FocusCanvas()
			if c == nil {
				return
			}
			cur := c.Focused()
			if cur == nil {
				f.term.GrabFocus()
				return
			}
			if e, ok := cur.(*findEntry); ok && e == f.entry {
				f.term.GrabFocus()
			}
		})
	}()
}

// focusEntry puts the caret back in the search box.
func (f *findController) focusEntry() {
	if c := f.term.FocusCanvas(); c != nil {
		c.Focus(f.entry)
	}
}

// IsOpen reports whether the bar is visible.
func (f *findController) IsOpen() bool { return f.bar != nil && f.bar.Visible() }

// research rebuilds the match set for the current query and jumps to the first
// hit at or after the current viewport top, so opening find mid-scroll starts
// from what you are looking at rather than from the top of history.
func (f *findController) research() {
	query := f.entry.Text
	if strings.TrimSpace(query) == "" {
		f.matches = nil
		f.current = -1
		f.lastQuery = ""
		f.status.SetText("")
		if f.term.selection != nil {
			f.term.selection.Clear()
		}
		f.term.updatePending.Store(true)
		return
	}
	if query == f.lastQuery && f.matchCase.Checked == f.lastCasing {
		return
	}

	f.matches = f.term.findInScrollback(query, f.matchCase.Checked)
	f.lastQuery = query
	f.lastCasing = f.matchCase.Checked

	if len(f.matches) == 0 {
		f.current = -1
		f.status.SetText("0/0")
		if f.term.selection != nil {
			f.term.selection.Clear()
		}
		f.term.updatePending.Store(true)
		return
	}

	// Start at the first hit at or below the current viewport top.
	top := f.term.viewportTopAbs()
	f.current = 0
	for i, m := range f.matches {
		if m.line >= top {
			f.current = i
			break
		}
	}
	f.show()
}

func (f *findController) next() {
	if len(f.matches) == 0 {
		f.research()
		return
	}
	f.current = (f.current + 1) % len(f.matches)
	f.show()
}

func (f *findController) prev() {
	if len(f.matches) == 0 {
		f.research()
		return
	}
	f.current--
	if f.current < 0 {
		f.current = len(f.matches) - 1
	}
	f.show()
}

// show scrolls the current match into view and highlights it via the normal
// selection path, so the hit is also ready to copy.
func (f *findController) show() {
	if f.current < 0 || f.current >= len(f.matches) {
		return
	}
	m := f.matches[f.current]
	f.status.SetText(fmt.Sprintf("%d/%d", f.current+1, len(f.matches)))
	f.term.ScrollToAbsLine(m.line)
	if f.term.selection != nil {
		f.term.selection.SetSelection(m.line, m.col, m.line, m.col+m.n)
	}
	f.term.updatePending.Store(true)
}

// ShowFind opens the find bar on this terminal. It is the entry point for
// callers outside the widget (the Edit menu); the keyboard path goes through
// TypedShortcut. No-op before the widget has been rendered, since the bar is
// built in CreateRenderer and has nowhere to live until then - which cannot
// happen for the active tab.
func (t *NativeTerminalWidget) ShowFind() {
	if t.find == nil {
		return
	}
	t.find.Open()
}

// --- search over the virtual buffer ------------------------------------------

// findInScrollback returns every match of query across history plus the live
// screen, ordered oldest to newest. Matching is plain substring (not regex):
// the common search here is an interface name, an IP, or an error string, and a
// literal match means a pasted token never needs escaping.
//
// Columns are rune indices so they line up with the grid and the selection.
func (t *NativeTerminalWidget) findInScrollback(query string, matchCase bool) []findMatch {
	if t.screen == nil || query == "" {
		return nil
	}
	total := t.screen.GetTotalContentLines()
	if total <= 0 {
		return nil
	}
	lines := t.screen.GetLinesInRange(0, total)
	if len(lines) == 0 {
		return nil
	}

	needle := query
	if !matchCase {
		needle = strings.ToLower(needle)
	}
	needleRunes := len([]rune(query))

	var out []findMatch
	for i, line := range lines {
		// Search the collapsed text but report grid columns. A display row
		// carries a continuation spacer after every double-width glyph; a
		// user's query never contains one, so searching the row directly
		// would fail on any run that spans a wide glyph.
		hay, colOf := stripWithColumns(line)
		if !matchCase {
			hay = strings.ToLower(hay)
		}
		from := 0
		for {
			idx := strings.Index(hay[from:], needle)
			if idx < 0 {
				break
			}
			byteAt := from + idx
			at := len([]rune(hay[:byteAt])) // byte offset -> rune index
			if at >= len(colOf) {
				break
			}
			startCol := colOf[at]
			// The highlight is drawn in column space, so its width is the
			// span of columns the match occupies -- wider than its rune
			// count wherever it covers a double-width glyph.
			endCol := len([]rune(line))
			if at+needleRunes < len(colOf) {
				endCol = colOf[at+needleRunes]
			}
			out = append(out, findMatch{
				line: i,
				col:  startCol,
				n:    endCol - startCol,
			})
			from = byteAt + len(needle)
			if from >= len(hay) {
				break
			}
		}
	}
	return out
}

// stripWithColumns removes continuation spacers from a display row and
// returns the collapsed text together with a map from rune index in that
// text back to the grid column it came from. Find needs both halves: it must
// match against collapsed text, and it must report columns, because the
// highlight and the selection both work in column space.
func stripWithColumns(line string) (string, []int) {
	runes := []rune(line)
	out := make([]rune, 0, len(runes))
	colOf := make([]int, 0, len(runes))
	for i, r := range runes {
		if r == gopyte.ContinuationRune {
			continue
		}
		out = append(out, r)
		colOf = append(colOf, i)
	}
	return string(out), colOf
}

// --- scrolling ---------------------------------------------------------------

// viewportTopAbs is the absolute virtual-buffer line currently at the top of the
// visible window.
func (t *NativeTerminalWidget) viewportTopAbs() int {
	if t.screen == nil {
		return 0
	}
	vp := t.calculateUnifiedViewport(t.screen.GetDisplay())
	return t.screen.GetViewportStart() + vp.scrollOffset
}

// ScrollToAbsLine scrolls the virtual scrollback so the given absolute line is
// visible, seated about a third of the way down the window rather than flush
// against an edge, so the surrounding context comes with it.
//
// Like ScrollToFraction this converts an absolute target into the same
// ScrollUp/ScrollDown deltas the wheel uses; for very deep scrollback the
// line->view mapping is slightly nonlinear, so a jump can settle a line or two
// off over a frame. The highlight is anchored to the text, not the viewport, so
// it stays correct either way.
func (t *NativeTerminalWidget) ScrollToAbsLine(absLine int) {
	if t.screen == nil || t.screen.IsUsingAlternate() {
		return
	}
	vp := t.calculateUnifiedViewport(t.screen.GetDisplay())
	total := t.screen.GetTotalContentLines()

	maxTop := total - vp.visibleLines
	if maxTop < 0 {
		maxTop = 0
	}
	targetTop := absLine - vp.visibleLines/3
	if targetTop < 0 {
		targetTop = 0
	}
	if targetTop > maxTop {
		targetTop = maxTop
	}

	delta := targetTop - (t.screen.GetViewportStart() + vp.scrollOffset)
	switch {
	case delta > 0:
		t.screen.ScrollDown(delta)
	case delta < 0:
		t.screen.ScrollUp(-delta)
	}
	t.updatePending.Store(true)
}

// showTransientNotice puts a short message in the find bar's status area,
// showing the bar just long enough to carry it. Used for the one case where
// find is refused (alternate screen) so the keystroke isn't silently swallowed.
func (t *NativeTerminalWidget) showTransientNotice(msg string) {
	if t.find == nil {
		return
	}
	t.find.status.SetText(msg)
	t.find.bar.Show()
	t.find.bar.Refresh()
}
