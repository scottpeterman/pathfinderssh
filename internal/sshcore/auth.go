// internal/sshcore/auth.go
// Authentication chain: agent -> public key -> password -> keyboard-interactive.
//
// Ported from the tetherssh backend with one upgrade: agent auth is actually
// implemented here (the baseline stubbed it). Unix-socket agents only for
// now; Windows OpenSSH agent runs on a named pipe and needs go-winio, which
// is deliberately deferred to keep the dependency surface at x/crypto.
package sshcore

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// expandHome expands a leading "~/" against the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func defaultKnownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

// buildAuthMethods assembles the auth chain for the target connection.
func buildAuthMethods(cfg *Config) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	// 1. SSH agent
	if cfg.UseAgent {
		if m := agentAuth(); m != nil {
			methods = append(methods, m)
		}
	}

	// 2. Public key
	if len(cfg.PrivateKey) > 0 || cfg.PrivateKeyPath != "" {
		m, err := publicKeyAuth(cfg)
		if err != nil {
			return nil, err
		}
		if m != nil {
			methods = append(methods, m)
		}
	}

	// 3. Password — either a known secret, or a callback that asks once.
	// Without the callback, servers that offer only "password" (not
	// keyboard-interactive) never prompt when Password is empty, so Save
	// password never runs and the session looks like it "won't save".
	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	} else if cfg.AuthPrompt != nil {
		methods = append(methods, ssh.PasswordCallback(func() (string, error) {
			return cfg.AuthPrompt("Password:", false)
		}))
	}

	// 4. Keyboard-interactive — always present; handles MFA/RADIUS and
	// servers that only expose password prompts through KI.
	methods = append(methods, ssh.KeyboardInteractive(keyboardInteractive(cfg)))

	if len(methods) == 0 {
		return nil, errors.New("no authentication methods available")
	}
	return methods, nil
}

// agentAuth returns agent-backed auth when SSH_AUTH_SOCK points at a live
// agent, nil otherwise. Signers are fetched lazily at handshake time.
func agentAuth() ssh.AuthMethod {
	if runtime.GOOS == "windows" {
		return nil // named-pipe agent support deferred
	}
	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket == "" {
		return nil
	}
	return ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			return nil, fmt.Errorf("ssh agent: %w", err)
		}
		// Connection intentionally left open: signatures happen over it
		// during the handshake. Closed when the process exits.
		return agent.NewClient(conn).Signers()
	})
}

// publicKeyAuth builds key auth from the in-memory key (preferred) or the
// configured path, prompting for a passphrase via AuthPrompt when the key is
// encrypted and no passphrase was supplied.
func publicKeyAuth(cfg *Config) (ssh.AuthMethod, error) {
	var keyData []byte
	var err error

	switch {
	case len(cfg.PrivateKey) > 0:
		keyData = cfg.PrivateKey
	case cfg.PrivateKeyPath != "":
		keyData, err = os.ReadFile(expandHome(cfg.PrivateKeyPath))
		if err != nil {
			return nil, fmt.Errorf("read private key %s: %w", cfg.PrivateKeyPath, err)
		}
	default:
		return nil, nil
	}

	signer, err := parseSigner(keyData, cfg.KeyPassphrase)
	if err == nil {
		return ssh.PublicKeys(signer), nil
	}

	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) && cfg.AuthPrompt != nil {
		passphrase, perr := cfg.AuthPrompt("Enter passphrase for private key:", false)
		if perr != nil {
			return nil, fmt.Errorf("key passphrase: %w", perr)
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(keyData, []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("parse private key with passphrase: %w", err)
		}
		return ssh.PublicKeys(signer), nil
	}
	return nil, fmt.Errorf("parse private key: %w", err)
}

func parseSigner(keyData []byte, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		return ssh.ParsePrivateKeyWithPassphrase(keyData, []byte(passphrase))
	}
	return ssh.ParsePrivateKey(keyData)
}

// keyboardInteractive answers KI challenges: password-looking questions are
// auto-answered from config, everything else goes to the AuthPrompt callback.
func keyboardInteractive(cfg *Config) ssh.KeyboardInteractiveChallenge {
	return func(user, instruction string, questions []string, echos []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i, q := range questions {
			if strings.Contains(strings.ToLower(q), "password") && cfg.Password != "" {
				answers[i] = cfg.Password
				continue
			}
			if cfg.AuthPrompt == nil {
				return nil, fmt.Errorf("no handler for keyboard-interactive question: %q", q)
			}
			a, err := cfg.AuthPrompt(q, echos[i])
			if err != nil {
				return nil, fmt.Errorf("keyboard-interactive %q: %w", q, err)
			}
			answers[i] = a
		}
		return answers, nil
	}
}
