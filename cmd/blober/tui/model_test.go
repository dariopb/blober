package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func entryNames(entries []Entry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

func TestSpaceTogglesSelectedFile(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{{Name: "a.txt", Path: "a.txt", Size: 10}}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(Model)
	if _, ok := m.panels[0].selected["a.txt"]; !ok {
		t.Fatal("expected file to be selected")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(Model)
	if _, ok := m.panels[0].selected["a.txt"]; ok {
		t.Fatal("expected file to be unselected")
	}
}

func TestSpaceTogglesSelectedDirectory(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{{Name: "dir/", Path: "dir", IsDir: true}}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(Model)
	if _, ok := m.panels[0].selected["dir"]; !ok {
		t.Fatal("expected directory to be selected")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(Model)
	if _, ok := m.panels[0].selected["dir"]; ok {
		t.Fatal("expected directory to be unselected")
	}
}

func TestSpaceDoesNotSelectParentDirectory(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{{Name: "..", Path: "", IsDir: true, parent: true}}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(Model)
	if len(m.panels[0].selected) != 0 {
		t.Fatalf("parent entry should not be selected: %#v", m.panels[0].selected)
	}
	if !strings.Contains(m.status, "parent directory selection is not supported") {
		t.Fatalf("status = %q, want parent selection warning", m.status)
	}
}

func TestTabSwitchesActivePanel(t *testing.T) {
	m := New(Config{})
	if m.active != 0 {
		t.Fatalf("active = %d, want 0", m.active)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.active != 1 {
		t.Fatalf("active = %d, want 1", m.active)
	}
}

func TestStatusBarShowsHelpShortcut(t *testing.T) {
	m := New(Config{})
	if m.status != helpHint {
		t.Fatalf("status = %q, want help shortcut only", m.status)
	}
	if got := m.View(); !strings.Contains(got, helpHint) {
		t.Fatalf("view missing help shortcut:\n%s", got)
	}

	m.panels[0].entries = []Entry{{Name: "a.txt", Path: "a.txt", Size: 10}}
	m.status = "copied 1 file"
	got := m.View()
	for _, want := range []string{helpHint, "Remote | a.txt | 10 bytes", "copied 1 file"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status bar missing %q:\n%s", want, got)
		}
	}
}

func TestOneOpensHelpModal(t *testing.T) {
	m := New(Config{})

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = updated.(Model)
	if cmd != nil || !m.showingHelp {
		t.Fatalf("expected help modal, cmd=%v showingHelp=%v", cmd, m.showingHelp)
	}
	got := m.View()
	for _, want := range []string{"Keyboard help", "Tab                  switch active panel", "c                    copy selected or highlighted item", "[ Close ]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("help modal missing %q:\n%s", want, got)
		}
	}

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = updated.(Model)
	if cmd != nil || m.showingHelp {
		t.Fatalf("expected help modal closed, cmd=%v showingHelp=%v", cmd, m.showingHelp)
	}
}

func TestContainerModalSelectsContainer(t *testing.T) {
	m := New(Config{AccountName: "acct"})
	m.containers = []string{"alpha", "beta"}
	if !m.showingContainers {
		t.Fatal("expected startup container picker when no container is configured")
	}

	got := m.View()
	for _, want := range []string{"Select container", "alpha", "[ Select ]", "[ Cancel ]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("container modal missing %q:\n%s", want, got)
		}
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.containerCursor != 1 {
		t.Fatalf("containerCursor = %d, want 1", m.containerCursor)
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil || m.showingContainers || m.container != "beta" {
		t.Fatalf("container selection failed: cmd nil=%v showing=%v container=%q", cmd == nil, m.showingContainers, m.container)
	}
}

func TestContainerModalCancelKeepsExistingContainer(t *testing.T) {
	m := New(Config{Container: "alpha"})
	m.showingContainers = true
	m.containers = []string{"alpha", "beta"}
	m.containerButton = 1

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || m.showingContainers || m.container != "alpha" {
		t.Fatalf("cancel failed: cmd=%v showing=%v container=%q", cmd, m.showingContainers, m.container)
	}
}

func TestCopySourcesUsesSelection(t *testing.T) {
	p := panel{
		entries: []Entry{
			{Name: "a.txt", Path: "a.txt", Size: 1},
			{Name: "b.txt", Path: "b.txt", Size: 2},
		},
		selected: map[string]Entry{
			"b.txt": {Name: "b.txt", Path: "b.txt", Size: 2},
		},
	}
	sources := p.copySources()
	if len(sources) != 1 || sources[0].Name != "b.txt" {
		t.Fatalf("sources = %#v, want selected b.txt", sources)
	}
}

func TestCopySourcesAllowsHighlightedDirectory(t *testing.T) {
	p := panel{
		entries:  []Entry{{Name: "dir/", Path: "dir", IsDir: true}},
		selected: map[string]Entry{},
	}
	sources := p.copySources()
	if len(sources) != 1 || !sources[0].IsDir {
		t.Fatalf("sources = %#v, want highlighted directory", sources)
	}
}

func TestForceCopyRequiresConfirmation(t *testing.T) {
	m := New(Config{Force: true})
	m.panels[0].entries = []Entry{{Name: "a.txt", Path: "a.txt", Size: 10}}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("expected confirmation before copy command")
	}
	if !m.confirming {
		t.Fatal("expected confirmation state")
	}
	if !strings.Contains(m.status, "Enter confirms") {
		t.Fatalf("status = %q, want confirmation prompt", m.status)
	}
}

func TestDirectoryCopyRequiresRecursiveConfirmation(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{{Name: "dir/", Path: "logs/dir/", IsDir: true}}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("expected confirmation before recursive copy command")
	}
	if !m.confirming || m.confirmAction != confirmCopy {
		t.Fatalf("expected copy confirmation, confirming=%v action=%v", m.confirming, m.confirmAction)
	}
	got := m.View()
	for _, want := range []string{"Confirm recursive copy", "Copy 1 item(s) recursively?", "[ Yes ]", "[ No / Cancel ]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("view missing %q:\n%s", want, got)
		}
	}
}

