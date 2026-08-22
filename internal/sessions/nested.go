// Nested folder paths for the session tree.
//
// A folder path is slash-separated leaf names, e.g. "3_Customers/PBB/Hagerstown MD".
// Leaf names may contain spaces but not "/". The display tree shows only the
// leaf; the path is what Add/Replace/Import and the widget UIDs share.
package sessions

import (
	"fmt"
	"strings"
)

// PathSep joins folder names into a stable path key.
const PathSep = "/"

// SplitPath breaks a folder path into leaf names.
func SplitPath(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	// Accept the older path-encoded form "A / B / C" from flat CRT imports.
	if strings.Contains(path, " / ") && !strings.Contains(path, PathSep) {
		parts := strings.Split(path, " / ")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	raw := strings.Split(path, PathSep)
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// JoinPath builds a folder path from leaf names.
func JoinPath(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			clean = append(clean, p)
		}
	}
	return strings.Join(clean, PathSep)
}

// PathLeaf is the last segment of a path (the name shown in the tree).
func PathLeaf(path string) string {
	parts := SplitPath(path)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// walkFolder finds a folder by path from the tree root. nil if missing.
func (t *Tree) walkFolder(parts []string) *Folder {
	if len(parts) == 0 {
		return nil
	}
	list := &t.Folders
	var cur *Folder
	for _, name := range parts {
		want := strings.ToLower(name)
		found := -1
		for i := range *list {
			if strings.ToLower(strings.TrimSpace((*list)[i].Name)) == want {
				found = i
				break
			}
		}
		if found < 0 {
			return nil
		}
		cur = &(*list)[found]
		list = &cur.Folders
	}
	return cur
}

// EnsurePath creates every folder on the path and returns the leaf folder.
func (t *Tree) EnsurePath(path string) (*Folder, error) {
	parts := SplitPath(path)
	if len(parts) == 0 {
		return nil, fmt.Errorf("folder path is required")
	}
	list := &t.Folders
	var cur *Folder
	for _, name := range parts {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("empty folder name in path")
		}
		if strings.Contains(name, PathSep) {
			return nil, fmt.Errorf("folder name %q must not contain %q", name, PathSep)
		}
		want := strings.ToLower(name)
		found := -1
		for i := range *list {
			if strings.ToLower(strings.TrimSpace((*list)[i].Name)) == want {
				found = i
				break
			}
		}
		if found < 0 {
			*list = append(*list, Folder{Name: name})
			found = len(*list) - 1
		}
		cur = &(*list)[found]
		list = &cur.Folders
	}
	return cur, nil
}

// FolderAt returns the folder at path, or an error if missing.
func (t *Tree) FolderAt(path string) (*Folder, error) {
	parts := SplitPath(path)
	if len(parts) == 0 {
		return nil, fmt.Errorf("folder path is required")
	}
	f := t.walkFolder(parts)
	if f == nil {
		return nil, fmt.Errorf("no folder called %q", path)
	}
	return f, nil
}

// ExpandEncodedPaths turns legacy flat folders named "A / B / C" into nested
// folders. Safe to run on every load: already-nested trees are unchanged.
func (t *Tree) ExpandEncodedPaths() {
	old := append([]Folder(nil), t.Folders...)
	t.Folders = nil
	for _, f := range old {
		if strings.Contains(f.Name, " / ") && len(f.Folders) == 0 {
			parts := SplitPath(f.Name)
			if len(parts) <= 1 {
				t.Folders = append(t.Folders, f)
				continue
			}
			path := JoinPath(parts...)
			leaf, err := t.EnsurePath(path)
			if err != nil {
				t.Folders = append(t.Folders, f)
				continue
			}
			leaf.Sessions = append(leaf.Sessions, f.Sessions...)
			continue
		}
		t.Folders = append(t.Folders, f)
	}
}

// WalkSessions calls fn for every session with the folder path that holds it.
func (t Tree) WalkSessions(fn func(folder string, n Node)) {
	var walk func(prefix string, folders []Folder)
	walk = func(prefix string, folders []Folder) {
		for _, f := range folders {
			p := f.Name
			if prefix != "" {
				p = JoinPath(prefix, f.Name)
			}
			for _, n := range f.Sessions {
				fn(p, n)
			}
			walk(p, f.Folders)
		}
	}
	walk("", t.Folders)
}

// AllFolderPaths lists every folder path depth-first (file order).
func (t Tree) AllFolderPaths() []string {
	var out []string
	var walk func(prefix string, folders []Folder)
	walk = func(prefix string, folders []Folder) {
		for _, f := range folders {
			p := f.Name
			if prefix != "" {
				p = JoinPath(prefix, f.Name)
			}
			out = append(out, p)
			walk(p, f.Folders)
		}
	}
	walk("", t.Folders)
	return out
}

// CountSessions returns sessions in this folder and all descendants.
func (f Folder) CountSessions() int {
	n := len(f.Sessions)
	for _, c := range f.Folders {
		n += c.CountSessions()
	}
	return n
}
