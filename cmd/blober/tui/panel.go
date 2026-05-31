package tui

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	azure_storage "github.com/dariopb/blober/pkg/azure_storage"
)

type PanelKind int

const (
	LocalPanel PanelKind = iota
	RemotePanel
)

type Entry struct {
	Name         string
	Path         string
	IsDir        bool
	Size         int64
	LastModified time.Time
	parent       bool
}

type panel struct {
	kind     PanelKind
	title    string
	location string
	entries  []Entry
	cursor   int
	offset   int
	selected map[string]Entry
	err      error
}

func newLocalPanel(path string) panel {
	return panel{kind: LocalPanel, title: "Local", location: path, selected: map[string]Entry{}}
}

func newRemotePanel(prefix string) panel {
	return panel{kind: RemotePanel, title: "Remote", location: prefix, selected: map[string]Entry{}}
}

func (p panel) current() (Entry, bool) {
	if len(p.entries) == 0 || p.cursor < 0 || p.cursor >= len(p.entries) {
		return Entry{}, false
	}
	return p.entries[p.cursor], true
}

func (p *panel) toggleCurrent() string {
	entry, ok := p.current()
	if !ok {
		return "nothing selected"
	}
	if entry.IsDir {
		return "directory selection is not supported"
	}
	if _, ok := p.selected[entry.Path]; ok {
		delete(p.selected, entry.Path)
		return "unselected " + entry.Name
	}
	p.selected[entry.Path] = entry
	return "selected " + entry.Name
}

func (p panel) copySources() []Entry {
	if len(p.selected) == 0 {
		entry, ok := p.current()
		if !ok || entry.parent {
			return nil
		}
		return []Entry{entry}
	}
	sources := make([]Entry, 0, len(p.selected))
	for _, entry := range p.selected {
		sources = append(sources, entry)
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Name < sources[j].Name
	})
	return sources
}

func (p panel) deleteSources() []Entry {
	if len(p.selected) == 0 {
		entry, ok := p.current()
		if !ok || entry.parent {
			return nil
		}
		return []Entry{entry}
	}
	sources := make([]Entry, 0, len(p.selected))
	for _, entry := range p.selected {
		sources = append(sources, entry)
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Name < sources[j].Name
	})
	return sources
}

func (p *panel) setEntries(entries []Entry, err error) {
	sortEntries(entries)
	p.entries = entries
	p.err = err
	if p.cursor >= len(p.entries) {
		p.cursor = len(p.entries) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

func listLocal(path string) ([]Entry, error) {
	dirEntries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(dirEntries)+1)
	if parent := filepath.Dir(path); parent != path {
		entries = append(entries, Entry{Name: "..", Path: parent, IsDir: true, parent: true})
	}
	for _, dirEntry := range dirEntries {
		info, err := dirEntry.Info()
		if err != nil {
			return nil, err
		}
		name := dirEntry.Name()
		displayName := name
		if info.IsDir() {
			displayName += string(os.PathSeparator)
		}
		entries = append(entries, Entry{
			Name:         displayName,
			Path:         filepath.Join(path, name),
			IsDir:        info.IsDir(),
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
	}
	sortEntries(entries)
	return entries, nil
}

func listRemote(ctx context.Context, client *azblob.Client, container, prefix string) ([]Entry, error) {
	blobEntries, err := azure_storage.ListBlobEntries(ctx, client, container, prefix)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(blobEntries)+1)
	if prefix != "" {
		parent := parentBlobPrefix(prefix)
		entries = append(entries, Entry{Name: "..", Path: parent, IsDir: true, parent: true})
	}
	for _, blobEntry := range blobEntries {
		path := blobEntry.Key
		if blobEntry.IsDir {
			path = blobEntry.Prefix
		}
		entries = append(entries, Entry{
			Name:         blobEntry.Name,
			Path:         path,
			IsDir:        blobEntry.IsDir,
			Size:         blobEntry.Size,
			LastModified: blobEntry.LastModified,
		})
	}
	return entries, nil
}

func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].parent || entries[j].parent {
			return entries[i].parent
		}
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entrySortName(entries[i]) < entrySortName(entries[j])
	})
}

func entrySortName(entry Entry) string {
	name := strings.TrimPrefix(displayEntryName(entry), "/")
	return strings.ToLower(name)
}

func parentBlobPrefix(prefix string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	if slash := strings.LastIndexByte(prefix, '/'); slash >= 0 {
		return prefix[:slash+1]
	}
	return ""
}
