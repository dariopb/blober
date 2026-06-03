package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	azure_storage "github.com/dariopb/blober/pkg/azure_storage"
)

type Config struct {
	Client      *azblob.Client
	AccountName string
	Container   string
	Prefix      string
	LocalPath   string
	Force       bool
	Theme       Theme
}

const helpHint = "1 -> help"

type Model struct {
	client      *azblob.Client
	accountName string
	container   string
	force       bool
	theme       Theme

	panels           [2]panel
	active           int
	status           string
	width            int
	height           int
	pendingSelect    [2]string
	pendingSelectSet [2]bool
	rememberedSelect [2]map[string]string

	copying      bool
	copyProgress transferProgress
	progressCh   chan tea.Msg
	copyCancel   context.CancelFunc

	confirming         bool
	confirmAction      confirmAction
	confirmYes         bool
	pendingSource      int
	pendingCopy        []Entry
	pendingItems       []copyItem
	pendingDelete      []Entry
	overwriteConflicts []int
	overwriteAt        int
	overwriteBtn       int
	showingHelp        bool
	showingTheme       bool
	themeCursor        int
	themeDraft         string
	themeEditing       bool
	themeButton        int
	creatingDir        bool
	createYes          bool
	newDirName         string
	filtering          bool
	filterPanel        int
	filterOriginal     string

	showingContainers bool
	containers        []string
	containerCursor   int
	containerButton   int
	containerErr      error

	providerModal      bool
	providerModalPanel int
	providerType       PanelKind
	providerField      int
	providerButton     int
	providerForm       scpConfig
	providerCursor     int
	connecting         bool

	keyBrowse        bool
	keyBrowseDir     string
	keyBrowseEntries []Entry
	keyBrowseCursor  int
	keyBrowseOffset  int
	keyBrowseErr     error
}

type Theme struct {
	Background      string `json:"background"`
	Highlight       string `json:"highlight"`
	Text            string `json:"text"`
	HeaderText      string `json:"header_text"`
	Selected        string `json:"selected"`
	Error           string `json:"error"`
	ModalBorder     string `json:"modal_border"`
	ModalBackground string `json:"modal_background"`
	ModalShadow     string `json:"modal_shadow"`
}

func DefaultTheme() Theme {
	return Theme{
		Background:      "#0011EE",
		Highlight:       "#00AAFF",
		Text:            "#D0D0D0",
		HeaderText:      "#000000",
		Selected:        "#FFFF00",
		Error:           "#FF0000",
		ModalBorder:     "#111111",
		ModalBackground: "#D0D0D0",
		ModalShadow:     "#000000",
	}
}

func LoadThemeFile(path string) (Theme, error) {
	theme := DefaultTheme()
	if path == "" {
		discovered, ok, err := discoverThemeFile()
		if err != nil {
			return Theme{}, err
		}
		if !ok {
			return theme, nil
		}
		path = discovered
	}
	return loadThemeFile(path, theme)
}

func discoverThemeFile() (string, bool, error) {
	candidates := []string{".theme.json"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		homeTheme := filepath.Join(home, ".theme.json")
		if homeTheme != candidates[0] {
			candidates = append(candidates, homeTheme)
		}
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}
	return "", false, nil
}

func loadThemeFile(path string, theme Theme) (Theme, error) {
	if path == "" {
		return theme, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Theme{}, err
	}
	if err := json.Unmarshal(data, &theme); err != nil {
		return Theme{}, err
	}
	theme = normalizeTheme(theme)
	if err := validateTheme(theme); err != nil {
		return Theme{}, err
	}
	return theme, nil
}

func SaveThemeFile(path string, theme Theme) error {
	theme = normalizeTheme(theme)
	if err := validateTheme(theme); err != nil {
		return err
	}
	data, err := json.MarshalIndent(theme, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

type confirmAction int

const (
	confirmNone confirmAction = iota
	confirmCopy
	confirmOverwrite
	confirmDelete
)

type entriesLoadedMsg struct {
	index   int
	gen     uint64
	entries []Entry
	err     error
}

type providerConnectedMsg struct {
	index   int
	gen     uint64
	kind    PanelKind
	session *scpSession
	root    string
	label   string
	err     error
}

type transferProgress struct {
	File       string
	FileDone   int64
	FileTotal  int64
	BatchDone  int64
	BatchTotal int64
	SpeedBps   float64
}

const progressUpdateInterval = 200 * time.Millisecond

type copyItem struct {
	entry     Entry
	targetRel string
}

type copyDoneMsg struct {
	source int
	err    error
}

type overwriteCheckedMsg struct {
	source    int
	items     []copyItem
	conflicts []int
	err       error
}

type deleteDoneMsg struct {
	source int
	count  int
	err    error
}

type mkdirDoneMsg struct {
	index int
	name  string
	err   error
}

type containersLoadedMsg struct {
	containers []string
	err        error
}

func New(cfg Config) Model {
	theme := normalizeTheme(cfg.Theme)
	applyTheme(theme)
	localPath := cfg.LocalPath
	if localPath == "" {
		localPath = "."
	}
	absLocal, err := filepath.Abs(localPath)
	if err == nil {
		localPath = absLocal
	}
	m := Model{
		client:      cfg.Client,
		accountName: cfg.AccountName,
		container:   cfg.Container,
		force:       cfg.Force,
		theme:       theme,
		panels: [2]panel{
			newRemotePanel(cfg.Prefix),
			newLocalPanel(localPath),
		},
		containerButton: -1,
		filterPanel:     -1,
		status:          helpHint,
		rememberedSelect: [2]map[string]string{
			{},
			{},
		},
	}
	if m.container == "" && (m.accountName != "" || m.client != nil) {
		m.showingContainers = true
		m.status = "select a container"
	}
	m.panels[0].provider = m.makeAzureProvider()
	m.providerModalPanel = -1
	return m
}

func (m Model) makeAzureProvider() Provider {
	return azureProvider{client: m.client, container: m.container}
}

// resolveProvider returns the panel's provider, falling back to a kind-derived
// provider for panels constructed without one (used by tests).
func (m Model) resolveProvider(p panel) Provider {
	if p.provider != nil {
		return p.provider
	}
	switch p.kind {
	case RemotePanel:
		return m.makeAzureProvider()
	case SCPPanel:
		if p.scp != nil {
			return scpProvider{session: p.scp}
		}
		return localProvider{}
	default:
		return localProvider{}
	}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.listCmd(1)}
	if m.container == "" {
		cmds = append(cmds, m.listContainersCmd())
	} else {
		cmds = append(cmds, m.listCmd(0))
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		if m.showingContainers {
			switch msg.String() {
			case "ctrl+c", "q":
				return m, tea.Quit
			case "esc":
				if m.container != "" {
					m.showingContainers = false
				}
			case "up", "k":
				m.moveContainerCursor(-1)
			case "down", "j", "tab":
				m.moveContainerCursor(1)
			case "left":
				if m.containerButton >= 0 {
					m.containerButton = (m.containerButton + 1) % 2
				}
			case "right":
				if m.containerButton >= 0 {
					m.containerButton = (m.containerButton + 1) % 2
				} else if len(m.containers) > 0 && m.containerCursor == len(m.containers)-1 {
					m.containerButton = 0
				}
			case "enter":
				if m.containerButton == 1 {
					if m.container != "" {
						m.showingContainers = false
					}
					return m, nil
				}
				return m.selectContainer()
			case "r":
				return m, m.listContainersCmd()
			}
			return m, nil
		}
		if m.showingHelp {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "1", "esc", "q", "enter":
				m.showingHelp = false
			}
			return m, nil
		}
		if m.showingTheme {
			if (msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH) && m.themeButton < 0 {
				m.editThemeDraft(-1, 0)
				return m, nil
			}
			switch msg.String() {
			case "t", "esc", "q":
				m.showingTheme = false
			case "up", "k":
				if m.themeButton >= 0 {
					m.themeButton = -1
					m.themeDraft = themeValue(m.theme, m.themeCursor)
				} else {
					m.moveThemeCursor(-1)
				}
			case "down", "j":
				m.moveThemeDown()
			case "left":
				if m.themeButton >= 0 {
					m.themeButton = (m.themeButton + 2) % 3
				}
			case "right", "tab":
				if m.themeButton >= 0 {
					m.themeButton = (m.themeButton + 1) % 3
				} else {
					m.moveThemeDown()
				}
			case "enter", "e":
				if m.themeButton >= 0 {
					return m.activateThemeButton()
				}
				m.themeEditing = true
				m.themeDraft = themeValue(m.theme, m.themeCursor)
			case "s":
				if err := SaveThemeFile(".theme.json", m.theme); err != nil {
					m.status = err.Error()
				} else {
					m.status = "saved theme to .theme.json"
				}
			default:
				if len(msg.Runes) == 1 {
					m.themeButton = -1
					m.editThemeDraft(0, msg.Runes[0])
				}
			}
			return m, nil
		}
		if m.providerModal {
			return m.updateProviderModal(msg)
		}
		if m.creatingDir {
			if msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH {
				if len(m.newDirName) > 0 {
					m.newDirName = m.newDirName[:len(m.newDirName)-1]
				}
				m.createYes = true
				return m, nil
			}
			switch msg.String() {
			case "esc", "ctrl+c":
				m.creatingDir = false
				m.newDirName = ""
				m.status = "create directory cancelled"
			case "left", "up":
				m.createYes = true
			case "right", "down", "tab":
				m.createYes = false
			case "enter":
				if m.createYes {
					return m.createDirectory()
				}
				m.creatingDir = false
				m.newDirName = ""
				m.status = "create directory cancelled"
			default:
				if len(msg.Runes) > 0 {
					m.newDirName += string(msg.Runes)
					m.createYes = true
				}
			}
			return m, nil
		}
		if m.confirming {
			switch msg.String() {
			case "left", "up":
				if m.confirmAction == confirmOverwrite {
					m.moveOverwriteButton(-1)
				} else {
					m.confirmYes = true
				}
			case "right", "down", "tab":
				if m.confirmAction == confirmOverwrite {
					m.moveOverwriteButton(1)
				} else {
					m.confirmYes = false
				}
			case "enter":
				if m.confirmAction == confirmOverwrite {
					return m.acceptOverwriteChoice()
				} else if m.confirmYes {
					return m.acceptConfirmation()
				}
				m.cancelConfirmation()
			case "y", "Y":
				if m.confirmAction == confirmOverwrite {
					return m.acceptOverwriteChoice()
				}
				return m.acceptConfirmation()
			case "n", "N", "esc":
				m.cancelConfirmation()
			}
			return m, nil
		}
		if m.copying {
			switch msg.String() {
			case "esc", "ctrl+c":
				if m.copyCancel != nil {
					m.copyCancel()
					m.status = "cancelling copy..."
				}
			}
			return m, nil
		}
		if m.filtering {
			return m.updateFilter(msg)
		}
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			return m, tea.Quit
		case "1":
			m.showingHelp = true
		case "tab":
			m.active = 1 - m.active
		case "up", "k":
			m.moveCursor(-1)
		case "down", "j":
			m.moveCursor(1)
		case "pgup":
			m.moveCursor(-m.panePageSize())
		case "pgdown":
			m.moveCursor(m.panePageSize())
		case "home":
			m.moveCursorTo(0)
		case "end":
			m.moveCursorTo(len(m.panels[m.active].entries) - 1)
		case " ":
			m.status = m.panels[m.active].toggleCurrent()
		case "enter", "right":
			return m.openCurrent()
		case "backspace", "h", "left":
			return m.openParent()
		case "r":
			return m, m.listCmd(m.active)
		case "l":
			m.showingContainers = true
			m.containerButton = -1
			return m, m.listContainersCmd()
		case "c":
			return m.startCopy()
		case "d":
			return m.startDelete()
		case "n":
			m.creatingDir = true
			m.createYes = true
			m.newDirName = ""
			m.status = "enter new directory name"
		case "/":
			m.filtering = true
			m.filterPanel = m.active
			m.filterOriginal = m.panels[m.active].filter
		case "t":
			m.showingTheme = true
			m.themeCursor = 0
			m.themeButton = -1
			m.themeDraft = themeValue(m.theme, m.themeCursor)
		case "p":
			m.openProviderModal()
		}
	case entriesLoadedMsg:
		if msg.index < 0 || msg.index >= len(m.panels) || msg.gen != m.panels[msg.index].gen {
			return m, nil
		}
		m.panels[msg.index].setEntries(msg.entries, msg.err)
		if m.pendingSelectSet[msg.index] {
			m.selectEntry(msg.index, m.pendingSelect[msg.index])
			m.pendingSelect[msg.index] = ""
			m.pendingSelectSet[msg.index] = false
		}
		m.clampPanelViewport(msg.index)
		if msg.err != nil {
			m.status = msg.err.Error()
		}
	case providerConnectedMsg:
		return m.applyProviderConnected(msg)
	case containersLoadedMsg:
		m.containers = msg.containers
		m.containerErr = msg.err
		m.containerCursor = min(m.containerCursor, max(0, len(m.containers)-1))
		if msg.err != nil {
			m.status = msg.err.Error()
		} else if len(msg.containers) == 0 {
			m.status = "no accessible containers found"
		} else {
			m.status = fmt.Sprintf("found %d container(s)", len(msg.containers))
		}
	case overwriteCheckedMsg:
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		if len(msg.conflicts) == 0 {
			return m.beginCopyItems(msg.source, msg.items, false)
		}
		m.confirming = true
		m.confirmAction = confirmOverwrite
		m.confirmYes = true
		m.pendingSource = msg.source
		m.pendingItems = msg.items
		m.overwriteConflicts = msg.conflicts
		m.pendingCopy = nil
		m.overwriteAt = 0
		m.overwriteBtn = 0
		m.pendingDelete = nil
		m.status = fmt.Sprintf("destination exists: %s", m.overwriteTarget())
	case transferProgress:
		m.copyProgress = msg
		return m, waitProgress(m.progressCh)
	case copyDoneMsg:
		m.copying = false
		m.copyCancel = nil
		m.progressCh = nil
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) {
				m.status = "copy cancelled"
			} else {
				m.status = msg.err.Error()
			}
			return m, nil
		}
		m.panels[msg.source].selected = map[string]Entry{}
		m.status = "copy complete"
		return m, tea.Batch(m.listCmd(0), m.listCmd(1))
	case deleteDoneMsg:
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.panels[msg.source].selected = map[string]Entry{}
		m.status = fmt.Sprintf("deleted %d file(s)", msg.count)
		return m, m.listCmd(msg.source)
	case mkdirDoneMsg:
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.status = "created directory " + msg.name
		return m, m.listCmd(msg.index)
	}
	return m, nil
}

