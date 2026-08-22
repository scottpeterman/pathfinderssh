package gopyte

// rowstore.go
//
// Single grow-only row list backing both scrollback and the live screen.
//
// The old model kept two containers: a fixed [][]rune live buffer and a
// container/list scrollback, with lines physically copied from one to the
// other on every linefeed (addToHistory + scrollUpInternal). Every consumer
// then had to re-fuse them at read time, and "viewing history" was a full
// screen state swap (saveCurrentScreen / restoreCurrentScreen).
//
// Here there is one list. The live screen is a window into it, defined by
// base. A linefeed is base++ plus one appended row: no copy, no seam, no
// swap. Viewing scrollback moves a read offset (viewTop); it never touches
// screen state, so there is nothing to save or restore and nothing to get
// out of sync.
//
// Widths live on the Row and are never aliased into a second grid.
//
// Every mutation bumps gen. Display caches compare gen instead of relying on
// scattered manual invalidation calls.

// Row is one line of terminal content. Widths is parallel to Chars:
// 0 = continuation cell of a wide char, 1 = normal, 2 = wide.
type Row struct {
	Chars  []rune
	Attrs  []Attributes
	Widths []int
}

// NewRow allocates a blank row of the given width.
func NewRow(cols int) Row {
	r := Row{
		Chars:  make([]rune, cols),
		Attrs:  make([]Attributes, cols),
		Widths: make([]int, cols),
	}
	for i := 0; i < cols; i++ {
		r.Chars[i] = ' '
		r.Attrs[i] = Attributes{Fg: "default", Bg: "default"}
		r.Widths[i] = 1
	}
	return r
}

// Clear resets the row in place, preserving capacity.
func (r *Row) Clear() {
	for i := range r.Chars {
		r.Chars[i] = ' '
		r.Attrs[i] = Attributes{Fg: "default", Bg: "default"}
		r.Widths[i] = 1
	}
}

// Resize grows or shrinks the row to cols, preserving existing content.
func (r *Row) Resize(cols int) {
	old := len(r.Chars)
	if cols == old {
		return
	}
	if cols < old {
		r.Chars = r.Chars[:cols]
		r.Attrs = r.Attrs[:cols]
		r.Widths = r.Widths[:cols]
		return
	}
	for i := old; i < cols; i++ {
		r.Chars = append(r.Chars, ' ')
		r.Attrs = append(r.Attrs, Attributes{Fg: "default", Bg: "default"})
		r.Widths = append(r.Widths, 1)
	}
}

// RowStore is a grow-only list of rows with a trimmed head.
//
// Absolute indices are stable for the lifetime of a row: trimming the head
// advances origin rather than renumbering, so a caller holding an absolute
// index can always ask whether it is still resident via Resident.
type RowStore struct {
	rows   []Row
	origin int // absolute index of rows[0]
	base   int // absolute index of live screen line 0
	cols   int
	lines  int
	max    int    // max scrollback rows retained above base; 0 = none (alt screen)
	gen    uint64 // bumped on every mutation
}

// NewRowStore creates a store with one screenful of blank rows.
func NewRowStore(cols, lines, maxScrollback int) *RowStore {
	s := &RowStore{
		rows:  make([]Row, 0, lines+maxScrollback/4),
		cols:  cols,
		lines: lines,
		max:   maxScrollback,
	}
	for i := 0; i < lines; i++ {
		s.rows = append(s.rows, NewRow(cols))
	}
	return s
}

// Gen returns the mutation counter. A display cache that stores the gen it
// was built from can detect staleness with a single comparison, which is
// what replaces the manual InvalidateCache calls in the CLI layer.
func (s *RowStore) Gen() uint64 { return s.gen }

// Touch marks the store mutated. Callers that write through a *Row obtained
// from Line must call this.
func (s *RowStore) Touch() { s.gen++ }

// Cols and Lines report the current geometry.
func (s *RowStore) Cols() int  { return s.cols }
func (s *RowStore) Lines() int { return s.lines }

// Base is the absolute index of screen line 0.
func (s *RowStore) Base() int { return s.base }

// Origin is the absolute index of the oldest resident row.
func (s *RowStore) Origin() int { return s.origin }

// Total is the number of resident rows (scrollback plus live screen).
func (s *RowStore) Total() int { return len(s.rows) }

// Scrollback is the number of rows above the live screen.
func (s *RowStore) Scrollback() int { return s.base - s.origin }

// Resident reports whether an absolute index is still held.
func (s *RowStore) Resident(abs int) bool {
	return abs >= s.origin && abs < s.origin+len(s.rows)
}

// At returns the row at an absolute index, or nil if it has been trimmed.
func (s *RowStore) At(abs int) *Row {
	if !s.Resident(abs) {
		return nil
	}
	return &s.rows[abs-s.origin]
}

// Line returns live screen line y (0-based), or nil if out of range.
func (s *RowStore) Line(y int) *Row {
	if y < 0 || y >= s.lines {
		return nil
	}
	return s.At(s.base + y)
}

