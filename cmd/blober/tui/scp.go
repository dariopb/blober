package tui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// scpConfig holds the connection parameters collected from the provider modal.
type scpConfig struct {
	host    string
	port    string
	user    string
	pass    string
	keyPath string
	root    string
}

// cleanHost normalizes a host string for dialing. Besides trimming surrounding
// whitespace it strips any control or Unicode "format" runes (a stray carriage
// return, BOM, zero-width space, etc. that can ride along on a pasted value).
// Such hidden characters stop a literal IP from parsing and turn an otherwise
// valid hostname into one the resolver rejects, surfacing on Windows as
// "lookup <host>: invalid argument".
func cleanHost(h string) string {
	h = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, h)
	return strings.TrimSpace(h)
}

func (c scpConfig) address() string {
	port := cleanHost(c.port)
	if port == "" {
		port = "22"
	}
	return net.JoinHostPort(cleanHost(c.host), port)
}

func (c scpConfig) label() string {
	host := cleanHost(c.host)
	user := strings.TrimSpace(c.user)
	if user == "" {
		return "scp " + host
	}
	return fmt.Sprintf("scp %s@%s", user, host)
}

// scpSession owns the live ssh and sftp clients. A pointer to it is stored on
// the scpProvider so that bubbletea's value copies of the model share one
// connection.
type scpSession struct {
	ssh       *ssh.Client
	sftp      *sftp.Client
	label     string
	closeOnce sync.Once
	closeErr  error
}

func (s *scpSession) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.sftp != nil {
			s.closeErr = s.sftp.Close()
		}
		if s.ssh != nil {
			if cerr := s.ssh.Close(); s.closeErr == nil {
				s.closeErr = cerr
			}
		}
	})
	return s.closeErr
}

// connectSCP dials the host and opens an sftp session. It also resolves the
// starting directory (defaulting to the remote working directory).
func connectSCP(cfg scpConfig) (*scpSession, string, error) {
	if cleanHost(cfg.host) == "" {
		return nil, "", errors.New("host is required")
	}
	if strings.TrimSpace(cfg.user) == "" {
		return nil, "", errors.New("user is required")
	}
	var auths []ssh.AuthMethod
	if keyPath := strings.TrimSpace(cfg.keyPath); keyPath != "" {
		signer, err := loadSigner(keyPath)
		if err != nil {
			return nil, "", err
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if cfg.pass != "" {
		auths = append(auths, ssh.Password(cfg.pass))
	}
	// With neither an explicit key nor a password, fall back to the user's
	// default SSH credentials: the ssh-agent (if running) and the standard
	// private keys under ~/.ssh.
	var cleanup func()
	if len(auths) == 0 {
		var defaults []ssh.AuthMethod
		defaults, cleanup = defaultSSHAuths()
		if len(defaults) == 0 {
			return nil, "", errors.New("no password or key provided and no usable ssh-agent or ~/.ssh keys found")
		}
		auths = defaults
	}
	if cleanup != nil {
		defer cleanup()
	}

	hostKeyCallback, err := hostKeyCallback()
	if err != nil {
		return nil, "", err
	}

	clientConfig := &ssh.ClientConfig{
		User:            strings.TrimSpace(cfg.user),
		Auth:            auths,
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}
	// When we already trust a host key for this server, advertise its
	// algorithm(s) so the server presents that key type. Otherwise the Go
	// client may negotiate a different host-key type than the one stored,
	// which knownhosts reports as a "key mismatch" even though the host is
	// the one we trust. OpenSSH does the same reordering implicitly.
	if algos := knownHostKeyAlgorithms(hostKeyCallback, cfg.address()); len(algos) > 0 {
		clientConfig.HostKeyAlgorithms = algos
	}

	sshClient, err := ssh.Dial("tcp", cfg.address(), clientConfig)
	if err != nil {
		return nil, "", err
	}
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, "", err
	}

	root := strings.TrimSpace(cfg.root)
	if root == "" {
		if wd, err := sftpClient.Getwd(); err == nil && wd != "" {
			root = wd
		} else {
			root = "/"
		}
	}

	return &scpSession{ssh: sshClient, sftp: sftpClient, label: cfg.label()}, root, nil
}

func loadSigner(keyPath string) (ssh.Signer, error) {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse private key %s: %w", keyPath, err)
	}
	return signer, nil
}

