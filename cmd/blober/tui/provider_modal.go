package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	azure_storage "github.com/dariopb/blober/pkg/azure_storage"
)

// providerForm holds the editable connection parameters collected from the
// provider modal across all backend types (Azure, SCP and the URL-based
// HTTP/WebDAV).
type providerForm struct {
	url          string
	host         string
	port         string
	user         string
	pass         string
	keyPath      string
	account      string
	subscription string
	tenant       string
}

// providerField identifies the editable rows of the provider modal.
const (
	providerFieldType = iota
	providerFieldHost
	providerFieldPort
	providerFieldUser
	providerFieldPass
	providerFieldKey
	providerFieldURL
	providerFieldAccount
	providerFieldSubscription
	providerFieldTenant
)

func providerTypeName(kind providerKind) string {
	switch kind {
	case kindLocal:
		return "Local"
	case kindAzure:
		return "Azure"
	case kindSCP:
		return "SCP (ssh)"
	case kindHTTP:
		return "HTTP"
	case kindWebDAV:
		return "WebDAV"
	default:
		return "Local"
	}
}

// providerFieldIDs returns the ordered field ids shown for the currently
// selected provider type. The Type selector is always first.
func (m Model) providerFieldIDs() []int {
	switch m.providerType {
	case kindAzure:
		return []int{providerFieldType, providerFieldAccount, providerFieldSubscription, providerFieldTenant}
	case kindSCP:
		return []int{providerFieldType, providerFieldHost, providerFieldPort, providerFieldUser, providerFieldPass, providerFieldKey}
	case kindHTTP, kindWebDAV:
		return []int{providerFieldType, providerFieldURL, providerFieldUser, providerFieldPass}
	default:
		return []int{providerFieldType}
	}
}

// providerFieldCount returns how many editable rows the modal shows for the
// currently selected provider type.
func (m Model) providerFieldCount() int {
	return len(m.providerFieldIDs())
}

// providerFieldIndex returns the position of the active field within the
// ordered field list (0 when not found).
func (m Model) providerFieldIndex() int {
	for i, id := range m.providerFieldIDs() {
		if id == m.providerField {
			return i
		}
	}
	return 0
}

// providerKindOf maps a live provider to the modal type used to preselect it.
func providerKindOf(p Provider) providerKind {
	switch p.(type) {
	case azureProvider:
		return kindAzure
	case scpProvider:
		return kindSCP
	case httpProvider:
		return kindHTTP
	case webdavProvider:
		return kindWebDAV
	default:
		return kindLocal
	}
}