func (m Model) View() string {
	screenWidth := m.width
	if screenWidth <= 0 {
		screenWidth = 80
	}
	screenHeight := m.height
	if screenHeight <= 0 {
		screenHeight = 24
	}
	panelAreaHeight := max(3, screenHeight-2)
	leftWidth := max(1, screenWidth/2)
	rightWidth := max(1, screenWidth-leftWidth)
	header := fullWidthStyle(headerStyle, screenWidth).Render(truncate(m.headerText(), screenWidth))
	left := m.renderPanel(0, leftWidth, panelAreaHeight)
	right := m.renderPanel(1, rightWidth, panelAreaHeight)
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	status := m.renderFooter(screenWidth)
	base := lipgloss.JoinVertical(lipgloss.Left, header, body, status)
	if m.copying {
		return renderScreen(overlayCentered(base, m.renderProgressModal(), screenWidth, screenHeight), screenWidth, screenHeight)
	}
	if m.confirming {
		return renderScreen(overlayCentered(base, m.renderConfirmModal(), screenWidth, screenHeight), screenWidth, screenHeight)
	}
	if m.creatingDir {
		return renderScreen(overlayCentered(base, m.renderNewDirModal(), screenWidth, screenHeight), screenWidth, screenHeight)
	}
	if m.showingContainers {
		return renderScreen(overlayCentered(base, m.renderContainerModal(), screenWidth, screenHeight), screenWidth, screenHeight)
	}
	if m.showingHelp {
		return renderScreen(overlayCentered(base, m.renderHelpModal(), screenWidth, screenHeight), screenWidth, screenHeight)
	}
	if m.showingTheme {
		return renderScreen(overlayCentered(base, m.renderThemeModal(), screenWidth, screenHeight), screenWidth, screenHeight)
	}
	if m.providerModal {
		overlay := m.renderProviderModal()
		if m.keyBrowse {
			overlay = m.renderKeyBrowser()
		}
		return renderScreen(overlayCentered(base, overlay, screenWidth, screenHeight), screenWidth, screenHeight)
	}
	// base is already composed to exactly screenWidth x screenHeight with every
	// cell painted (header, panels and status fill their own backgrounds), so the
	// outer renderScreen re-wrap is redundant work on the hot navigation path.
	return base
}

func (m Model) headerText() string {
	remote := m.panels[0]
	if remote.kind != RemotePanel {
		location := remote.location
		if location == "" {
			location = "(root)"
		}
		return fmt.Sprintf("%s: %s", m.resolveProvider(remote).Label(), location)
	}
	account := m.accountName
	if account == "" {
		account = "(unknown account)"
	}
	prefix := remote.location
	if prefix == "" {
		prefix = "(root)"
	}
	container := m.container
	if container == "" {
		container = "(select container)"
	}
	return fmt.Sprintf("Remote: account=%s container=%s prefix=%s", account, container, prefix)
}

func (m Model) footerText() string {
	parts := []string{helpHint}
	entry, ok := m.panels[m.active].current()
	if ok {
		size := "-"
		if !entry.IsDir && !entry.parent {
			size = fmt.Sprintf("%d bytes", entry.Size)
		}
		parts = append(parts, fmt.Sprintf("%s | %s | %s", m.panels[m.active].title, entry.Name, size))
	}
	if m.status != "" && m.status != helpHint {
		parts = append(parts, m.status)
	}
	return strings.Join(parts, " | ")
}

