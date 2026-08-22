package sessiondial

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
)

// Humanize rewrites low-level dial/handshake failures into text an operator
// can act on. Raw "EOF" is especially useless: it almost always means the
// peer closed during the SSH/telnet handshake (offline, wrong port, or not
// the expected service).
func Humanize(err error) error {
	if err == nil {
		return nil
	}
	var already *explainedError
	if errors.As(err, &already) {
		return err
	}

	msg := err.Error()
	lower := strings.ToLower(msg)

	switch {
	case errors.Is(err, io.EOF) || isBareEOF(msg):
		return explain(err, "the host closed the connection before login finished — usually offline, wrong port, firewall reset, or something other than SSH/telnet answered")

	case errors.Is(err, context.Canceled):
		return explain(err, "connection cancelled")

	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		strings.Contains(lower, "i/o timeout") || strings.Contains(lower, "timed out") ||
		strings.Contains(lower, "deadline exceeded"):
		return explain(err, "timed out reaching the host — offline, filtered, or too slow to answer")

	case isConnRefused(err, lower):
		return explain(err, "connection refused — nothing is listening on that port (device offline or wrong port)")

	case strings.Contains(lower, "no route to host") || strings.Contains(lower, "network is unreachable"):
		return explain(err, "network unreachable — check routing, VPN, or that the host address is correct")

	case strings.Contains(lower, "no such host") || strings.Contains(lower, "server misbehaving"):
		return explain(err, "hostname could not be resolved — check DNS or use an IP address")

	case strings.Contains(lower, "connection reset"):
		return explain(err, "connection reset by peer — device rebooting, ACL drop, or service crashed mid-handshake")
	}

	return err
}

type explainedError struct {
	cause error
	hint  string
}

func (e *explainedError) Error() string {
	if e.cause == nil {
		return e.hint
	}
	// Lead with the hint so dialog boxes are readable; keep the cause for logs.
	return e.hint + "\n\n(" + e.cause.Error() + ")"
}

func (e *explainedError) Unwrap() error { return e.cause }

func explain(cause error, hint string) error {
	return &explainedError{cause: cause, hint: hint}
}

func isBareEOF(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "EOF" || strings.HasSuffix(msg, ": EOF") || strings.HasSuffix(msg, ": eof") {
		return true
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "handshake") && strings.Contains(lower, "eof")
}

func isConnRefused(err error, lower string) bool {
	var ne *net.OpError
	if errors.As(err, &ne) && ne.Err != nil {
		lower = strings.ToLower(ne.Err.Error())
	}
	return strings.Contains(lower, "connection refused") || strings.Contains(lower, "actively refused")
}