func (m *Model) openProviderModal() {
	m.providerModal = true
	m.providerModalPanel = m.active
	m.providerField = providerFieldType
	m.providerButton = -1
	m.providerType = providerKindOf(m.resolveProvider(m.panels[m.active]))
	// Pre-fill the Azure fields from the values supplied on the command line (or
	// from an account already connected this session) so the common case needs no
	// retyping.
	m.providerForm = providerForm{
		port:         "22",
		account:      m.accountName,
		subscription: m.azureDefaults.subscription,
		tenant:       m.azureDefaults.tenant,
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
			ids := m.providerFieldIDs()
			m.providerField = ids[len(ids)-1]
		} else {
			ids := m.providerFieldIDs()
			if idx := m.providerFieldIndex(); idx > 0 {
				m.providerField = ids[idx-1]
			}
		}
		m.providerCursorToEnd()
	case "down", "tab":
		if m.providerButton >= 0 {
			return m, nil
		}
		ids := m.providerFieldIDs()
		if idx := m.providerFieldIndex(); idx < len(ids)-1 {
			m.providerField = ids[idx+1]
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
	order := []providerKind{kindLocal, kindAzure, kindSCP, kindHTTP, kindWebDAV}
	idx := 0
	for i, k := range order {
		if k == m.providerType {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(order)) % len(order)
	m.providerType = order[idx]
	// Field sets differ per type; return to the Type row to stay valid.
	m.providerField = providerFieldType
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
	case providerFieldURL:
		return &m.providerForm.url
	case providerFieldAccount:
		return &m.providerForm.account
	case providerFieldSubscription:
		return &m.providerForm.subscription
	case providerFieldTenant:
		return &m.providerForm.tenant
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
	case kindLocal:
		m.providerModal = false
		m.closePanelSession(index)
		p := &m.panels[index]
		p.provider = localProvider{}
		p.title = "Local"
		p.location = localStartDir()
		p.cursor, p.offset = 0, 0
		p.gen++
		m.resetPanelContent(index)
		m.clearPanelFilter(index)
		m.status = "switched to local filesystem"
		return m, m.listCmd(index)
	case kindAzure:
		return m.applyAzureProvider(index)
	case kindSCP:
		f := m.providerForm
		if strings.TrimSpace(f.host) == "" {
			m.status = "host is required"
			return m, nil
		}
		if strings.TrimSpace(f.user) == "" {
			m.status = "user is required"
			return m, nil
		}
		cfg := scpConfig{host: f.host, port: f.port, user: f.user, pass: f.pass, keyPath: f.keyPath}
		m.connecting = true
		if f.pass == "" && strings.TrimSpace(f.keyPath) == "" {
			m.status = "connecting to " + cfg.label() + " using ~/.ssh keys..."
		} else {
			m.status = "connecting to " + cfg.label() + "..."
		}
		gen := m.panels[index].gen + 1
		m.panels[index].gen = gen
		return m, connectSCPCmd(index, gen, cfg)
	case kindHTTP, kindWebDAV:
		f := m.providerForm
		if strings.TrimSpace(f.url) == "" {
			m.status = "URL is required"
			return m, nil
		}
		m.connecting = true
		m.status = "connecting to " + strings.TrimSpace(f.url) + "..."
		gen := m.panels[index].gen + 1
		m.panels[index].gen = gen
		return m, connectWebCmd(index, gen, m.providerType, f.url, f.user, f.pass)
	}
	return m, nil
}

func (m *Model) closePanelSession(index int) {
	if index < 0 || index >= len(m.panels) {
		return
	}
	if p := m.panels[index].provider; p != nil {
		_ = p.Close()
	}
}

// applyAzureProvider validates the Azure connection fields and either reuses the
// existing authenticated client or starts an in-TUI sign-in. The sign-in runs in
// a tea.Cmd goroutine; its interactive prompts are delivered over a channel.
func (m Model) applyAzureProvider(index int) (tea.Model, tea.Cmd) {
	f := m.providerForm
	account := strings.TrimSpace(f.account)
	subscription := strings.TrimSpace(f.subscription)
	tenant := strings.TrimSpace(f.tenant)
	if err := azure_storage.ValidateAccountName(account); err != nil {
		m.status = err.Error()
		return m, nil
	}
	if subscription == "" {
		m.status = "subscription is required"
		return m, nil
	}
	m.azurePanel = index
	// Remember the chosen parameters so they pre-fill the modal next time.
	m.azureDefaults.subscription = subscription
	m.azureDefaults.tenant = tenant

	// Reuse an existing authenticated client when the account and tenant are
	// unchanged, so the user is not prompted to sign in again.
	if m.client != nil && m.azureCred != nil && account == m.accountName && tenant == m.azureConnectedTenant {
		return m.switchPaneToAzure(index, "switched to Azure")
	}

	cfg := azure_storage.Config{
		SubscriptionID: subscription,
		AccountName:    account,
		TenantID:       tenant,
		ClientID:       m.azureDefaults.clientID,
		TokenFile:      m.azureDefaults.tokenFile,
		UserFlow:       m.azureDefaults.userFlow,
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan tea.Msg, 4)
	m.authGen++
	gen := m.authGen
	m.authenticating = true
	m.authPrompt = nil
	m.authCh = ch
	m.authCancel = cancel
	m.connecting = false
	m.providerModal = false
	m.status = "signing in to Azure..."
	return m, tea.Batch(waitAuth(ch), azureLoginCmd(ctx, cancel, cfg, ch, gen, index, account, tenant))
}

// switchPaneToAzure points the given pane at the Azure provider, prompting for a
// container first when none is selected yet.
func (m Model) switchPaneToAzure(index int, okStatus string) (tea.Model, tea.Cmd) {
	if index < 0 || index >= len(m.panels) {
		return m, nil
	}
	m.azurePanel = index
	m.closePanelSession(index)
	p := &m.panels[index]
	p.provider = m.makeAzureProvider()
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
	m.status = okStatus
	return m, m.listCmd(index)
}

func azureLoginCmd(ctx context.Context, cancel context.CancelFunc, cfg azure_storage.Config, ch chan tea.Msg, gen uint64, panel int, account, tenant string) tea.Cmd {
	return func() tea.Msg {
		cfg.Prompt = func(p azure_storage.AuthPrompt) {
			ch <- authPromptMsg{gen: gen, prompt: p}
		}
		cred, err := azure_storage.Login(ctx, cfg)
		// No more prompts will fire once Login returns; closing here lets the
		// waitAuth subscriber unblock and stop.
		close(ch)
		return azureLoginDoneMsg{gen: gen, panel: panel, account: account, tenant: tenant, cred: cred, cancel: cancel, err: err}
	}
}

func (m Model) applyProviderConnected(msg providerConnectedMsg) (tea.Model, tea.Cmd) {
	if msg.index < 0 || msg.index >= len(m.panels) {
		if msg.provider != nil {
			_ = msg.provider.Close()
		}
		return m, nil
	}
	if msg.gen != m.panels[msg.index].gen {
		// Stale connection for a pane that has since changed; discard it.
		if msg.provider != nil {
			_ = msg.provider.Close()
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
	p.provider = msg.provider
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
			index:    index,
			gen:      gen,
			provider: scpProvider{session: session},
			root:     root,
			label:    session.label,
		}
	}
}

func connectWebCmd(index int, gen uint64, kind providerKind, rawURL, user, pass string) tea.Cmd {
	return func() tea.Msg {
		prov, root, err := connectWeb(kind, rawURL, user, pass)
		if err != nil {
			return providerConnectedMsg{index: index, gen: gen, err: err}
		}
		// Validate the connection (and credentials) with an initial listing.
		if _, err := prov.List(context.Background(), root); err != nil {
			_ = prov.Close()
			return providerConnectedMsg{index: index, gen: gen, err: err}
		}
		return providerConnectedMsg{
			index:    index,
			gen:      gen,
			provider: prov,
			root:     root,
			label:    prov.Label(),
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
	switch m.providerType {
	case kindAzure:
		lines = append(lines,
			m.providerRow(contentWidth, providerFieldAccount, "Account", m.providerForm.account),
			m.providerRow(contentWidth, providerFieldSubscription, "Subscr.", m.providerForm.subscription),
			m.providerRow(contentWidth, providerFieldTenant, "Tenant", m.providerForm.tenant),
			fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("Leave Tenant empty to use the common endpoint", contentWidth)),
			fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("Connect signs in if needed (device/browser)", contentWidth)),
		)
	case kindSCP:
		lines = append(lines,
			m.providerRow(contentWidth, providerFieldHost, "Host", m.providerForm.host),
			m.providerRow(contentWidth, providerFieldPort, "Port", m.providerForm.port),
			m.providerRow(contentWidth, providerFieldUser, "User", m.providerForm.user),
			m.providerRow(contentWidth, providerFieldPass, "Password", strings.Repeat("*", len(m.providerForm.pass))),
			m.providerRow(contentWidth, providerFieldKey, "Key file", m.providerForm.keyPath),
			fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("Enter on Key file to browse for a key", contentWidth)),
			fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("Leave password & key empty to use ~/.ssh keys", contentWidth)),
		)
	case kindHTTP, kindWebDAV:
		hint := "Plain HTTP: lists <a> links, uploads via PUT"
		if m.providerType == kindWebDAV {
			hint = "WebDAV: PROPFIND listing, MKCOL/PUT/DELETE"
		}
		lines = append(lines,
			m.providerRow(contentWidth, providerFieldURL, "URL", m.providerForm.url),
			m.providerRow(contentWidth, providerFieldUser, "User", m.providerForm.user),
			m.providerRow(contentWidth, providerFieldPass, "Password", strings.Repeat("*", len(m.providerForm.pass))),
			fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(hint, contentWidth)),
			fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("Leave user & password empty for anonymous", contentWidth)),
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

