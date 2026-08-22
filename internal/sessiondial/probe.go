package sessiondial

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/scottpeterman/pathfinderssh/internal/sessions"
)

// DefaultReachTimeout is how long Probe waits for a TCP accept before giving up.
// Kept short so an unreachable host fails before the session UI feels frozen.
const DefaultReachTimeout = 5 * time.Second

// ProbeTCP checks that host:port accepts a TCP connection within timeout.
// It does not speak SSH/telnet — only that the IP/port is reachable.
func ProbeTCP(host string, port int, timeout time.Duration) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("no host to reach")
	}
	if port <= 0 {
		port = 22
	}
	if timeout <= 0 {
		timeout = DefaultReachTimeout
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return Humanize(fmt.Errorf("%s is not reachable: %w", addr, err))
	}
	_ = conn.Close()
	return nil
}

// ProbeNode checks the first TCP hop for an SSH or telnet session (the jump
// host when configured, otherwise the target). Serial sessions are skipped.
func ProbeNode(n sessions.Node, resolve func(string) string, timeout time.Duration) error {
	n = n.Normalize()
	switch n.Transport {
	case sessions.TransportSerial:
		return nil
	case sessions.TransportSSH, sessions.TransportTelnet:
		// ok
	default:
		return nil
	}
	if resolve == nil {
		resolve = func(h string) string { return h }
	}
	if timeout <= 0 {
		timeout = DefaultReachTimeout
		if sec := n.ConnectTimeoutSec; sec > 0 {
			if d := time.Duration(sec) * time.Second; d < timeout {
				timeout = d
			}
		}
	}
	if n.Transport == sessions.TransportSSH && n.Jump.InUse() {
		port := n.Jump.Port
		if port == 0 {
			port = 22
		}
		return ProbeTCP(resolve(n.Jump.Host), port, timeout)
	}
	port := n.Port
	if port == 0 {
		if n.Transport == sessions.TransportTelnet {
			port = 23
		} else {
			port = 22
		}
	}
	return ProbeTCP(resolve(n.Host), port, timeout)
}