// defaultSSHAuths builds authentication methods from the user's default SSH
// credentials: a running ssh-agent (via SSH_AUTH_SOCK) and the standard private
// keys under ~/.ssh. The returned cleanup closes the agent connection and may be
// nil. Passphrase-protected or unparseable keys are skipped.
func defaultSSHAuths() ([]ssh.AuthMethod, func()) {
	var auths []ssh.AuthMethod
	var cleanup func()
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			agentClient := agent.NewClient(conn)
			auths = append(auths, ssh.PublicKeysCallback(agentClient.Signers))
			cleanup = func() { _ = conn.Close() }
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		var signers []ssh.Signer
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_dsa"} {
			data, err := os.ReadFile(filepath.Join(home, ".ssh", name))
			if err != nil {
				continue
			}
			signer, err := ssh.ParsePrivateKey(data)
			if err != nil {
				continue
			}
			signers = append(signers, signer)
		}
		if len(signers) > 0 {
			auths = append(auths, ssh.PublicKeys(signers...))
		}
	}
	return auths, cleanup
}

// hostKeyCallback verifies against ~/.ssh/known_hosts. Unknown hosts are added
// on first use (trust on first use); known hosts with a changed key are
// rejected to guard against man-in-the-middle attacks.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	sshDir := path.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return nil, err
	}
	knownHostsPath := path.Join(sshDir, "known_hosts")
	if _, err := os.Stat(knownHostsPath); errors.Is(err, os.ErrNotExist) {
		if f, err := os.OpenFile(knownHostsPath, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_ = f.Close()
		}
	}
	verify, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, err
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := verify(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
			// Unknown host: record the key and accept it.
			return appendKnownHost(knownHostsPath, hostname, remote, key)
		}
		return err
	}, nil
}

// knownHostKeyAlgorithms returns the host-key algorithms we already trust for
// the given address in known_hosts, so they can be advertised during the SSH
// handshake. The known_hosts database is probed with a throwaway key: a
// resulting KeyError carries the trusted keys for the host, whose types we map
// to negotiable algorithms (expanding ssh-rsa to its rsa-sha2 variants). When
// the host is unknown the result is empty so first-use (TOFU) still works.
func knownHostKeyAlgorithms(cb ssh.HostKeyCallback, address string) []string {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil
	}
	// db.check requires a parseable remote address; the hostname in `address`
	// takes precedence for the actual lookup.
	remote := &net.TCPAddr{IP: net.IPv4zero, Port: 0}
	var keyErr *knownhosts.KeyError
	if err := cb(address, remote, signer.PublicKey()); !errors.As(err, &keyErr) || len(keyErr.Want) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var algos []string
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			algos = append(algos, a)
		}
	}
	for _, known := range keyErr.Want {
		switch known.Key.Type() {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA256)
			add(ssh.KeyAlgoRSASHA512)
			add(ssh.KeyAlgoRSA)
		default:
			add(known.Key.Type())
		}
	}
	return algos
}

func appendKnownHost(knownHostsPath, hostname string, remote net.Addr, key ssh.PublicKey) error {
	f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	addresses := []string{hostname}
	if remote != nil {
		addresses = append(addresses, remote.String())
	}
	line := knownhosts.Line(addresses, key)
	_, err = f.WriteString(line + "\n")
	return err
}

// scpProvider implements Provider over an sftp session.
type scpProvider struct {
	session *scpSession
}

func (p scpProvider) client() *sftp.Client { return p.session.sftp }

func (p scpProvider) Close() error { return p.session.Close() }

