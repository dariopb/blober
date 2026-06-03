package tui

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// providerField identifies the editable rows of the provider modal.
const (
	providerFieldType = iota
	providerFieldHost
	providerFieldPort
	providerFieldUser
	providerFieldPass
	providerFieldKey
)

func providerTypeName(kind PanelKind) string {
	switch kind {
	case LocalPanel:
		return "Local"
	case RemotePanel:
		return "Azure"
	case SCPPanel:
		return "SCP (ssh)"
	default:
		return "Local"
	}
}

// providerFieldCount returns how many editable rows the modal shows for the
// currently selected provider type (only SCP needs the connection fields).
func (m Model) providerFieldCount() int {
	if m.providerType == SCPPanel {
		return providerFieldKey + 1
	}
	return 1
}

func (m *Model) openProviderModal() {
	m.providerModal = true
	m.providerModalPanel = m.active
	m.providerField = providerFieldType
	m.providerButton = -1
	m.providerType = m.panels[m.active].kind
	m.providerForm = scpConfig{port: "22"}
	if m.panels[m.active].scp != nil {
		// Pre-fill nothing sensitive; just keep defaults.
		m.providerType = SCPPanel
	}
	m.providerCursor = 0
	m.keyBrowse = false
	m.status = "select provider for active pane"
}

func (m Model) updateProviderModal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.keyBrowse {
		return m.updateKeyBrowser(msg)
	}
	if m.connecting {
		if msg.String() == "esc" || msg.String() == "ctrl+c" {
			m.connecting = false
			m.providerModal = false
			// Invalidate the in-flight connection so a late success is
			// discarded (and its session closed) instead of switching the pane.
			if m.providerModalPanel >= 0 && m.providerModalPanel < len(m.panels) {
				m.panels[m.providerModalPanel].gen++
			}
			m.status = "connection cancelled"
		}
		return m, nil
	}
	if msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH {
		m.deleteProviderRuneBefore()
		return m, nil
	}
	if msg.Type == tea.KeyDelete {
		m.deleteProviderRuneAt()
		return m, nil
	}
	switch msg.String() {
	case "esc", "ctrl+c":
		m.providerModal = false
		m.status = "provider selection cancelled"
	case "up":
		if m.providerButton >= 0 {
			m.providerButton = -1
			m.providerField = m.providerFieldCount() - 1
		} else if m.providerField > 0 {
			m.providerField--
		}
		m.providerCursorToEnd()
	case "down", "tab":
		if m.providerButton >= 0 {
			return m, nil
		}
		if m.providerField < m.providerFieldCount()-1 {
			m.providerField++
		} else {
			m.providerButton = 0
		}
		m.providerCursorToEnd()
	case "left":
		if m.providerButton >= 0 {
			m.providerButton = (m.providerButton + 1) % 2
		} else if m.providerField == providerFieldType {
			m.cycleProviderType(-1)
		} else {
			m.moveProviderCursor(-1)
		}
	case "right":
		if m.providerButton >= 0 {
			m.providerButton = (m.providerButton + 1) % 2
		} else if m.providerField == providerFieldType {
			m.cycleProviderType(1)
		} else {
			m.moveProviderCursor(1)
		}
	case "home":
		if m.providerButton < 0 && m.providerField != providerFieldType {
			m.providerCursor = 0
		}
	case "end":
		if m.providerButton < 0 && m.providerField != providerFieldType {
			m.providerCursorToEnd()
		}
	case "enter":
		if m.providerButton < 0 && m.providerField == providerFieldKey {
			m.openKeyBrowser()
			return m, nil
		}
		if m.providerButton == 1 {
			m.providerModal = false
			m.status = "provider selection cancelled"
			return m, nil
		}
		return m.applyProvider()
	default:
		if len(msg.Runes) > 0 && m.providerButton < 0 && m.providerField != providerFieldType {
			m.insertProviderRunes(msg.Runes)
		}
	}
	return m, nil
}

