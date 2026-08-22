package sessiondial

import (
	"errors"
	"io"
	"testing"
)

func TestHumanizeEOF(t *testing.T) {
	err := Humanize(io.EOF)
	if err == nil {
		t.Fatal("nil")
	}
	msg := err.Error()
	if !errors.Is(err, io.EOF) {
		t.Fatal("should unwrap to EOF")
	}
	if msg == "EOF" || msg == "eof" {
		t.Fatalf("still bare EOF: %q", msg)
	}
	if !containsFold(msg, "offline") && !containsFold(msg, "closed") {
		t.Fatalf("expected operator hint, got %q", msg)
	}
}

func TestHumanizeHandshakeEOF(t *testing.T) {
	err := Humanize(errors.New("SSH handshake with 10.0.0.1:22: EOF"))
	msg := err.Error()
	if containsFold(msg, "usually offline") || containsFold(msg, "closed the connection") {
		return
	}
	t.Fatalf("expected handshake hint, got %q", msg)
}

func TestHumanizePassthrough(t *testing.T) {
	in := errors.New("permission denied")
	out := Humanize(in)
	if out != in {
		t.Fatalf("got %v", out)
	}
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && containsFoldSlow(s, sub)))
}

func containsFoldSlow(s, sub string) bool {
	ls, lsub := []rune(s), []rune(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		ok := true
		for j := range lsub {
			a, b := ls[i+j], lsub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