func (m Model) renderFooter(width int) string {
	filterText, hasFilter := m.filterStatusText()
	if !hasFilter {
		return fullWidthStyle(statusStyle, width).Render(truncate(m.footerText(), width))
	}
	normalText := m.footerText()
	if normalText == "" {
		return fullWidthStyle(filterStatusStyle, width).Render(truncate(filterText, width))
	}
	separator := " | "
	filterText = truncate(filterText, width)
	filterWidth := lipgloss.Width(filterText)
	normalWidth := width - lipgloss.Width(separator) - filterWidth
	if normalWidth < 0 {
		return fullWidthStyle(filterStatusStyle, width).Render(truncate(filterText, width))
	}
	normalText = truncate(normalText, normalWidth)
	normalSegment := statusStyle.Render(normalText + separator)
	filterSegment := filterStatusStyle.Render(filterText)
	remaining := width - lipgloss.Width(normalText) - lipgloss.Width(separator) - filterWidth
	if remaining < 0 {
		remaining = 0
	}
	return normalSegment + filterSegment + statusStyle.Render(strings.Repeat(" ", remaining))
}

func (m Model) filterStatusText() (string, bool) {
	index := m.active
	if m.filtering {
		index = m.filterPanel
	}
	if index < 0 || index >= len(m.panels) {
		return "", false
	}
	filter := m.panels[index].filter
	if filter == "" && !m.filtering {
		return "", false
	}
	return "filter: " + filter, true
}

func (m Model) renderPanel(index, outerWidth, outerHeight int) string {
	p := m.panels[index]
	style := inactivePanelStyle
	if index == m.active {
		style = activePanelStyle
	}
	blockWidth := max(1, outerWidth-style.GetHorizontalBorderSize())
	blockHeight := max(1, outerHeight-style.GetVerticalBorderSize())
	textWidth := max(1, blockWidth-style.GetHorizontalPadding())
	textHeight := max(1, blockHeight-style.GetVerticalPadding())
	lines := make([]string, 0, textHeight)
	lines = append(lines, fullWidthStyle(titleStyle, textWidth).Render(truncate(fmt.Sprintf("%s: %s", p.title, p.location), textWidth)))
	if p.err != nil {
		lines = append(lines, fullWidthStyle(errorStyle, textWidth).Render(truncate(p.err.Error(), textWidth)))
	}
	if len(lines) < textHeight {
		lines = append(lines, fullWidthStyle(titleStyle, textWidth).Render(columnHeader(textWidth)))
	}
	visibleHeight := max(0, textHeight-len(lines))
	for row := 0; row < visibleHeight; row++ {
		entryIndex := p.offset + row
		if entryIndex >= len(p.entries) {
			lines = append(lines, fullWidthStyle(rowStyle, textWidth).Render(emptyEntryColumns(textWidth)))
			continue
		}
		entry := p.entries[entryIndex]
		rowStyleForEntry := rowStyle
		cursorRow := index == m.active && entryIndex == p.cursor
		_, selected := p.selected[entry.Path]
		switch {
		case cursorRow && selected:
			rowStyleForEntry = selectedCursorStyle
		case cursorRow:
			rowStyleForEntry = cursorStyle
		case selected:
			rowStyleForEntry = selectedStyle
		}
		line := fullWidthStyle(rowStyleForEntry, textWidth).Render(renderEntryColumns(entry, textWidth, cursorRow))
		lines = append(lines, line)
	}
	return style.Width(blockWidth).Height(blockHeight).Render(strings.Join(lines, "\n"))
}

func columnHeader(width int) string {
	fileWidth, sizeWidth, dateWidth := columnWidths(width)
	if sizeWidth == 0 || dateWidth == 0 {
		return fmt.Sprintf("%-*s", fileWidth, truncate("File", fileWidth))
	}
	return fmt.Sprintf("%-*s│%*s│%-*s",
		fileWidth, truncate("File", fileWidth),
		sizeWidth, truncate("Size", sizeWidth),
		dateWidth, truncate("Modified", dateWidth),
	)
}

func renderEntryColumns(entry Entry, width int, cursor bool) string {
	fileWidth, sizeWidth, dateWidth := columnWidths(width)
	name := displayEntryName(entry)
	if cursor {
		name = "> " + name
	} else {
		name = "  " + name
	}
	size := ""
	if !entry.IsDir && !entry.parent {
		size = humanEntrySize(entry.Size)
	}
	modified := ""
	if !entry.LastModified.IsZero() {
		modified = entry.LastModified.Local().Format("2006-01-02 15:04")
	}
	if sizeWidth == 0 || dateWidth == 0 {
		return fmt.Sprintf("%-*s", fileWidth, truncate(name, fileWidth))
	}
	return fmt.Sprintf("%-*s│%*s│%-*s",
		fileWidth, truncate(name, fileWidth),
		sizeWidth, truncate(size, sizeWidth),
		dateWidth, truncate(modified, dateWidth),
	)
}

func displayEntryName(entry Entry) string {
	name := entry.Name
	if entry.IsDir {
		name = strings.TrimSuffix(name, "/")
		name = strings.TrimSuffix(name, string(os.PathSeparator))
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
	}
	return name
}

func emptyEntryColumns(width int) string {
	fileWidth, sizeWidth, dateWidth := columnWidths(width)
	if sizeWidth == 0 || dateWidth == 0 {
		return fmt.Sprintf("%-*s", fileWidth, "")
	}
	return fmt.Sprintf("%-*s│%*s│%-*s", fileWidth, "", sizeWidth, "", dateWidth, "")
}

func columnWidths(width int) (fileWidth, sizeWidth, dateWidth int) {
	const (
		minFileWidth = 8
		defaultSize  = 9
		defaultDate  = 16
		delimiters   = 2
	)
	if width < 24 {
		return max(1, width), 0, 0
	}
	sizeWidth = defaultSize
	dateWidth = defaultDate
	fileWidth = width - sizeWidth - dateWidth - delimiters
	if fileWidth >= minFileWidth {
		return fileWidth, sizeWidth, dateWidth
	}
	shortage := minFileWidth - fileWidth
	dateWidth = max(8, dateWidth-shortage)
	fileWidth = width - sizeWidth - dateWidth - delimiters
	if fileWidth >= minFileWidth {
		return fileWidth, sizeWidth, dateWidth
	}
	sizeWidth = max(5, sizeWidth-(minFileWidth-fileWidth))
	fileWidth = max(1, width-sizeWidth-dateWidth-delimiters)
	return fileWidth, sizeWidth, dateWidth
}

func humanEntrySize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}

func (m Model) renderProgressModal() string {
	p := m.copyProgress
	filePercent := percent(p.FileDone, p.FileTotal)
	batchPercent := percent(p.BatchDone, p.BatchTotal)
	const width = 52
	contentWidth := width - modalStyle.GetHorizontalFrameSize()
	content := strings.Join([]string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render("Copying"),
		fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(p.File, contentWidth)),
		fullWidthStyle(modalRowStyle, contentWidth).Render(fmt.Sprintf("File  %s %5.1f%%", progressBar(filePercent, 24), filePercent)),
		fullWidthStyle(modalRowStyle, contentWidth).Render(fmt.Sprintf("%d/%d bytes", p.FileDone, p.FileTotal)),
		fullWidthStyle(modalRowStyle, contentWidth).Render(fmt.Sprintf("Speed %s/s", humanEntrySize(int64(p.SpeedBps)))),
		fullWidthStyle(modalRowStyle, contentWidth).Render(fmt.Sprintf("Total %s %5.1f%%", progressBar(batchPercent, 24), batchPercent)),
		fullWidthStyle(modalRowStyle, contentWidth).Render(fmt.Sprintf("%d/%d bytes", p.BatchDone, p.BatchTotal)),
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		fullWidthStyle(cancelButtonStyle, contentWidth).Render("[ Cancel: Esc or Ctrl+C ]"),
	}, "\n")
	return modalStyle.
		Width(width - modalStyle.GetHorizontalBorderSize()).
		Height(11).
		MaxWidth(width).
		MaxHeight(13).
		Render(content)
}

func (m Model) renderConfirmModal() string {
	const width = 52
	contentWidth := width - modalStyle.GetHorizontalFrameSize()
	title := "Confirm"
	message := "Proceed?"
	switch m.confirmAction {
	case confirmCopy:
		title = "Confirm overwrite"
		message = fmt.Sprintf("Overwrite existing targets if needed for %d file(s)?", len(m.pendingCopy))
		if copyHasDirectories(m.pendingCopy) {
			title = "Confirm recursive copy"
			message = fmt.Sprintf("Copy %d item(s) recursively?", len(m.pendingCopy))
			if m.force {
				message = fmt.Sprintf("Copy %d item(s) recursively and overwrite existing targets if needed?", len(m.pendingCopy))
			}
		}
	case confirmOverwrite:
		title = "Confirm overwrite"
		message = fmt.Sprintf("Overwrite %s?", m.overwriteTarget())
	case confirmDelete:
		title = "Confirm delete"
		message = fmt.Sprintf("Delete %d file(s) from %s?", len(m.pendingDelete), m.panels[m.pendingSource].title)
	}
	if m.confirmAction == confirmOverwrite {
		buttons := renderOverwriteButtons(contentWidth, m.overwriteBtn, m.overwriteButtonCount())
		content := strings.Join([]string{
			fullWidthStyle(modalTitleStyle, contentWidth).Render(title),
			fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(message, contentWidth)),
			fullWidthStyle(modalRowStyle, contentWidth).Render(""),
			buttons,
		}, "\n")
		return modalStyle.
			Width(width - modalStyle.GetHorizontalBorderSize()).
			Height(7).
			MaxWidth(width).
			MaxHeight(9).
			Render(content)
	}
	content := strings.Join([]string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render(title),
		fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(message, contentWidth)),
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		renderDialogButtons(contentWidth, m.confirmYes, "Yes", "No / Cancel"),
	}, "\n")
	return modalStyle.
		Width(width - modalStyle.GetHorizontalBorderSize()).
		Height(6).
		MaxWidth(width).
		MaxHeight(8).
		Render(content)
}

