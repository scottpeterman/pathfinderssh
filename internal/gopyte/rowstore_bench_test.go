package gopyte

import "testing"

// BenchmarkAdvanceAtCap measures scroll cost once scrollback is full.
// The old trim() copied the entire row slice on every Advance, so a
// "show running-config" (thousands of lines past the cap) locked the UI.
func BenchmarkAdvanceAtCap(b *testing.B) {
	const cols, rows, maxHist = 80, 24, 1000
	s := NewRowStore(cols, rows, maxHist)
	for i := 0; i < maxHist+rows; i++ {
		s.Advance()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Advance()
	}
}
