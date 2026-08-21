// cmd/rawdump is a diagnostic tool: connect, send one command, and print
// both the exact raw bytes received (escaped, so control characters are
// visible) and the same text run through netexec.Normalize, side by side.
//
// It exists because the interesting failures in a new platform's session
// handling are invisible at the text level -- a device that redraws its
// prompt with absolute cursor addressing instead of a trailing newline, a
// pager banner phrased differently from every other platform's, a doubled
// CR before the line feed -- and the tool's own session/prompt code
// already cleans and discards exactly the bytes that would show you why.
// Three separate live bugs during ArubaOS-Switch, ArubaOS-CX, and Comware
// 7 validation were only diagnosable by looking at what this tool prints:
// a cursor-addressed prompt gluing a command's echo to the next prompt
// line, an ArubaOS-CX pager format the generic continue-prompt detector
// didn't yet recognize, and a Comware 7 switch whose doubled "\r\r\n" line
// endings were silently wiping out every line of real output before it
// ever reached the classifier.
//
// Reach for this whenever a new platform's fingerprint or crawl step
// times out or misclassifies for a reason that isn't obvious from the
// cleaned-up output alone -- point it at the same host, user, and command
// that failed, and compare RAW against NORM.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/scottpeterman/pathfinderssh/internal/netexec"
)

func main() {
	host := flag.String("host", "", "host:port")
	user := flag.String("user", "", "username")
	cmd := flag.String("cmd", "no page", "command to send")
	termType := flag.String("term", "vt100", "TERM value sent with the pty-req")
	legacy := flag.Bool("legacy", false, "add legacy host key algorithms")
	flag.Parse()

	if *host == "" || *user == "" {
		fmt.Fprintln(os.Stderr, "usage: rawdump -host ip:22 -user NAME [-cmd \"no page\"] [-legacy]")
		os.Exit(1)
	}

	fmt.Fprint(os.Stderr, "password: ")
	pwBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read password:", err)
		os.Exit(1)
	}
	password := string(pwBytes)

	// Mirrors internal/sshcore/algorithms.go's legacy tail exactly -- that
	// package's own lists are unexported, so this is a deliberate copy, not
	// a shortcut. A first cut of this tool only added legacy host-key
	// algorithms and forgot ciphers/KEX/MACs entirely, which meant it
	// failed to even reach a Comware switch that crawl.exe's real -legacy
	// path connected to seconds earlier -- confirmed live (2026-08-21).
	hostKeyAlgos := []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "rsa-sha2-512", "rsa-sha2-256"}
	sshConfig := ssh.Config{
		KeyExchanges: []string{
			"curve25519-sha256", "curve25519-sha256@libssh.org",
			"ecdh-sha2-nistp256", "ecdh-sha2-nistp384", "ecdh-sha2-nistp521",
			"diffie-hellman-group14-sha256", "diffie-hellman-group16-sha512", "diffie-hellman-group18-sha512",
		},
		Ciphers: []string{
			"chacha20-poly1305@openssh.com", "aes128-gcm@openssh.com", "aes256-gcm@openssh.com",
			"aes128-ctr", "aes192-ctr", "aes256-ctr",
		},
		MACs: []string{
			"hmac-sha2-256-etm@openssh.com", "hmac-sha2-512-etm@openssh.com",
			"hmac-sha2-256", "hmac-sha2-512",
		},
	}
	if *legacy {
		hostKeyAlgos = append(hostKeyAlgos, "ssh-rsa", "ssh-dss")
		sshConfig.KeyExchanges = append(sshConfig.KeyExchanges,
			"diffie-hellman-group14-sha1", "diffie-hellman-group1-sha1",
			"diffie-hellman-group-exchange-sha256", "diffie-hellman-group-exchange-sha1")
		sshConfig.Ciphers = append(sshConfig.Ciphers, "aes128-cbc", "aes192-cbc", "aes256-cbc", "3des-cbc")
		sshConfig.MACs = append(sshConfig.MACs, "hmac-sha1", "hmac-sha1-96", "hmac-md5", "hmac-md5-96")
	}

	cfg := &ssh.ClientConfig{
		Config:            sshConfig,
		User:              *user,
		Auth:              []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback:   ssh.InsecureIgnoreHostKey(), // diagnostic only, never in shipped code
		HostKeyAlgorithms: hostKeyAlgos,
		Timeout:           10 * time.Second,
	}

	client, err := ssh.Dial("tcp", *host, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		fmt.Fprintln(os.Stderr, "session:", err)
		os.Exit(1)
	}
	defer sess.Close()

	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := sess.RequestPty(*termType, 60, 511, modes); err != nil {
		fmt.Fprintln(os.Stderr, "pty:", err)
		os.Exit(1)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stdin:", err)
		os.Exit(1)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stdout:", err)
		os.Exit(1)
	}
	if err := sess.Shell(); err != nil {
		fmt.Fprintln(os.Stderr, "shell:", err)
		os.Exit(1)
	}

	out := make(chan []byte, 256)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				out <- chunk
			}
			if err != nil {
				close(out)
				return
			}
		}
	}()

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	var accum []byte
	drain := func(d time.Duration) {
		deadline := time.After(d)
		for {
			select {
			case chunk, ok := <-out:
				if !ok {
					return
				}
				accum = append(accum, chunk...)
				fmt.Fprintf(w, "RAW:  %q\n", chunk)
				fmt.Fprintf(w, "NORM: %q\n", netexec.Normalize(string(accum)))
				w.Flush()
			case <-deadline:
				return
			}
		}
	}

	fmt.Fprintln(os.Stderr, "--- login banner / initial prompt (5s) ---")
	drain(5 * time.Second)

	// If a "press any key" style gate showed up, nudge it once and drain
	// again -- best effort, this is a diagnostic tool, not the real
	// session logic.
	fmt.Fprint(os.Stderr, "sending space in case of a continue-prompt gate...\n")
	stdin.Write([]byte(" "))
	drain(3 * time.Second)

	fmt.Fprintf(os.Stderr, "--- sending %q ---\n", *cmd)
	stdin.Write([]byte(strings.TrimRight(*cmd, "\r\n") + "\n"))
	drain(8 * time.Second)

	fmt.Fprintln(os.Stderr, "--- done ---")
}