func (m *Model) cycleProviderType(delta int) {
	order := []PanelKind{LocalPanel, RemotePanel, SCPPanel}
	idx := 0
	for i, k := range order {
		if k == m.providerType {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(order)) % len(order)
	m.providerType = order[idx]
	if m.providerField >= m.providerFieldCount() {
		m.providerField = m.providerFieldCount() - 1
	}
	m.providerCursorToEnd()
}

// providerFieldPtr returns a pointer to the string backing the active editable
// text field, or nil when the active row is not an editable text field (the
// Type selector or a focused button).
func (m *Model) providerFieldPtr() *string {
	if m.providerButton >= 0 {
		return nil
	}
	switch m.providerField {
	case providerFieldHost:
		return &m.providerForm.host
	case providerFieldPort:
		return &m.providerForm.port
	case providerFieldUser:
		return &m.providerForm.user
	case providerFieldPass:
		return &m.providerForm.pass
	case providerFieldKey:
		return &m.providerForm.keyPath
	}
	return nil
}

func (m *Model) providerCursorToEnd() {
	if p := m.providerFieldPtr(); p != nil {
		m.providerCursor = len([]rune(*p))
	} else {
		m.providerCursor = 0
	}
}

func (m *Model) moveProviderCursor(delta int) {
	p := m.providerFieldPtr()
	if p == nil {
		return
	}
	n := len([]rune(*p))
	m.providerCursor += delta
	if m.providerCursor < 0 {
		m.providerCursor = 0
	}
	if m.providerCursor > n {
		m.providerCursor = n
	}
}

func (m *Model) insertProviderRunes(runes []rune) {
	p := m.providerFieldPtr()
	if p == nil {
		return
	}
	r := []rune(*p)
	if m.providerCursor < 0 {
		m.providerCursor = 0
	}
	if m.providerCursor > len(r) {
		m.providerCursor = len(r)
	}
	out := make([]rune, 0, len(r)+len(runes))
	out = append(out, r[:m.providerCursor]...)
	out = append(out, runes...)
	out = append(out, r[m.providerCursor:]...)
	*p = string(out)
	m.providerCursor += len(runes)
}

func (m *Model) deleteProviderRuneBefore() {
	p := m.providerFieldPtr()
	if p == nil {
		return
	}
	r := []rune(*p)
	if m.providerCursor <= 0 || len(r) == 0 {
		return
	}
	if m.providerCursor > len(r) {
		m.providerCursor = len(r)
	}
	out := append([]rune{}, r[:m.providerCursor-1]...)
	out = append(out, r[m.providerCursor:]...)
	*p = string(out)
	m.providerCursor--
}

func (m *Model) deleteProviderRuneAt() {
	p := m.providerFieldPtr()
	if p == nil {
		return
	}
	r := []rune(*p)
	if m.providerCursor < 0 || m.providerCursor >= len(r) {
		return
	}
	out := append([]rune{}, r[:m.providerCursor]...)
	out = append(out, r[m.providerCursor+1:]...)
	*p = string(out)
}

// applyProvider switches the target pane to the selected provider. Local and
// Azure apply immediately; SCP dials the host asynchronously.
func (m Model) applyProvider() (tea.Model, tea.Cmd) {
	index := m.providerModalPanel
	if index < 0 || index >= len(m.panels) {
		m.providerModal = false
		return m, nil
	}
	switch m.providerType {
	case LocalPanel:
		m.providerModal = false
		m.closePanelSession(index)
		p := &m.panels[index]
		p.kind = LocalPanel
		p.provider = localProvider{}
		p.scp = nil
		p.title = "Local"
		p.location = localStartDir()
		p.cursor, p.offset = 0, 0
		p.gen++
		m.resetPanelContent(index)
		m.clearPanelFilter(index)
		m.status = "switched to local filesystem"
		return m, m.listCmd(index)
	case RemotePanel:
		m.providerModal = false
		m.closePanelSession(index)
		p := &m.panels[index]
		p.kind = RemotePanel
		p.provider = m.makeAzureProvider()
		p.scp = nil
		p.title = "Remote"
		p.location = ""
		p.cursor, p.offset = 0, 0
		p.gen++
		m.resetPanelContent(index)
		m.clearPanelFilter(index)
		if m.container == "" {
			m.showingContainers = true
			m.containerButton = -1
			m.status = "select a container"
			return m, m.listContainersCmd()
		}
		m.status = "switched to Azure"
		return m, m.listCmd(index)
	case SCPPanel:
		cfg := m.providerForm
		if strings.TrimSpace(cfg.host) == "" {
			m.status = "host is required"
			return m, nil
		}
		if strings.TrimSpace(cfg.user) == "" {
			m.status = "user is required"
			return m, nil
		}
		m.connecting = true
		if cfg.pass == "" && strings.TrimSpace(cfg.keyPath) == "" {
			m.status = "connecting to " + cfg.label() + " using ~/.ssh keys..."
		} else {
			m.status = "connecting to " + cfg.label() + "..."
		}
		gen := m.panels[index].gen + 1
		m.panels[index].gen = gen
		return m, connectSCPCmd(index, gen, cfg)
	}
	return m, nil
}

func (m *Model) closePanelSession(index int) {
	if index < 0 || index >= len(m.panels) {
		return
	}
	if session := m.panels[index].scp; session != nil {
		_ = session.Close()
		m.panels[index].scp = nil
	}
}

func (m Model) applyProviderConnected(msg providerConnectedMsg) (tea.Model, tea.Cmd) {
	if msg.index < 0 || msg.index >= len(m.panels) {
		return m, nil
	}
	if msg.gen != m.panels[msg.index].gen {
		// Stale connection for a pane that has since changed; discard it.
		if msg.session != nil {
			_ = msg.session.Close()
		}
		return m, nil
	}
	m.connecting = false
	if msg.err != nil {
		m.status = "connection failed: " + msg.err.Error()
		return m, nil
	}
	m.providerModal = false
	m.closePanelSession(msg.index)
	p := &m.panels[msg.index]
	p.kind = SCPPanel
	p.scp = msg.session
	p.provider = scpProvider{session: msg.session}
	p.title = msg.label
	p.location = msg.root
	p.cursor, p.offset = 0, 0
	m.resetPanelContent(msg.index)
	m.clearPanelFilter(msg.index)
	m.status = "connected to " + msg.label
	return m, m.listCmd(msg.index)
}

func connectSCPCmd(index int, gen uint64, cfg scpConfig) tea.Cmd {
	return func() tea.Msg {
		session, root, err := connectSCP(cfg)
		if err != nil {
			return providerConnectedMsg{index: index, gen: gen, err: err}
		}
		return providerConnectedMsg{
			index:   index,
			gen:     gen,
			kind:    SCPPanel,
			session: session,
			root:    root,
			label:   session.label,
		}
	}
}

func localStartDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

func (m Model) renderProviderModal() string {
	const width = 60
	contentWidth := width - modalStyle.GetHorizontalFrameSize()
	lines := []string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render(fmt.Sprintf("Provider for pane %d", m.providerModalPanel+1)),
	}
	if m.connecting {
		lines = append(lines,
			fullWidthStyle(modalRowStyle, contentWidth).Render(""),
			fullWidthStyle(modalRowStyle, contentWidth).Render("Connecting... (Esc to cancel)"),
			fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		)
		return m.frameProviderModal(width, lines)
	}

	lines = append(lines, m.providerRow(contentWidth, providerFieldType, "Type", providerTypeName(m.providerType)+"  (←/→)"))
	if m.providerType == SCPPanel {
		lines = append(lines,
			m.providerRow(contentWidth, providerFieldHost, "Host", m.providerForm.host),
			m.providerRow(contentWidth, providerFieldPort, "Port", m.providerForm.port),
			m.providerRow(contentWidth, providerFieldUser, "User", m.providerForm.user),
			m.providerRow(contentWidth, providerFieldPass, "Password", strings.Repeat("*", len(m.providerForm.pass))),
			m.providerRow(contentWidth, providerFieldKey, "Key file", m.providerForm.keyPath),
			fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("Enter on Key file to browse for a key", contentWidth)),
			fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("Leave password & key empty to use ~/.ssh keys", contentWidth)),
		)
	}
	lines = append(lines,
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		renderDialogButtonsByIndex(contentWidth, m.providerButton, "Connect", "Cancel"),
	)
	return m.frameProviderModal(width, lines)
}