func TestOverwritePromptAdvancesOneConflictAtATime(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "b.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Container: "data", LocalPath: tmp})
	m.panels[0].entries = []Entry{
		{Name: "a.txt", Path: "a.txt", Size: 1},
		{Name: "b.txt", Path: "b.txt", Size: 1},
	}
	m.panels[0].selected = map[string]Entry{
		"a.txt": {Name: "a.txt", Path: "a.txt", Size: 1},
		"b.txt": {Name: "b.txt", Path: "b.txt", Size: 1},
	}

	msg := m.checkOverwriteConflictsCmd(0, m.panels[0].copySources())().(overwriteCheckedMsg)
	if len(msg.conflicts) != 2 {
		t.Fatalf("conflicts = %v, want two", msg.conflicts)
	}
	updated, cmd := m.Update(msg)
	m = updated.(Model)
	if cmd != nil || !m.confirming || m.confirmAction != confirmOverwrite {
		t.Fatalf("expected overwrite confirmation, cmd=%v confirming=%v action=%v", cmd, m.confirming, m.confirmAction)
	}
	got := m.View()
	for _, want := range []string{"Confirm overwrite", "a.txt", "[ Overwrite ]", "[ Overwrite All ]", "[ Cancel ]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("overwrite modal missing %q:\n%s", want, got)
		}
	}

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || !m.confirming || !strings.Contains(m.overwriteTarget(), "b.txt") {
		t.Fatalf("expected next overwrite prompt, cmd=%v confirming=%v target=%q", cmd, m.confirming, m.overwriteTarget())
	}
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil || m.confirming || !m.copying {
		t.Fatalf("expected copy to begin after last overwrite, cmd nil=%v confirming=%v copying=%v", cmd == nil, m.confirming, m.copying)
	}
}

