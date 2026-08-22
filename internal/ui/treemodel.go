// internal/ui/treemodel.go
//
// What the session tree widget displays, as data.
package ui

import (
	"sort"
	"strconv"
	"strings"

	"github.com/scottpeterman/pathfinderssh/internal/sessions"
)

// TreeRootUID is the invisible parent fyne.Tree asks about first.
const TreeRootUID = ""

// TreeRow is one visible line.
type TreeRow struct {
	UID      string
	IsFolder bool
	Folder   string // full folder path this row is in, or is
	Label    string
	Detail   string // right-hand text: the target, for sessions
	Node     sessions.Node
}

// TreeView is a whole rendered tree: rows by UID, and each row's children.
type TreeView struct {
	Rows     map[string]TreeRow
	Children map[string][]string

	// Matched is how many sessions the filter let through.
	Matched int
	// TotalSessions is the inventory size (ignores filter).
	TotalSessions int
}

// FolderUID and SessionUID build the identifiers.
func FolderUID(folderPath string) string { return "f:" + folderPath }

// SessionUID keys on folder path AND label.
func SessionUID(folderPath, label string) string {
	return "s:" + folderPath + "/" + label
}

// BuildTreeView renders a session tree for display, applying a filter.
//
// Nested folders are preserved. Filtering keeps a folder when its own name
// matches (whole subtree shows) or when any descendant session matches (only
// matching branches survive). Empty folders stay visible only with no filter.
func BuildTreeView(t sessions.Tree, filter string) TreeView {
	q := strings.ToLower(strings.TrimSpace(filter))
	v := TreeView{
		Rows:          map[string]TreeRow{},
		Children:      map[string][]string{},
		TotalSessions: countAllSessions(t.Folders),
	}
	v.Children[TreeRootUID] = buildFolderLevel(&v, "", t.Folders, q, false)
	return v
}

func countAllSessions(folders []sessions.Folder) int {
	n := 0
	for _, f := range folders {
		n += len(f.Sessions) + countAllSessions(f.Folders)
	}
	return n
}

// buildFolderLevel adds folders at this level. Returns child UIDs.
// ancestorHit means a parent folder name already matched the filter.
func buildFolderLevel(v *TreeView, parentPath string, folders []sessions.Folder, q string, ancestorHit bool) []string {
	uids := make([]string, 0, len(folders))
	for _, f := range folders {
		path := f.Name
		if parentPath != "" {
			path = sessions.JoinPath(parentPath, f.Name)
		}
		folderHit := ancestorHit || (q != "" && strings.Contains(strings.ToLower(f.Name), q))
		showAll := q == "" || folderHit

		childFolderUIDs := buildFolderLevel(v, path, f.Folders, q, folderHit)

		sessionUIDs := make([]string, 0, len(f.Sessions))
		for _, n := range f.Sessions {
			if !showAll && !MatchesSession(n, q) {
				continue
			}
			label := n.Label()
			uid := SessionUID(path, label)
			for _, taken := v.Rows[uid]; taken; _, taken = v.Rows[uid] {
				label += " "
				uid = SessionUID(path, label)
			}
			v.Rows[uid] = TreeRow{
				UID:    uid,
				Folder: path,
				Label:  label,
				Detail: n.Target(),
				Node:   n,
			}
			sessionUIDs = append(sessionUIDs, uid)
			v.Matched++
		}

		kids := append(childFolderUIDs, sessionUIDs...)
		if q != "" && !folderHit && len(kids) == 0 {
			continue
		}

		fuid := FolderUID(path)
		// CountSessions walks the model once per folder; cheap vs GetDisplay
		// on the terminal, and labels need the full subtree size.
		total := f.CountSessions()
		label := f.Name
		if total > 0 {
			label = f.Name + " (" + strconv.Itoa(total) + ")"
		}
		visible := len(sessionUIDs)
		for _, cuid := range childFolderUIDs {
			visible += visibleSessionsUnder(v, cuid)
		}
		v.Rows[fuid] = TreeRow{
			UID:      fuid,
			IsFolder: true,
			Folder:   path,
			Label:    label,
			Detail:   folderDetail(visible),
		}
		v.Children[fuid] = kids
		uids = append(uids, fuid)
	}
	return uids
}

func visibleSessionsUnder(v *TreeView, folderUID string) int {
	n := 0
	for _, c := range v.Children[folderUID] {
		if v.Rows[c].IsFolder {
			n += visibleSessionsUnder(v, c)
		} else {
			n++
		}
	}
	return n
}

func folderDetail(n int) string {
	if n == 1 {
		return "1 session"
	}
	return strconv.Itoa(n) + " sessions"
}

// IsBranch answers fyne.Tree's second question. THE ROOT IS ALWAYS A BRANCH.
func (v TreeView) IsBranch(uid string) bool {
	if uid == TreeRootUID {
		return true
	}
	return v.Rows[uid].IsFolder
}

// MatchesSession reports whether a node matches a lowercased query.
func MatchesSession(n sessions.Node, q string) bool {
	if q == "" {
		return true
	}
	for _, field := range []string{
		n.Name,
		n.Host,
		n.SerialPort,
		n.DeviceType,
		n.Vendor,
		n.Model,
		n.Username,
		n.Notes,
	} {
		if field != "" && strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

// FolderNames lists every folder path depth-first (for pickers).
func FolderNames(t sessions.Tree) []string {
	return t.AllFolderPaths()
}

// SortedFolderNames is the same list alphabetically.
func SortedFolderNames(t sessions.Tree) []string {
	out := FolderNames(t)
	sort.Strings(out)
	return out
}

// ExpandedFor is which folders the widget should open.
// With a filter, every surviving folder opens so matches are visible.
// Prefer OpenAllBranches() on the widget (one Refresh) over looping OpenBranch.
func ExpandedFor(v TreeView, filter string) []string {
	if strings.TrimSpace(filter) == "" {
		return nil
	}
	out := make([]string, 0)
	var walk func(uid string)
	walk = func(uid string) {
		for _, c := range v.Children[uid] {
			if v.Rows[c].IsFolder {
				out = append(out, c)
				walk(c)
			}
		}
	}
	walk(TreeRootUID)
	return out
}