func (m Model) providerRow(width, field int, label, value string) string {
	active := m.providerButton < 0 && m.providerField == field
	prefix := "  "
	if active {
		prefix = "> "
	}
	labelText := fmt.Sprintf("%s%-9s ", prefix, label+":")

	// The Type selector row has no in-field text cursor.
	if field == providerFieldType {
		style := modalRowStyle
		if active {
			style = selectedDialogButtonStyle
		}
		if value == "" {
			value = " "
		}
		return fullWidthStyle(style, width).Render(truncate(labelText+value, width))
	}

	if !active {
		if value == "" {
			value = " "
		}
		return fullWidthStyle(modalRowStyle, width).Render(truncate(labelText+value, width))
	}

	// Active editable text field: highlight the label and draw a block cursor
	// at m.providerCursor within the value (which scrolls to keep it visible).
	labelSeg := selectedDialogButtonStyle.Render(labelText)
	valueWidth := width - lipgloss.Width(labelSeg)
	if valueWidth < 1 {
		valueWidth = 1
	}
	return labelSeg + renderValueWithCursor(value, m.providerCursor, valueWidth)
}

// renderValueWithCursor renders exactly width cells of value with a block cursor
// at position cursor, scrolling horizontally so the cursor stays visible. An
// extra slot past the end represents the cursor sitting after the last rune.
func renderValueWithCursor(value string, cursor, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(value)
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(r) {
		cursor = len(r)
	}
	total := len(r) + 1
	start := 0
	if total > width {
		if cursor >= width {
			start = cursor - width + 1
		}
		if start+width > total {
			start = total - width
		}
		if start < 0 {
			start = 0
		}
	}
	end := start + width
	if end > total {
		end = total
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		ch := " "
		if i < len(r) {
			ch = string(r[i])
		}
		if i == cursor {
			b.WriteString(modalCursorStyle.Render(ch))
		} else {
			b.WriteString(modalRowStyle.Render(ch))
		}
	}
	if rendered := end - start; rendered < width {
		b.WriteString(modalRowStyle.Render(strings.Repeat(" ", width-rendered)))
	}
	return b.String()
}

func (m Model) frameProviderModal(width int, lines []string) string {
	height := len(lines) + modalStyle.GetVerticalFrameSize() + modalStyle.GetVerticalPadding()
	return modalStyle.
		Width(width - modalStyle.GetHorizontalBorderSize()).
		Height(height - modalStyle.GetVerticalBorderSize()).
		MaxWidth(width).
		MaxHeight(height).
		Render(strings.Join(lines, "\n"))
}
