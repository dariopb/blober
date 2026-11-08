package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type renamer interface {
	Rename(context.Context, string, string) error
}

var errRenameUnsupported = errors.New("native rename is not supported")

func (localProvider) Rename(ctx context.Context, source, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("destination exists: %s", target)
	}
	return os.Rename(source, target)
}

func (p scpProvider) Rename(ctx context.Context, source, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := p.client().Lstat(target); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("destination exists: %s", target)
	}
	// Some servers implement standard SFTP rename with replacement semantics,
	// so check explicitly rather than relying on the protocol's no-replace rule.
	return p.client().Rename(source, target)
}

func (p webdavProvider) Rename(ctx context.Context, source, target string) error {
	req, err := p.newRequest(ctx, "MOVE", source, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", target)
	req.Header.Set("Overwrite", "F")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return errRenameUnsupported
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("MOVE %s: %s", source, resp.Status)
	}
	return nil
}

type fileActionDoneMsg struct {
	index  int
	target string
	label  string
	err    error
}

func (m Model) startMove() (Model, tea.Cmd) {
	sources := m.panels[m.active].copySources()
	if len(sources) == 0 {
		m.status = "select a file or directory to move"
		return m, nil
	}
	m.confirming, m.confirmYes = true, true
	m.confirmAction = confirmMove
	m.pendingSource, m.pendingCopy = m.active, sources
	m.status = "confirm move; originals are removed only after successful transfer"
	return m, nil
}

func (m Model) beginMove(index int, sources []Entry) (Model, tea.Cmd) {
	target := m.panels[1-index]
	return m.beginFileAction(index, sources, target, "", "move")
}

func (m Model) beginRename(entry Entry, name string) (tea.Model, tea.Cmd) {
	if name == copyTargetName(entry) {
		m.input = nil
		m.status = "name unchanged"
		return m, nil
	}
	return m.beginFileAction(m.active, []Entry{entry}, m.panels[m.active], name, "rename")
}

func (m Model) beginFileAction(index int, sources []Entry, target panel, name, label string) (Model, tea.Cmd) {
	source := m.panels[index]
	ctx, cancel := context.WithCancel(context.Background())
	m.clearConfirmation()
	m.input = nil
	m.copying = true
	m.transferLabel = "Moving"
	if label == "rename" {
		m.transferLabel = "Renaming"
	}
	m.copyProgress = transferProgress{}
	m.copyCancel = cancel
	m.progressCh = make(chan tea.Msg, 32)
	ch := m.progressCh
	return m, tea.Batch(waitProgress(ch), func() tea.Msg {
		defer cancel()
		defer close(ch)
		dst, err := m.moveEntries(ctx, source, target, sources, name, ch)
		return fileActionDoneMsg{index: index, target: dst, label: label, err: err}
	})
}

func (m Model) applyFileActionDone(msg fileActionDoneMsg) (tea.Model, tea.Cmd) {
	m.copying = false
	m.copyCancel, m.progressCh = nil, nil
	m.transferLabel = ""
	if msg.err != nil {
		m.status = msg.label + " failed: " + msg.err.Error() + "; uncopied sources were kept"
		if errors.Is(msg.err, context.Canceled) {
			m.status = msg.label + " cancelled; uncopied sources were kept"
		}
	} else {
		m.panels[msg.index].selected = map[string]Entry{}
		m.status = msg.label + " complete"
		if msg.label == "rename" {
			m.setPendingSelect(msg.index, msg.target)
		}
	}
	return m, tea.Batch(m.listCmd(0), m.listCmd(1))
}

func sameBackend(a, b Provider) bool {
	switch x := a.(type) {
	case localProvider:
		_, ok := b.(localProvider)
		return ok
	case scpProvider:
		y, ok := b.(scpProvider)
		if !ok || x.session == nil || y.session == nil {
			return false
		}
		if x.session == y.session {
			return true
		}
		if x.session.ssh != nil && y.session.ssh != nil {
			return x.session.ssh.RemoteAddr().String() == y.session.ssh.RemoteAddr().String() &&
				x.session.ssh.User() == y.session.ssh.User()
		}
		return x.session.config.host != "" &&
			strings.EqualFold(x.session.config.address(), y.session.config.address()) &&
			x.session.config.user == y.session.config.user
	case azureProvider:
		y, ok := b.(azureProvider)
		if !ok || x.container != y.container {
			return false
		}
		if x.client != nil && y.client != nil {
			return x.client.URL() == y.client.URL()
		}
		return x.accountName == y.accountName
	case httpProvider:
		switch y := b.(type) {
		case httpProvider:
			return sameWebBackend(x.webBase, y.webBase)
		case webdavProvider:
			return sameWebBackend(x.webBase, y.webBase)
		}
	case webdavProvider:
		switch y := b.(type) {
		case httpProvider:
			return sameWebBackend(x.webBase, y.webBase)
		case webdavProvider:
			return sameWebBackend(x.webBase, y.webBase)
		}
	}
	return false
}

