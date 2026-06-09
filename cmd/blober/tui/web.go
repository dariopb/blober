package tui

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// webBase holds the shared state and HTTP plumbing common to the plain-HTTP and
// WebDAV providers. base is the directory root URL (with a trailing slash) and
// all locations / Entry paths are full URL strings.
type webBase struct {
	base   *url.URL
	user   string
	pass   string
	label  string
	client *http.Client
}

func (b webBase) Label() string { return b.label }
func (b webBase) Virtual() bool { return false }
func (b webBase) Close() error  { return nil }

// newWebClient builds an HTTP client that follows redirects while preserving
// Basic auth only on same-host redirects (never on a host change or an
// https->http downgrade) to avoid leaking credentials to a redirect target.
func newWebClient(user, pass string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			prev := via[len(via)-1]
			// Go rewrites unsafe methods (PUT/DELETE/PROPFIND/MKCOL) to GET on
			// 301/302/303. Refuse such redirects so an upload or delete cannot
			// silently succeed as a no-op GET.
			if req.Method != prev.Method {
				return fmt.Errorf("refusing redirect that changes method %s -> %s", prev.Method, req.Method)
			}
			// Never forward credentials embedded in the URL across redirects.
			req.URL.User = nil
			if user == "" {
				return nil
			}
			sameHost := sameOrigin(req.URL, prev.URL)
			downgrade := prev.URL.Scheme == "https" && req.URL.Scheme == "http"
			if sameHost && !downgrade {
				req.SetBasicAuth(user, pass)
			} else {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
}

// sameOrigin reports whether two URLs share a scheme and host, normalizing host
// case and the default port so that e.g. http://H:80 == http://h.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && normalHost(a) == normalHost(b)
}

