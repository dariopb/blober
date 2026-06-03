package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

func (c scpConfig) address() string {
	port := strings.TrimSpace(c.port)
	if port == "" {
		port = "22"
	}
	return net.JoinHostPort(strings.TrimSpace(c.host), port)
}

func (c scpConfig) label() string {
	user := strings.TrimSpace(c.user)
	if user == "" {
		return "scp " + strings.TrimSpace(c.host)
	}
	return fmt.Sprintf("scp %s@%s", user, strings.TrimSpace(c.host))
}

// scpSession owns the live ssh and sftp clients. A pointer to it is stored on
// the panel so that bubbletea's value copies of the model share one connection.
type scpSession struct {
	ssh   *ssh.Client
	sftp  *sftp.Client
	label string
}

func (s *scpSession) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.sftp != nil {
		err = s.sftp.Close()
	}
	if s.ssh != nil {
		if cerr := s.ssh.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// connectSCP dials the host and opens an sftp session. It also resolves the
// starting directory (defaulting to the remote working directory).
func connectSCP(cfg scpConfig) (*scpSession, string, error) {
	if strings.TrimSpace(cfg.host) == "" {
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
	if _, err := io.Copy(f, r); err != nil {
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
