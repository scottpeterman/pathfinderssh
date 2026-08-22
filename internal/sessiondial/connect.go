// internal/sessiondial/connect.go
//
// One Connect. A session node in, a live term.Transport out.
//
// The terminal already treats SSH, telnet and serial identically — they all
// satisfy term.Transport and the widget contains no branch on which one it
// got. This package is the matching statement on the other side: the three-way
// switch over transports exists exactly once, here, and nothing above it ever
// writes another one.
//
// That is not tidiness. cmd/pfterm has a private dial() with the same shape;
// so did TetherSSH's session manager, which is how it grew to 2,000 lines and
// how a flag ended up working in one dialog and not the other. A second place
// that knows there is more than one kind of connection is a second place for
// them to diverge.
//
// # What this package must not do
//
// It does not read a vault, prompt, own a window, or decide policy. Secrets
// arrive through a Lookup the caller supplies; prompts arrive as callbacks.
// The caller unlocked the vault at startup and knows whether it has a terminal
// to prompt on — this layer only knows how to dial.
package sessiondial

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/scottpeterman/pathfinderssh/internal/serialx"
	"github.com/scottpeterman/pathfinderssh/internal/sessions"
	"github.com/scottpeterman/pathfinderssh/internal/sshcore"
	"github.com/scottpeterman/pathfinderssh/internal/telnetx"
	"github.com/scottpeterman/pathfinderssh/internal/term"
)

// Credential is the secret material for one connection, in the shape this
// package needs it.
//
// It is not vault.Credential. A dial layer that imported the vault would make
// the vault a build-time dependency of connecting at all, and a node that
// authenticates with a typed password would drag an encrypted store into a
// code path that has no use for one. The caller adapts.
type Credential struct {
	Username      string
	AuthType      string // sessions.AuthPassword | AuthPublicKey | AuthAgent
	Password      string
	KeyPath       string
	KeyPassphrase string
}

// Lookup resolves a credential reference — a vault name or id — to secret
// material.
//
// A reference that resolves to nothing is an ordinary error and must stay one:
// a session file written on another machine names credentials that do not
// exist here, and that node still has to load, still has to appear in the
// tree, and still has to be editable. It simply does not connect yet.
type Lookup func(ref string) (Credential, error)

// Options are the things the caller supplies that are not part of the node.
type Options struct {
	// Credentials resolves Node.Credential and Node.Jump.Credential. nil
	// means no vault is available, and a node that references one fails
	// with a message saying so rather than silently dialing with nothing.
	Credentials Lookup

	// HostKeyPrompt is consulted on first contact under the TOFU policy.
	// nil means unknown hosts are refused even under TOFU — no prompt, no
	// acceptance. A key that CHANGED never reaches here; sshcore fails it
	// closed regardless of policy.
	HostKeyPrompt sshcore.HostKeyPromptFunc

	// OnNewHostKey reports a key accepted on first contact, so trust on
	// first use is auditable rather than blind. It fires only after the
	// prompt said yes.
	OnNewHostKey func(host, keyType, fingerprint string)

	// AuthPrompt answers password and keyboard-interactive challenges.
	AuthPrompt sshcore.AuthPromptFunc

	// Resolve maps the configured host to what should actually be dialed.
	// nil means ResolveCGNAT.
	Resolve func(host string) string

	// Size is the initial window. Invalid values fall back to term's
	// default, which is 80x24 — the size every network OS assumes.
	Size term.Size

	// Log receives one line per connection describing what was dialed and
	// how. nil discards it.
	Log func(format string, args ...any)

	// ReachTimeout bounds the pre-dial TCP check. Zero uses DefaultReachTimeout
	// (capped by the session's connect timeout when shorter).
	ReachTimeout time.Duration

	// SkipReachCheck disables the TCP probe (tests / callers that already probed).
	SkipReachCheck bool
}

func (o Options) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

func (o Options) resolve(host string) string {
	if o.Resolve != nil {
		return o.Resolve(host)
	}
	return ResolveCGNAT(host, o.Log)
}

