package tui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func entryByName(entries []Entry, name string) (Entry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

func TestConnectWebNormalizesURL(t *testing.T) {
	cases := []struct {
		in      string
		kind    providerKind
		wantErr bool
		want    string
	}{
		{in: "example.com:70/uploads", kind: kindHTTP, want: "http://example.com:70/uploads/"},
		{in: "http://h/dir/", kind: kindHTTP, want: "http://h/dir/"},
		{in: "https://h", kind: kindWebDAV, want: "https://h/"},
		{in: "ftp://h/x", kind: kindHTTP, wantErr: true},
		{in: "   ", kind: kindHTTP, wantErr: true},
	}
	for _, c := range cases {
		prov, root, err := connectWeb(c.kind, c.in, "", "")
		if c.wantErr {
			if err == nil {
				t.Fatalf("connectWeb(%q) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("connectWeb(%q) unexpected error: %v", c.in, err)
		}
		if root != c.want {
			t.Fatalf("connectWeb(%q) root = %q, want %q", c.in, root, c.want)
		}
		if c.kind == kindWebDAV {
			if _, ok := prov.(webdavProvider); !ok {
				t.Fatalf("connectWeb webdav returned %T", prov)
			}
		} else {
			if _, ok := prov.(httpProvider); !ok {
				t.Fatalf("connectWeb http returned %T", prov)
			}
		}
	}
}

func TestHTTPProviderListParsesIndex(t *testing.T) {
	const indexHTML = `<html><body><pre>
<a href="../">Parent Directory</a>
<a href="?C=N;O=D">sort</a>
<a href="#top">top</a>
<a href="file1.txt">file1.txt</a>                03-Jun-2026 00:30               21595
<a href="sub/">sub/</a>                          03-Jun-2026 00:31                   -
<a href="sub/deep.txt">deep file</a>
<a href="http://other.example/x">offsite</a>
</pre></body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/uploads/" {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, indexHTML)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	prov, root, err := connectWeb(kindHTTP, srv.URL+"/uploads", "", "")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := prov.List(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}

	f1, ok := entryByName(entries, "file1.txt")
	if !ok {
		t.Fatalf("file1.txt missing: %#v", entries)
	}
	if f1.Size != 21595 {
		t.Fatalf("file1.txt size = %d, want 21595", f1.Size)
	}
	sub, ok := entryByName(entries, "sub/")
	if !ok || !sub.IsDir {
		t.Fatalf("sub/ dir missing or not a dir: %#v", entries)
	}
	// Nested, query, fragment, and off-site links must be excluded.
	for _, bad := range []string{"deep file", "sort", "top", "offsite"} {
		if _, ok := entryByName(entries, bad); ok {
			t.Fatalf("unexpected entry %q included: %#v", bad, entries)
		}
	}
}

func TestHTTPProviderCreateAndExists(t *testing.T) {
	var got []byte
	var gotCT string
	var gotCL int64
	var chunked bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			gotCT = r.Header.Get("Content-Type")
			gotCL = r.ContentLength
			chunked = len(r.TransferEncoding) > 0 && r.TransferEncoding[0] == "chunked"
			got, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		case http.MethodHead:
			if got != nil {
				w.WriteHeader(http.StatusOK)
			} else {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	prov, root, err := connectWeb(kindHTTP, srv.URL+"/uploads", "", "")
	if err != nil {
		t.Fatal(err)
	}
	target, err := prov.Join(root, "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	body := "hello world"
	if err := prov.Create(context.Background(), target, int64(len(body)), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("uploaded body = %q, want %q", got, body)
	}
	if gotCT == "" {
		t.Fatal("PUT sent no Content-Type")
	}
	if gotCL != int64(len(body)) {
		t.Fatalf("PUT Content-Length = %d, want %d", gotCL, len(body))
	}
	if chunked {
		t.Fatal("PUT used chunked transfer encoding")
	}
	exists, err := prov.Exists(context.Background(), target)
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v; want true,nil", exists, err)
	}
}

func TestHTTPProviderCreateUnknownSizeBuffers(t *testing.T) {
	var gotCL int64
	var chunked bool
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			gotCL = r.ContentLength
			chunked = len(r.TransferEncoding) > 0 && r.TransferEncoding[0] == "chunked"
			got, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	prov, root, err := connectWeb(kindHTTP, srv.URL+"/uploads", "", "")
	if err != nil {
		t.Fatal(err)
	}
	target, _ := prov.Join(root, "blob.bin")
	body := "unknown-size body"
	// size -1 signals unknown; put must buffer and still send Content-Length.
	if err := prov.Create(context.Background(), target, -1, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
	if gotCL != int64(len(body)) {
		t.Fatalf("Content-Length = %d, want %d", gotCL, len(body))
	}
	if chunked {
		t.Fatal("unknown-size PUT used chunked transfer encoding")
	}
}

func TestWebDAVProviderListParsesPropfind(t *testing.T) {
	const ms = `<?xml version="1.0"?>
	<D:multistatus xmlns:D="DAV:">
	  <D:response>
	    <D:href>/dav/</D:href>
	    <D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop>
	      <D:status>HTTP/1.1 200 OK</D:status></D:propstat>
	  </D:response>
	  <D:response>
	    <D:href>/dav/notes.txt</D:href>
	    <D:propstat><D:prop>
	      <D:resourcetype/>
	      <D:getcontentlength>12</D:getcontentlength>
	    </D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
	  </D:response>
	  <D:response>
	    <D:href>/dav/photos/</D:href>
	    <D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop>
	      <D:status>HTTP/1.1 200 OK</D:status></D:propstat>
	  </D:response>
	</D:multistatus>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" && r.URL.Path == "/dav/" {
			w.WriteHeader(http.StatusMultiStatus)
			io.WriteString(w, ms)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	prov, root, err := connectWeb(kindWebDAV, srv.URL+"/dav", "", "")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := prov.List(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	notes, ok := entryByName(entries, "notes.txt")
	if !ok || notes.IsDir || notes.Size != 12 {
		t.Fatalf("notes.txt entry wrong: %#v", entries)
	}
	photos, ok := entryByName(entries, "photos/")
	if !ok || !photos.IsDir {
		t.Fatalf("photos/ dir missing: %#v", entries)
	}
	// The collection's own self entry (/dav/) must be skipped.
	for _, e := range entries {
		if e.Name == "/dav/" || e.Name == "dav/" {
			t.Fatalf("self entry not skipped: %#v", entries)
		}
	}
}

func TestConnectWebExtractsUserinfo(t *testing.T) {
	prov, root, err := connectWeb(kindHTTP, "http://bob:secret@h/dir", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(root, "bob") || strings.Contains(root, "secret") {
		t.Fatalf("credentials leaked into root URL: %q", root)
	}
	p := prov.(httpProvider)
	if p.user != "bob" || p.pass != "secret" {
		t.Fatalf("userinfo not extracted: user=%q pass=%q", p.user, p.pass)
	}
}

func TestWebPutRejectsMethodChangingRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/uploads/x.txt":
			// 302 to a GET-able location; a compliant client would rewrite the
			// PUT to GET. Our client must refuse this rather than no-op succeed.
			http.Redirect(w, r, "/uploads/elsewhere", http.StatusFound)
		case "/uploads/elsewhere":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	prov, root, err := connectWeb(kindHTTP, srv.URL+"/uploads", "", "")
	if err != nil {
		t.Fatal(err)
	}
	target, _ := prov.Join(root, "x.txt")
	if err := prov.Create(context.Background(), target, 3, strings.NewReader("abc")); err == nil {
		t.Fatal("expected PUT to fail on method-changing redirect, got nil")
	}
}

func TestWebDAVExistsDistinguishesMissingFromError(t *testing.T) {
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()

	prov, root, err := connectWeb(kindWebDAV, srv.URL+"/dav", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p := prov.(webdavProvider)

	status = http.StatusNotFound
	exists, err := p.Exists(context.Background(), root)
	if err != nil || exists {
		t.Fatalf("404: exists=%v err=%v; want false,nil", exists, err)
	}

	status = http.StatusInternalServerError
	if _, err := p.Exists(context.Background(), root); err == nil {
		t.Fatal("500: expected error to propagate, got nil")
	}
}

func TestWebProviderParentBoundedByRoot(t *testing.T) {
	prov, root, err := connectWeb(kindHTTP, "http://h/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p := prov.(httpProvider)
	parent, ok := p.Parent("http://h/a/b/")
	if !ok || parent != "http://h/a/" {
		t.Fatalf("Parent = %q,%v; want http://h/a/,true", parent, ok)
	}
	// At the root, there is no parent.
	if _, ok := p.Parent(root); ok {
		t.Fatalf("expected no parent above root")
	}
}