// normalHost returns a lowercased host with the scheme's default port elided.
func normalHost(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	switch {
	case port == "":
	case u.Scheme == "http" && port == "80":
		port = ""
	case u.Scheme == "https" && port == "443":
		port = ""
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

func (b webBase) newRequest(ctx context.Context, method, urlStr string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	if b.user != "" {
		req.SetBasicAuth(b.user, b.pass)
	}
	return req, nil
}

// Parent returns the parent directory URL, bounded by the base root.
func (b webBase) Parent(location string) (string, bool) {
	u, err := url.Parse(location)
	if err != nil {
		return "", false
	}
	if !strings.HasPrefix(u.Path, b.base.Path) {
		return "", false
	}
	trimmed := strings.TrimSuffix(u.Path, "/")
	baseTrimmed := strings.TrimSuffix(b.base.Path, "/")
	if trimmed == baseTrimmed || trimmed == "" {
		return "", false
	}
	parent := path.Dir(trimmed)
	if !strings.HasSuffix(parent, "/") {
		parent += "/"
	}
	if len(parent) < len(b.base.Path) {
		parent = b.base.Path
	}
	pu := *u
	pu.Path = parent
	pu.RawQuery = ""
	pu.Fragment = ""
	return pu.String(), true
}

// Join resolves a slash-separated, destination-relative path under location.
func (b webBase) Join(location, rel string) (string, error) {
	if err := safeRel(rel); err != nil {
		return "", err
	}
	u, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	ref := &url.URL{Path: rel}
	return u.ResolveReference(ref).String(), nil
}

// Open downloads target via GET and reports its size (-1 if unknown).
func (b webBase) Open(ctx context.Context, target string) (io.ReadCloser, int64, error) {
	req, err := b.newRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("GET %s: %s", target, resp.Status)
	}
	size := int64(-1)
	if resp.ContentLength >= 0 {
		size = resp.ContentLength
	}
	return resp.Body, size, nil
}

// put uploads r to target via PUT, mirroring `curl --data-binary`: it always
// sends an explicit Content-Length (never chunked transfer encoding, which DAV
// servers such as nginx reject) and a Content-Type. When the size is unknown
// (<0) the body is buffered to determine its length.
func (b webBase) put(ctx context.Context, target string, size int64, r io.Reader) error {
	if size < 0 {
		buf, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		r = strings.NewReader(string(buf))
		size = int64(len(buf))
	}
	req, err := b.newRequest(ctx, http.MethodPut, target, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s: %s", target, resp.Status)
	}
	return nil
}

// delete removes target via HTTP DELETE, tolerating a missing target.
func (b webBase) delete(ctx context.Context, target string) error {
	req, err := b.newRequest(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("DELETE %s: %s", target, resp.Status)
	}
	return nil
}

// headExists reports whether target exists using a HEAD request.
func (b webBase) headExists(ctx context.Context, target string) (bool, error) {
	req, err := b.newRequest(ctx, http.MethodHead, target, nil)
	if err != nil {
		return false, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s: %s", target, resp.Status)
	}
}

// connectWeb constructs an HTTP or WebDAV provider from the modal parameters.
// It returns the provider and the normalized starting directory URL.
func connectWeb(kind providerKind, rawURL, user, pass string) (Provider, string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return nil, "", errors.New("URL is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, "", errors.New("URL host is required")
	}
	// Move any credentials embedded in the URL into the explicit user/pass so
	// they never appear in displayed locations or error strings.
	if u.User != nil {
		if user == "" {
			user = u.User.Username()
		}
		if pass == "" {
			if p, ok := u.User.Password(); ok {
				pass = p
			}
		}
		u.User = nil
	}
	if u.Path == "" {
		u.Path = "/"
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	u.RawQuery = ""
	u.Fragment = ""
	scheme := "http"
	if kind == kindWebDAV {
		scheme = "webdav"
	}
	base := webBase{
		base:   u,
		user:   user,
		pass:   pass,
		label:  fmt.Sprintf("%s %s%s", scheme, u.Host, u.Path),
		client: newWebClient(user, pass),
	}
	root := u.String()
	if kind == kindWebDAV {
		return webdavProvider{base}, root, nil
	}
	return httpProvider{base}, root, nil
}

// ---- plain HTTP provider ----

// httpProvider lists directories by parsing the <a href> links of an HTML index
// page and uploads files with PUT.
type httpProvider struct {
	webBase
}

func (p httpProvider) List(ctx context.Context, location string) ([]Entry, error) {
	req, err := p.newRequest(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("GET %s: %s", location, resp.Status)
	}
	cur := resp.Request.URL
	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, err
	}

	var entries []Entry
	if parent, ok := p.Parent(location); ok {
		entries = append(entries, Entry{Name: "..", Path: parent, IsDir: true, parent: true})
	}
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			var href string
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					href = attr.Val
					break
				}
			}
			if href != "" {
				// Common autoindex listings (e.g. nginx) print the date and
				// byte size in the text node that follows the anchor.
				meta := ""
				if sib := n.NextSibling; sib != nil && sib.Type == html.TextNode {
					meta = sib.Data
				}
				if entry, ok := childEntry(cur, href, meta); ok && !seen[entry.Path] {
					seen[entry.Path] = true
					entries = append(entries, entry)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	sortEntries(entries)
	return entries, nil
}

// childEntry resolves an href against the current directory URL and returns an
// Entry only when it is a direct child (a single path segment below cur). meta
// is the trailing index text after the link (used to recover file size/date).
func childEntry(cur *url.URL, href, meta string) (Entry, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "?") {
		return Entry{}, false
	}
	ref, err := url.Parse(href)
	if err != nil {
		return Entry{}, false
	}
	resolved := cur.ResolveReference(ref)
	if !sameOrigin(resolved, cur) {
		return Entry{}, false
	}
	if !strings.HasPrefix(resolved.Path, cur.Path) {
		return Entry{}, false
	}
	rest := strings.TrimPrefix(resolved.Path, cur.Path)
	isDir := strings.HasSuffix(rest, "/")
	name := strings.TrimSuffix(rest, "/")
	if name == "" || strings.Contains(name, "/") {
		return Entry{}, false
	}
	display := name
	if unescaped, err := url.PathUnescape(name); err == nil {
		display = unescaped
	}
	child := *cur
	child.Path = cur.Path + rest
	child.RawQuery = ""
	child.Fragment = ""
	entry := Entry{Name: display, Path: child.String(), IsDir: isDir}
	if isDir {
		entry.Name += "/"
	} else {
		entry.Size, entry.LastModified = parseIndexMeta(meta)
	}
	return entry, true
}

