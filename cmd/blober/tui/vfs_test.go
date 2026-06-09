package tui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSafeRelRejectsTraversal(t *testing.T) {
	bad := []string{"", "/etc/passwd", "..", "a/../b", "a/./b", "../x", "a//b", "a\\..\\b"}
	for _, rel := range bad {
		if err := safeRel(rel); err == nil {
			t.Errorf("safeRel(%q) = nil, want error", rel)
		}
	}
	good := []string{"a", "a/b", "dir/nested/a.txt"}
	for _, rel := range good {
		if err := safeRel(rel); err != nil {
			t.Errorf("safeRel(%q) = %v, want nil", rel, err)
		}
	}
}

func TestResolveProviderDefaultsToLocal(t *testing.T) {
	m := New(Config{})
	// A panel without an explicit provider resolves to localProvider.
	if _, ok := m.resolveProvider(panel{}).(localProvider); !ok {
		t.Fatalf("empty panel did not resolve to localProvider")
	}
	// An explicit provider on the panel is honored.
	p := panel{provider: azureProvider{}}
	if _, ok := m.resolveProvider(p).(azureProvider); !ok {
		t.Fatalf("explicit provider was not honored")
	}
}

func TestLocalProviderJoinAndParent(t *testing.T) {
	lp := localProvider{}
	joined, err := lp.Join("/base", "dir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if joined != filepath.Join("/base", "dir", "file.txt") {
		t.Fatalf("Join = %q", joined)
	}
	if _, err := lp.Join("/base", "../escape"); err == nil {
		t.Fatal("Join allowed traversal")
	}
	parent, ok := lp.Parent("/base/dir")
	if !ok || parent != filepath.Dir("/base/dir") {
		t.Fatalf("Parent = %q ok=%v", parent, ok)
	}
}

func TestLocalProviderWalkOpenCreateRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "dir")
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	lp := localProvider{}
	ctx := context.Background()
	items, err := lp.Walk(ctx, Entry{Name: "dir/", Path: dir, IsDir: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Rel != "dir/nested/a.txt" || items[0].Size != 5 {
		t.Fatalf("walk items = %#v", items)
	}

	// Open the source, then Create a copy at a fresh destination.
	rc, size, err := lp.Open(ctx, items[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if size != 5 {
		t.Fatalf("size = %d", size)
	}
	dst := filepath.Join(tmp, "out", "copy.txt")
	if err := lp.Create(ctx, dst, size, rc); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("copied content = %q", got)
	}
	// Create must not leave temp files behind in the destination directory.
	files, _ := os.ReadDir(filepath.Dir(dst))
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".blober_download_") {
			t.Fatalf("leftover temp file %q", f.Name())
		}
	}
}

func TestLocalToLocalCopyViaCopyCmd(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{})
	// Pane 1 (right) is the source; pane 0 is the destination.
	m.panels[0].provider = localProvider{}
	m.panels[0].location = dstDir
	m.panels[1].location = srcDir
	m.panels[1].entries = []Entry{{Name: "a.txt", Path: filepath.Join(srcDir, "a.txt"), Size: 7}}
	m.active = 1

	items := []copyItem{{entry: m.panels[1].entries[0], targetRel: "a.txt"}}
	progressCh := make(chan tea.Msg, 16)
	done := make(chan struct{})
	go func() {
		for range progressCh {
		}
		close(done)
	}()
	cmd := m.copyCmd(context.Background(), 1, items, false, progressCh)
	if cmd == nil {
		t.Fatal("copyCmd returned nil")
	}
	msg := cmd()
	<-done
	if dm, ok := msg.(copyDoneMsg); !ok || dm.err != nil {
		t.Fatalf("copy result = %#v", msg)
	}
	got, err := os.ReadFile(filepath.Join(dstDir, "a.txt"))
	if err != nil {
		t.Fatalf("copied file missing: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("copied content = %q", got)
	}
}

func TestProviderModalOpensAndSwitchesToLocal(t *testing.T) {
	m := New(Config{Container: "logs"})
	m.active = 0
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(Model)
	if !m.providerModal || m.providerModalPanel != 0 {
		t.Fatalf("p did not open provider modal: modal=%v panel=%d", m.providerModal, m.providerModalPanel)
	}
	view := m.View()
	if !strings.Contains(view, "Provider for pane 1") {
		t.Fatalf("modal view missing title:\n%s", view)
	}

	// Cycle the type selector to Local (start type is Azure for pane 0).
	m.providerType = kindLocal
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.providerModal {
		t.Fatal("modal should close after selecting Local")
	}
	if _, ok := m.panels[0].provider.(localProvider); !ok {
		t.Fatalf("pane 0 provider = %T, want localProvider", m.panels[0].provider)
	}
	if cmd == nil {
		t.Fatal("expected a list command after switching provider")
	}
}

