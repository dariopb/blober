package tui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestBookmarksPersistWithoutSecretsAndReconnect(t *testing.T) {
	m := New(Config{})
	m.bookmarksPath = filepath.Join(t.TempDir(), "config", "bookmarks.json")
	prov, root, err := connectWeb(kindWebDAV, "https://alice:url-password@example.test/files/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m.panels[0].provider, m.panels[0].location = prov, root+"nested/"
	m.panels[0].connection = providerForm{url: "https://alice:url-password@example.test/files/", pass: "modal-password"}
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyCtrlB})
	m.input.value = "work"
	m, _ = actionKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input != nil || m.status != "saved bookmark work" {
		t.Fatalf("save failed: input=%v status=%q", m.input, m.status)
	}
	data, err := os.ReadFile(m.bookmarksPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"url-password", "modal-password", "password", "token"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("bookmark persisted secret %q", secret)
		}
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(m.bookmarksPath)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("bookmark permissions = %o", info.Mode().Perm())
		}
	}
	restored := New(Config{})
	restored.bookmarksPath = m.bookmarksPath
	restored, _ = actionKey(restored, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if !restored.showingBookmarks || len(restored.bookmarks) != 1 {
		t.Fatal("fresh model failed to load saved bookmark")
	}
	restored, _ = actionKey(restored, tea.KeyMsg{Type: tea.KeyEnter})
	if !restored.providerModal || restored.providerType != kindWebDAV || restored.providerForm.pass != "" {
		t.Fatal("remote bookmarks must reopen the connection form without a password")
	}
	if restored.providerForm.url != root || restored.providerForm.location != root+"nested/" || restored.providerForm.user != "alice" {
		t.Fatalf("connection was not restored: %+v", restored.providerForm)
	}
}

func TestBookmarksKeepSCPAndAzureConnectionDetails(t *testing.T) {
	m := New(Config{SubscriptionID: "sub", TenantID: "tenant"})
	m.panels[0].provider = scpProvider{session: &scpSession{config: scpConfig{
		host: "server", port: "2222", user: "alice", pass: "never-save", keyPath: "/keys/id_ed25519",
	}}}
	m.panels[0].location = "/data"
	scp, err := m.currentBookmark("ssh")
	if err != nil {
		t.Fatal(err)
	}
	m.panels[0].provider = azureProvider{accountName: "account", container: "container"}
	m.panels[0].location = "logs/2026/"
	azure, err := m.currentBookmark("azure")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []bookmark{scp, azure} {
		m.bookmarks, m.bookmarkCursor = []bookmark{b}, 0
		updated, _ := m.openBookmark()
		m = updated.(Model)
		if b.Provider == "scp" {
			if m.providerForm.host != "server" || m.providerForm.port != "2222" || m.providerForm.keyPath != "/keys/id_ed25519" || m.providerForm.location != "/data" {
				t.Fatalf("SCP form = %+v", m.providerForm)
			}
		} else if m.providerForm.account != "account" || m.providerForm.container != "container" ||
			m.providerForm.subscription != "sub" || m.providerForm.tenant != "tenant" || m.providerForm.location != "logs/2026/" {
			t.Fatalf("Azure form = %+v", m.providerForm)
		}
		if m.providerForm.pass != "" {
			t.Fatal("bookmark restored a password")
		}
	}
}

