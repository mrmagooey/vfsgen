package vfsgen

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	pathpkg "path"
	"sort"
	"strconv"
	"text/template"
	"time"

	"github.com/shurcooL/httpfs/vfsutil"
)

var t = &template.Template{}

func comment(s string) (string, error) {
	var buf bytes.Buffer
	cw := &commentWriter{W: &buf}
	_, err := io.WriteString(cw, s)
	if err != nil {
		return "", err
	}
	err = cw.Close()
	return buf.String(), err
}

func regenTemplateWithOptions(opt Options) {
	newTemplateString := &bytes.Buffer{}
	templateWithOptions := template.Must(template.New("").Delims("<<<", ">>>").Parse(templateString))
	err := templateWithOptions.Execute(newTemplateString, opt)
	if err != nil {
		log.Fatal(err)
	}
	t = template.Must(template.New("").Funcs(template.FuncMap{
		"quote":   strconv.Quote,
		"comment": comment,
	}).Parse(string(newTemplateString.String())))
}

// Generate Go code that statically implements input filesystem,
// write the output to a file specified in opt.
func Generate(input http.FileSystem, opt Options) error {
	opt.fillMissing()
	// regenerate template string with options filled in, subsequent calls will use this template
	regenTemplateWithOptions(opt)

	// Use an in-memory buffer to generate the entire output.
	buf := new(bytes.Buffer)

	err := t.ExecuteTemplate(buf, "Header", opt)
	if err != nil {
		return err
	}

	var toc toc
	err = findAndWriteFiles(buf, input, &toc)
	if err != nil {
		return err
	}

	err = t.ExecuteTemplate(buf, "DirEntries", toc.dirs)
	if err != nil {
		return err
	}

	err = t.ExecuteTemplate(buf, "Trailer", toc)
	if err != nil {
		return err
	}

	// Write output file (all at once).
	fmt.Println("writing", opt.Filename)
	err = ioutil.WriteFile(opt.Filename, buf.Bytes(), 0644)
	return err
}

type toc struct {
	dirs []*dirInfo

	HasCompressedFile bool // There's at least one compressedFile.
	HasFile           bool // There's at least one uncompressed file.
}

// fileInfo is a definition of a file.
type fileInfo struct {
	Path             string
	Name             string
	ModTime          time.Time
	UncompressedSize int64
}

// dirInfo is a definition of a directory.
type dirInfo struct {
	Path    string
	Name    string
	ModTime time.Time
	Entries []string
}

// findAndWriteFiles recursively finds all the file paths in the given directory tree.
// They are added to the given map as keys. Values will be safe function names
// for each file, which will be used when generating the output code.
func findAndWriteFiles(buf *bytes.Buffer, fs http.FileSystem, toc *toc) error {
	walkFn := func(path string, fi os.FileInfo, r io.ReadSeeker, err error) error {
		if err != nil {
			// Consider all errors reading the input filesystem as fatal.
			return err
		}

		switch fi.IsDir() {
		case false:
			file := &fileInfo{
				Path:             path,
				Name:             pathpkg.Base(path),
				ModTime:          fi.ModTime().UTC(),
				UncompressedSize: fi.Size(),
			}

			marker := buf.Len()

			// Write CompressedFileInfo.
			err = writeCompressedFileInfo(buf, file, r)
			switch err {
			default:
				return err
			case nil:
				toc.HasCompressedFile = true
			// If compressed file is not smaller than original, revert and write original file.
			case errCompressedNotSmaller:
				_, err = r.Seek(0, io.SeekStart)
				if err != nil {
					return err
				}

				buf.Truncate(marker)

				// Write FileInfo.
				err = writeFileInfo(buf, file, r)
				if err != nil {
					return err
				}
				toc.HasFile = true
			}
		case true:
			entries, err := readDirPaths(fs, path)
			if err != nil {
				return err
			}

			dir := &dirInfo{
				Path:    path,
				Name:    pathpkg.Base(path),
				ModTime: fi.ModTime().UTC(),
				Entries: entries,
			}

			toc.dirs = append(toc.dirs, dir)

			// Write DirInfo.
			err = t.ExecuteTemplate(buf, "DirInfo", dir)
			if err != nil {
				return err
			}
		}

		return nil
	}

	err := vfsutil.WalkFiles(fs, "/", walkFn)
	return err
}