func (p scpProvider) Label() string {
	if p.session != nil {
		return p.session.label
	}
	return "scp"
}

func (p scpProvider) List(_ context.Context, location string) ([]Entry, error) {
	infos, err := p.client().ReadDir(location)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(infos)+1)
	if parent := path.Dir(location); parent != location {
		entries = append(entries, Entry{Name: "..", Path: parent, IsDir: true, parent: true})
	}
	for _, info := range infos {
		name := info.Name()
		displayName := name
		if info.IsDir() {
			displayName += "/"
		}
		entries = append(entries, Entry{
			Name:         displayName,
			Path:         path.Join(location, name),
			IsDir:        info.IsDir(),
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
	}
	sortEntries(entries)
	return entries, nil
}

func (scpProvider) Parent(location string) (string, bool) {
	parent := path.Dir(location)
	if parent == location {
		return "", false
	}
	return parent, true
}

func (scpProvider) Join(location, rel string) (string, error) {
	if err := safeRel(rel); err != nil {
		return "", err
	}
	return path.Join(location, rel), nil
}

func (p scpProvider) Mkdir(_ context.Context, location, name string) (string, bool, error) {
	dir := path.Join(location, name)
	if err := p.client().Mkdir(dir); err != nil {
		return "", false, err
	}
	return dir, false, nil
}

func (scpProvider) Virtual() bool { return false }

func (p scpProvider) Remove(_ context.Context, entry Entry) error {
	if entry.IsDir {
		return p.removeAll(entry.Path)
	}
	return p.client().Remove(entry.Path)
}

func (p scpProvider) removeAll(root string) error {
	infos, err := p.client().ReadDir(root)
	if err != nil {
		return err
	}
	for _, info := range infos {
		child := path.Join(root, info.Name())
		if info.IsDir() {
			if err := p.removeAll(child); err != nil {
				return err
			}
			continue
		}
		if err := p.client().Remove(child); err != nil {
			return err
		}
	}
	return p.client().RemoveDirectory(root)
}

func (p scpProvider) Exists(_ context.Context, target string) (bool, error) {
	if _, err := p.client().Stat(target); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

func (p scpProvider) Open(_ context.Context, target string) (io.ReadCloser, int64, error) {
	f, err := p.client().Open(target)
	if err != nil {
		return nil, 0, err
	}
	size := int64(-1)
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	return f, size, nil
}

func (p scpProvider) Create(_ context.Context, target string, _ int64, r io.Reader) error {
	if dir := path.Dir(target); dir != "" && dir != "." {
		if err := p.client().MkdirAll(dir); err != nil {
			return err
		}
	}
	f, err := p.client().Create(target)
	if err != nil {
		return err
	}
	// ReadFromWithConcurrency pipelines many in-flight writes (like native scp)
	// instead of one round-trip per 32KB packet. A plain io.Copy/ReadFrom only
	// pipelines when the source reports its size, which the progressReader
	// wrapper does not; passing concurrency 0 selects the client default.
	if _, err := f.ReadFromWithConcurrency(r, 0); err != nil {
		_ = f.Close()
		_ = p.client().Remove(target)
		return err
	}
	return f.Close()
}

func (p scpProvider) Walk(_ context.Context, entry Entry) ([]WalkItem, error) {
	base := copyTargetName(entry)
	var items []WalkItem
	if err := p.walk(entry.Path, base, &items); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Rel < items[j].Rel })
	return items, nil
}

func (p scpProvider) walk(dir, relBase string, items *[]WalkItem) error {
	infos, err := p.client().ReadDir(dir)
	if err != nil {
		return err
	}
	for _, info := range infos {
		child := path.Join(dir, info.Name())
		childRel := path.Join(relBase, info.Name())
		if info.IsDir() {
			if err := p.walk(child, childRel, items); err != nil {
				return err
			}
			continue
		}
		*items = append(*items, WalkItem{
			Path:    child,
			Rel:     childRel,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return nil
}