func (m Model) renderNewDirModal() string {
	const contentWidth = 44
	name := m.newDirName
	if name == "" {
		name = " "
	}
	lines := []string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render("Create directory"),
		fullWidthStyle(modalRowStyle, contentWidth).Render("Name: " + truncate(name, contentWidth-6)),
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		renderDialogButtons(contentWidth, m.createYes, "Create", "Cancel"),
	}
	content := strings.Join(lines, "\n")
	return modalStyle.
		Width(contentWidth + modalStyle.GetHorizontalPadding()).
		Height(6).
		Render(content)
}

func renderDialogButtons(width int, firstSelected bool, firstLabel, secondLabel string) string {
	selected := 1
	if firstSelected {
		selected = 0
	}
	return renderDialogButtonsByIndex(width, selected, firstLabel, secondLabel)
}

func renderDialogButtonsByIndex(width, selected int, firstLabel, secondLabel string) string {
	first := renderDialogButton(firstLabel, selected == 0)
	second := renderDialogButton(secondLabel, selected == 1)
	gap := modalRowStyle.Render("    ")
	row := first + gap + second
	if lipgloss.Width(row) > width {
		row = first + modalRowStyle.Render(" ") + second
	}
	rowWidth := lipgloss.Width(row)
	leftPad := max(0, (width-rowWidth)/2)
	rightPad := max(0, width-rowWidth-leftPad)
	return modalRowStyle.Render(strings.Repeat(" ", leftPad)) + row + modalRowStyle.Render(strings.Repeat(" ", rightPad))
}

func renderOverwriteButtons(width, selected, count int) string {
	labels := []string{"Overwrite", "Overwrite All", "Cancel"}
	if count == 2 {
		labels = []string{"Overwrite", "Cancel"}
	}
	parts := make([]string, 0, len(labels))
	for i, label := range labels {
		parts = append(parts, renderDialogButton(label, selected == i))
	}
	gap := modalRowStyle.Render("  ")
	row := strings.Join(parts, gap)
	if lipgloss.Width(row) > width && len(parts) == 3 {
		row = strings.Join(parts, modalRowStyle.Render(" "))
	}
	rowWidth := lipgloss.Width(row)
	leftPad := max(0, (width-rowWidth)/2)
	rightPad := max(0, width-rowWidth-leftPad)
	return modalRowStyle.Render(strings.Repeat(" ", leftPad)) + row + modalRowStyle.Render(strings.Repeat(" ", rightPad))
}

func renderDialogButton(label string, selected bool) string {
	style := dialogButtonStyle
	if selected {
		style = selectedDialogButtonStyle
	}
	return style.Render("[ " + label + " ]")
}

func (m Model) renderThemeModal() string {
	const width = 52
	contentWidth := width - modalStyle.GetHorizontalFrameSize()
	lines := []string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render("Theme editor"),
		fullWidthStyle(modalRowStyle, contentWidth).Render("Select with ↑/↓, type hex to replace/edit"),
	}
	for i, item := range themeItems(m.theme) {
		value := item.value
		if i == m.themeCursor && m.themeDraft != "" {
			value = normalizeHex(m.themeDraft)
		}
		prefix := "  "
		editMarker := " "
		style := modalRowStyle
		if i == m.themeCursor {
			prefix = "> "
			editMarker = "*"
			style = selectedDialogButtonStyle
		}
		if i == m.themeCursor && m.themeEditing {
			editMarker = "E"
		}
		lines = append(lines, fullWidthStyle(style, contentWidth).Render(fmt.Sprintf("%s%s %-13s [%s]", prefix, editMarker, item.name, value)))
	}
	lines = append(lines,
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		renderThemeButtons(contentWidth, m.themeButton),
	)
	height := len(lines) + modalStyle.GetVerticalFrameSize() + modalStyle.GetVerticalPadding()
	return modalStyle.
		Width(width - modalStyle.GetHorizontalBorderSize()).
		Height(height - modalStyle.GetVerticalBorderSize()).
		MaxWidth(width).
		MaxHeight(height).
		Render(strings.Join(lines, "\n"))
}

func (m Model) renderContainerModal() string {
	const width = 52
	contentWidth := width - modalStyle.GetHorizontalFrameSize()
	lines := []string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render("Select container"),
	}
	if m.containerErr != nil {
		lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(truncate(m.containerErr.Error(), contentWidth)))
	} else if len(m.containers) == 0 {
		lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render("Loading containers..."))
	} else {
		visible := min(10, len(m.containers))
		start := 0
		if m.containerCursor >= visible {
			start = m.containerCursor - visible + 1
		}
		for i := 0; i < visible; i++ {
			index := start + i
			style := modalRowStyle
			prefix := "  "
			if index == m.containerCursor && m.containerButton < 0 {
				style = selectedDialogButtonStyle
				prefix = "> "
			}
			lines = append(lines, fullWidthStyle(style, contentWidth).Render(prefix+truncate(m.containers[index], contentWidth-2)))
		}
	}
	for len(lines) < 12 {
		lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(""))
	}
	lines = append(lines,
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		renderDialogButtonsByIndex(contentWidth, m.containerButton, "Select", "Cancel"),
	)
	height := len(lines) + modalStyle.GetVerticalFrameSize() + modalStyle.GetVerticalPadding()
	return modalStyle.
		Width(width - modalStyle.GetHorizontalBorderSize()).
		Height(height - modalStyle.GetVerticalBorderSize()).
		MaxWidth(width).
		MaxHeight(height).
		Render(strings.Join(lines, "\n"))
}

func (m Model) renderHelpModal() string {
	const width = 76
	contentWidth := width - modalStyle.GetHorizontalFrameSize()
	lines := []string{
		fullWidthStyle(modalTitleStyle, contentWidth).Render("Keyboard help"),
	}
	for _, item := range []struct {
		key    string
		action string
	}{
		{"1", "show or close this help"},
		{"Tab", "switch active panel"},
		{"l", "list and switch containers"},
		{"Up/Down or j/k", "move selection"},
		{"PageUp/PageDown", "move by one page"},
		{"Home/End", "move to first or last entry"},
		{"Space", "toggle item selection"},
		{"/", "filter active panel"},
		{"Enter/Right", "open directory"},
		{"Backspace/Left or h", "go to parent"},
		{"c", "copy selected or highlighted item"},
		{"d", "delete selected or highlighted item"},
		{"n", "create directory"},
		{"p", "change pane provider (local/azure/scp)"},
		{"r", "refresh active panel"},
		{"t", "edit theme"},
		{"Esc/q/Ctrl+C", "quit or cancel modal"},
	} {
		line := fmt.Sprintf("%-20s %s", item.key, item.action)
		lines = append(lines, fullWidthStyle(modalRowStyle, contentWidth).Render(line))
	}
	lines = append(lines,
		fullWidthStyle(modalRowStyle, contentWidth).Render(""),
		renderSingleDialogButton(contentWidth, "Close"),
	)
	height := len(lines) + modalStyle.GetVerticalFrameSize() + modalStyle.GetVerticalPadding()
	return modalStyle.
		Width(width - modalStyle.GetHorizontalBorderSize()).
		Height(height - modalStyle.GetVerticalBorderSize()).
		MaxWidth(width).
		MaxHeight(height).
		Render(strings.Join(lines, "\n"))
}

func renderSingleDialogButton(width int, label string) string {
	button := renderDialogButton(label, true)
	buttonWidth := lipgloss.Width(button)
	leftPad := max(0, (width-buttonWidth)/2)
	rightPad := max(0, width-buttonWidth-leftPad)
	return modalRowStyle.Render(strings.Repeat(" ", leftPad)) + button + modalRowStyle.Render(strings.Repeat(" ", rightPad))
}

func renderThemeButtons(width, selected int) string {
	edit := renderDialogButton("Edit", selected == 0)
	save := renderDialogButton("Save", selected == 1)
	closeButton := renderDialogButton("Close", selected == 2)
	gap := modalRowStyle.Render("  ")
	row := edit + gap + save + gap + closeButton
	rowWidth := lipgloss.Width(row)
	leftPad := max(0, (width-rowWidth)/2)
	rightPad := max(0, width-rowWidth-leftPad)
	return modalRowStyle.Render(strings.Repeat(" ", leftPad)) + row + modalRowStyle.Render(strings.Repeat(" ", rightPad))
}