func TestOverwritePromptOmitsAllForSingleConflict(t *testing.T) {
	m := New(Config{Container: "data"})
	m.panels[1].location = t.TempDir()
	m.pendingSource = 0
	m.pendingItems = []copyItem{{entry: Entry{Name: "a.txt", Path: "a.txt", Size: 1}, targetRel: "a.txt"}}
	m.overwriteConflicts = []int{0}
	m.confirming = true
	m.confirmAction = confirmOverwrite

	got := m.View()
	if !strings.Contains(got, "[ Overwrite ]") || !strings.Contains(got, "[ Cancel ]") {
		t.Fatalf("single overwrite modal missing buttons:\n%s", got)
	}
	if strings.Contains(got, "Overwrite All") {
		t.Fatalf("single overwrite modal should not show overwrite all:\n%s", got)
	}
}

func TestExpandLocalDirectoryCopySources(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "dir")
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{})
	items, err := m.expandCopySources(context.Background(), panel{kind: LocalPanel}, []Entry{{Name: "dir/", Path: dir, IsDir: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %#v, want one file", items)
	}
	if items[0].targetRel != "dir/nested/a.txt" {
		t.Fatalf("targetRel = %q, want dir/nested/a.txt", items[0].targetRel)
	}
	if items[0].entry.Size != 5 {
		t.Fatalf("size = %d, want 5", items[0].entry.Size)
	}
}

func TestDeleteRequiresConfirmation(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{{Name: "a.txt", Path: "a.txt", Size: 10}}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("expected confirmation before delete command")
	}
	if !m.confirming || m.confirmAction != confirmDelete {
		t.Fatalf("expected delete confirmation, confirming=%v action=%v", m.confirming, m.confirmAction)
	}
	got := m.View()
	for _, want := range []string{"Confirm delete", "Delete 1 file(s)", "[ Yes ]", "[ No / Cancel ]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("view missing %q:\n%s", want, got)
		}
	}
}

func TestDeleteLocalFilesAfterConfirmation(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "a.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{})
	m.panels[1].location = tmp
	m.panels[1].entries = []Entry{{Name: "a.txt", Path: file, Size: 5}}
	m.active = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("expected delete command")
	}
	msg := cmd()
	updated, _ = m.Update(msg)
	m = updated.(Model)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("expected file to be deleted, stat err = %v", err)
	}
	if !strings.Contains(m.status, "deleted 1 file") {
		t.Fatalf("status = %q, want deleted", m.status)
	}
}

func TestDeleteLocalDirectoryAfterConfirmation(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{})
	m.panels[1].location = tmp
	m.panels[1].entries = []Entry{{Name: "dir/", Path: dir, IsDir: true}}
	m.active = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("expected delete command")
	}
	msg := cmd()
	updated, _ = m.Update(msg)
	m = updated.(Model)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected directory to be deleted, stat err = %v", err)
	}
}

func TestRemoveLocalEntryRetriesReadOnlyFile(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "readonly.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(file, 0o600)
	})

	if err := removeLocalEntry(Entry{Name: "readonly.txt", Path: file}); err != nil {
		t.Fatalf("removeLocalEntry returned error: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("expected file to be deleted, stat err = %v", err)
	}
}

func TestConfirmModalArrowSelectsCancel(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{{Name: "a.txt", Path: "a.txt", Size: 10}}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)
	if m.confirmYes {
		t.Fatal("expected right arrow to select cancel")
	}
	if !strings.Contains(m.View(), "[ No / Cancel ]") {
		t.Fatalf("view should show cancel button:\n%s", m.View())
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || m.confirming || !strings.Contains(m.status, "delete cancelled") {
		t.Fatalf("expected enter on cancel to cancel, cmd=%v confirming=%v status=%q", cmd, m.confirming, m.status)
	}
}