// Connect dials the node and returns a live transport, ready to hand to a
// terminal.
//
// The context bounds the wait, not the dial. None of the three transports can
// be interrupted mid-open: sshcore.Dial takes no ctx, telnetx dials with its
// own timeout, and a serial open is a syscall — on Linux, opening a USB
// adapter that was hot-plugged into a half-enumerated port can park in the
// driver indefinitely, which is a crash-cart-shaped failure, not a hypothetical.
//
// So Connect runs the dial on its own goroutine and returns as soon as either
// the dial finishes or ctx ends. What it must not do is walk away and leave
// the result unowned: an abandoned dial that eventually succeeds holds an SSH
// connection, a socket or an exclusive serial port that nothing will ever
// close, and on serial that means the port stays locked until the process
// exits — so the next attempt fails with "device busy" and the operator blames
// the retry. The abandoning goroutine therefore stays alive solely to close
// whatever arrives.
//
// The honest limit: this bounds the CALLER, not the work. The stuck open is
// still stuck, still holding an OS thread, and a second attempt at the same
// dead port will hang the same way. It buys back the window, the Cancel
// button, and the ability to try something else — which is the whole of what
// was missing.
func Connect(ctx context.Context, n sessions.Node, o Options) (term.Transport, error) {
	n = n.Normalize()
	// A store may hold a default for a session that names no credential, so
	// the username and key rules cannot be settled until after the lookup.
	// See sessions.ValidateFor.
	if err := n.ErrFor(o.Credentials != nil); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	type result struct {
		tp  term.Transport
		err error
	}
	// Buffered: the dial goroutine must never block on a send nobody is
	// waiting for, or abandoning a dial would leak the goroutine as well as
	// the transport.
	done := make(chan result, 1)

	go func() {
		tp, err := dial(ctx, n, o)
		done <- result{tp, err}
	}()

	select {
	case r := <-done:
		return r.tp, Humanize(r.err)
	case <-ctx.Done():
		go func() {
			r := <-done
			if r.tp != nil {
				o.logf("[dial] abandoned dial to %s completed; closing it", n.Target())
				_ = r.tp.Close()
			}
		}()
		return nil, Humanize(fmt.Errorf("connect %s: %w", n.Target(), ctx.Err()))
	}
}

// dial is the three-way transport switch, and it exists exactly once. The node
// arrives already normalized and validated by Connect.
func dial(ctx context.Context, n sessions.Node, o Options) (term.Transport, error) {
	if !o.SkipReachCheck && n.Transport != sessions.TransportSerial {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := ProbeNode(n, o.resolve, o.ReachTimeout); err != nil {
			return nil, err
		}
		o.logf("[reach] %s is accepting TCP", n.Target())
	}
	switch n.Transport {
	case sessions.TransportSerial:
		return connectSerial(n, o)
	case sessions.TransportTelnet:
		return connectTelnet(n, o)
	case sessions.TransportSSH:
		return connectSSH(ctx, n, o)
	default:
		// Unreachable: Validate rejected it above. Present so that adding
		// a fourth transport fails here rather than silently dialing SSH.
		return nil, fmt.Errorf("unsupported transport %q", n.Transport)
	}
}

func connectSerial(n sessions.Node, o Options) (term.Transport, error) {
	b := serialx.New(serialx.Config{
		Port:     n.SerialPort,
		Baud:     n.Baud,
		DataBits: n.DataBits,
		Parity:   n.Parity,
		StopBits: n.StopBits,

		// Block until a byte arrives rather than polling. A console is
		// idle most of the time, and a positive timeout turns every
		// expiry into a read that returned nothing — which is what the
		// read loop then has to pace around.
		ReadTimeout: 0,
	})
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("serial %s: %w", n.SerialPort, err)
	}
	// Framing is worth logging in full. 8N1 is almost always right, and
	// the times it is not are exactly the times nobody remembers what was
	// set.
	o.logf("[serial] %s", n.Target())
	return b, nil
}

func connectTelnet(n sessions.Node, o Options) (term.Transport, error) {
	host := o.resolve(n.Host)
	cfg := telnetx.Config{
		Host:     host,
		Port:     n.Port,
		TermType: n.TermType,
	}
	// WithCRLF rather than the field directly, so a deliberate false is not
	// reset to the default on the way in.
	b := telnetx.New(cfg.WithCRLF(n.CRLF()))
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("telnet %s:%d: %w", host, n.Port, err)
	}
	o.logf("[telnet] %s:%d (plaintext, no auth)", host, n.Port)
	return b, nil
}