// parseIndexMeta extracts a byte size and modification time from the trailing
// text of an autoindex row, e.g. "  03-Jun-2026 00:30   21595". Directories
// (size "-") and unrecognized formats yield a zero size and time.
func parseIndexMeta(meta string) (int64, time.Time) {
	fields := strings.Fields(meta)
	if len(fields) == 0 {
		return 0, time.Time{}
	}
	last := fields[len(fields)-1]
	if last == "-" {
		return 0, time.Time{}
	}
	size, err := strconv.ParseInt(last, 10, 64)
	if err != nil {
		return 0, time.Time{}
	}
	var mod time.Time
	if len(fields) >= 3 {
		stamp := fields[len(fields)-3] + " " + fields[len(fields)-2]
		for _, layout := range []string{"02-Jan-2006 15:04", "02-Jan-2006 15:04:05"} {
			if t, e := time.Parse(layout, stamp); e == nil {
				mod = t
				break
			}
		}
	}
	return size, mod
}

func (httpProvider) Mkdir(context.Context, string, string) (string, bool, error) {
	return "", false, errors.New("directory creation is not supported over plain HTTP")
}

func (p httpProvider) Create(ctx context.Context, target string, size int64, r io.Reader) error {
	return p.put(ctx, target, size, r)
}

func (p httpProvider) Exists(ctx context.Context, target string) (bool, error) {
	return p.headExists(ctx, target)
}

func (p httpProvider) Remove(ctx context.Context, entry Entry) error {
	return p.delete(ctx, entry.Path)
}

func (p httpProvider) Walk(ctx context.Context, entry Entry) ([]WalkItem, error) {
	return walkWeb(ctx, entry, p.List)
}

// ---- WebDAV provider ----