func TestLoadThemeFileAndThemeModal(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "theme.json")
	if err := os.WriteFile(path, []byte(`{"background":"001122","highlight":"#DDEEFF","text":"#AABBCC","header_text":"000000","selected":"ffff00","error":"ff0000","modal_border":"ffff00","modal_background":"eeeeee","modal_shadow":"111111"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	theme, err := LoadThemeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if theme.Background != "#001122" || theme.Highlight != "#DDEEFF" || theme.Text != "#AABBCC" || theme.ModalBackground != "#EEEEEE" || theme.ModalShadow != "#111111" {
		t.Fatalf("theme = %#v", theme)
	}
	m := New(Config{Theme: theme})
	m.width = 80
	m.height = 24

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("expected no command for theme modal")
	}
	if !m.showingTheme {
		t.Fatal("expected theme modal to be visible")
	}
	got := m.View()
	for _, want := range []string{"Theme", "background", "#001122", "highlight", "#DDEEFF", "header_text", "#000000", "modal_background", "#EEEEEE", "modal_shadow", "#111111"} {
		if !strings.Contains(got, want) {
			t.Fatalf("theme modal missing %q:\n%s", want, got)
		}
	}
}

func TestLoadThemeFileDiscoversCurrentDirectoryTheme(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.WriteFile(".theme.json", []byte(`{"background":"112233"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".theme.json"), []byte(`{"background":"445566"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	theme, err := LoadThemeFile("")
	if err != nil {
		t.Fatal(err)
	}
	if theme.Background != "#112233" {
		t.Fatalf("background = %q, want current directory theme", theme.Background)
	}
}

func TestLoadThemeFileDiscoversHomeTheme(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(home, ".theme.json"), []byte(`{"background":"445566"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	theme, err := LoadThemeFile("")
	if err != nil {
		t.Fatal(err)
	}
	if theme.Background != "#445566" {
		t.Fatalf("background = %q, want home theme", theme.Background)
	}
}

func TestExplicitThemeFileOverridesDiscoveredTheme(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.WriteFile(".theme.json", []byte(`{"background":"112233"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(tmp, "explicit.theme.json")
	if err := os.WriteFile(explicit, []byte(`{"background":"778899"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	theme, err := LoadThemeFile(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if theme.Background != "#778899" {
		t.Fatalf("background = %q, want explicit theme", theme.Background)
	}
}

func TestThemeModalEditsAndSavesTheme(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	m := New(Config{})
	m.width = 80
	m.height = 24
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = updated.(Model)
	for _, r := range "123456" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	if m.theme.Background != "#123456" {
		t.Fatalf("background = %q, want #123456", m.theme.Background)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = updated.(Model)
	if !strings.Contains(m.status, "saved theme") {
		t.Fatalf("status = %q, want saved theme", m.status)
	}
	saved, err := LoadThemeFile(".theme.json")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Background != "#123456" {
		t.Fatalf("saved background = %q, want #123456", saved.Background)
	}
}

func TestThemeModalShowsEditableStateAndBottomBorder(t *testing.T) {
	m := New(Config{})
	m.width = 80
	m.height = 24
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = updated.(Model)
	got := m.View()
	for _, want := range []string{"Theme editor", "type hex", "> * background", "[ Edit ]", "[ Save ]", "[ Close ]", "╚"} {
		if !strings.Contains(got, want) {
			t.Fatalf("theme modal missing %q:\n%s", want, got)
		}
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = updated.(Model)
	got = m.View()
	for _, want := range []string{"> E background", "#1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("theme edit state missing %q:\n%s", want, got)
		}
	}
}

func TestThemeModalButtonsCanClose(t *testing.T) {
	m := New(Config{})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = updated.(Model)

	for range len(themeItems(m.theme)) {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = updated.(Model)
	}
	if m.themeButton != 0 {
		t.Fatalf("theme button = %d, want edit button selected", m.themeButton)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || m.showingTheme {
		t.Fatalf("expected close button to close theme modal, cmd=%v showing=%v", cmd, m.showingTheme)
	}
}

func TestLoadThemeFileRejectsNonRGBColors(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "theme.json")
	if err := os.WriteFile(path, []byte(`{"background":"blue"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadThemeFile(path); err == nil || !strings.Contains(err.Error(), "#RRGGBB") {
		t.Fatalf("expected #RRGGBB error, got %v", err)
	}
}

func TestViewIncludesHeaderAndFitsWidth(t *testing.T) {
	m := New(Config{AccountName: "acct123", Container: "data", Prefix: "logs/"})
	m.width = 60
	m.height = 20
	m.panels[0].entries = []Entry{{Name: "remote.txt", Path: "logs/remote.txt", Size: 10}}
	m.panels[1].entries = []Entry{{Name: "local.txt", Path: "/tmp/local.txt", Size: 20}}

	got := m.View()
	if !strings.Contains(got, "account=acct123") || !strings.Contains(got, "container=data") {
		t.Fatalf("view missing remote header:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if width := lipgloss.Width(line); width > m.width {
			t.Fatalf("line width = %d, want <= %d: %q", width, m.width, line)
		}
	}
}

func TestViewUsesExactScreenGeometry(t *testing.T) {
	m := New(Config{AccountName: "acct123", Container: "data", Prefix: "logs/"})
	m.width = 60
	m.height = 20
	m.panels[0].entries = []Entry{{Name: "remote.txt", Path: "logs/remote.txt", Size: 10}}
	m.panels[1].entries = []Entry{{Name: "local.txt", Path: "/tmp/local.txt", Size: 20}}

	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("view height = %d, want %d:\n%s", len(lines), m.height, strings.Join(lines, "\n"))
	}
	for i, line := range lines {
		if width := lipgloss.Width(line); width != m.width {
			t.Fatalf("line %d width = %d, want %d: %q", i, width, m.width, line)
		}
	}
	if !strings.Contains(lines[0], "Remote:") {
		t.Fatalf("first line should be header, got %q", lines[0])
	}
	footer := lines[len(lines)-1]
	if !strings.Contains(footer, "remote.txt") || !strings.Contains(footer, "10 bytes") {
		t.Fatalf("footer should include current file and full size, got %q", footer)
	}
}

func TestSlashFiltersActivePane(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{
		{Name: "alpha.txt", Path: "alpha.txt", Size: 1},
		{Name: "beta.txt", Path: "beta.txt", Size: 1},
		{Name: "alphabet.txt", Path: "alphabet.txt", Size: 1},
	}
	m.panels[1].entries = []Entry{
		{Name: "local-alpha.txt", Path: "/tmp/local-alpha.txt", Size: 1},
		{Name: "local-beta.txt", Path: "/tmp/local-beta.txt", Size: 1},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	for _, r := range "alp" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}

	if !m.filtering || m.filterPanel != 0 {
		t.Fatalf("expected filter mode on active pane, filtering=%v panel=%d", m.filtering, m.filterPanel)
	}
	if got := entryNames(m.panels[0].entries); strings.Join(got, ",") != "alpha.txt,alphabet.txt" {
		t.Fatalf("filtered remote entries = %v, want alpha matches", got)
	}
	if got := entryNames(m.panels[1].entries); strings.Join(got, ",") != "local-alpha.txt,local-beta.txt" {
		t.Fatalf("inactive local entries changed: %v", got)
	}
	if got := m.View(); !strings.Contains(got, "filter: alp") {
		t.Fatalf("view missing active filter:\n%s", got)
	}
}

func TestFilterBackspaceEnterAndEscape(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{
		{Name: "alpha.txt", Path: "alpha.txt", Size: 1},
		{Name: "beta.txt", Path: "beta.txt", Size: 1},
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	for _, r := range "alphaz" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = updated.(Model)
	if m.panels[0].filter != "alpha" || len(m.panels[0].entries) != 1 {
		t.Fatalf("backspace filter=%q entries=%v, want alpha match", m.panels[0].filter, entryNames(m.panels[0].entries))
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.filtering || m.panels[0].filter != "alpha" {
		t.Fatalf("enter should accept filter, filtering=%v filter=%q", m.filtering, m.panels[0].filter)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	for _, r := range "x" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.filtering || m.panels[0].filter != "alpha" || len(m.panels[0].entries) != 1 {
		t.Fatalf("esc should restore accepted filter, filtering=%v filter=%q entries=%v", m.filtering, m.panels[0].filter, entryNames(m.panels[0].entries))
	}
}

func TestFilterClearsOnDirectoryNavigation(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{
		{Name: "logs/", Path: "logs/", IsDir: true},
		{Name: "data/", Path: "data/", IsDir: true},
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(Model)
	for _, r := range "log" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)

	if cmd == nil || m.panels[0].location != "logs/" {
		t.Fatalf("right should enter filtered directory, location=%q cmd nil=%v", m.panels[0].location, cmd == nil)
	}
	if m.filtering || m.panels[0].filter != "" {
		t.Fatalf("filter should clear on navigation, filtering=%v filter=%q", m.filtering, m.panels[0].filter)
	}
}

func TestRenderPanelUsesRequestedOuterWidth(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{{
		Name:         "long-remote-file-name.txt",
		Path:         "long-remote-file-name.txt",
		Size:         1536,
		LastModified: time.Date(2026, 5, 8, 19, 30, 0, 0, time.UTC),
	}}

	panel := m.renderPanel(0, 60, 10)
	lines := strings.Split(panel, "\n")
	if len(lines) != 10 {
		t.Fatalf("panel height = %d, want 10:\n%s", len(lines), panel)
	}
	for i, line := range lines {
		if width := lipgloss.Width(line); width != 60 {
			t.Fatalf("panel line %d width = %d, want 60: %q", i, width, line)
		}
	}
	for _, want := range []string{"File", "Size", "Modified", "│", "1.5 KB", "2026-05-08"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("panel missing %q:\n%s", want, panel)
		}
	}
}

func TestRenderPanelPrefixesDirectories(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{
		{Name: "dir/", Path: "dir/", IsDir: true},
		{Name: "file.txt", Path: "file.txt", Size: 1},
	}

	panel := m.renderPanel(0, 50, 8)
	if !strings.Contains(panel, "/dir") {
		t.Fatalf("panel should prefix directories with slash:\n%s", panel)
	}
	if strings.Contains(panel, "/file.txt") {
		t.Fatalf("panel should not prefix files with slash:\n%s", panel)
	}
}

func TestPageHomeEndNavigation(t *testing.T) {
	m := New(Config{})
	m.height = 12
	for i := range 20 {
		m.panels[0].entries = append(m.panels[0].entries, Entry{Name: "file.txt", Path: string(rune('a' + i))})
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = updated.(Model)
	if m.panels[0].cursor != 6 || m.panels[0].offset != 1 {
		t.Fatalf("pgdown cursor/offset = %d/%d, want 6/1", m.panels[0].cursor, m.panels[0].offset)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(Model)
	if m.panels[0].cursor != 19 || m.panels[0].offset != 14 {
		t.Fatalf("end cursor/offset = %d/%d, want 19/14", m.panels[0].cursor, m.panels[0].offset)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = updated.(Model)
	if m.panels[0].cursor != 13 || m.panels[0].offset != 13 {
		t.Fatalf("pgup cursor/offset = %d/%d, want 13/13", m.panels[0].cursor, m.panels[0].offset)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m = updated.(Model)
	if m.panels[0].cursor != 0 || m.panels[0].offset != 0 {
		t.Fatalf("home cursor/offset = %d/%d, want 0/0", m.panels[0].cursor, m.panels[0].offset)
	}
}

func TestLeftRightNavigation(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{{Name: "dir/", Path: "logs/", IsDir: true}}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)
	if cmd == nil || m.panels[0].location != "logs/" {
		t.Fatalf("right should enter directory, location=%q cmd nil=%v", m.panels[0].location, cmd == nil)
	}

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(Model)
	if cmd == nil || m.panels[0].location != "" {
		t.Fatalf("left should go to parent, location=%q cmd nil=%v", m.panels[0].location, cmd == nil)
	}
}

func TestNewDirectoryModalCreatesLocalDirectory(t *testing.T) {
	tmp := t.TempDir()
	m := New(Config{LocalPath: tmp})
	m.active = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = updated.(Model)
	if !m.creatingDir || !strings.Contains(m.View(), "Create directory") {
		t.Fatalf("expected create directory modal")
	}
	for _, want := range []string{"[ Create ]", "[ Cancel ]"} {
		if !strings.Contains(m.View(), want) {
			t.Fatalf("create modal missing %q:\n%s", want, m.View())
		}
	}

	for _, r := range "newdir" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("expected mkdir command")
	}
	msg := cmd()
	updated, _ = m.Update(msg)
	m = updated.(Model)

	if _, err := os.Stat(filepath.Join(tmp, "newdir")); err != nil {
		t.Fatal(err)
	}
	if m.creatingDir || !strings.Contains(m.status, "created directory newdir") {
		t.Fatalf("unexpected mkdir state/status: creating=%v status=%q", m.creatingDir, m.status)
	}
}

func TestNewDirectoryModalArrowSelectsCancel(t *testing.T) {
	tmp := t.TempDir()
	m := New(Config{LocalPath: tmp})
	m.active = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = updated.(Model)
	for _, r := range "newdir" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)
	if m.createYes {
		t.Fatal("expected right arrow to select cancel")
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || m.creatingDir || !strings.Contains(m.status, "create directory cancelled") {
		t.Fatalf("expected enter on cancel to cancel, cmd=%v creating=%v status=%q", cmd, m.creatingDir, m.status)
	}
	if _, err := os.Stat(filepath.Join(tmp, "newdir")); !os.IsNotExist(err) {
		t.Fatalf("directory should not have been created, stat err=%v", err)
	}
}

func TestNewRemoteDirectoryDoesNotCreatePlaceholderBlob(t *testing.T) {
	m := New(Config{Prefix: "logs/"})

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = updated.(Model)
	for _, r := range "newdir" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.creatingDir {
		t.Fatal("expected remote mkdir modal to close")
	}
	if m.panels[0].location != "logs/newdir/" {
		t.Fatalf("remote location = %q, want logs/newdir/", m.panels[0].location)
	}
	if cmd == nil {
		t.Fatal("expected remote pane refresh command")
	}
}

func TestNewDirectoryRejectsPathSeparators(t *testing.T) {
	if err := validateNewDirName("bad/name"); err == nil {
		t.Fatal("expected path separator validation error")
	}
}

func TestDefaultThemeValues(t *testing.T) {
	theme := DefaultTheme()
	if theme.Background != "#0011EE" ||
		theme.Highlight != "#00AAFF" ||
		theme.Text != "#D0D0D0" ||
		theme.HeaderText != "#000000" ||
		theme.Selected != "#FFFF00" ||
		theme.Error != "#FF0000" ||
		theme.ModalBorder != "#111111" ||
		theme.ModalBackground != "#D0D0D0" ||
		theme.ModalShadow != "#000000" {
		t.Fatalf("unexpected default theme: %#v", theme)
	}
}

func TestOpenParentSelectsPreviousDirectory(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, "sibling"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := New(Config{LocalPath: child})
	m.active = 1
	m.panels[1].location = child

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("expected parent list command")
	}
	updated, _ = m.Update(cmd())
	m = updated.(Model)

	entry, ok := m.panels[1].current()
	if !ok || entry.Path != child {
		t.Fatalf("selected entry = %#v, want child path %q", entry, child)
	}
}

func TestOpenRemoteParentSelectsPreviousPrefix(t *testing.T) {
	m := New(Config{Prefix: "logs/2026/"})
	m.panels[0].location = "logs/2026/"

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(Model)
	updated, _ = m.Update(entriesLoadedMsg{
		index: 0,
		entries: []Entry{
			{Name: "2025/", Path: "logs/2025/", IsDir: true},
			{Name: "2026/", Path: "logs/2026/", IsDir: true},
		},
	})
	m = updated.(Model)

	entry, ok := m.panels[0].current()
	if !ok || entry.Path != "logs/2026/" {
		t.Fatalf("selected entry = %#v, want logs/2026/", entry)
	}
}

func TestReenterDirectoryRestoresPreviousSelection(t *testing.T) {
	m := New(Config{})
	m.panels[0].entries = []Entry{{Name: "logs/", Path: "logs/", IsDir: true}}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)
	if cmd == nil || m.panels[0].location != "logs/" {
		t.Fatalf("right should enter directory, location=%q cmd nil=%v", m.panels[0].location, cmd == nil)
	}

	childEntries := []Entry{
		{Name: "..", Path: "", IsDir: true, parent: true},
		{Name: "2025/", Path: "logs/2025/", IsDir: true},
		{Name: "2026/", Path: "logs/2026/", IsDir: true},
	}
	updated, _ = m.Update(entriesLoadedMsg{index: 0, entries: childEntries})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	entry, ok := m.panels[0].current()
	if !ok || entry.Path != "logs/2026/" {
		t.Fatalf("selected entry before leaving = %#v, want logs/2026/", entry)
	}

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = updated.(Model)
	if cmd == nil || m.panels[0].location != "" {
		t.Fatalf("left should return to root, location=%q cmd nil=%v", m.panels[0].location, cmd == nil)
	}
	updated, _ = m.Update(entriesLoadedMsg{
		index:   0,
		entries: []Entry{{Name: "logs/", Path: "logs/", IsDir: true}},
	})
	m = updated.(Model)

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)
	if cmd == nil || m.panels[0].location != "logs/" {
		t.Fatalf("right should re-enter directory, location=%q cmd nil=%v", m.panels[0].location, cmd == nil)
	}
	updated, _ = m.Update(entriesLoadedMsg{index: 0, entries: childEntries})
	m = updated.(Model)

	entry, ok = m.panels[0].current()
	if !ok || entry.Path != "logs/2026/" {
		t.Fatalf("restored entry = %#v, want logs/2026/", entry)
	}
}

func TestEmptyEntryColumnsKeepSeparators(t *testing.T) {
	line := emptyEntryColumns(40)
	if got := strings.Count(line, "│"); got != 2 {
		t.Fatalf("empty row separators = %d, want 2 in %q", got, line)
	}
}

func TestViewShowsProgressModal(t *testing.T) {
	m := New(Config{})
	m.width = 80
	m.height = 24
	m.copying = true
	m.copyProgress = transferProgress{
		File:       "a.txt",
		FileDone:   5,
		FileTotal:  10,
		BatchDone:  5,
		BatchTotal: 20,
		SpeedBps:   1536,
	}
	got := m.View()
	for _, want := range []string{"Copying", "a.txt", "File", "Speed 1.5 KB/s", "Total", "[ Cancel: Esc or Ctrl+C ]", "[████████████"} {
		if !strings.Contains(got, want) {
			t.Fatalf("view missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Remote:") {
		t.Fatalf("modal should overlay panes without blanking them:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	for i, line := range lines {
		if width := lipgloss.Width(line); width != m.width {
			t.Fatalf("line %d width = %d, want %d: %q", i, width, m.width, line)
		}
	}
	modalBorderLines := 0
	for _, line := range lines {
		if strings.Contains(line, "╔") || strings.Contains(line, "║") || strings.Contains(line, "╚") {
			modalBorderLines++
		}
	}
	if modalBorderLines == 0 {
		t.Fatalf("expected centered modal border:\n%s", got)
	}
}

func TestProgressThrottlerLimitsUpdatesAndReportsSpeed(t *testing.T) {
	ch := make(chan tea.Msg, 4)
	now := time.Unix(0, 0)
	reporter := newProgressThrottler("a.txt", 10, 100, ch, func() time.Time { return now })

	reporter.report(0, 50)
	now = now.Add(100 * time.Millisecond)
	reporter.report(10, 50)
	if got := len(ch); got != 1 {
		t.Fatalf("updates after throttled report = %d, want 1", got)
	}

	now = now.Add(100 * time.Millisecond)
	reporter.report(20, 50)
	if got := len(ch); got != 2 {
		t.Fatalf("updates after interval = %d, want 2", got)
	}
	msg := (<-ch).(transferProgress)
	if msg.SpeedBps != 0 {
		t.Fatalf("initial speed = %f, want 0", msg.SpeedBps)
	}
	msg = (<-ch).(transferProgress)
	if msg.BatchDone != 30 || msg.SpeedBps != 100 {
		t.Fatalf("progress = %#v, want batch done 30 and speed 100 B/s", msg)
	}

	now = now.Add(50 * time.Millisecond)
	reporter.report(50, 50)
	if got := len(ch); got != 1 {
		t.Fatalf("final update should bypass throttle, updates=%d", got)
	}
}

func TestEscCancelsCopy(t *testing.T) {
	m := New(Config{})
	m.copying = true
	cancelled := false
	m.copyCancel = func() {
		cancelled = true
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("expected no command while cancelling")
	}
	if !cancelled {
		t.Fatal("expected cancel function to be called")
	}
	if m.status != "cancelling copy..." {
		t.Fatalf("status = %q, want cancelling copy...", m.status)
	}
}