// readDirPaths reads the directory named by dirname and returns
// a sorted list of directory paths.
func readDirPaths(fs http.FileSystem, dirname string) ([]string, error) {
	fis, err := vfsutil.ReadDir(fs, dirname)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(fis))
	for i := range fis {
		paths[i] = pathpkg.Join(dirname, fis[i].Name())
	}
	sort.Strings(paths)
	return paths, nil
}

// writeCompressedFileInfo writes CompressedFileInfo.
// It returns errCompressedNotSmaller if compressed file is not smaller than original.
func writeCompressedFileInfo(w io.Writer, file *fileInfo, r io.Reader) error {
	err := t.ExecuteTemplate(w, "CompressedFileInfo-Before", file)
	if err != nil {
		return err
	}
	sw := &stringWriter{Writer: w}
	gw := gzip.NewWriter(sw)
	_, err = io.Copy(gw, r)
	if err != nil {
		return err
	}
	err = gw.Close()
	if err != nil {
		return err
	}
	if sw.N >= file.UncompressedSize {
		return errCompressedNotSmaller
	}
	err = t.ExecuteTemplate(w, "CompressedFileInfo-After", file)
	return err
}

var errCompressedNotSmaller = errors.New("compressed file is not smaller than original")

// Write FileInfo.
func writeFileInfo(w io.Writer, file *fileInfo, r io.Reader) error {
	err := t.ExecuteTemplate(w, "FileInfo-Before", file)
	if err != nil {
		return err
	}
	sw := &stringWriter{Writer: w}
	_, err = io.Copy(sw, r)
	if err != nil {
		return err
	}
	err = t.ExecuteTemplate(w, "FileInfo-After", file)
	return err
}