func TestProviderModalScpValidation(t *testing.T) {
	m := New(Config{})
	m.active = 1
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(Model)
	m.providerType = kindSCP

	// No host/user yet: Connect should report validation error, modal stays open.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if !m.providerModal {
		t.Fatal("modal closed despite invalid scp config")
	}
	if cmd != nil {
		t.Fatal("expected no command for invalid scp config")
	}
	if !strings.Contains(m.status, "host is required") {
		t.Fatalf("status = %q, want host required", m.status)
	}

	// Type a host into the Host field.
	m.providerField = providerFieldHost
	for _, r := range "example.com" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	if m.providerForm.host != "example.com" {
		t.Fatalf("host field = %q", m.providerForm.host)
	}
	// Backspace removes the last rune.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = updated.(Model)
	if m.providerForm.host != "example.co" {
		t.Fatalf("after backspace host = %q", m.providerForm.host)
	}
}

func TestProviderModalScpConnectsWithoutPasswordOrKey(t *testing.T) {
	m := New(Config{})
	m.active = 1
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(Model)
	m.providerType = kindSCP
	// Host and user only; no password and no key -> should fall back to the
	// user's ~/.ssh credentials and begin connecting rather than erroring.
	m.providerForm = providerForm{host: "h", port: "22", user: "u"}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if !m.connecting || cmd == nil {
		t.Fatalf("expected connect to start, connecting=%v cmd nil=%v status=%q", m.connecting, cmd == nil, m.status)
	}
}

func keyBrowseIndexOf(entries []Entry, name string) int {
	for i, e := range entries {
		if e.Name == name || e.Name == name+"/" {
			return i
		}
	}
	return -1
}

func TestKeyBrowserNavigatesAndSelectsFile(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "keys")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(sub, "id_test")
	if err := os.WriteFile(keyFile, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(Config{})
	m.active = 1
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(Model)
	m.providerType = kindSCP
	m.providerField = providerFieldKey
	// Seed the browser to start in tmp by pointing the key path inside it.
	m.providerForm.keyPath = filepath.Join(tmp, "placeholder")

	// Enter on the Key file field opens the browser.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if !m.keyBrowse || m.keyBrowseDir != tmp {
		t.Fatalf("browser not opened in tmp: keyBrowse=%v dir=%q", m.keyBrowse, m.keyBrowseDir)
	}

	// Descend into the "keys" subdirectory.
	idx := keyBrowseIndexOf(m.keyBrowseEntries, "keys")
	if idx < 0 {
		t.Fatalf("keys dir not listed: %#v", m.keyBrowseEntries)
	}
	m.keyBrowseCursor = idx
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.keyBrowseDir != sub {
		t.Fatalf("did not descend into keys: %q", m.keyBrowseDir)
	}

	// Select the key file.
	idx = keyBrowseIndexOf(m.keyBrowseEntries, "id_test")
	if idx < 0 {
		t.Fatalf("id_test not listed: %#v", m.keyBrowseEntries)
	}
	m.keyBrowseCursor = idx
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.keyBrowse {
		t.Fatal("browser should close after selecting a file")
	}
	if m.providerForm.keyPath != keyFile {
		t.Fatalf("keyPath = %q, want %q", m.providerForm.keyPath, keyFile)
	}
	if m.providerField != providerFieldKey {
		t.Fatalf("focus did not return to key field: %d", m.providerField)
	}
}

