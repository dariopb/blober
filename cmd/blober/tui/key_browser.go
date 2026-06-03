package tui

import (
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

const keyBrowseVisibleRows = 12

// openKeyBrowser starts the private-key file picker for the SCP Key file field.
// It begins in the directory of the current key path if valid, otherwise in
// ~/.ssh, the home directory, or the working directory.
func (m *Model) openKeyBrowser() {
	dir := ""
	if kp := strings.TrimSpace(m.providerForm.keyPath); kp != "" {
		candidate := filepath.Dir(kp)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			dir = candidate
		}
	}
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			sshDir := filepath.Join(home, ".ssh")
			if info, err := os.Stat(sshDir); err == nil && info.IsDir() {
				dir = sshDir
			} else {
				dir = home
			}
		}
	}
	if dir == "" {
		if wd, err := os.Getwd(); err == nil && wd != "" {
			dir = wd
		} else {
			dir = "."
		}
	}
	m.keyBrowse = true
	m.keyBrowseCursor = 0
	m.keyBrowseOffset = 0
	m.loadKeyBrowser(dir)
}

func (m *Model) loadKeyBrowser(dir string) {
	entries, err := listLocal(dir)
	m.keyBrowseDir = dir
	m.keyBrowseEntries = entries
	m.keyBrowseErr = err
	if m.keyBrowseCursor >= len(entries) {
		m.keyBrowseCursor = max(0, len(entries)-1)
	}
	m.ensureKeyBrowseVisible()
}

func (m *Model) currentKeyBrowseEntry() (Entry, bool) {
	if m.keyBrowseCursor < 0 || m.keyBrowseCursor >= len(m.keyBrowseEntries) {
		return Entry{}, false
	}
	return m.keyBrowseEntries[m.keyBrowseCursor], true
}

func (m *Model) ensureKeyBrowseVisible() {
	if m.keyBrowseCursor < m.keyBrowseOffset {
		m.keyBrowseOffset = m.keyBrowseCursor
	}
	if m.keyBrowseCursor >= m.keyBrowseOffset+keyBrowseVisibleRows {
		m.keyBrowseOffset = m.keyBrowseCursor - keyBrowseVisibleRows + 1
	}
	if m.keyBrowseOffset < 0 {
		m.keyBrowseOffset = 0
	}
}

func (m Model) updateKeyBrowser(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.keyBrowse = false
	case "up", "k":
		if m.keyBrowseCursor > 0 {
			m.keyBrowseCursor--
		}
		m.ensureKeyBrowseVisible()
	case "down", "j":
		if m.keyBrowseCursor < len(m.keyBrowseEntries)-1 {
			m.keyBrowseCursor++
		}
		m.ensureKeyBrowseVisible()
	case "pgup":
		m.keyBrowseCursor = max(0, m.keyBrowseCursor-keyBrowseVisibleRows)
		m.ensureKeyBrowseVisible()
	case "pgdown":
		m.keyBrowseCursor = min(len(m.keyBrowseEntries)-1, m.keyBrowseCursor+keyBrowseVisibleRows)
		m.ensureKeyBrowseVisible()
	case "home":
		m.keyBrowseCursor = 0
		m.ensureKeyBrowseVisible()
	case "end":
		m.keyBrowseCursor = max(0, len(m.keyBrowseEntries)-1)
		m.ensureKeyBrowseVisible()
	case "left", "h", "backspace":
		parent := filepath.Dir(m.keyBrowseDir)
		if parent != m.keyBrowseDir {
			m.keyBrowseCursor = 0
			m.keyBrowseOffset = 0
			m.loadKeyBrowser(parent)
		}
	case "right", "l", "enter":
		entry, ok := m.currentKeyBrowseEntry()
		if !ok {
			return m, nil
		}
		if entry.IsDir {
			m.keyBrowseCursor = 0
			m.keyBrowseOffset = 0
			m.loadKeyBrowser(entry.Path)
		} else {
			info, err := os.Stat(entry.Path)
			if err != nil {
				m.status = "cannot use key: " + err.Error()
				return m, nil
			}
			if !info.Mode().IsRegular() {
				m.status = "not a regular file: " + entry.Path
				return m, nil
			}
			m.providerForm.keyPath = entry.Path
			m.providerField = providerFieldKey
			m.providerCursor = len([]rune(entry.Path))
			m.keyBrowse = false
			m.status = "selected key " + entry.Path
		}
	}
	return m, nil
}

func (m Model) renderKeyBrowser() string {
	const width = 60
	contentWidth := width - modalStyle.GetHorizontalFrameSize()
	lines := []string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render("Select private key"),
		fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(m.keyBrowseDir, contentWidth)),
	}
	if m.keyBrowseErr != nil {
		lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(m.keyBrowseErr.Error(), contentWidth)))
	} else if len(m.keyBrowseEntries) == 0 {
		lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render("(empty directory)"))
	} else {
		end := min(len(m.keyBrowseEntries), m.keyBrowseOffset+keyBrowseVisibleRows)
		for i := m.keyBrowseOffset; i < end; i++ {
			entry := m.keyBrowseEntries[i]
			name := displayEntryName(entry)
			if entry.IsDir && !entry.parent {
				name = name + "/"
			}
			style := modalRowStyle
			prefix := "  "
			if i == m.keyBrowseCursor {
				style = selectedDialogButtonStyle
				prefix = "> "
			}
			lines = append(lines, fullWidthStyle(style, contentWidth).Render(truncate(prefix+name, contentWidth)))
		}
	}
	lines = append(lines,
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("Enter/→ open or select  ←/Bksp up  Esc cancel", contentWidth)),
	)
	return m.frameKeyBrowser(width, lines)
}

func (m Model) frameKeyBrowser(width int, lines []string) string {
	height := len(lines) + modalStyle.GetVerticalFrameSize() + modalStyle.GetVerticalPadding()
	return modalStyle.
		Width(width - modalStyle.GetHorizontalBorderSize()).
		Height(height - modalStyle.GetVerticalBorderSize()).
		MaxWidth(width).
		MaxHeight(height).
		Render(strings.Join(lines, "\n"))
}
