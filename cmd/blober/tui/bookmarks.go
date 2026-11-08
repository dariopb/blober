package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// Keep this schema separate from providerForm: passwords, tokens and credential
// objects must never be serialized into the bookmark file.
type bookmark struct {
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	Location     string `json:"location"`
	URL          string `json:"url,omitempty"`
	Host         string `json:"host,omitempty"`
	Port         string `json:"port,omitempty"`
	User         string `json:"user,omitempty"`
	KeyFile      string `json:"key_file,omitempty"`
	Account      string `json:"account,omitempty"`
	Subscription string `json:"subscription,omitempty"`
	Tenant       string `json:"tenant,omitempty"`
	Container    string `json:"container,omitempty"`
}

func bookmarkKind(name string) (providerKind, error) {
	switch name {
	case "local":
		return kindLocal, nil
	case "azure":
		return kindAzure, nil
	case "scp":
		return kindSCP, nil
	case "http":
		return kindHTTP, nil
	case "webdav":
		return kindWebDAV, nil
	}
	return 0, fmt.Errorf("unknown bookmark provider %q", name)
}

func validateBookmarks(items []bookmark) error {
	names := map[string]bool{}
	for _, b := range items {
		if strings.TrimSpace(b.Name) == "" || names[b.Name] {
			return errors.New("bookmark names must be non-empty and unique")
		}
		names[b.Name] = true
		kind, err := bookmarkKind(b.Provider)
		if err != nil {
			return err
		}
		for _, value := range []string{b.Name, b.Location, b.URL, b.Host, b.Port, b.User, b.KeyFile, b.Account, b.Subscription, b.Tenant, b.Container} {
			if strings.ContainsFunc(value, unicode.IsControl) {
				return errors.New("bookmark fields must not contain control characters")
			}
		}
		if kind == kindHTTP || kind == kindWebDAV {
			for _, value := range []string{b.URL, b.Location} {
				u, err := url.Parse(value)
				if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
					return errors.New("bookmark requires an HTTP(S) URL")
				}
				if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
					return errors.New("URLs containing credentials, queries or fragments cannot be bookmarked")
				}
			}
			if kind == kindAzure {
				if b.Container == "" {
					return errors.New("select an Azure container before saving a bookmark")
				}
				if _, err := resolveLocation(azureProvider{}, "", b.Location); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func loadBookmarks(file string) ([]bookmark, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []bookmark
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("read bookmarks: %w", err)
	}
	if err := validateBookmarks(items); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func writeBookmarks(file string, items []bookmark) error {
	if err := validateBookmarks(items); err != nil {
		return err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(file), ".bookmarks-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, file)
}

func (m *Model) bookmarkFile() (string, error) {
	if m.bookmarksPath != "" {
		return m.bookmarksPath, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "blober", "bookmarks.json"), nil
}

func (m Model) currentBookmark(name string) (bookmark, error) {
	p := m.panels[m.active]
	b := bookmark{Name: strings.TrimSpace(name), Location: p.location}
	switch prov := m.resolveProvider(p).(type) {
	case localProvider:
		b.Provider = "local"
	case azureProvider:
		b.Provider, b.Account, b.Container = "azure", prov.accountName, prov.container
		b.Subscription, b.Tenant = p.connection.subscription, p.connection.tenant
		if b.Subscription == "" {
			b.Subscription, b.Tenant = m.azureDefaults.subscription, m.azureDefaults.tenant
		}
	case scpProvider:
		if prov.session == nil {
			return b, errors.New("SCP is not connected")
		}
		cfg := prov.session.config
		b.Provider, b.Host, b.Port, b.User, b.KeyFile = "scp", cfg.host, cfg.port, cfg.user, cfg.keyPath
	case httpProvider:
		b.Provider, b.URL, b.User = "http", prov.base.String(), prov.user
	case webdavProvider:
		b.Provider, b.URL, b.User = "webdav", prov.base.String(), prov.user
	default:
		return b, errors.New("provider cannot be bookmarked")
	}
	return b, validateBookmarks([]bookmark{b})
}

func (m *Model) saveCurrentBookmark(name string) error {
	b, err := m.currentBookmark(name)
	if err != nil {
		return err
	}
	file, err := m.bookmarkFile()
	if err != nil {
		return err
	}
	items, err := loadBookmarks(file)
	if err != nil {
		return err
	}
	for _, existing := range items {
		if existing.Name == b.Name {
			return errors.New("bookmark name already exists; choose another name or delete it first")
		}
	}
	items = append(items, b)
	if err := writeBookmarks(file, items); err != nil {
		return err
	}
	m.status = "saved bookmark " + b.Name
	return nil
}

func (m *Model) openBookmarks() {
	file, err := m.bookmarkFile()
	if err == nil {
		m.bookmarks, err = loadBookmarks(file)
	}
	if err != nil {
		m.status = err.Error()
		return
	}
	m.showingBookmarks = true
	m.bookmarkCursor, m.bookmarkButton = 0, -1
}

func (m Model) updateBookmarks(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c", "b":
		m.showingBookmarks = false
	case "up", "k":
		if m.bookmarkButton >= 0 {
			m.bookmarkButton = -1
		} else {
			m.bookmarkCursor = max(0, m.bookmarkCursor-1)
		}
	case "down", "j":
		if m.bookmarkCursor < len(m.bookmarks)-1 && m.bookmarkButton < 0 {
			m.bookmarkCursor++
		} else {
			m.bookmarkButton = 0
		}
	case "tab":
		m.bookmarkButton = (m.bookmarkButton + 1) % 3
	case "left":
		if m.bookmarkButton >= 0 {
			m.bookmarkButton = (m.bookmarkButton + 2) % 3
		}
	case "right":
		if m.bookmarkButton >= 0 {
			m.bookmarkButton = (m.bookmarkButton + 1) % 3
		}
	case "enter":
		switch m.bookmarkButton {
		case 1:
			return m.deleteBookmark()
		case 2:
			m.showingBookmarks = false
		default:
			return m.openBookmark()
		}
	case "delete":
		return m.deleteBookmark()
	}
	return m, nil
}

func (m Model) deleteBookmark() (tea.Model, tea.Cmd) {
	if m.bookmarkCursor < 0 || m.bookmarkCursor >= len(m.bookmarks) {
		return m, nil
	}
	file, err := m.bookmarkFile()
	if err != nil {
		m.status = err.Error()
		return m, nil
	}
	// Reload before changing the file so another session's additions survive.
	items, err := loadBookmarks(file)
	if err != nil {
		m.status = err.Error()
		return m, nil
	}
	name := m.bookmarks[m.bookmarkCursor].Name
	kept := make([]bookmark, 0, len(items))
	for _, b := range items {
		if b.Name != name {
			kept = append(kept, b)
		}
	}
	if err := writeBookmarks(file, kept); err != nil {
		m.status = err.Error()
		return m, nil
	}
	m.bookmarks = kept
	m.bookmarkCursor = min(m.bookmarkCursor, max(0, len(kept)-1))
	m.status = "deleted bookmark " + name
	return m, nil
}

func (m Model) openBookmark() (tea.Model, tea.Cmd) {
	if m.bookmarkCursor < 0 || m.bookmarkCursor >= len(m.bookmarks) {
		return m, nil
	}
	b := m.bookmarks[m.bookmarkCursor]
	kind, err := bookmarkKind(b.Provider)
	if err != nil {
		m.status = err.Error()
		return m, nil
	}
	m.showingBookmarks = false
	if kind == kindLocal {
		if _, ok := m.resolveProvider(m.panels[m.active]).(localProvider); ok {
			m.openActionInput(inputLocation, b.Location)
			return m.goToLocation(b.Location)
		}
		// Validate the local directory before replacing a live remote session.
		m.openProviderModal()
		m.providerType = kindLocal
		m.providerForm = providerForm{location: b.Location}
		m.connecting = true
		index := m.active
		m.panels[index].gen++
		gen := m.panels[index].gen
		return m, func() tea.Msg {
			_, err := listLocal(b.Location)
			return providerConnectedMsg{index: index, gen: gen, provider: localProvider{}, root: b.Location, label: "Local", err: err}
		}
	}
	m.openProviderModal()
	m.providerType = kind
	m.providerForm = providerForm{
		url: b.URL, host: b.Host, port: b.Port, user: b.User, keyPath: b.KeyFile,
		account: b.Account, subscription: b.Subscription, tenant: b.Tenant,
		location: b.Location, container: b.Container,
	}
	m.status = "review saved connection; enter password if needed, then Connect"
	return m, nil
}

func (m Model) renderBookmarks() string {
	width := 68
	if m.width > 0 {
		width = min(width, max(12, m.width-4))
	}
	contentWidth := max(1, width-modalStyle.GetHorizontalFrameSize())
	lines := []string{fullWidthStyle(modalTitleStyle, contentWidth).Render("Bookmarks / saved connections")}
	rows := 10
	if m.height > 0 {
		rows = max(1, min(rows, m.height-10))
	}
	start := max(0, m.bookmarkCursor-rows+1)
	if len(m.bookmarks) == 0 {
		lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("No bookmarks. Ctrl+B saves the current connection and path.", contentWidth)))
	}
	for i := start; i < min(len(m.bookmarks), start+rows); i++ {
		b := m.bookmarks[i]
		style := modalRowStyle
		if i == m.bookmarkCursor && m.bookmarkButton < 0 {
			style = selectedDialogButtonStyle
		}
		lines = append(lines, fullWidthStyle(style, contentWidth).Render(truncate(b.Name+" ["+b.Provider+"] "+b.Location, contentWidth)))
	}
	buttons := make([]string, 0, 3)
	for i, label := range []string{"Open", "Delete", "Close"} {
		buttons = append(buttons, renderDialogButton(label, m.bookmarkButton == i || (m.bookmarkButton < 0 && i == 0)))
	}
	lines = append(lines,
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		fullWidthStyle(modalRowStyle, contentWidth).Render(strings.Join(buttons, " ")),
	)
	return m.frameProviderModal(width, lines)
}

func connectBookmarkWebCmd(connect tea.Cmd, location string) tea.Cmd {
	return func() tea.Msg {
		msg := connect().(providerConnectedMsg)
		if msg.err != nil {
			return msg
		}
		root, err := resolveLocation(msg.provider, msg.root, location)
		if err == nil {
			_, err = msg.provider.List(context.Background(), root)
		}
		if err != nil {
			_ = msg.provider.Close()
			msg.provider, msg.err = nil, err
			return msg
		}
		msg.root = root
		return msg
	}
}