func progressBar(percent float64, width int) string {
	if width <= 0 {
		return ""
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := int((percent / 100) * float64(width))
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

func removeLocalEntry(entry Entry) error {
	if entry.IsDir {
		err := os.RemoveAll(entry.Path)
		if err == nil || !errors.Is(err, os.ErrPermission) {
			return err
		}
		if chmodErr := chmodTreeWritable(entry.Path); chmodErr != nil {
			return err
		}
		return os.RemoveAll(entry.Path)
	}
	if isCurrentExecutable(entry.Path) {
		return fmt.Errorf("cannot delete running executable: %s; exit blober and delete it from the shell", entry.Path)
	}
	err := os.Remove(entry.Path)
	if err == nil || !errors.Is(err, os.ErrPermission) {
		return err
	}
	if chmodErr := os.Chmod(entry.Path, 0o600); chmodErr != nil {
		return err
	}
	return os.Remove(entry.Path)
}

func chmodTreeWritable(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if entry.IsDir() {
			mode = 0o700
		}
		return os.Chmod(path, mode)
	})
}

func isCurrentExecutable(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	exeAbs, err := filepath.Abs(exe)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(exeAbs), filepath.Clean(pathAbs))
}

func cleanupRemoteBlob(ctx context.Context, client *azblob.Client, container, key string) error {
	_, err := client.DeleteBlob(ctx, container, key, nil)
	if err == nil {
		return nil
	}
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) && responseErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (m *Model) clearConfirmation() {
	m.confirming = false
	m.confirmAction = confirmNone
	m.confirmYes = true
	m.pendingSource = 0
	m.pendingCopy = nil
	m.pendingItems = nil
	m.pendingDelete = nil
	m.overwriteConflicts = nil
	m.overwriteAt = 0
	m.overwriteBtn = 0
}

func (m *Model) moveThemeCursor(delta int) {
	count := len(themeItems(m.theme))
	if count == 0 {
		return
	}
	m.themeButton = -1
	m.themeCursor += delta
	if m.themeCursor < 0 {
		m.themeCursor = count - 1
	}
	if m.themeCursor >= count {
		m.themeCursor = 0
	}
	m.themeDraft = themeValue(m.theme, m.themeCursor)
	m.themeEditing = false
}

func (m *Model) moveThemeDown() {
	count := len(themeItems(m.theme))
	if count == 0 {
		return
	}
	if m.themeButton >= 0 {
		return
	}
	if m.themeCursor == count-1 {
		m.themeButton = 0
		m.themeEditing = false
		return
	}
	m.moveThemeCursor(1)
}

func (m Model) activateThemeButton() (Model, tea.Cmd) {
	switch m.themeButton {
	case 0:
		m.themeButton = -1
		m.themeEditing = true
		m.themeDraft = themeValue(m.theme, m.themeCursor)
	case 1:
		if err := SaveThemeFile(".theme.json", m.theme); err != nil {
			m.status = err.Error()
		} else {
			m.status = "saved theme to .theme.json"
		}
	case 2:
		m.showingTheme = false
		m.themeButton = -1
	}
	return m, nil
}

func (m *Model) editThemeDraft(deleteCount int, r rune) {
	draft := m.themeDraft
	if !m.themeEditing {
		if deleteCount < 0 {
			draft = themeValue(m.theme, m.themeCursor)
		} else {
			draft = "#"
		}
		m.themeEditing = true
	}
	if deleteCount < 0 {
		if len(draft) > 0 {
			draft = draft[:len(draft)-1]
		}
	} else if isHexInputRune(r) || r == '#' {
		draft += string(r)
	}
	draft = normalizeHex(draft)
	if len(draft) > 7 {
		draft = draft[:7]
	}
	m.themeDraft = draft
	if isRGBHex(draft) {
		setThemeValue(&m.theme, m.themeCursor, draft)
		applyTheme(m.theme)
	}
}

func isHexInputRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func (m *Model) moveCursor(delta int) {
	p := &m.panels[m.active]
	if len(p.entries) == 0 {
		return
	}
	m.moveCursorTo(p.cursor + delta)
}

func (m *Model) moveContainerCursor(delta int) {
	if len(m.containers) == 0 {
		m.containerButton = 0
		return
	}
	if m.containerButton >= 0 {
		if delta < 0 {
			m.containerButton = -1
			m.containerCursor = len(m.containers) - 1
		}
		return
	}
	m.containerCursor += delta
	if m.containerCursor < 0 {
		m.containerCursor = 0
	}
	if m.containerCursor >= len(m.containers) {
		m.containerCursor = len(m.containers) - 1
		m.containerButton = 0
	}
}

func (m Model) selectContainer() (Model, tea.Cmd) {
	if len(m.containers) == 0 {
		m.status = "no container selected"
		return m, nil
	}
	m.container = m.containers[m.containerCursor]
	m.panels[0] = newRemotePanel("")
	m.panels[0].provider = m.makeAzureProvider()
	m.showingContainers = false
	m.containerButton = -1
	m.status = "selected container " + m.container
	return m, m.listCmd(0)
}

func (m *Model) moveCursorTo(index int) {
	p := &m.panels[m.active]
	if len(p.entries) == 0 {
		return
	}
	p.cursor = index
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= len(p.entries) {
		p.cursor = len(p.entries) - 1
	}
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	visible := m.panePageSize()
	if p.cursor >= p.offset+visible {
		p.offset = p.cursor - visible + 1
	}
}