// webdavProvider lists directories with PROPFIND and creates collections with
// MKCOL.
type webdavProvider struct {
	webBase
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>` +
	`<D:propfind xmlns:D="DAV:"><D:prop>` +
	`<D:resourcetype/><D:getcontentlength/><D:getlastmodified/>` +
	`</D:prop></D:propfind>`

type davMultistatus struct {
	XMLName   xml.Name      `xml:"DAV: multistatus"`
	Responses []davResponse `xml:"DAV: response"`
}

type davResponse struct {
	Href      string        `xml:"DAV: href"`
	Propstats []davPropstat `xml:"DAV: propstat"`
}

type davPropstat struct {
	Status string  `xml:"DAV: status"`
	Prop   davProp `xml:"DAV: prop"`
}

type davProp struct {
	Collection    *struct{} `xml:"DAV: resourcetype>collection"`
	ContentLength string    `xml:"DAV: getcontentlength"`
	LastModified  string    `xml:"DAV: getlastmodified"`
}

// errNotFound is returned by propfind when the target does not exist.
var errNotFound = errors.New("not found")

func (p webdavProvider) propfind(ctx context.Context, location, depth string) (*davMultistatus, *url.URL, error) {
	req, err := p.newRequest(ctx, "PROPFIND", location, strings.NewReader(propfindBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, nil, errNotFound
	}
	if resp.StatusCode != http.StatusMultiStatus {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, nil, fmt.Errorf("PROPFIND %s: %s", location, resp.Status)
	}
	var ms davMultistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, nil, err
	}
	return &ms, resp.Request.URL, nil
}

func (p webdavProvider) List(ctx context.Context, location string) ([]Entry, error) {
	ms, cur, err := p.propfind(ctx, location, "1")
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if parent, ok := p.Parent(location); ok {
		entries = append(entries, Entry{Name: "..", Path: parent, IsDir: true, parent: true})
	}
	seen := map[string]bool{}
	for _, r := range ms.Responses {
		entry, ok := davEntry(cur, r)
		if !ok || seen[entry.Path] {
			continue
		}
		seen[entry.Path] = true
		entries = append(entries, entry)
	}
	sortEntries(entries)
	return entries, nil
}

// davEntry converts a PROPFIND response element into a direct-child Entry,
// skipping the directory's own (self) entry and anything deeper than one level.
func davEntry(cur *url.URL, r davResponse) (Entry, bool) {
	href := strings.TrimSpace(r.Href)
	if href == "" {
		return Entry{}, false
	}
	ref, err := url.Parse(href)
	if err != nil {
		return Entry{}, false
	}
	resolved := cur.ResolveReference(ref)
	if !sameOrigin(resolved, cur) {
		return Entry{}, false
	}
	if strings.TrimSuffix(resolved.Path, "/") == strings.TrimSuffix(cur.Path, "/") {
		return Entry{}, false
	}
	if !strings.HasPrefix(resolved.Path, cur.Path) {
		return Entry{}, false
	}
	rest := strings.Trim(strings.TrimPrefix(resolved.Path, cur.Path), "/")
	if rest == "" || strings.Contains(rest, "/") {
		return Entry{}, false
	}

	isDir := false
	var size int64
	var modTime time.Time
	for _, ps := range r.Propstats {
		if ps.Prop.Collection != nil {
			isDir = true
		}
		if ps.Prop.ContentLength != "" {
			if n, err := strconv.ParseInt(strings.TrimSpace(ps.Prop.ContentLength), 10, 64); err == nil {
				size = n
			}
		}
		if ps.Prop.LastModified != "" {
			if t, err := http.ParseTime(strings.TrimSpace(ps.Prop.LastModified)); err == nil {
				modTime = t
			}
		}
	}

	display := rest
	child := *cur
	child.Path = cur.Path + rest
	child.RawQuery = ""
	child.Fragment = ""
	if isDir {
		child.Path += "/"
		display += "/"
		size = 0
	}
	return Entry{Name: display, Path: child.String(), IsDir: isDir, Size: size, LastModified: modTime}, true
}

func (p webdavProvider) Mkdir(ctx context.Context, location, name string) (string, bool, error) {
	child, err := p.Join(location, name)
	if err != nil {
		return "", false, err
	}
	if !strings.HasSuffix(child, "/") {
		child += "/"
	}
	if err := p.mkcol(ctx, child); err != nil {
		return "", false, err
	}
	return child, false, nil
}

// mkcol creates a single collection, tolerating one that already exists.
func (p webdavProvider) mkcol(ctx context.Context, target string) error {
	req, err := p.newRequest(ctx, "MKCOL", target, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusMethodNotAllowed, http.StatusMovedPermanently:
		// 405/301 generally mean the collection already exists.
		return nil
	}
	return fmt.Errorf("MKCOL %s: %s", target, resp.Status)
}

// ensureParents creates any missing ancestor collections of target under base.
func (p webdavProvider) ensureParents(ctx context.Context, target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(u.Path, p.base.Path) {
		return nil
	}
	rest := strings.TrimPrefix(u.Path, p.base.Path)
	segs := strings.Split(rest, "/")
	if len(segs) <= 1 {
		return nil
	}
	dir := p.base.Path
	for _, seg := range segs[:len(segs)-1] {
		if seg == "" {
			continue
		}
		dir += seg + "/"
		cu := *u
		cu.Path = dir
		cu.RawQuery = ""
		cu.Fragment = ""
		if err := p.mkcol(ctx, cu.String()); err != nil {
			return err
		}
	}
	return nil
}

func (p webdavProvider) Create(ctx context.Context, target string, size int64, r io.Reader) error {
	if err := p.ensureParents(ctx, target); err != nil {
		return err
	}
	return p.put(ctx, target, size, r)
}

func (p webdavProvider) Exists(ctx context.Context, target string) (bool, error) {
	_, _, err := p.propfind(ctx, target, "0")
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (p webdavProvider) Remove(ctx context.Context, entry Entry) error {
	return p.delete(ctx, entry.Path)
}

func (p webdavProvider) Walk(ctx context.Context, entry Entry) ([]WalkItem, error) {
	return walkWeb(ctx, entry, p.List)
}

// walkWeb recursively expands a directory into a flat file list using the given
// List function, guarding against cycles via a visited set.
func walkWeb(ctx context.Context, entry Entry, list func(context.Context, string) ([]Entry, error)) ([]WalkItem, error) {
	base := copyTargetName(entry)
	visited := map[string]bool{}
	var items []WalkItem
	var recurse func(loc, rel string) error
	recurse = func(loc, rel string) error {
		if visited[loc] {
			return nil
		}
		visited[loc] = true
		entries, err := list(ctx, loc)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.parent {
				continue
			}
			childRel := path.Join(rel, strings.TrimSuffix(e.Name, "/"))
			if e.IsDir {
				if err := recurse(e.Path, childRel); err != nil {
					return err
				}
				continue
			}
			items = append(items, WalkItem{Path: e.Path, Rel: childRel, Size: e.Size, ModTime: e.LastModified})
		}
		return nil
	}
	if err := recurse(entry.Path, base); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Rel < items[j].Rel })
	return items, nil
}
