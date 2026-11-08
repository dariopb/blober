package tui

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/pkg/sftp"
)

func actionKey(m Model, key tea.KeyMsg) (Model, tea.Cmd) {
	updated, cmd := m.Update(key)
	return updated.(Model), cmd
}

func TestGoToValidatesBeforeChangingPane(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	m := New(Config{LocalPath: root})
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	m.input.value = "missing"
	m, cmd := actionKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.input.busy || m.panels[0].location != root {
		t.Fatal("go-to should load asynchronously without changing the pane")
	}
	updated, _ := m.Update(cmd())
	m = updated.(Model)
	if m.input == nil || m.input.err == "" || m.panels[0].location != root {
		t.Fatal("failed navigation must retain the old location and show an error")
	}
	m.input.value = "child"
	m, cmd = actionKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	result := cmd()
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	updated, _ = m.Update(result)
	m = updated.(Model)
	if m.panels[0].location != root {
		t.Fatal("cancelled navigation accepted a late result")
	}
	m.openActionInput(inputLocation, child)
	updated, cmd = m.goToLocation(child)
	m = updated.(Model)
	updated, _ = m.Update(cmd())
	m = updated.(Model)
	if m.input != nil || m.panels[0].location != child {
		t.Fatalf("successful navigation = %q, input=%v", m.panels[0].location, m.input)
	}
}

func TestActionInputEditsUnicodeAtCursor(t *testing.T) {
	m := New(Config{})
	m.openActionInput(inputLocation, "a界c")
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyLeft}, {Type: tea.KeyBackspace},
		{Type: tea.KeyRunes, Runes: []rune("é")},
		{Type: tea.KeyDelete},
	} {
		m, _ = actionKey(m, key)
	}
	if m.input.value != "aé" || m.input.cursor != 2 {
		t.Fatalf("input = %q, cursor = %d", m.input.value, m.input.cursor)
	}
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if m.input.value != "" {
		t.Fatal("Ctrl+U should clear the input")
	}
}

func TestResolveProviderLocations(t *testing.T) {
	prov, root, err := connectWeb(kindHTTP, "https://example.test/root/", "user", "secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		prov    Provider
		current string
		input   string
		want    string
		fail    bool
	}{
		{azureProvider{}, "old/", "/logs/2026", "logs/2026/", false},
		{azureProvider{}, "old/", "/", "", false},
		{azureProvider{}, "", "../escape", "", true},
		{scpProvider{}, "/home/user", "../data", "/home/data", false},
		{prov, root, "sub directory", "https://example.test/root/sub%20directory/", false},
		{prov, root + "sub/", "../", root, false},
		{prov, root, "../../escape", "", true},
		{prov, root, "%2e%2e/escape", "", true},
		{prov, root, "https://other.test/root/", "", true},
		{prov, root, "https://user:pass@example.test/root/", "", true},
		{prov, root, "?token=secret", "", true},
	} {
		got, err := resolveLocation(tc.prov, tc.current, tc.input)
		if (err != nil) != tc.fail || (!tc.fail && got != tc.want) {
			t.Errorf("resolveLocation(%T, %q, %q) = %q, %v; want %q, fail=%v", tc.prov, tc.current, tc.input, got, err, tc.want, tc.fail)
		}
	}
}

func TestRenameModalAndMoveConfirmation(t *testing.T) {
	root, dst := t.TempDir(), t.TempDir()
	src := filepath.Join(root, "before")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{LocalPath: root})
	m.panels[0].entries = []Entry{{Name: "before", Path: src, Size: 7}}
	m.panels[1].location = dst
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyF2})
	m.input.value = "../bad"
	m, cmd := actionKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || m.input == nil || m.input.err == "" {
		t.Fatal("rename should reject names containing separators")
	}
	m.input.value = "after"
	m, cmd = actionKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.copying || m.input != nil {
		t.Fatal("rename should run asynchronously")
	}
	batch := cmd().(tea.BatchMsg)
	result := batch[1]()
	updated, _ := m.Update(result)
	m = updated.(Model)
	target := filepath.Join(root, "after")
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "payload" {
		t.Fatalf("renamed content = %q, %v", data, err)
	}
	if !m.pendingSelectSet[0] || m.pendingSelect[0] != target {
		t.Fatal("rename should retain cursor on the renamed entry")
	}
	m.panels[0].entries = []Entry{{Name: "after", Path: target, Size: 7}}
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if !m.confirming || !strings.Contains(m.View(), "Confirm move") {
		t.Fatal("move should ask for confirmation")
	}
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.confirming {
		t.Fatal("Esc should cancel move confirmation")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("cancelled move changed the source")
	}
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	m, cmd = actionKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	batch = cmd().(tea.BatchMsg)
	result = batch[1]()
	updated, _ = m.Update(result)
	m = updated.(Model)
	if m.status != "move complete" {
		t.Fatalf("move status = %q", m.status)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists after move: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "after")); err != nil || string(data) != "payload" {
		t.Fatalf("moved content = %q, %v", data, err)
	}
}