func TestProviderModalCursorEditing(t *testing.T) {
	m := New(Config{})
	m.active = 1
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(Model)
	m.providerType = kindSCP
	m.providerField = providerFieldHost
	m.providerCursorToEnd()

	for _, r := range "abc" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	if m.providerForm.host != "abc" || m.providerCursor != 3 {
		t.Fatalf("after typing host=%q cursor=%d", m.providerForm.host, m.providerCursor)
	}

	// Move the cursor left twice and insert in the middle.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(Model)
	if m.providerCursor != 1 {
		t.Fatalf("cursor after two lefts = %d, want 1", m.providerCursor)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})
	m = updated.(Model)
	if m.providerForm.host != "aXbc" || m.providerCursor != 2 {
		t.Fatalf("mid-insert host=%q cursor=%d, want aXbc/2", m.providerForm.host, m.providerCursor)
	}

	// Backspace deletes the rune before the cursor.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = updated.(Model)
	if m.providerForm.host != "abc" || m.providerCursor != 1 {
		t.Fatalf("after backspace host=%q cursor=%d, want abc/1", m.providerForm.host, m.providerCursor)
	}

	// Delete removes the rune at the cursor.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDelete})
	m = updated.(Model)
	if m.providerForm.host != "ac" || m.providerCursor != 1 {
		t.Fatalf("after delete host=%q cursor=%d, want ac/1", m.providerForm.host, m.providerCursor)
	}

	// Home/End jump to the bounds.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m = updated.(Model)
	if m.providerCursor != 0 {
		t.Fatalf("home cursor = %d", m.providerCursor)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(Model)
	if m.providerCursor != 2 {
		t.Fatalf("end cursor = %d", m.providerCursor)
	}

	// Switching fields resets the cursor to the end of the new field.
	m.providerForm.user = "root"
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.providerField != providerFieldPort {
		t.Fatalf("down did not move to port field: %d", m.providerField)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.providerField != providerFieldUser || m.providerCursor != len("root") {
		t.Fatalf("field=%d cursor=%d, want user/%d", m.providerField, m.providerCursor, len("root"))
	}
}

func TestProviderSwitchClearsStaleEntries(t *testing.T) {
	m := New(Config{Container: "logs"})
	// Pane 0 is Azure with some cached entries and a selection.
	m.panels[0].entries = []Entry{{Name: "old.txt", Path: "old.txt", Size: 3}}
	m.panels[0].allEntries = m.panels[0].entries
	m.panels[0].selected["old.txt"] = m.panels[0].entries[0]
	m.active = 0

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(Model)
	m.providerType = kindLocal
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if len(m.panels[0].entries) != 0 || len(m.panels[0].allEntries) != 0 {
		t.Fatalf("entries not cleared after provider switch: %#v", m.panels[0].entries)
	}
	if len(m.panels[0].selected) != 0 {
		t.Fatalf("selection not cleared after provider switch: %#v", m.panels[0].selected)
	}
}

func TestScpConnectCancelInvalidatesInflight(t *testing.T) {
	m := New(Config{})
	m.active = 1
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = updated.(Model)
	m.providerType = kindSCP
	m.providerForm = providerForm{host: "h", port: "22", user: "u", pass: "p"}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil || !m.connecting {
		t.Fatalf("expected async connect, connecting=%v cmd nil=%v", m.connecting, cmd == nil)
	}
	staleGen := m.panels[1].gen

	// User cancels while connecting.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.connecting || m.providerModal {
		t.Fatal("cancel did not close the connecting modal")
	}

	// A late successful connection arrives carrying the stale generation; it
	// must be discarded rather than switching the pane to SCP.
	updated, _ = m.Update(providerConnectedMsg{
		index:    1,
		gen:      staleGen,
		provider: scpProvider{session: &scpSession{label: "scp u@h"}},
		root:     "/home/u",
		label:    "scp u@h",
	})
	m = updated.(Model)
	if _, ok := m.panels[1].provider.(scpProvider); ok {
		t.Fatal("stale connection was applied after cancel")
	}
}

// writerToReader is a reader that also implements io.WriterTo, standing in for a
// concurrent source such as *sftp.File so the progressReader fast path can be
// exercised without a live server.
type writerToReader struct {
	data     []byte
	wroteVia string // "writeto" if WriteTo was used, "read" otherwise
}

func (w *writerToReader) Read(b []byte) (int, error) {
	if len(w.data) == 0 {
		return 0, io.EOF
	}
	n := copy(b, w.data)
	w.data = w.data[n:]
	w.wroteVia = "read"
	return n, nil
}

func (w *writerToReader) WriteTo(dst io.Writer) (int64, error) {
	w.wroteVia = "writeto"
	n, err := dst.Write(w.data)
	w.data = w.data[n:]
	return int64(n), err
}

func TestProgressReaderUsesWriteToFastPath(t *testing.T) {
	payload := []byte("the quick brown fox jumps over the lazy dog")
	src := &writerToReader{data: append([]byte(nil), payload...)}
	var reported int64
	pr := &progressReader{ctx: context.Background(), r: src, report: func(done int64) { reported = done }}

	var dst strings.Builder
	n, err := io.Copy(&dst, pr) // io.Copy prefers src.WriteTo
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if src.wroteVia != "writeto" {
		t.Fatalf("fast path not used: source consumed via %q", src.wroteVia)
	}
	if n != int64(len(payload)) || dst.String() != string(payload) {
		t.Fatalf("copied %d bytes %q, want %d %q", n, dst.String(), len(payload), payload)
	}
	if reported != int64(len(payload)) {
		t.Fatalf("progress reported %d, want %d", reported, len(payload))
	}
}

// plainReader implements only Read (no WriteTo), forcing the fallback path.
type plainReader struct{ data []byte }

func (p *plainReader) Read(b []byte) (int, error) {
	if len(p.data) == 0 {
		return 0, io.EOF
	}
	n := copy(b, p.data)
	p.data = p.data[n:]
	return n, nil
}

func TestProgressReaderFallbackCountsBytes(t *testing.T) {
	payload := []byte("hello fallback world")
	pr := &progressReader{ctx: context.Background(), r: &plainReader{data: append([]byte(nil), payload...)}}
	var reported int64
	pr.report = func(done int64) { reported = done }

	var dst strings.Builder
	n, err := io.Copy(&dst, pr)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != int64(len(payload)) || dst.String() != string(payload) {
		t.Fatalf("copied %d %q, want %d %q", n, dst.String(), len(payload), payload)
	}
	if reported != int64(len(payload)) {
		t.Fatalf("progress reported %d, want %d", reported, len(payload))
	}
}
