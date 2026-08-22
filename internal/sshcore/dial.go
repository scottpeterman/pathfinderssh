// internal/sshcore/dial.go
// Dialing: direct targets and jump-host (bastion) tunneling.
//
// The one invariant carried over from the tetherssh backend, on purpose and
// with emphasis: the jump-host hop and the target hop use the IDENTICAL
// algorithm policy and host-key preference order. Diverging them is what
// produced false host-key MISMATCH reports when the same box was reached
// both directly and as a bastion.
package sshcore

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client is an established SSH connection, plus the bastion client when the
// target was reached through a jump host.
type Client struct {
	ssh  *ssh.Client
	jump *ssh.Client // nil for direct connections
	addr string
}

// SSH exposes the underlying x/crypto client for higher layers (netexec).
func (c *Client) SSH() *ssh.Client { return c.ssh }

// Addr returns the target address this client dialed ("host:port"). This is
// the string that was dialed, which may be a name.
func (c *Client) Addr() string { return c.addr }

// RemoteAddr returns the peer address of the underlying connection, which is
// the resolved address rather than the string that was dialed. A device
// reached by name on one hop and by address on another is only recognizable
// as one device if something records the address it actually answered on.
//
// Through a jump host this is the tunneled connection's view and may still be
// a name, in which case the caller gets no worse than Addr gives it.
func (c *Client) RemoteAddr() string {
	if c.ssh == nil || c.ssh.Conn == nil {
		return ""
	}
	ra := c.ssh.Conn.RemoteAddr()
	if ra == nil {
		return ""
	}
	return ra.String()
}

// Close tears down the target connection, then the bastion.
func (c *Client) Close() error {
	var first error
	if c.ssh != nil {
		first = c.ssh.Close()
	}
	if c.jump != nil {
		if err := c.jump.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Dial connects to cfg.Host, tunneling through cfg.Jump when configured.
func Dial(cfg Config) (*Client, error) {
	c := cfg.withDefaults()
	addr := net.JoinHostPort(c.Host, fmt.Sprintf("%d", c.Port))

	authMethods, err := buildAuthMethods(&c)
	if err != nil {
		return nil, err
	}
	hostKeyCB, err := buildHostKeyCallback(&c)
	if err != nil {
		return nil, err
	}

	clientConfig := &ssh.ClientConfig{
		User:              c.Username,
		Auth:              authMethods,
		HostKeyCallback:   hostKeyCB,
		Timeout:           c.Timeout,
		Config:            algorithmPolicy(c.LegacyAlgorithms),
		HostKeyAlgorithms: hostKeyAlgos(c.LegacyAlgorithms),
	}

	var (
		conn       net.Conn
		jumpClient *ssh.Client
	)

	if c.Jump != nil && c.Jump.Host != "" {
		jumpClient, err = dialJump(&c, hostKeyCB)
		if err != nil {
			return nil, err
		}
		conn, err = jumpClient.Dial("tcp", addr)
		if err != nil {
			jumpClient.Close()
			return nil, fmt.Errorf("reach %s through jump host: %w", addr, err)
		}
	} else {
		conn, err = net.DialTimeout("tcp", addr, c.Timeout)
		if err != nil {
			return nil, fmt.Errorf("connect to %s: %w", addr, err)
		}
	}

	// TCP-level keepalive survives host sleep better than app-level pings.
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	// Bound the handshake, then clear the deadline for the session.
	conn.SetDeadline(time.Now().Add(c.Timeout))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientConfig)
	if err != nil {
		conn.Close()
		if jumpClient != nil {
			jumpClient.Close()
		}
		// EOF here is the classic "TCP connected, then the peer vanished"
		// case — offline gear, wrong port, or a non-SSH listener.
		if err == io.EOF || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("SSH handshake with %s: host closed the connection (EOF) — usually offline, wrong port, or not an SSH service", addr)
		}
		return nil, fmt.Errorf("SSH handshake with %s: %w", addr, err)
	}
	conn.SetDeadline(time.Time{})

	return &Client{
		ssh:  ssh.NewClient(sshConn, chans, reqs),
		jump: jumpClient,
		addr: addr,
	}, nil
}

// dialJump establishes the bastion connection. Same algorithm policy, same
// host-key callback, same known_hosts file as the target hop.
func dialJump(c *Config, hostKeyCB ssh.HostKeyCallback) (*ssh.Client, error) {
	j := c.Jump
	port := j.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(j.Host, fmt.Sprintf("%d", port))

	var methods []ssh.AuthMethod
	if j.PrivateKeyPath != "" {
		keyData, err := readKeyFile(j.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("jump host key: %w", err)
		}
		signer, err := parseSigner(keyData, j.KeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("jump host key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if j.Password != "" {
		methods = append(methods, ssh.Password(j.Password))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("jump host %s: no usable credentials (set a key or password)", addr)
	}

	jumpClient, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:              j.Username,
		Auth:              methods,
		HostKeyCallback:   hostKeyCB,
		Timeout:           c.Timeout,
		Config:            algorithmPolicy(c.LegacyAlgorithms),
		HostKeyAlgorithms: hostKeyAlgos(c.LegacyAlgorithms),
	})
	if err != nil {
		return nil, fmt.Errorf("connect to jump host %s: %w", addr, err)
	}
	return jumpClient, nil
}

func readKeyFile(path string) ([]byte, error) {
	return os.ReadFile(expandHome(path))
}