func TestMoveRejectsConflictsSelfAndDescendants(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(dir, "file")
	if err := os.WriteFile(original, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{LocalPath: root})
	entry := Entry{Name: "dir/", Path: dir, IsDir: true}
	for _, target := range []string{root, dir, filepath.Join(dir, "nested")} {
		_, err := m.moveEntries(context.Background(), m.panels[0], newLocalPanel(target), []Entry{entry}, "", make(chan tea.Msg, 32))
		if err == nil {
			t.Fatalf("move into %s should fail", target)
		}
	}
	existing := filepath.Join(root, "existing")
	if err := os.WriteFile(existing, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := m.moveEntries(context.Background(), newLocalPanel(dir), newLocalPanel(root), []Entry{{Name: "file", Path: original}}, "existing", make(chan tea.Msg, 32))
	if err == nil {
		t.Fatal("rename should reject existing target")
	}
	data, _ := os.ReadFile(existing)
	if string(data) != "untouched" {
		t.Fatal("rename overwrote an existing file")
	}
	if runtime.GOOS != "windows" {
		alias := filepath.Join(root, "alias")
		if err := os.Symlink(dir, alias); err != nil {
			t.Fatal(err)
		}
		_, err := m.moveEntries(context.Background(), m.panels[0], newLocalPanel(alias), []Entry{entry}, "", make(chan tea.Msg, 32))
		if err == nil {
			t.Fatal("move must detect a descendant through a symlink")
		}
	}
}

// Hiding Rename forces the streaming copy/delete path while using real files.
type streamingLocal struct{ Provider }

type failingMoveTarget struct {
	Provider
	fail   string
	cancel context.CancelFunc
}

func (p failingMoveTarget) Create(ctx context.Context, target string, size int64, r io.Reader) error {
	if p.cancel != nil {
		p.cancel()
		return context.Canceled
	}
	if strings.HasSuffix(target, p.fail) {
		return errors.New("injected write failure")
	}
	return p.Provider.Create(ctx, target, size, r)
}

func TestStreamingMovePreservesSourcesOnFailureAndEmptyDirectories(t *testing.T) {
	for _, mode := range []string{"success", "failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root, dst := t.TempDir(), t.TempDir()
			source := filepath.Join(root, "tree")
			if err := os.MkdirAll(filepath.Join(source, "empty"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			m := New(Config{LocalPath: root})
			m.panels[0].provider = streamingLocal{localProvider{}}
			target := newLocalPanel(dst)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "failure":
				target.provider = failingMoveTarget{Provider: localProvider{}, fail: "b"}
			case "cancel":
				target.provider = failingMoveTarget{Provider: localProvider{}, cancel: cancel}
			}
			_, err := m.moveEntries(ctx, m.panels[0], target, []Entry{{Name: "tree/", Path: source, IsDir: true}}, "", make(chan tea.Msg, 32))
			if mode == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("source was not removed: %v", err)
				}
				info, err := os.Stat(filepath.Join(dst, "tree", "empty"))
				if err != nil || !info.IsDir() {
					t.Fatalf("empty directory was not preserved: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected transfer failure")
				}
				for _, name := range []string{"a", "b"} {
					if data, err := os.ReadFile(filepath.Join(source, name)); err != nil || string(data) != name {
						t.Fatalf("failed move lost source %s: %q, %v", name, data, err)
					}
				}
			}
		})
	}
}

func TestMoveRefusesUnrepresentableEmptyDirectory(t *testing.T) {
	tree := moveTree{dirs: []string{"tree", "tree/empty"}, files: []copyItem{{targetRel: "tree/file"}}}
	for _, dst := range []Provider{azureProvider{}, httpProvider{}} {
		if err := validateMoveDirectories(dst, tree); err == nil {
			t.Fatalf("%T must reject an empty directory", dst)
		}
	}
}

func TestSCPNativeRename(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	server, err := sftp.NewServer(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve()
	}()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		server.Close()
		clientConn.Close()
		serverConn.Close()
		<-done
	})
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	if err := os.WriteFile(source, []byte("ssh data"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := scpProvider{session: &scpSession{sftp: client}}
	if err := prov.Rename(context.Background(), source, target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prov.Rename(context.Background(), source, target); err == nil {
		t.Fatal("SFTP rename overwrote an existing target")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "ssh data" {
		t.Fatalf("SFTP moved content = %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		dangling := filepath.Join(root, "dangling")
		if err := os.Symlink(filepath.Join(root, "missing"), dangling); err != nil {
			t.Fatal(err)
		}
		exists, err := moveTargetExists(context.Background(), prov, dangling, false)
		if err != nil || !exists {
			t.Fatalf("SFTP dangling symlink must count as an existing destination: %v, %v", exists, err)
		}
	}
}

func TestWebDAVNativeMoveNeverOverwrites(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "MOVE" || r.URL.Path != "/root/source" || r.Header.Get("Overwrite") != "F" {
			t.Errorf("unexpected MOVE request: %s %s, overwrite=%q", r.Method, r.URL.Path, r.Header.Get("Overwrite"))
		}
		if !strings.HasSuffix(r.Header.Get("Destination"), "/root/target") {
			t.Errorf("destination = %q", r.Header.Get("Destination"))
		}
		if requests == 1 {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusPreconditionFailed)
		}
	}))
	defer server.Close()
	prov, root, err := connectWeb(kindWebDAV, server.URL+"/root/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p := prov.(webdavProvider)
	if err := p.Rename(context.Background(), root+"source", root+"target"); err != nil {
		t.Fatal(err)
	}
	if err := p.Rename(context.Background(), root+"source", root+"target"); err == nil {
		t.Fatal("MOVE must surface an existing-destination failure")
	}
}

func TestHelpFitsAndScrolls(t *testing.T) {
	m := New(Config{})
	m.width, m.height, m.showingHelp = 80, 24, true
	if got := m.renderHelpModal(); lipgloss.Height(got) > m.height || !strings.Contains(got, "[ Close ]") {
		t.Fatal("help must fit with a visible Close button")
	}
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(m.View(), "quit or cancel modal") {
		t.Fatal("help scrolling did not reach the last shortcut")
	}
}
