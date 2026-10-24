// Package host reads a host file and edits its profiles and modules arrays
// in place.
//
// The host file is written by people, so arcus rewrites only those two
// arrays and leaves every other byte (comments, blank lines) as it was.
package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/grunclepug/arcus/internal/atomicfile"
	"github.com/grunclepug/arcus/internal/module"
)

// File is a loaded host file.
type File struct {
	Path string
	// Profiles are expanded first, in order; Modules are this machine's own
	// additions on top of them.
	Profiles []string
	Modules  []string

	src []byte
}

type lists struct {
	Profiles []string `toml:"profiles"`
	Modules  []string `toml:"modules"`
}

// New returns an empty host file that Save will create at path.
func New(path string) *File {
	return &File{Path: path, Profiles: []string{}, Modules: []string{}}
}

// Load reads the host file at path. A missing file is an error wrapping
// fs.ErrNotExist: treating it as empty would make apply turn everything off.
func Load(path string) (*File, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading host file: %w", err)
	}
	l, err := parse(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &File{Path: path, Profiles: l.Profiles, Modules: l.Modules, src: src}, nil
}

func parse(src []byte) (lists, error) {
	var l lists
	md, err := toml.Decode(string(src), &l)
	if err != nil {
		return l, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return l, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	for _, list := range [][]string{l.Profiles, l.Modules} {
		for i, name := range list {
			if err := module.ValidateName(name); err != nil {
				return l, err
			}
			if slices.Contains(list[:i], name) {
				return l, fmt.Errorf("%q is listed twice", name)
			}
		}
	}
	if l.Profiles == nil {
		l.Profiles = []string{}
	}
	if l.Modules == nil {
		l.Modules = []string{}
	}
	return l, nil
}

func add(list *[]string, name string) bool {
	if slices.Contains(*list, name) {
		return false
	}
	*list = append(*list, name)
	return true
}

func remove(list *[]string, name string) bool {
	i := slices.Index(*list, name)
	if i < 0 {
		return false
	}
	*list = slices.Delete(*list, i, i+1)
	return true
}

// AddModule appends name to the modules and reports whether it was missing.
func (f *File) AddModule(name string) bool { return add(&f.Modules, name) }

// RemoveModule drops name from the modules and reports whether it was there.
func (f *File) RemoveModule(name string) bool { return remove(&f.Modules, name) }

// AddProfile appends name to the profiles and reports whether it was missing.
func (f *File) AddProfile(name string) bool { return add(&f.Profiles, name) }

// RemoveProfile drops name from the profiles and reports whether it was there.
func (f *File) RemoveProfile(name string) bool { return remove(&f.Profiles, name) }

// Save writes both arrays back, one entry per line. Comments outside the
// arrays survive; comments inside them do not. The result is parsed again
// before it is written, so a bug here can't leave a broken host file.
func (f *File) Save() error {
	out := f.src
	for _, a := range []struct {
		key  string
		list []string
	}{{"profiles", f.Profiles}, {"modules", f.Modules}} {
		var err error
		if out, err = render(out, a.key, a.list); err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	got, err := parse(out)
	if err != nil || !slices.Equal(got.Profiles, f.Profiles) || !slices.Equal(got.Modules, f.Modules) {
		return fmt.Errorf("%s: rewriting the arrays produced %+v (%v); file left unchanged", f.Path, got, err)
	}

	// Write through a symlinked host file instead of replacing the link.
	path, mode := f.Path, os.FileMode(0o644)
	if real, err := filepath.EvalSymlinks(f.Path); err == nil {
		path = real
		if info, err := os.Stat(real); err == nil {
			mode = info.Mode().Perm()
		}
	}
	if err := atomicfile.Write(path, out, mode); err != nil {
		return fmt.Errorf("saving host file: %w", err)
	}
	f.src = out
	return nil
}

// render replaces the top-level array key in src. If src has no such key, an
// array is appended, unless list is empty.
func render(src []byte, key string, list []string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(key + " = [\n")
	for _, name := range list {
		fmt.Fprintf(&b, "  %q,\n", name)
	}
	b.WriteString("]")
	array := b.String()

	start, end, err := findArray(src, key)
	if err != nil {
		return nil, err
	}
	if start < 0 {
		if len(list) == 0 {
			return src, nil
		}
		out := bytes.Clone(src)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		return append(out, array+"\n"...), nil
	}
	out := append(bytes.Clone(src[:start]), array...)
	return append(out, src[end:]...), nil
}

// findArray returns the byte range from the start of the top-level key to
// just past its closing bracket, or -1, -1 if there is no such key. It skips
// comments and strings, so a "]" inside them doesn't end the array.
func findArray(src []byte, key string) (start, end int, err error) {
	re := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*=[ \t]*\[`)
	loc := re.FindIndex(src)
	if loc == nil {
		return -1, -1, nil
	}
	depth := 1
	for i := loc[1]; i < len(src); i++ {
		switch c := src[i]; c {
		case '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case '"', '\'':
			for i++; i < len(src) && src[i] != c && src[i] != '\n'; i++ {
				if c == '"' && src[i] == '\\' {
					i++
				}
			}
		case '[':
			depth++
		case ']':
			if depth--; depth == 0 {
				return loc[0], i + 1, nil
			}
		}
	}
	return -1, -1, fmt.Errorf("%s array is not closed", key)
}