func (m *Model) clampPanelViewport(index int) {
	if index < 0 || index >= len(m.panels) {
		return
	}
	p := &m.panels[index]
	if len(p.entries) == 0 {
		p.cursor = 0
		p.offset = 0
		return
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor >= len(p.entries) {
		p.cursor = len(p.entries) - 1
	}
	if p.offset < 0 || p.cursor < p.offset {
		p.offset = p.cursor
	}
	visible := m.panePageSizeFor(index)
	if p.cursor >= p.offset+visible {
		p.offset = p.cursor - visible + 1
	}
	maxOffset := max(0, len(p.entries)-visible)
	if p.offset > maxOffset {
		p.offset = maxOffset
	}
}

func (m Model) panePageSize() int {
	return m.panePageSizeFor(m.active)
}

func (m Model) panePageSizeFor(index int) int {
	screenHeight := m.height
	if screenHeight <= 0 {
		screenHeight = 24
	}
	style := inactivePanelStyle
	if index == 0 || index == 1 {
		style = activePanelStyle
	}
	outerHeight := max(1, screenHeight-2)
	blockHeight := max(1, outerHeight-style.GetVerticalBorderSize())
	textHeight := max(1, blockHeight-style.GetVerticalPadding())
	headerLines := 1
	if m.panels[index].err != nil {
		headerLines++
	}
	if headerLines < textHeight {
		headerLines++
	}
	return max(1, textHeight-headerLines)
}

func (m *Model) selectEntry(index int, path string) bool {
	if index < 0 || index >= len(m.panels) {
		return false
	}
	p := &m.panels[index]
	for i, entry := range p.entries {
		if entry.Path == path {
			p.cursor = i
			if p.cursor < p.offset {
				p.offset = p.cursor
			}
			visible := m.panePageSizeFor(index)
			if p.cursor >= p.offset+visible {
				p.offset = p.cursor - visible + 1
			}
			return true
		}
	}
	return false
}

func (m *Model) applyPanelFilter(index int, filter string) {
	if index < 0 || index >= len(m.panels) {
		return
	}
	current, hasCurrent := m.panels[index].current()
	m.panels[index].setFilter(filter)
	if hasCurrent && m.selectEntry(index, current.Path) {
		return
	}
	m.panels[index].cursor = 0
	m.panels[index].offset = 0
	m.clampPanelViewport(index)
}

func (m Model) updateFilter(msg tea.KeyMsg) (Model, tea.Cmd) {
	index := m.filterPanel
	if index < 0 || index >= len(m.panels) {
		m.filtering = false
		m.filterPanel = -1
		return m, nil
	}
	switch msg.String() {
	case "esc", "ctrl+c":
		m.applyPanelFilter(index, m.filterOriginal)
		m.filtering = false
		m.filterPanel = -1
		m.filterOriginal = ""
	case "enter":
		m.filtering = false
		m.filterPanel = -1
		m.filterOriginal = ""
	default:
		filter := m.panels[index].filter
		if msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH {
			runes := []rune(filter)
			if len(runes) > 0 {
				filter = string(runes[:len(runes)-1])
			}
		} else if len(msg.Runes) > 0 {
			filter += string(msg.Runes)
		}
		m.applyPanelFilter(index, filter)
	}
	return m, nil
}

func (m *Model) clearPanelFilter(index int) {
	if index < 0 || index >= len(m.panels) {
		return
	}
	if m.panels[index].filter != "" {
		m.applyPanelFilter(index, "")
	}
	if m.filtering && m.filterPanel == index {
		m.filtering = false
		m.filterPanel = -1
		m.filterOriginal = ""
	}
}

func (m *Model) rememberSelection(index int) {
	if index < 0 || index >= len(m.panels) {
		return
	}
	entry, ok := m.panels[index].current()
	if !ok {
		return
	}
	if m.rememberedSelect[index] == nil {
		m.rememberedSelect[index] = map[string]string{}
	}
	m.rememberedSelect[index][m.panels[index].location] = entry.Path
}

func (m *Model) restoreRememberedSelection(index int, location string) {
	if index < 0 || index >= len(m.panels) || m.rememberedSelect[index] == nil {
		return
	}
	path, ok := m.rememberedSelect[index][location]
	if ok {
		m.setPendingSelect(index, path)
	}
}

func (m *Model) setPendingSelect(index int, path string) {
	if index < 0 || index >= len(m.panels) {
		return
	}
	m.pendingSelect[index] = path
	m.pendingSelectSet[index] = true
}

// resetPanelContent clears the cached listing, selections and pending state for
// a pane so that stale entries from a previous provider can never be acted on
// before the new provider's listing arrives.
func (m *Model) resetPanelContent(index int) {
	if index < 0 || index >= len(m.panels) {
		return
	}
	p := &m.panels[index]
	p.entries = nil
	p.allEntries = nil
	p.filter = ""
	p.err = nil
	p.cursor = 0
	p.offset = 0
	p.selected = map[string]Entry{}
	m.pendingSelect[index] = ""
	m.pendingSelectSet[index] = false
	m.rememberedSelect[index] = nil
}

func (m Model) openCurrent() (Model, tea.Cmd) {
	p := &m.panels[m.active]
	entry, ok := p.current()
	if !ok || !entry.IsDir {
		return m, nil
	}
	m.rememberSelection(m.active)
	p.location = entry.Path
	p.cursor = 0
	p.offset = 0
	m.clearPanelFilter(m.active)
	m.restoreRememberedSelection(m.active, p.location)
	return m, m.listCmd(m.active)
}

func (m Model) openParent() (Model, tea.Cmd) {
	p := &m.panels[m.active]
	child := p.location
	parent, ok := m.resolveProvider(*p).Parent(p.location)
	if !ok {
		return m, nil
	}
	m.rememberSelection(m.active)
	p.location = parent
	p.cursor = 0
	p.offset = 0
	m.clearPanelFilter(m.active)
	m.setPendingSelect(m.active, child)
	return m, m.listCmd(m.active)
}

func (m Model) startCopy() (Model, tea.Cmd) {
	sources := m.panels[m.active].copySources()
	if len(sources) == 0 {
		m.status = "select a file or directory to copy"
		return m, nil
	}
	if m.force || copyHasDirectories(sources) {
		m.confirming = true
		m.confirmAction = confirmCopy
		m.confirmYes = true
		m.pendingSource = m.active
		m.pendingCopy = sources
		if copyHasDirectories(sources) {
			m.status = fmt.Sprintf("confirm recursive copy of %d item(s)", len(sources))
		} else {
			m.status = fmt.Sprintf("overwrite existing targets if needed? Enter confirms (%d file(s))", len(sources))
		}
		return m, nil
	}
	return m.checkOverwriteConflicts(m.active, sources)
}

func (m Model) beginCopy(sourceIndex int, sources []Entry) (Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	items, err := m.expandCopySources(ctx, m.panels[sourceIndex], sources)
	if err != nil {
		cancel()
		m.clearConfirmation()
		m.status = err.Error()
		return m, nil
	}
	return m.beginCopyItemsWithCancel(ctx, cancel, sourceIndex, items, m.force)
}

func (m Model) beginCopyItems(sourceIndex int, items []copyItem, overwrite bool) (Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	return m.beginCopyItemsWithCancel(ctx, cancel, sourceIndex, items, overwrite)
}

func (m Model) beginCopyItemsWithCancel(ctx context.Context, cancel context.CancelFunc, sourceIndex int, items []copyItem, overwrite bool) (Model, tea.Cmd) {
	if len(items) == 0 {
		cancel()
		m.clearConfirmation()
		m.status = "nothing to copy"
		return m, nil
	}
	m.copying = true
	m.copyProgress = transferProgress{File: items[0].targetRel, BatchTotal: totalCopySize(items)}
	m.progressCh = make(chan tea.Msg, 32)
	m.copyCancel = cancel
	m.clearConfirmation()
	return m, tea.Batch(waitProgress(m.progressCh), m.copyCmd(ctx, sourceIndex, items, overwrite, m.progressCh))
}

func (m Model) startDelete() (Model, tea.Cmd) {
	sources := m.panels[m.active].deleteSources()
	if len(sources) == 0 {
		m.status = "select a file or directory to delete"
		return m, nil
	}
	m.confirming = true
	m.confirmAction = confirmDelete
	m.confirmYes = true
	m.pendingSource = m.active
	m.pendingDelete = sources
	m.status = fmt.Sprintf("confirm delete of %d file(s)", len(sources))
	return m, nil
}

func (m Model) beginDelete(sourceIndex int, sources []Entry) (Model, tea.Cmd) {
	m.clearConfirmation()
	return m, m.deleteCmd(sourceIndex, sources)
}

func (m Model) createDirectory() (Model, tea.Cmd) {
	name := strings.TrimSpace(m.newDirName)
	if err := validateNewDirName(name); err != nil {
		m.status = err.Error()
		return m, nil
	}
	m.creatingDir = false
	m.createYes = true
	m.newDirName = ""
	p := &m.panels[m.active]
	prov := m.resolveProvider(*p)
	if prov.Virtual() {
		child, _, err := prov.Mkdir(context.Background(), p.location, name)
		if err != nil {
			m.status = err.Error()
			return m, nil
		}
		p.location = child
		p.cursor = 0
		p.offset = 0
		m.status = "created virtual directory " + name
		return m, m.listCmd(m.active)
	}
	return m, m.mkdirCmd(m.active, name)
}

func (m Model) acceptConfirmation() (Model, tea.Cmd) {
	m.confirming = false
	switch m.confirmAction {
	case confirmCopy:
		if !m.force {
			return m.checkOverwriteConflicts(m.pendingSource, m.pendingCopy)
		}
		return m.beginCopy(m.pendingSource, m.pendingCopy)
	case confirmDelete:
		return m.beginDelete(m.pendingSource, m.pendingDelete)
	default:
		m.clearConfirmation()
		return m, nil
	}
}

func (m Model) checkOverwriteConflicts(sourceIndex int, sources []Entry) (Model, tea.Cmd) {
	m.status = "checking overwrite conflicts..."
	return m, m.checkOverwriteConflictsCmd(sourceIndex, sources)
}

func (m *Model) cancelConfirmation() {
	action := m.confirmAction
	m.clearConfirmation()
	if action == confirmDelete {
		m.status = "delete cancelled"
	} else {
		m.status = "copy cancelled"
	}
}

func (m Model) acceptOverwriteChoice() (Model, tea.Cmd) {
	count := m.overwriteButtonCount()
	cancelButton := count - 1
	if m.overwriteBtn == cancelButton {
		m.cancelConfirmation()
		return m, nil
	}
	if count == 3 && m.overwriteBtn == 1 {
		return m.beginCopyItems(m.pendingSource, m.pendingItems, true)
	}
	m.overwriteAt++
	if m.overwriteAt >= len(m.overwriteConflicts) {
		return m.beginCopyItems(m.pendingSource, m.pendingItems, true)
	}
	m.overwriteBtn = 0
	m.status = fmt.Sprintf("destination exists: %s", m.overwriteTarget())
	return m, nil
}

func (m *Model) moveOverwriteButton(delta int) {
	count := m.overwriteButtonCount()
	if count <= 0 {
		m.overwriteBtn = 0
		return
	}
	m.overwriteBtn = (m.overwriteBtn + delta + count) % count
}

func (m Model) overwriteButtonCount() int {
	if len(m.overwriteConflicts) > 1 {
		return 3
	}
	return 2
}

func (m Model) overwriteTarget() string {
	if m.overwriteAt < 0 || m.overwriteAt >= len(m.overwriteConflicts) {
		return "(unknown target)"
	}
	index := m.overwriteConflicts[m.overwriteAt]
	if index < 0 || index >= len(m.pendingItems) {
		return "(unknown target)"
	}
	return m.copyTargetDisplay(m.pendingItems[index])
}

func (m Model) listCmd(index int) tea.Cmd {
	p := m.panels[index]
	prov := m.resolveProvider(p)
	gen := p.gen
	location := p.location
	return func() tea.Msg {
		entries, err := prov.List(context.Background(), location)
		return entriesLoadedMsg{index: index, gen: gen, entries: entries, err: err}
	}
}

func (m Model) listContainersCmd() tea.Cmd {
	return func() tea.Msg {
		if m.client == nil {
			return containersLoadedMsg{err: errors.New("azure client is not configured")}
		}
		containers, err := azure_storage.ListContainers(context.Background(), m.client)
		if err != nil {
			return containersLoadedMsg{err: err}
		}
		names := make([]string, 0, len(containers))
		for _, container := range containers {
			names = append(names, container.Name)
		}
		return containersLoadedMsg{containers: names}
	}
}

func (m Model) mkdirCmd(index int, name string) tea.Cmd {
	p := m.panels[index]
	prov := m.resolveProvider(p)
	location := p.location
	return func() tea.Msg {
		_, _, err := prov.Mkdir(context.Background(), location, name)
		return mkdirDoneMsg{index: index, name: name, err: err}
	}
}

func validateNewDirName(name string) error {
	if name == "" {
		return errors.New("directory name is required")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid directory name: %s", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("directory name must not contain path separators: %s", name)
	}
	return nil
}

func (m Model) checkOverwriteConflictsCmd(sourceIndex int, sources []Entry) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		source := m.panels[sourceIndex]
		target := m.panels[1-sourceIndex]
		items, err := m.expandCopySources(ctx, source, sources)
		if err != nil {
			return overwriteCheckedMsg{source: sourceIndex, err: err}
		}
		conflicts := make([]int, 0)
		for i, item := range items {
			exists, err := m.copyTargetExists(ctx, source, target, item)
			if err != nil {
				return overwriteCheckedMsg{source: sourceIndex, err: err}
			}
			if exists {
				conflicts = append(conflicts, i)
			}
		}
		return overwriteCheckedMsg{source: sourceIndex, items: items, conflicts: conflicts}
	}
}

