package gopyte

import "testing"

func TestTrimKeepsLiveScreen(t *testing.T) {
	const cols, rows, maxHist = 80, 24, 100
	s := NewRowStore(cols, rows, maxHist)
	for i := 0; i < maxHist+rows+5000; i++ {
		s.Advance()
		if s.Total() < rows {
			t.Fatalf("after %d advances: total=%d < rows=%d origin=%d base=%d scrollback=%d",
				i+1, s.Total(), rows, s.Origin(), s.Base(), s.Scrollback())
		}
		if s.Scrollback() > maxHist {
			t.Fatalf("scrollback %d > max %d", s.Scrollback(), maxHist)
		}
		for y := 0; y < rows; y++ {
			if s.Line(y) == nil {
				t.Fatalf("live line %d nil after %d advances (total=%d origin=%d base=%d)",
					y, i+1, s.Total(), s.Origin(), s.Base())
			}
		}
	}
}