func connectSSH(ctx context.Context, n sessions.Node, o Options) (term.Transport, error) {
	cred, err := resolveWithDefault(o.Credentials, n, n.Credential)
	if err != nil {
		return nil, err
	}

	host := o.resolve(n.Host)

	cfg := sshcore.Config{
		Host:             host,
		Port:             n.Port,
		Timeout:          time.Duration(n.ConnectTimeoutSec) * time.Second,
		LegacyAlgorithms: n.LegacyAlgorithms,
		KnownHostsPath:   n.KnownHostsPath,
		AuthPrompt:       o.AuthPrompt,
	}

	policy, prompt := hostKeyPolicy(n.HostKeyPolicy, o)
	cfg.HostKeys = policy
	cfg.HostKeyPrompt = prompt
	if policy == sshcore.HostKeyInsecure {
		o.logf("[ssh] host-key verification disabled for %s", host)
	}

	applyCredential(&cfg, n, cred)

	// Checked here rather than in Validate because here is where the answer
	// is known: the node, the credential it named and the store default have
	// all had their turn. Without this the gap arrives as an authentication
	// failure, which sends somebody looking at the device.
	if strings.TrimSpace(cfg.Username) == "" {
		return nil, fmt.Errorf("no username: the session names none, and no credential supplied one")
	}

	if n.Jump.InUse() {
		// Resolved HERE and not above, because a node carrying a stale
		// Jump.Credential -- a bastion it no longer goes through, naming
		// a vault entry since deleted -- used to be refused a DIRECT
		// connection for a reason with nothing to do with the connection
		// being made.
		jumpCred, err := resolve(o.Credentials, n.Jump.Credential)
		if err != nil {
			return nil, fmt.Errorf("jump host: %w", err)
		}
		jump := &sshcore.JumpConfig{
			Host:     o.resolve(n.Jump.Host),
			Port:     n.Jump.Port,
			Username: firstNonEmpty(jumpCred.Username, n.Jump.Username),
		}
		// The bastion gets the same strict treatment as the target: one
		// auth type, never a password offered alongside a key.
		if key := firstNonEmpty(jumpCred.KeyPath, n.Jump.KeyPath); key != "" && jumpCred.AuthType != sessions.AuthPassword {
			jump.PrivateKeyPath = key
			jump.KeyPassphrase = firstNonEmpty(jumpCred.KeyPassphrase, n.Jump.KeyPassphrase)
		} else {
			jump.Password = firstNonEmpty(jumpCred.Password, n.Jump.Password)
		}
		cfg.Jump = jump
		o.logf("[ssh] via %s@%s:%d", jump.Username, jump.Host, jump.Port)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c, err := sshcore.Dial(cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s:%d: %w", cfg.Username, host, n.Port, err)
	}

	s, err := term.Open(c, term.Options{
		Term: n.TermType,
		Size: o.Size,

		// OwnsClient: this client was dialed for this session and nothing
		// else holds it, so closing the tab takes the connection with it.
		OwnsClient: true,
	})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("ssh %s: opening shell: %w", host, err)
	}
	for _, e := range s.EnvErrors {
		// Not a failure. Most network operating systems reject env
		// requests outright and the session is fine without them.
		o.logf("[ssh] env rejected: %v", e)
	}
	o.logf("[ssh] %s@%s:%d", cfg.Username, host, n.Port)
	return s, nil
}

// applyCredential sets exactly one authentication method on the config.
//
// Strictness is the point. Offering a password alongside a key means a device
// that rejects the key falls through to a password attempt the user did not
// ask for, which on gear with a low failed-login threshold is how an account
// gets locked out by a typo in a key path.
func applyCredential(cfg *sshcore.Config, n sessions.Node, cred Credential) {
	cfg.Username = firstNonEmpty(cred.Username, n.Username)

	switch authTypeFor(n.AuthType, cred) {
	case sessions.AuthPublicKey:
		cfg.PrivateKeyPath = firstNonEmpty(cred.KeyPath, n.KeyPath)
		cfg.KeyPassphrase = firstNonEmpty(cred.KeyPassphrase, n.KeyPassphrase)
		cfg.UseAgent = false
	case sessions.AuthPassword:
		cfg.Password = firstNonEmpty(cred.Password, n.Password)
		cfg.UseAgent = false
	default: // agent
		// No material of our own: the agent answers, and failing that the
		// AuthPrompt does. This is what an unconfigured node uses, and it
		// is why a node with nothing filled in still connects on a machine
		// with a loaded agent.
		cfg.UseAgent = true
	}
}

// authTypeFor decides how to authenticate.
//
// A referenced credential wins: choosing one from the vault IS choosing how to
// authenticate, and a node whose auth selector still reads "agent" from before
// the credential was picked should not override it. With no credential, the
// node's own setting stands.
func authTypeFor(nodeAuth string, cred Credential) string {
	if t := strings.TrimSpace(cred.AuthType); t != "" {
		return t
	}
	return nodeAuth
}