func (m Model) copyTargetExists(ctx context.Context, source, target panel, item copyItem) (bool, error) {
	prov := m.resolveProvider(target)
	dst, err := prov.Join(target.location, item.targetRel)
	if err != nil {
		return false, err
	}
	return prov.Exists(ctx, dst)
}

func (m Model) copyTargetDisplay(item copyItem) string {
	return m.copyTargetDisplayFor(m.pendingSource, item)
}

func (m Model) copyTargetDisplayFor(sourceIndex int, item copyItem) string {
	target := m.panels[1-sourceIndex]
	dst, err := m.resolveProvider(target).Join(target.location, item.targetRel)
	if err != nil {
		return item.targetRel
	}
	return dst
}

func (m Model) copyCmd(ctx context.Context, sourceIndex int, items []copyItem, overwrite bool, progressCh chan tea.Msg) tea.Cmd {
	source := m.panels[sourceIndex]
	target := m.panels[1-sourceIndex]
	srcProv := m.resolveProvider(source)
	dstProv := m.resolveProvider(target)
	return func() tea.Msg {
		if !overwrite {
			for i, item := range items {
				exists, err := m.copyTargetExists(ctx, source, target, item)
				if err != nil {
					close(progressCh)
					return copyDoneMsg{source: sourceIndex, err: err}
				}
				if exists {
					close(progressCh)
					return copyDoneMsg{source: sourceIndex, err: fmt.Errorf("destination exists: %s", m.copyTargetDisplayFor(sourceIndex, items[i]))}
				}
			}
		}
		var batchDone int64
		batchTotal := totalCopySize(items)
		for _, item := range items {
			completedBeforeFile := batchDone
			reporter := newProgressThrottler(item.targetRel, completedBeforeFile, batchTotal, progressCh, time.Now)
			report := reporter.report

			if err := m.copyOne(ctx, srcProv, dstProv, target.location, item, report); err != nil {
				close(progressCh)
				return copyDoneMsg{source: sourceIndex, err: err}
			}
			batchDone += item.entry.Size
		}
		close(progressCh)
		return copyDoneMsg{source: sourceIndex}
	}
}

// copyOne streams a single file from the source provider to the destination
// provider, reporting progress as bytes are read.
func (m Model) copyOne(ctx context.Context, srcProv, dstProv Provider, targetLocation string, item copyItem, report func(done, total int64)) error {
	dst, err := dstProv.Join(targetLocation, item.targetRel)
	if err != nil {
		return err
	}
	// Only a destination that did not already exist may be cleaned up on
	// cancellation; otherwise we would delete a pre-existing file the user is
	// overwriting.
	preExisting, _ := dstProv.Exists(ctx, dst)
	rc, size, err := srcProv.Open(ctx, item.entry.Path)
	if err != nil {
		return err
	}
	if size < 0 {
		size = item.entry.Size
	}
	report(0, size)
	reader := &progressReader{ctx: ctx, r: rc, report: func(done int64) { report(done, size) }}
	createErr := dstProv.Create(ctx, dst, size, reader)
	closeErr := rc.Close()
	if createErr != nil {
		if (errors.Is(createErr, context.Canceled) || ctx.Err() != nil) && !preExisting {
			// Best-effort cleanup of a partially written, newly created target.
			_ = dstProv.Remove(context.Background(), Entry{Path: dst})
		}
		return createErr
	}
	if closeErr != nil {
		return closeErr
	}
	report(size, size)
	return nil
}

func (m Model) expandCopySources(ctx context.Context, source panel, sources []Entry) ([]copyItem, error) {
	prov := m.resolveProvider(source)
	var items []copyItem
	for _, entry := range sources {
		if !entry.IsDir {
			items = append(items, copyItem{entry: entry, targetRel: copyTargetName(entry)})
			continue
		}
		walked, err := prov.Walk(ctx, entry)
		if err != nil {
			return nil, err
		}
		for _, item := range walked {
			items = append(items, copyItem{
				entry: Entry{
					Name:         item.Rel,
					Path:         item.Path,
					Size:         item.Size,
					LastModified: item.ModTime,
				},
				targetRel: item.Rel,
			})
		}
	}
	return items, nil
}

type progressThrottler struct {
	mu                  sync.Mutex
	file                string
	completedBeforeFile int64
	batchTotal          int64
	ch                  chan<- tea.Msg
	now                 func() time.Time
	lastEmit            time.Time
	lastDone            int64
}

func newProgressThrottler(file string, completedBeforeFile, batchTotal int64, ch chan<- tea.Msg, now func() time.Time) *progressThrottler {
	return &progressThrottler{
		file:                file,
		completedBeforeFile: completedBeforeFile,
		batchTotal:          batchTotal,
		ch:                  ch,
		now:                 now,
	}
}

func (p *progressThrottler) report(done, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	final := total > 0 && done >= total
	if !p.lastEmit.IsZero() && now.Sub(p.lastEmit) < progressUpdateInterval && !final {
		return
	}
	var speed float64
	if !p.lastEmit.IsZero() {
		elapsed := now.Sub(p.lastEmit).Seconds()
		if elapsed > 0 {
			speed = float64(max(0, done-p.lastDone)) / elapsed
		}
	}
	p.lastEmit = now
	p.lastDone = done
	p.ch <- transferProgress{
		File:       p.file,
		FileDone:   done,
		FileTotal:  total,
		BatchDone:  p.completedBeforeFile + done,
		BatchTotal: p.batchTotal,
		SpeedBps:   speed,
	}
}

func copyTargetName(entry Entry) string {
	name := strings.TrimRight(entry.Name, "/\\")
	if name == "" {
		name = azure_storage.BlobBaseName(entry.Path)
	}
	if name == "" {
		name = filepath.Base(entry.Path)
	}
	return name
}

func copyHasDirectories(entries []Entry) bool {
	for _, entry := range entries {
		if entry.IsDir {
			return true
		}
	}
	return false
}

func (m Model) deleteCmd(sourceIndex int, sources []Entry) tea.Cmd {
	prov := m.resolveProvider(m.panels[sourceIndex])
	return func() tea.Msg {
		for _, entry := range sources {
			if err := prov.Remove(context.Background(), entry); err != nil {
				return deleteDoneMsg{source: sourceIndex, err: err}
			}
		}
		return deleteDoneMsg{source: sourceIndex, count: len(sources)}
	}
}

func remoteBlobExists(ctx context.Context, client *azblob.Client, container, key string) (bool, error) {
	_, err := client.ServiceClient().NewContainerClient(container).NewBlobClient(key).GetProperties(ctx, nil)
	if err == nil {
		return true, nil
	}
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) && responseErr.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func deleteRemotePrefix(ctx context.Context, client *azblob.Client, container, prefix string) error {
	blobs, err := azure_storage.ListBlobs(ctx, client, container, prefix)
	if err != nil {
		return err
	}
	for _, blob := range blobs {
		if err := cleanupRemoteBlob(ctx, client, container, blob.Key); err != nil {
			return err
		}
	}
	return nil
}

func waitProgress(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func totalSize(entries []Entry) int64 {
	var total int64
	for _, entry := range entries {
		total += entry.Size
	}
	return total
}

func totalCopySize(items []copyItem) int64 {
	var total int64
	for _, item := range items {
		total += item.entry.Size
	}
	return total
}

func percent(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return (float64(done) / float64(total)) * 100
}

func truncate(s string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}
	if width == 1 {
		return "."
	}
	return s[:width-1] + "."
}

func fullWidthStyle(style lipgloss.Style, width int) lipgloss.Style {
	return style.Width(width).MaxWidth(width)
}

func overlayCentered(base, overlay string, width, height int) string {
	baseLines := normalizeBlock(base, width, height)
	overlayLines := strings.Split(overlay, "\n")
	overlayHeight := len(overlayLines)
	overlayWidth := 0
	for _, line := range overlayLines {
		overlayWidth = max(overlayWidth, lipgloss.Width(line))
	}
	startRow := max(0, (height-overlayHeight)/2)
	startCol := max(0, (width-overlayWidth)/2)
	shadowWidth := min(overlayWidth, max(0, width-startCol-1))
	if shadowWidth > 0 && startRow+overlayHeight < len(baseLines) {
		baseLines[startRow+overlayHeight] = replaceDisplaySpan(baseLines[startRow+overlayHeight], modalShadowStyle.Render(strings.Repeat(" ", shadowWidth)), startCol+1, width)
	}
	shadowCol := startCol + overlayWidth
	if shadowCol < width {
		for i := 1; i <= overlayHeight; i++ {
			row := startRow + i
			if row >= len(baseLines) {
				break
			}
			baseLines[row] = replaceDisplaySpan(baseLines[row], modalShadowStyle.Render(" "), shadowCol, width)
		}
	}
	for i, overlayLine := range overlayLines {
		row := startRow + i
		if row >= len(baseLines) {
			break
		}
		baseLines[row] = replaceDisplaySpan(baseLines[row], overlayLine, startCol, width)
	}
	return strings.Join(baseLines, "\n")
}

