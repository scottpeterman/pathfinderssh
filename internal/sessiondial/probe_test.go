package sessiondial_test

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/scottpeterman/pathfinderssh/internal/sessiondial"
	"github.com/scottpeterman/pathfinderssh/internal/sessions"
)

func TestProbeTCPOpenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessiondial.ProbeTCP("127.0.0.1", port, 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestProbeTCPClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessiondial.ProbeTCP("127.0.0.1", port, 500*time.Millisecond); err == nil {
		t.Fatal("expected unreachable error")
	}
}

func TestProbeNodeSkipsSerial(t *testing.T) {
	n := sessions.Node{Transport: sessions.TransportSerial, SerialPort: "COM1"}
	if err := sessiondial.ProbeNode(n, nil, time.Second); err != nil {
		t.Fatal(err)
	}
}