func TestBookmarkDuplicatesCorruptionAndDeletion(t *testing.T) {
	m := New(Config{LocalPath: t.TempDir()})
	m.bookmarksPath = filepath.Join(t.TempDir(), "bookmarks.json")
	if err := m.saveCurrentBookmark("first"); err != nil {
		t.Fatal(err)
	}
	if err := m.saveCurrentBookmark("first"); err == nil {
		t.Fatal("duplicate names must not silently overwrite bookmarks")
	}
	m.openBookmarks()
	// Simulate an addition by another session after opening the picker.
	other := m
	if err := other.saveCurrentBookmark("second"); err != nil {
		t.Fatal(err)
	}
	updated, _ := m.deleteBookmark()
	m = updated.(Model)
	items, err := loadBookmarks(m.bookmarksPath)
	if err != nil || len(items) != 1 || items[0].Name != "second" {
		t.Fatalf("deletion lost another session's bookmark: %+v, %v", items, err)
	}
	if err := os.WriteFile(m.bookmarksPath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.saveCurrentBookmark("third"); err == nil {
		t.Fatal("save should not overwrite a corrupt file")
	}
	data, _ := os.ReadFile(m.bookmarksPath)
	if string(data) != "{broken" {
		t.Fatal("corrupt bookmark file was overwritten")
	}
}

func TestLocalBookmarkOpensSavedDirectory(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	m := New(Config{LocalPath: root})
	m.bookmarks = []bookmark{{Name: "target", Provider: "local", Location: target}}
	updated, cmd := m.openBookmark()
	m = updated.(Model)
	updated, _ = m.Update(cmd())
	m = updated.(Model)
	if m.panels[0].location != target || m.input != nil {
		t.Fatal("local bookmark did not navigate to the saved directory")
	}
}

func TestBookmarkRejectsCredentialBearingURLs(t *testing.T) {
	for _, raw := range []string{
		"https://user:password@example.test/root/",
		"https://example.test/root/?sig=secret",
		"https://example.test/root/#secret",
	} {
		items := []bookmark{{Name: "unsafe", Provider: "http", URL: raw, Location: raw}}
		if err := writeBookmarks(filepath.Join(t.TempDir(), "bookmarks.json"), items); err == nil {
			t.Errorf("saved a credential-bearing URL: %q", raw)
		}
	}
}

func TestWebBookmarkConnectionValidatesSavedPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files/" || r.URL.Path == "/files/nested/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	for _, tc := range []struct {
		location string
		fail     bool
	}{
		{server.URL + "/files/nested/", false},
		{server.URL + "/files/missing/", true},
		{"https://other.test/files/", true},
	} {
		cmd := connectBookmarkWebCmd(connectWebCmd(0, 1, kindHTTP, server.URL+"/files/", "", ""), tc.location)
		msg := cmd().(providerConnectedMsg)
		if (msg.err != nil) != tc.fail {
			t.Errorf("connect bookmark %q: %v", tc.location, msg.err)
		}
		if !tc.fail && msg.root != tc.location {
			t.Errorf("restored root = %q, want %q", msg.root, tc.location)
		}
	}
}

func TestMoveToHTTPRemovesSourceOnlyAfterSuccessfulUpload(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			var content string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					http.NotFound(w, r)
					return
				}
				if r.Method != http.MethodPut {
					t.Errorf("unexpected method %s", r.Method)
				}
				if fail {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				buf := new(strings.Builder)
				if _, err := io.Copy(buf, r.Body); err != nil {
					t.Error(err)
				}
				content = buf.String()
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
			root := t.TempDir()
			file := filepath.Join(root, "file")
			if err := os.WriteFile(file, []byte("upload"), 0o600); err != nil {
				t.Fatal(err)
			}
			m := New(Config{LocalPath: root})
			prov, location, err := connectWeb(kindHTTP, server.URL+"/files/", "", "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.moveEntries(context.Background(), m.panels[0], panel{provider: prov, location: location},
				[]Entry{{Name: "file", Path: file, Size: 6}}, "", make(chan tea.Msg, 32))
			if (err != nil) != fail {
				t.Fatalf("move error = %v, fail=%v", err, fail)
			}
			_, statErr := os.Stat(file)
			if fail && statErr != nil {
				t.Fatal("failed HTTP move lost its source")
			}
			if !fail && (!errors.Is(statErr, os.ErrNotExist) || content != "upload") {
				t.Fatalf("HTTP move did not complete: content=%q, source=%v", content, statErr)
			}
		})
	}
}