func normalizeBlock(block string, width, height int) []string {
	rawLines := strings.Split(block, "\n")
	lines := make([]string, height)
	for i := 0; i < height; i++ {
		if i < len(rawLines) {
			lines[i] = padDisplayWidth(rawLines[i], width)
		} else {
			lines[i] = fullWidthStyle(rowStyle, width).Render("")
		}
	}
	return lines
}

func replaceDisplaySpan(base, replacement string, startCol, width int) string {
	left := ansi.Cut(base, 0, startCol)
	rightStart := startCol + lipgloss.Width(replacement)
	right := ansi.Cut(base, rightStart, width)
	return padDisplayWidth(left+replacement+right, width)
}

func padDisplayWidth(s string, width int) string {
	if lipgloss.Width(s) >= width {
		return ansi.Cut(s, 0, width)
	}
	return s + fullWidthStyle(rowStyle, width-lipgloss.Width(s)).Render("")
}

func renderScreen(content string, width, height int) string {
	return rootStyle.
		Width(width).
		Height(height).
		MaxWidth(width).
		MaxHeight(height).
		Render(content)
}

type themeItem struct {
	name  string
	value string
}

func themeItems(theme Theme) []themeItem {
	theme = normalizeTheme(theme)
	return []themeItem{
		{"background", theme.Background},
		{"highlight", theme.Highlight},
		{"text", theme.Text},
		{"header_text", theme.HeaderText},
		{"selected", theme.Selected},
		{"error", theme.Error},
		{"modal_border", theme.ModalBorder},
		{"modal_background", theme.ModalBackground},
		{"modal_shadow", theme.ModalShadow},
	}
}

func themeValue(theme Theme, index int) string {
	items := themeItems(theme)
	if index < 0 || index >= len(items) {
		return ""
	}
	return items[index].value
}

func setThemeValue(theme *Theme, index int, value string) {
	value = normalizeHex(value)
	switch index {
	case 0:
		theme.Background = value
	case 1:
		theme.Highlight = value
	case 2:
		theme.Text = value
	case 3:
		theme.HeaderText = value
	case 4:
		theme.Selected = value
	case 5:
		theme.Error = value
	case 6:
		theme.ModalBorder = value
	case 7:
		theme.ModalBackground = value
	case 8:
		theme.ModalShadow = value
	}
}

func normalizeTheme(theme Theme) Theme {
	defaults := DefaultTheme()
	if theme.Background == "" {
		theme.Background = defaults.Background
	}
	if theme.Highlight == "" {
		theme.Highlight = defaults.Highlight
	}
	if theme.Text == "" {
		theme.Text = defaults.Text
	}
	if theme.HeaderText == "" {
		theme.HeaderText = defaults.HeaderText
	}
	if theme.Selected == "" {
		theme.Selected = defaults.Selected
	}
	if theme.Error == "" {
		theme.Error = defaults.Error
	}
	if theme.ModalBorder == "" {
		theme.ModalBorder = defaults.ModalBorder
	}
	if theme.ModalBackground == "" {
		theme.ModalBackground = defaults.ModalBackground
	}
	if theme.ModalShadow == "" {
		theme.ModalShadow = defaults.ModalShadow
	}
	theme.Background = normalizeHex(theme.Background)
	theme.Highlight = normalizeHex(theme.Highlight)
	theme.Text = normalizeHex(theme.Text)
	theme.HeaderText = normalizeHex(theme.HeaderText)
	theme.Selected = normalizeHex(theme.Selected)
	theme.Error = normalizeHex(theme.Error)
	theme.ModalBorder = normalizeHex(theme.ModalBorder)
	theme.ModalBackground = normalizeHex(theme.ModalBackground)
	theme.ModalShadow = normalizeHex(theme.ModalShadow)
	return theme
}

func normalizeHex(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	if !strings.HasPrefix(value, "#") {
		value = "#" + value
	}
	return strings.ToUpper(value)
}

func validateTheme(theme Theme) error {
	for _, item := range themeItems(theme) {
		if !isRGBHex(item.value) {
			return fmt.Errorf("theme color %s must be #RRGGBB, got %q", item.name, item.value)
		}
	}
	return nil
}

func isRGBHex(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	for _, r := range value[1:] {
		if (r < '0' || r > '9') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func applyTheme(theme Theme) {
	theme = normalizeTheme(theme)
	mcBlue := lipgloss.Color(theme.Background)
	mcLightBlue := lipgloss.Color(theme.Highlight)
	mcGray := lipgloss.Color(theme.Text)
	mcBlack := lipgloss.Color(theme.HeaderText)
	mcBrightYellow := lipgloss.Color(theme.Selected)
	mcError := lipgloss.Color(theme.Error)
	mcModalBorder := lipgloss.Color(theme.ModalBorder)
	mcModalBackground := lipgloss.Color(theme.ModalBackground)
	mcModalShadow := lipgloss.Color(theme.ModalShadow)

	rootStyle = lipgloss.NewStyle().Background(mcBlue).Foreground(mcGray).ColorWhitespace(true)
	activePanelStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(mcGray).BorderBackground(mcBlue).Background(mcBlue).Foreground(mcGray).ColorWhitespace(true).Padding(0, 1)
	inactivePanelStyle = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(mcGray).BorderBackground(mcBlue).Background(mcBlue).Foreground(mcGray).ColorWhitespace(true).Padding(0, 1)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(mcBlack).Background(mcLightBlue).ColorWhitespace(true)
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(mcGray).Background(mcBlue).ColorWhitespace(true)
	rowStyle = lipgloss.NewStyle().Foreground(mcGray).Background(mcBlue).ColorWhitespace(true)
	cursorStyle = lipgloss.NewStyle().Background(mcLightBlue).Foreground(mcBlack).ColorWhitespace(true)
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(mcBrightYellow).Background(mcBlue).ColorWhitespace(true)
	selectedCursorStyle = lipgloss.NewStyle().Bold(true).Foreground(mcBrightYellow).Background(mcLightBlue).ColorWhitespace(true)
	statusStyle = lipgloss.NewStyle().Foreground(mcBlack).Background(mcLightBlue).ColorWhitespace(true)
	filterStatusStyle = lipgloss.NewStyle().Bold(true).Foreground(mcBlack).Background(mcBrightYellow).ColorWhitespace(true)
	errorStyle = lipgloss.NewStyle().Foreground(mcError).Background(mcBlue).ColorWhitespace(true)
	modalTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(mcBlack).Background(mcModalBackground).ColorWhitespace(true)
	modalRowStyle = lipgloss.NewStyle().Foreground(mcBlack).Background(mcModalBackground).ColorWhitespace(true)
	modalCursorStyle = lipgloss.NewStyle().Foreground(mcModalBackground).Background(mcBlack).ColorWhitespace(true)
	dialogButtonStyle = lipgloss.NewStyle().Foreground(mcBlack).Background(mcModalBackground).ColorWhitespace(true)
	selectedDialogButtonStyle = lipgloss.NewStyle().Bold(true).Foreground(mcBlack).Background(mcLightBlue).ColorWhitespace(true)
	cancelButtonStyle = lipgloss.NewStyle().Bold(true).Foreground(mcBlack).Background(mcLightBlue).ColorWhitespace(true).Align(lipgloss.Center)
	confirmButtonStyle = lipgloss.NewStyle().Bold(true).Foreground(mcBlack).Background(mcLightBlue).ColorWhitespace(true).Align(lipgloss.Center)
	modalShadowStyle = lipgloss.NewStyle().Background(mcModalShadow).Foreground(mcModalShadow).ColorWhitespace(true)
	modalStyle = lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(mcModalBorder).
		BorderBackground(mcModalBackground).
		Background(mcModalBackground).
		Foreground(mcBlack).
		ColorWhitespace(true).
		Padding(1, 2)
}

var (
	rootStyle                 lipgloss.Style
	activePanelStyle          lipgloss.Style
	inactivePanelStyle        lipgloss.Style
	headerStyle               lipgloss.Style
	titleStyle                lipgloss.Style
	rowStyle                  lipgloss.Style
	cursorStyle               lipgloss.Style
	selectedStyle             lipgloss.Style
	selectedCursorStyle       lipgloss.Style
	statusStyle               lipgloss.Style
	filterStatusStyle         lipgloss.Style
	errorStyle                lipgloss.Style
	modalTitleStyle           lipgloss.Style
	modalRowStyle             lipgloss.Style
	modalCursorStyle          lipgloss.Style
	dialogButtonStyle         lipgloss.Style
	selectedDialogButtonStyle lipgloss.Style
	cancelButtonStyle         lipgloss.Style
	confirmButtonStyle        lipgloss.Style
	modalShadowStyle          lipgloss.Style
	modalStyle                lipgloss.Style
)

func init() {
	applyTheme(DefaultTheme())
}