var templateString = `{{define "Header"}}// Code generated by vfsgen; DO NOT EDIT.

{{with .BuildTags}}// +build {{.}}

{{end}}package {{.PackageName}}

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	pathpkg "path"
	"time"
)

{{comment .VariableComment}}
var {{.VariableName}} = func() http.FileSystem {
	fs := vfsgen<<<.VariableName>>>۰FS{
{{end}}



{{define "CompressedFileInfo-Before"}}		{{quote .Path}}: &vfsgen<<<.VariableName>>>۰CompressedFileInfo{
			name:             {{quote .Name}},
			modTime:          {{template "Time" .ModTime}},
			uncompressedSize: {{.UncompressedSize}},
{{/* This blank line separating compressedContent is neccessary to prevent potential gofmt issues. See issue #19. */}}
			compressedContent: []byte("{{end}}{{define "CompressedFileInfo-After"}}"),
		},
{{end}}



{{define "FileInfo-Before"}}		{{quote .Path}}: &vfsgen<<<.VariableName>>>۰FileInfo{
			name:    {{quote .Name}},
			modTime: {{template "Time" .ModTime}},
			content: []byte("{{end}}{{define "FileInfo-After"}}"),
		},
{{end}}



{{define "DirInfo"}}		{{quote .Path}}: &vfsgen<<<.VariableName>>>۰DirInfo{
			name:    {{quote .Name}},
			modTime: {{template "Time" .ModTime}},
		},
{{end}}



{{define "DirEntries"}}	}
{{range .}}{{if .Entries}}	fs[{{quote .Path}}].(*vfsgen<<<.VariableName>>>۰DirInfo).entries = []os.FileInfo{{"{"}}{{range .Entries}}
		fs[{{quote .}}].(os.FileInfo),{{end}}
	}
{{end}}{{end}}
	return fs
}()
{{end}}



{{define "Trailer"}}
type vfsgen<<<.VariableName>>>۰FS map[string]interface{}

func (fs vfsgen<<<.VariableName>>>۰FS) Open(path string) (http.File, error) {
	path = pathpkg.Clean("/" + path)
	f, ok := fs[path]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}

	switch f := f.(type) {{"{"}}{{if .HasCompressedFile}}
	case *vfsgen<<<.VariableName>>>۰CompressedFileInfo:
		gr, err := gzip.NewReader(bytes.NewReader(f.compressedContent))
		if err != nil {
			// This should never happen because we generate the gzip bytes such that they are always valid.
			panic("unexpected error reading own gzip compressed bytes: " + err.Error())
		}
		return &vfsgen<<<.VariableName>>>۰CompressedFile{
			vfsgen<<<.VariableName>>>۰CompressedFileInfo: f,
			gr:                        gr,
		}, nil{{end}}{{if .HasFile}}
	case *vfsgen<<<.VariableName>>>۰FileInfo:
		return &vfsgen<<<.VariableName>>>۰File{
			vfsgen<<<.VariableName>>>۰FileInfo: f,
			Reader:          bytes.NewReader(f.content),
		}, nil{{end}}
	case *vfsgen<<<.VariableName>>>۰DirInfo:
		return &vfsgen<<<.VariableName>>>۰Dir{
			vfsgen<<<.VariableName>>>۰DirInfo: f,
		}, nil
	default:
		// This should never happen because we generate only the above types.
		panic(fmt.Sprintf("unexpected type %T", f))
	}
}
{{if .HasCompressedFile}}
// vfsgen<<<.VariableName>>>۰CompressedFileInfo is a static definition of a gzip compressed file.
type vfsgen<<<.VariableName>>>۰CompressedFileInfo struct {
	name              string
	modTime           time.Time
	compressedContent []byte
	uncompressedSize  int64
}

func (f *vfsgen<<<.VariableName>>>۰CompressedFileInfo) Readdir(count int) ([]os.FileInfo, error) {
	return nil, fmt.Errorf("cannot Readdir from file %s", f.name)
}
func (f *vfsgen<<<.VariableName>>>۰CompressedFileInfo) Stat() (os.FileInfo, error) { return f, nil }

func (f *vfsgen<<<.VariableName>>>۰CompressedFileInfo) GzipBytes() []byte {
	return f.compressedContent
}

func (f *vfsgen<<<.VariableName>>>۰CompressedFileInfo) Name() string       { return f.name }
func (f *vfsgen<<<.VariableName>>>۰CompressedFileInfo) Size() int64        { return f.uncompressedSize }
func (f *vfsgen<<<.VariableName>>>۰CompressedFileInfo) Mode() os.FileMode  { return 0444 }
func (f *vfsgen<<<.VariableName>>>۰CompressedFileInfo) ModTime() time.Time { return f.modTime }
func (f *vfsgen<<<.VariableName>>>۰CompressedFileInfo) IsDir() bool        { return false }
func (f *vfsgen<<<.VariableName>>>۰CompressedFileInfo) Sys() interface{}   { return nil }

// vfsgen<<<.VariableName>>>۰CompressedFile is an opened compressedFile instance.
type vfsgen<<<.VariableName>>>۰CompressedFile struct {
	*vfsgen<<<.VariableName>>>۰CompressedFileInfo
	gr      *gzip.Reader
	grPos   int64 // Actual gr uncompressed position.
	seekPos int64 // Seek uncompressed position.
}

func (f *vfsgen<<<.VariableName>>>۰CompressedFile) Read(p []byte) (n int, err error) {
	if f.grPos > f.seekPos {
		// Rewind to beginning.
		err = f.gr.Reset(bytes.NewReader(f.compressedContent))
		if err != nil {
			return 0, err
		}
		f.grPos = 0
	}
	if f.grPos < f.seekPos {
		// Fast-forward.
		_, err = io.CopyN(ioutil.Discard, f.gr, f.seekPos-f.grPos)
		if err != nil {
			return 0, err
		}
		f.grPos = f.seekPos
	}
	n, err = f.gr.Read(p)
	f.grPos += int64(n)
	f.seekPos = f.grPos
	return n, err
}
func (f *vfsgen<<<.VariableName>>>۰CompressedFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.seekPos = 0 + offset
	case io.SeekCurrent:
		f.seekPos += offset
	case io.SeekEnd:
		f.seekPos = f.uncompressedSize + offset
	default:
		panic(fmt.Errorf("invalid whence value: %v", whence))
	}
	return f.seekPos, nil
}
func (f *vfsgen<<<.VariableName>>>۰CompressedFile) Close() error {
	return f.gr.Close()
}
{{else}}
// We already imported "compress/gzip" and "io/ioutil", but ended up not using them. Avoid unused import error.
var _ = gzip.Reader{}
var _ = ioutil.Discard
{{end}}{{if .HasFile}}
// vfsgen<<<.VariableName>>>۰FileInfo is a static definition of an uncompressed file (because it's not worth gzip compressing).
type vfsgen<<<.VariableName>>>۰FileInfo struct {
	name    string
	modTime time.Time
	content []byte
}

func (f *vfsgen<<<.VariableName>>>۰FileInfo) Readdir(count int) ([]os.FileInfo, error) {
	return nil, fmt.Errorf("cannot Readdir from file %s", f.name)
}
func (f *vfsgen<<<.VariableName>>>۰FileInfo) Stat() (os.FileInfo, error) { return f, nil }

func (f *vfsgen<<<.VariableName>>>۰FileInfo) NotWorthGzipCompressing() {}

func (f *vfsgen<<<.VariableName>>>۰FileInfo) Name() string       { return f.name }
func (f *vfsgen<<<.VariableName>>>۰FileInfo) Size() int64        { return int64(len(f.content)) }
func (f *vfsgen<<<.VariableName>>>۰FileInfo) Mode() os.FileMode  { return 0444 }
func (f *vfsgen<<<.VariableName>>>۰FileInfo) ModTime() time.Time { return f.modTime }
func (f *vfsgen<<<.VariableName>>>۰FileInfo) IsDir() bool        { return false }
func (f *vfsgen<<<.VariableName>>>۰FileInfo) Sys() interface{}   { return nil }

// vfsgen<<<.VariableName>>>۰File is an opened file instance.
type vfsgen<<<.VariableName>>>۰File struct {
	*vfsgen<<<.VariableName>>>۰FileInfo
	*bytes.Reader
}

func (f *vfsgen<<<.VariableName>>>۰File) Close() error {
	return nil
}
{{else if not .HasCompressedFile}}
// We already imported "bytes", but ended up not using it. Avoid unused import error.
var _ = bytes.Reader{}
{{end}}
// vfsgen<<<.VariableName>>>۰DirInfo is a static definition of a directory.
type vfsgen<<<.VariableName>>>۰DirInfo struct {
	name    string
	modTime time.Time
	entries []os.FileInfo
}

func (d *vfsgen<<<.VariableName>>>۰DirInfo) Read([]byte) (int, error) {
	return 0, fmt.Errorf("cannot Read from directory %s", d.name)
}
func (d *vfsgen<<<.VariableName>>>۰DirInfo) Close() error               { return nil }
func (d *vfsgen<<<.VariableName>>>۰DirInfo) Stat() (os.FileInfo, error) { return d, nil }

func (d *vfsgen<<<.VariableName>>>۰DirInfo) Name() string       { return d.name }
func (d *vfsgen<<<.VariableName>>>۰DirInfo) Size() int64        { return 0 }
func (d *vfsgen<<<.VariableName>>>۰DirInfo) Mode() os.FileMode  { return 0755 | os.ModeDir }
func (d *vfsgen<<<.VariableName>>>۰DirInfo) ModTime() time.Time { return d.modTime }
func (d *vfsgen<<<.VariableName>>>۰DirInfo) IsDir() bool        { return true }
func (d *vfsgen<<<.VariableName>>>۰DirInfo) Sys() interface{}   { return nil }

// vfsgen<<<.VariableName>>>۰Dir is an opened dir instance.
type vfsgen<<<.VariableName>>>۰Dir struct {
	*vfsgen<<<.VariableName>>>۰DirInfo
	pos int // Position within entries for Seek and Readdir.
}

func (d *vfsgen<<<.VariableName>>>۰Dir) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekStart {
		d.pos = 0
		return 0, nil
	}
	return 0, fmt.Errorf("unsupported Seek in directory %s", d.name)
}

func (d *vfsgen<<<.VariableName>>>۰Dir) Readdir(count int) ([]os.FileInfo, error) {
	if d.pos >= len(d.entries) && count > 0 {
		return nil, io.EOF
	}
	if count <= 0 || count > len(d.entries)-d.pos {
		count = len(d.entries) - d.pos
	}
	e := d.entries[d.pos : d.pos+count]
	d.pos += count
	return e, nil
}
{{end}}



{{define "Time"}}
{{- if .IsZero -}}
	time.Time{}
{{- else -}}
	time.Date({{.Year}}, {{printf "%d" .Month}}, {{.Day}}, {{.Hour}}, {{.Minute}}, {{.Second}}, {{.Nanosecond}}, time.UTC)
{{- end -}}
{{end}}
`