func sameWebBackend(a, b webBase) bool {
	return a.base != nil && b.base != nil && sameOrigin(a.base, b.base)
}

func comparablePath(prov Provider, value string) (string, error) {
	switch p := prov.(type) {
	case localProvider:
		abs, err := filepath.Abs(value)
		if err != nil {
			return "", err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return "", err
		}
		abs = filepath.ToSlash(filepath.Join(parent, filepath.Base(abs)))
		if runtime.GOOS == "windows" {
			abs = strings.ToLower(abs)
		}
		return abs, nil
	case scpProvider:
		if p.session == nil || p.session.sftp == nil {
			return path.Clean(value), nil
		}
		parent, err := p.client().RealPath(path.Dir(value))
		if err != nil {
			return "", err
		}
		return path.Join(parent, path.Base(value)), nil
	case httpProvider, webdavProvider:
		u, err := url.Parse(value)
		if err != nil {
			return "", err
		}
		return path.Clean(u.Path), nil
	default:
		return strings.TrimSuffix(value, "/"), nil
	}
}

func guardMovePaths(prov Provider, entry Entry, target string) error {
	source, err := comparablePath(prov, entry.Path)
	if err != nil {
		return err
	}
	dst, err := comparablePath(prov, target)
	if err != nil {
		return err
	}
	if source == dst || (entry.IsDir && strings.HasPrefix(dst, strings.TrimSuffix(source, "/")+"/")) {
		return errors.New("cannot move an item onto itself or into its own directory")
	}
	return nil
}