// Advance scrolls the live window down by one line: the top screen line
// becomes the newest scrollback row and a fresh blank row is appended at the
// bottom. This is the whole of what addToHistory + scrollUpInternal used to
// do, minus the copying and minus the seam.
func (s *RowStore) Advance() {
	s.base++
	s.rows = append(s.rows, NewRow(s.cols))
	s.trim()
	s.gen++
}

// trim drops rows that have fallen out of the scrollback budget.
//
// This MUST stay O(1) on the steady-state path. Once scrollback is full, every
// linefeed calls Advance → trim. The previous implementation copied the entire
// live+history slice on every call (O(scrollback)), so a "show running-config"
// of tens of thousands of lines past the cap locked the UI for seconds under
// the screen mutex. Reslicing drops the head in constant time; we only allocate
// a compacting copy when the backing array has grown wastefully large.
func (s *RowStore) trim() {
	keep := s.base - s.max
	if keep <= s.origin {
		return
	}
	drop := keep - s.origin
	if drop >= len(s.rows) {
		drop = len(s.rows) - 1
	}
	if drop <= 0 {
		return
	}
	s.rows = s.rows[drop:]
	s.origin += drop

	// Compact only when the backing array is badly bloated. The previous
	// threshold (3× live) fired during ordinary show-run / Enter bursts and
	// copied the whole scrollback under the screen lock — felt like Return
	// hanging every few dozen lines. 8× / 2×max keeps memory bounded without
	// hitching on the hot Advance path.
	live := len(s.rows)
	waste := cap(s.rows) - live
	if live > 0 && cap(s.rows) > live*8 && waste > s.max*2+s.lines {
		fresh := make([]Row, live)
		copy(fresh, s.rows)
		s.rows = fresh
	}
}

// ScrollRegion scrolls lines [top,bottom] of the live screen up by one,
// without pushing anything to scrollback. Used when a DECSTBM region is set
// and the region does not include the top of the screen.
//
// When top is 0 the caller should use Advance instead, so that the displaced
// line is retained.
func (s *RowStore) ScrollRegion(top, bottom int) {
	// top == bottom is a legitimate one-row region: the loop below does not
	// run, the row is cleared, and that is the correct result. Rejecting it
	// (the old top >= bottom guard) silently turned a one-row scroll into a
	// no-op, which is how vim's status line survived a scroll it should have
	// been excluded from.
	if top < 0 || bottom >= s.lines || top > bottom {
		return
	}
	saved := s.Line(top)
	if saved == nil {
		return
	}
	spare := *saved
	for y := top; y < bottom; y++ {
		src := s.Line(y + 1)
		dst := s.Line(y)
		if src == nil || dst == nil {
			return
		}
		*dst = *src
	}
	spare.Clear()
	if dst := s.Line(bottom); dst != nil {
		*dst = spare
	}
	s.gen++
}

// ScrollRegionDown scrolls lines [top,bottom] down by one (reverse index).
func (s *RowStore) ScrollRegionDown(top, bottom int) {
	// See ScrollRegion: a one-row region clears, it does not no-op.
	if top < 0 || bottom >= s.lines || top > bottom {
		return
	}
	saved := s.Line(bottom)
	if saved == nil {
		return
	}
	spare := *saved
	for y := bottom; y > top; y-- {
		src := s.Line(y - 1)
		dst := s.Line(y)
		if src == nil || dst == nil {
			return
		}
		*dst = *src
	}
	spare.Clear()
	if dst := s.Line(top); dst != nil {
		*dst = spare
	}
	s.gen++
}

// Range returns rows for absolute indices [from,to). Trimmed indices are
// skipped. The returned slice aliases the store; callers must not retain it
// across mutations.
func (s *RowStore) Range(from, to int) []Row {
	if from < s.origin {
		from = s.origin
	}
	end := s.origin + len(s.rows)
	if to > end {
		to = end
	}
	if from >= to {
		return nil
	}
	return s.rows[from-s.origin : to-s.origin]
}

// Resize changes geometry. Rows are reflowed to the new width; the live
// window keeps its bottom anchored so the visible content does not jump.
func (s *RowStore) Resize(cols, lines int) {
	if cols != s.cols {
		for i := range s.rows {
			s.rows[i].Resize(cols)
		}
		s.cols = cols
	}
	if lines != s.lines {
		bottom := s.base + s.lines // absolute index one past the last live line
		s.lines = lines
		s.base = bottom - lines
		if s.base < s.origin {
			s.base = s.origin
		}
		// Ensure the window is fully backed.
		need := s.base + s.lines - (s.origin + len(s.rows))
		for i := 0; i < need; i++ {
			s.rows = append(s.rows, NewRow(s.cols))
		}
	}
	s.gen++
}

// Reset clears everything and starts a fresh screen.
func (s *RowStore) Reset() {
	s.rows = s.rows[:0]
	for i := 0; i < s.lines; i++ {
		s.rows = append(s.rows, NewRow(s.cols))
	}
	s.origin = 0
	s.base = 0
	s.gen++
}

// SetMaxScrollback changes the retention budget and trims immediately.
func (s *RowStore) SetMaxScrollback(max int) {
	if max < 0 {
		max = 0
	}
	s.max = max
	s.trim()
	s.gen++
}