// renderAuthModal shows the in-TUI Azure sign-in instructions (device code or
// browser flow), sizing itself to fit any QR code the device flow provides.
func (m Model) renderAuthModal() string {
	var qrLines []string
	if m.authPrompt != nil && m.authPrompt.QRCode != "" {
		qrLines = strings.Split(strings.TrimRight(m.authPrompt.QRCode, "\n"), "\n")
	}
	contentWidth := 56
	for _, l := range qrLines {
		if w := lipgloss.Width(l); w > contentWidth {
			contentWidth = w
		}
	}
	// Keep the modal within the visible screen when one is known.
	if m.width > 8 {
		if max := m.width - modalStyle.GetHorizontalFrameSize() - 4; contentWidth > max {
			contentWidth = max
		}
	}
	width := contentWidth + modalStyle.GetHorizontalFrameSize()

	lines := []string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render("Azure sign-in"),
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
	}
	if m.authPrompt == nil {
		lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render("Requesting sign-in..."))
	} else {
		p := m.authPrompt
		for _, line := range strings.Split(strings.TrimRight(p.Message, "\n"), "\n") {
			lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(line, contentWidth)))
		}
		if p.UserCode != "" {
			lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(truncate("Code: "+p.UserCode, contentWidth)))
		}
		if p.VerificationURL != "" {
			lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(p.VerificationURL, contentWidth)))
		}
		for _, line := range qrLines {
			lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(line))
		}
	}
	lines = append(lines,
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		fullWidthStyle(modalRowStyle, contentWidth).Render("Esc to cancel"),
	)
	return m.frameProviderModal(width, lines)
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