// hostKeyPolicy maps the session-level policy name onto sshcore's, and pairs
// it with the right prompt.
//
// Note what TOFU without a prompt does: it refuses the unknown host rather
// than accepting it. A missing callback is a caller that has nowhere to ask,
// and "nobody could be asked" must not resolve to yes.
func hostKeyPolicy(name string, o Options) (sshcore.HostKeyPolicy, sshcore.HostKeyPromptFunc) {
	switch name {
	case sessions.HostKeyInsecure:
		// Deliberately reachable. Lab gear gets rebuilt and its keys
		// change every time; forcing verification there teaches people to
		// answer yes on reflex to the prompt that actually matters.
		return sshcore.HostKeyInsecure, nil
	case sessions.HostKeyStrict:
		return sshcore.HostKeyStrict, nil
	default:
		if o.HostKeyPrompt == nil {
			return sshcore.HostKeyTOFU, nil
		}
		return sshcore.HostKeyTOFU, func(hostname string, remote net.Addr, key ssh.PublicKey) (bool, error) {
			ok, err := o.HostKeyPrompt(hostname, remote, key)
			if ok && err == nil && o.OnNewHostKey != nil {
				o.OnNewHostKey(hostname, key.Type(), ssh.FingerprintSHA256(key))
			}
			return ok, err
		}
	}
}

// resolveWithDefault resolves the session's own credential reference, and asks
// the store what it uses when a session names nothing.
//
// The default is ALL-OR-NOTHING and it stays out of a node that states auth of
// its own. Merging field by field would take a username from one place and a
// password from another -- a credential nobody assembled and nobody can debug
// from the screen. It is also the only route back to manual auth once a
// default exists: typing a username is how a session opts out.
//
// An error from the empty case is SWALLOWED. "" names nothing, so it cannot be
// missing, and every lookup written before the default existed rejects it.
func resolveWithDefault(lookup Lookup, n sessions.Node, ref string) (Credential, error) {
	if strings.TrimSpace(ref) != "" {
		// A NAMED credential outranks the node: naming one is a choice.
		return resolve(lookup, ref)
	}
	if lookup == nil || nodeStatesItsOwnAuth(n) {
		return Credential{}, nil
	}
	c, err := lookup("")
	if err != nil {
		return Credential{}, nil
	}
	return c, nil
}

// nodeStatesItsOwnAuth reports whether the node says anything about who it
// connects as.
//
// AuthType is deliberately NOT part of this. Normalize() fills an empty one
// with agent, so "somebody chose agent" and "the node said nothing" are the
// same value by the time anything reads it -- and every node produced by a map
// import carries agent.
func nodeStatesItsOwnAuth(n sessions.Node) bool {
	return strings.TrimSpace(n.Username) != "" ||
		strings.TrimSpace(n.Password) != "" ||
		strings.TrimSpace(n.KeyPath) != ""
}

// resolve turns a credential reference into secret material. An empty ref
// names nothing and resolves to nothing -- the jump path uses this, because a
// default is what THIS session uses when it names nothing, and silently
// authenticating to somebody's bastion with the estate default is a decision
// nobody made.
func resolve(lookup Lookup, ref string) (Credential, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Credential{}, nil
	}
	if lookup == nil {
		return Credential{}, fmt.Errorf("session references credential %q but no credential store is available", ref)
	}
	c, err := lookup(ref)
	if err != nil {
		return Credential{}, fmt.Errorf("credential %q: %w", ref, err)
	}
	return c, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// cgnat is the shared address space from RFC 6598.
var cgnat = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// ResolveCGNAT turns a carrier-NAT address into a name when one exists.
//
// An address in 100.64.0.0/10 is not a stable identity: the same address
// reaches different equipment depending on where you are standing. Resolving
// it and using the name pins the session to a device rather than to an address
// that may point somewhere else tomorrow.
//
// Anything else — a name, a routable address, an address that does not
// resolve — is returned untouched. This never fails a connection; the worst
// case is that the address is used as given, which is what would have happened
// anyway.
func ResolveCGNAT(host string, logf func(string, ...any)) string {
	ip := net.ParseIP(host)
	if ip == nil || !cgnat.Contains(ip) {
		return host
	}
	names, err := net.LookupAddr(host)
	if err != nil || len(names) == 0 {
		if logf != nil {
			logf("[cgnat] %s is in 100.64.0.0/10 and does not resolve; using the address as given", host)
		}
		return host
	}
	name := strings.TrimSuffix(names[0], ".")
	if logf != nil {
		logf("[cgnat] %s is in 100.64.0.0/10, resolved to %s", host, name)
	}
	return name
}