func moveTargetExists(ctx context.Context, prov Provider, target string, dir bool) (bool, error) {
	if _, ok := prov.(localProvider); ok {
		_, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	if p, ok := prov.(scpProvider); ok {
		_, err := p.client().Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	exists, err := prov.Exists(ctx, target)
	if exists || err != nil || !dir {
		return exists, err
	}
	if _, ok := prov.(azureProvider); ok {
		entries, err := prov.List(ctx, strings.TrimSuffix(target, "/")+"/")
		for _, entry := range entries {
			if !entry.parent {
				return true, err
			}
		}
		return false, err
	}
	return false, nil
}

type moveTree struct {
	entry Entry
	rel   string
	dirs  []string
	files []copyItem
}

func buildMoveTree(ctx context.Context, prov Provider, entry Entry, rel string) (moveTree, error) {
	tree := moveTree{entry: entry, rel: rel}
	visited := map[string]bool{}
	var visit func(Entry, string) error
	visit = func(e Entry, relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := safeRel(relative); err != nil {
			return err
		}
		// Copy/delete cannot preserve symlinks or special files. Native rename
		// handles them; otherwise keep the source instead of silently changing it.
		var info os.FileInfo
		var err error
		switch p := prov.(type) {
		case localProvider:
			info, err = os.Lstat(e.Path)
		case scpProvider:
			info, err = p.client().Lstat(e.Path)
		}
		if err != nil {
			return err
		}
		if info != nil && !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("copy/delete move cannot preserve symlink or special file: %s", e.Path)
		}
		if !e.IsDir {
			tree.files = append(tree.files, copyItem{entry: e, targetRel: relative})
			return nil
		}
		if visited[e.Path] {
			return fmt.Errorf("directory cycle at %s", e.Path)
		}
		visited[e.Path] = true
		tree.dirs = append(tree.dirs, relative)
		children, err := prov.List(ctx, e.Path)
		if err != nil {
			return err
		}
		for _, child := range children {
			if !child.parent {
				if err := visit(child, relative+"/"+copyTargetName(child)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := visit(entry, rel)
	return tree, err
}

func (m Model) moveEntries(ctx context.Context, source, target panel, sources []Entry, rename string, ch chan tea.Msg) (string, error) {
	src, dst := m.resolveProvider(source), m.resolveProvider(target)
	var trees []moveTree
	var lastTarget string
	targetNames := map[string]bool{}
	native, canRename := src.(renamer)
	canRename = canRename && sameBackend(src, dst)
	if _, ok := src.(webdavProvider); ok {
		_, canRename = dst.(webdavProvider)
		canRename = canRename && sameBackend(src, dst)
	}
	for _, entry := range sources {
		if entry.parent {
			return "", errors.New("cannot move a parent entry")
		}
		rel := copyTargetName(entry)
		if rename != "" {
			rel = rename
		}
		if targetNames[rel] {
			return "", fmt.Errorf("multiple sources have the same destination name: %s", rel)
		}
		targetNames[rel] = true
		for _, other := range sources {
			if other.Path != entry.Path && other.IsDir {
				parent, err := comparablePath(src, other.Path)
				if err != nil {
					return "", err
				}
				child, err := comparablePath(src, entry.Path)
				if err != nil {
					return "", err
				}
				if strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/") {
					return "", errors.New("move selection contains both a directory and an item inside it")
				}
			}
		}
		dstPath, err := dst.Join(target.location, rel)
		if err != nil {
			return "", err
		}
		if sameBackend(src, dst) {
			if err := guardMovePaths(src, entry, dstPath); err != nil {
				return "", err
			}
		}
		exists, err := moveTargetExists(ctx, dst, dstPath, entry.IsDir)
		if err != nil {
			return "", err
		}
		if exists {
			return "", fmt.Errorf("destination exists: %s", dstPath)
		}
		tree := moveTree{entry: entry, rel: rel}
		if !canRename {
			tree, err = buildMoveTree(ctx, src, entry, rel)
			if err != nil {
				return "", err
			}
			if err := validateMoveDirectories(dst, tree); err != nil {
				return "", err
			}
		}
		trees = append(trees, tree)
	}
	for _, tree := range trees {
		if err := ctx.Err(); err != nil {
			return lastTarget, err
		}
		dstPath, _ := dst.Join(target.location, tree.rel)
		if tree.entry.IsDir {
			switch dst.(type) {
			case azureProvider, httpProvider, webdavProvider:
				dstPath = strings.TrimSuffix(dstPath, "/") + "/"
			}
		}
		if canRename {
			err := native.Rename(ctx, tree.entry.Path, dstPath)
			if err == nil {
				lastTarget = dstPath
				continue
			}
			if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, errRenameUnsupported) {
				return lastTarget, err
			}
			tree, err = buildMoveTree(ctx, src, tree.entry, tree.rel)
			if err != nil {
				return lastTarget, err
			}
			if err := validateMoveDirectories(dst, tree); err != nil {
				return lastTarget, err
			}
		}
		for _, dir := range tree.dirs {
			parent, base := path.Split(dir)
			location := target.location
			if parent != "" {
				var err error
				location, err = dst.Join(location, strings.TrimSuffix(parent, "/"))
				if err != nil {
					return lastTarget, err
				}
			}
			if _, _, err := dst.Mkdir(ctx, location, base); err != nil {
				// Plain HTTP has no mkdir; non-empty trees use nested PUTs.
				if _, ok := dst.(httpProvider); !ok {
					return lastTarget, err
				}
			}
		}
		var done int64
		total := totalCopySize(tree.files)
		for _, item := range tree.files {
			reporter := newProgressThrottler(item.targetRel, done, total, ch, time.Now)
			if err := m.copyOne(ctx, src, dst, target.location, item, reporter.report); err != nil {
				return lastTarget, err
			}
			done += max(0, item.entry.Size)
		}
		if err := ctx.Err(); err != nil {
			return lastTarget, err
		}
		// Never remove a source tree until every file and empty directory has
		// been transferred successfully. A failure leaves originals intact.
		if err := src.Remove(ctx, tree.entry); err != nil {
			return lastTarget, fmt.Errorf("copied but could not remove source %s: %w", tree.entry.Path, err)
		}
		lastTarget = dstPath
	}
	return lastTarget, nil
}

func validateMoveDirectories(dst Provider, tree moveTree) error {
	_, plainHTTP := dst.(httpProvider)
	if !dst.Virtual() && !plainHTTP {
		return nil
	}
	for _, dir := range tree.dirs {
		hasFile := false
		for _, file := range tree.files {
			if strings.HasPrefix(file.targetRel, dir+"/") {
				hasFile = true
				break
			}
		}
		if !hasFile {
			return fmt.Errorf("destination cannot preserve empty directory %s; source kept", dir)
		}
	}
	return nil
}
