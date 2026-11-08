package tui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

type inputKind int

const (
	inputLocation inputKind = iota
	inputRename
	inputBookmark
)

type actionInput struct {
	kind   inputKind
	value  string
	cursor int
	button int
	err    string
	busy   bool
	cancel context.CancelFunc
	entry  Entry
}

type locationLoadedMsg struct {
	index    int
	gen      uint64
	request  uint64
	location string
	entries  []Entry
	err      error
}

func (m *Model) openActionInput(kind inputKind, value string) {
	m.inputGen++
	m.input = &actionInput{kind: kind, value: value, cursor: len([]rune(value)), button: -1}
}

func (m *Model) openRename() {
	entry, ok := m.panels[m.active].current()
	if !ok || entry.parent {
		m.status = "select a file or directory to rename"
		return
	}
	m.openActionInput(inputRename, copyTargetName(entry))
	m.input.entry = entry
}

func (m Model) updateActionInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Copy the input so Bubble Tea model values do not mutate each other.
	draft := *m.input
	m.input = &draft
	in := m.input
	if msg.String() == "esc" || msg.String() == "ctrl+c" {
		if in.cancel != nil {
			in.cancel()
		}
		m.input = nil
		m.inputGen++
		m.status = "cancelled"
		return m, nil
	}
	if in.busy {
		return m, nil
	}
	runes := []rune(in.value)
	switch msg.String() {
	case "tab", "down":
		in.button = (in.button + 1) % 2
	case "shift+tab", "up":
		in.button = -1
	case "left":
		if in.button >= 0 {
			in.button = 1 - in.button
		} else {
			in.cursor = max(0, in.cursor-1)
		}
	case "right":
		if in.button >= 0 {
			in.button = 1 - in.button
		} else {
			in.cursor = min(len(runes), in.cursor+1)
		}
	case "home":
		in.cursor = 0
	case "end":
		in.cursor = len(runes)
	case "ctrl+u":
		in.value, in.cursor = "", 0
		in.button = -1
	case "backspace", "ctrl+h":
		if in.button < 0 && in.cursor > 0 {
			in.value = string(append(runes[:in.cursor-1], runes[in.cursor:]...))
			in.cursor--
		}
	case "delete":
		if in.button < 0 && in.cursor < len(runes) {
			in.value = string(append(runes[:in.cursor], runes[in.cursor+1:]...))
		}
	case "enter":
		if in.button == 1 {
			m.input = nil
			return m, nil
		}
		switch in.kind {
		case inputLocation:
			return m.goToLocation(in.value)
		case inputRename:
			name := strings.TrimSpace(in.value)
			if err := validateNewDirName(name); err != nil {
				in.err = "enter a single non-empty name without path separators"
				return m, nil
			}
			return m.beginRename(in.entry, name)
		case inputBookmark:
			if err := m.saveCurrentBookmark(in.value); err != nil {
				in.err = err.Error()
				return m, nil
			}
			m.input = nil
		}
	default:
		if in.button < 0 && len(msg.Runes) > 0 {
			text := strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return -1
				}
				return r
			}, string(msg.Runes))
			out := append([]rune{}, runes[:in.cursor]...)
			out = append(out, []rune(text)...)
			out = append(out, runes[in.cursor:]...)
			in.value = string(out)
			in.cursor += len([]rune(text))
		}
	}
	return m, nil
}

func (m Model) renderActionInput() string {
	in := m.input
	title, action, hint := "Go to directory / prefix", "Go", "Relative paths use the current directory; Ctrl+U clears"
	switch in.kind {
	case inputRename:
		title, action, hint = "Rename", "Rename", "Existing destinations are never overwritten"
	case inputBookmark:
		title, action, hint = "Save named bookmark", "Save", "Connection and path only; passwords/tokens are not saved"
	}
	width := 68
	if m.width > 0 {
		width = min(width, max(12, m.width-4))
	}
	contentWidth := max(1, width-modalStyle.GetHorizontalFrameSize())
	lines := []string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render(truncate(title, contentWidth)),
	}
	if in.button < 0 && !in.busy {
		lines = append(lines, renderValueWithCursor(in.value, in.cursor, contentWidth))
	} else {
		lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(in.value, contentWidth)))
	}
	if in.busy {
		hint = "Loading... Esc cancels"
	}
	lines = append(lines,
		fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(hint, contentWidth)),
		fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(in.err, contentWidth)),
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		renderDialogButtons(contentWidth, in.button != 1, action, "Cancel"),
	)
	return m.frameProviderModal(width, lines)
}

