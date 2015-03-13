// Package gzip_file_server ...
package gzip_file_server

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"golang.org/x/tools/godoc/vfs"
	"golang.org/x/tools/godoc/vfs/httpfs"
)

type gzipFileServer struct {
	root http.FileSystem
}

// New returns a raw file server, that serves the given virtual file system without special handling of index.html.
func New(root vfs.FileSystem) http.Handler {
	return &gzipFileServer{root: httpfs.New(root)}
}

// NewUsingHttpFs returns a raw file server.
func NewUsingHttpFs(root http.FileSystem) http.Handler {
	return &gzipFileServer{root: root}
}

func (f *gzipFileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/") {
		r.URL.Path = "/" + r.URL.Path
	}
	serveFile(w, r, f.root, path.Clean(r.URL.Path))
}

func dirList(w http.ResponseWriter, f http.File, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<pre>\n")
	fmt.Fprintf(w, "<a href=\"%s\">%s</a>\n", path.Clean(name+"/.."), "..")
	for {
		dirs, err := f.Readdir(100)
		if err != nil || len(dirs) == 0 {
			break
		}
		sort.Sort(byName(dirs))
		for _, d := range dirs {
			name := d.Name()
			if d.IsDir() {
				name += "/"
			}
			// name may contain '?' or '#', which must be escaped to remain
			// part of the URL path, and not indicate the start of a query
			// string or fragment.
			url := url.URL{Path: name}
			fmt.Fprintf(w, "<a href=\"%s\">%s</a>\n", url.String(), html.EscapeString(name))
		}
	}
	fmt.Fprintf(w, "</pre>\n")
}

// name is '/'-separated, not filepath.Separator.
func serveFile(w http.ResponseWriter, r *http.Request, fs http.FileSystem, name string) {
	f, err := fs.Open(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	d, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// redirect to canonical path: / at end of directory url
	// r.URL.Path always begins with /
	url := r.URL.Path
	if d.IsDir() {
		if url[len(url)-1] != '/' {
			localRedirect(w, r, path.Base(url)+"/")
			return
		}
	} else {
		if url[len(url)-1] == '/' {
			localRedirect(w, r, "../"+path.Base(url))
			return
		}
	}

	// A directory?
	if d.IsDir() {
		// TODO: Consider using checkLastModified?
		/*if checkLastModified(w, r, d.ModTime()) {
			return
		}*/
		dirList(w, f, name)
		return
	}

	if _, plain := r.URL.Query()["plain"]; plain {
		w.Header().Set("Content-Type", "text/plain")
	}
	switch gzipFile, ok := f.(gzipByter); {
	case ok && isGzipEncodingAccepted(r):
		w.Header().Set("Content-Encoding", "gzip")
		http.ServeContent(w, r, d.Name(), d.ModTime(), bytes.NewReader(gzipFile.GzipBytes()))
	default:
		http.ServeContent(w, r, d.Name(), d.ModTime(), f)
	}
}

type gzipByter interface {
	GzipBytes() []byte
}

// localRedirect gives a Moved Permanently response.
// It does not convert relative paths to absolute paths like Redirect does.
func localRedirect(w http.ResponseWriter, r *http.Request, newPath string) {
	if q := r.URL.RawQuery; q != "" {
		newPath += "?" + q
	}
	w.Header().Set("Location", newPath)
	w.WriteHeader(http.StatusMovedPermanently)
}

// byName implements sort.Interface.
type byName []os.FileInfo

func (f byName) Len() int           { return len(f) }
func (f byName) Less(i, j int) bool { return f[i].Name() < f[j].Name() }
func (f byName) Swap(i, j int)      { f[i], f[j] = f[j], f[i] }

// isGzipEncodingAccepted returns true if the request includes "gzip" under Accept-Encoding header.
func isGzipEncodingAccepted(req *http.Request) bool {
	for _, v := range strings.Split(req.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(v) == "gzip" {
			return true
		}
	}
	return false
}