func resolveLocation(prov Provider, current, value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("invalid path")
	}
	switch p := prov.(type) {
	case localProvider:
		if value == "" {
			return "", errors.New("directory is required")
		}
		if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			value = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(value[1:], "/"), `\`))
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(current, value)
		}
		return filepath.Abs(value)
	case scpProvider:
		if value == "" {
			return "", errors.New("directory is required")
		}
		if !path.IsAbs(value) {
			value = path.Join(current, value)
		}
		return path.Clean(value), nil
	case azureProvider:
		value = strings.Trim(value, "/")
		if value == "" {
			return "", nil
		}
		if err := safeRel(value); err != nil {
			return "", err
		}
		return value + "/", nil
	case httpProvider:
		return resolveWebLocation(p.webBase, current, value)
	case webdavProvider:
		return resolveWebLocation(p.webBase, current, value)
	default:
		return "", errors.New("provider does not support direct path entry")
	}
}

func resolveWebLocation(b webBase, current, value string) (string, error) {
	base, err := url.Parse(current)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(value)
	if err != nil {
		return "", errors.New("invalid directory URL")
	}
	u := base.ResolveReference(ref)
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", errors.New("directory URLs must not contain credentials, queries or fragments")
	}
	u.Path = path.Clean(u.Path) + "/"
	if u.Path == "//" {
		u.Path = "/"
	}
	u.RawPath = ""
	if b.base == nil || !sameOrigin(u, b.base) || !strings.HasPrefix(u.Path, b.base.Path) {
		return "", errors.New("directory must be within this connection's root URL; use p for a new connection")
	}
	return u.String(), nil
}

func (m Model) goToLocation(value string) (tea.Model, tea.Cmd) {
	index := m.active
	prov := m.resolveProvider(m.panels[index])
	location, err := resolveLocation(prov, m.panels[index].location, value)
	if err != nil {
		m.input.err = err.Error()
		return m, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.input.busy, m.input.cancel, m.input.err = true, cancel, ""
	m.panels[index].gen++
	gen, request := m.panels[index].gen, m.inputGen
	return m, func() tea.Msg {
		defer cancel()
		entries, err := prov.List(ctx, location)
		return locationLoadedMsg{index: index, gen: gen, request: request, location: location, entries: entries, err: err}
	}
}

func (m Model) applyLocationLoaded(msg locationLoadedMsg) (tea.Model, tea.Cmd) {
	if m.input == nil || !m.input.busy || msg.request != m.inputGen ||
		msg.index < 0 || msg.index >= len(m.panels) || msg.gen != m.panels[msg.index].gen {
		return m, nil
	}
	in := *m.input
	m.input = &in
	in.busy, in.cancel = false, nil
	if msg.err != nil {
		in.err = msg.err.Error()
		return m, nil
	}
	m.rememberSelection(msg.index)
	p := &m.panels[msg.index]
	p.location = msg.location
	p.cursor, p.offset = 0, 0
	p.filter = ""
	p.selected = map[string]Entry{}
	m.pendingSelectSet[msg.index] = false
	p.setEntries(msg.entries, nil)
	if remembered, ok := m.rememberedSelect[msg.index][msg.location]; ok {
		m.selectEntry(msg.index, remembered)
	}
	m.clampPanelViewport(msg.index)
	m.input = nil
	m.status = fmt.Sprintf("opened %s", msg.location)
	return m, nil
}
