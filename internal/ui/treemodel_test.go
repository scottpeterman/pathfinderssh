// internal/ui/treemodel_test.go
package ui

import (
	"testing"

	"github.com/scottpeterman/pathfinderssh/internal/sessions"
)

func node(name, host string) sessions.Node {
	return sessions.Node{Name: name, Transport: sessions.TransportSSH, Host: host}.Normalize()
}

func fixture() sessions.Tree {
	t := sessions.Tree{}
	_ = t.Add("Lab", node("eng-spine-1", "172.16.2.5"))
	_ = t.Add("Lab", node("eng-leaf-1", "172.16.11.41"))
	_ = t.Add("Core", node("wan-core-1", "172.16.1.2"))
	_ = t.AddFolder("Empty")
	return t
}

func TestUnfilteredShowsEverythingIncludingEmptyFolders(t *testing.T) {
	v := BuildTreeView(fixture(), "")
	if got := len(v.Children[TreeRootUID]); got != 3 {
		t.Fatalf("got %d folders, want 3 including the empty one", got)
	}
	if v.Matched != 3 {
		t.Errorf("Matched = %d, want 3", v.Matched)
	}
	if got := len(v.Children[FolderUID("Lab")]); got != 2 {
		t.Errorf("Lab has %d children", got)
	}
	if got := len(v.Children[FolderUID("Empty")]); got != 0 {
		t.Errorf("Empty has %d children", got)
	}
}

func TestFilterKeepsOnlyMatchingSessions(t *testing.T) {
	v := BuildTreeView(fixture(), "leaf")
	if v.Matched != 1 {
		t.Fatalf("Matched = %d, want 1", v.Matched)
	}
	if got := len(v.Children[TreeRootUID]); got != 1 {
		t.Fatalf("got %d folders, want just Lab", got)
	}
	if got := len(v.Children[FolderUID("Lab")]); got != 1 {
		t.Errorf("Lab shows %d sessions", got)
	}
}

func TestFilterDropsAFolderWithNoMatchesRatherThanShowingItEmpty(t *testing.T) {
	v := BuildTreeView(fixture(), "leaf")
	if _, ok := v.Rows[FolderUID("Core")]; ok {
		t.Error("Core survived with no matches")
	}
	if _, ok := v.Rows[FolderUID("Empty")]; ok {
		t.Error("Empty survived a filter")
	}
}

func TestAFolderNameMatchShowsTheWholeFolder(t *testing.T) {
	v := BuildTreeView(fixture(), "lab")
	if got := len(v.Children[FolderUID("Lab")]); got != 2 {
		t.Fatalf("got %d sessions, want the whole folder", got)
	}
	if got := len(v.Children[TreeRootUID]); got != 1 {
		t.Errorf("got %d folders", got)
	}
}

func TestFilterMatchesHostAndMetadataNotJustName(t *testing.T) {
	tr := sessions.Tree{}
	n := node("box", "10.44.0.9")
	n.DeviceType = "arista_eos"
	n.Vendor = "Arista"
	n.Notes = "behind the blue rack"
	_ = tr.Add("Lab", n)

	for _, q := range []string{"10.44", "arista_eos", "blue rack", "BOX"} {
		if v := BuildTreeView(tr, q); v.Matched != 1 {
			t.Errorf("%q matched %d, want 1", q, v.Matched)
		}
	}
}

func TestFilterDoesNotMatchAKeyPathOrThemeName(t *testing.T) {
	// Matching a device because its key file path contains the query is a
	// result nobody can explain from what they see on screen.
	tr := sessions.Tree{}
	n := node("box", "10.0.0.1")
	n.KeyPath = "~/.ssh/core-router-key"
	n.TerminalTheme = "amber-crt"
	n.Credential = "net-admin"
	_ = tr.Add("Lab", n)

	for _, q := range []string{"core-router-key", "amber", "net-admin"} {
		if v := BuildTreeView(tr, q); v.Matched != 0 {
			t.Errorf("%q matched %d, want 0", q, v.Matched)
		}
	}
}

func TestAFilterThatMatchesNothingIsAnEmptyViewNotAWrongOne(t *testing.T) {
	v := BuildTreeView(fixture(), "zzz-no-such-device")
	if v.Matched != 0 || len(v.Children[TreeRootUID]) != 0 {
		t.Fatalf("Matched=%d folders=%d", v.Matched, len(v.Children[TreeRootUID]))
	}
}

func TestTheSameSessionNameInTwoFoldersGetsTwoRows(t *testing.T) {
	tr := sessions.Tree{}
	_ = tr.Add("Site A", node("core-1", "10.1.0.1"))
	_ = tr.Add("Site B", node("core-1", "10.2.0.1"))

	v := BuildTreeView(tr, "")
	if v.Matched != 2 {
		t.Fatalf("Matched = %d", v.Matched)
	}
	a := v.Rows[SessionUID("Site A", "core-1")]
	b := v.Rows[SessionUID("Site B", "core-1")]
	if a.Node.Host != "10.1.0.1" || b.Node.Host != "10.2.0.1" {
		t.Fatalf("rows collided: %q and %q", a.Node.Host, b.Node.Host)
	}
}

func TestADuplicateLabelInOneFolderStillGetsItsOwnRow(t *testing.T) {
	// Add refuses this, but a hand-edited file can contain it, and a repeated
	// UID makes fyne.Tree render one row and silently lose the other.
	tr := sessions.Tree{Folders: []sessions.Folder{{
		Name:     "Lab",
		Sessions: []sessions.Node{node("core-1", "10.1.0.1"), node("core-1", "10.2.0.1")},
	}}}

	v := BuildTreeView(tr, "")
	if v.Matched != 2 {
		t.Fatalf("Matched = %d, want both", v.Matched)
	}
	if got := len(v.Children[FolderUID("Lab")]); got != 2 {
		t.Fatalf("folder shows %d rows", got)
	}
	seen := map[string]bool{}
	for _, uid := range v.Children[FolderUID("Lab")] {
		if seen[uid] {
			t.Fatalf("duplicate UID %q", uid)
		}
		seen[uid] = true
	}
}

func TestRowsCarryTheirFolderAndTarget(t *testing.T) {
	v := BuildTreeView(fixture(), "")
	row := v.Rows[SessionUID("Lab", "eng-spine-1")]
	if row.Folder != "Lab" {
		t.Errorf("Folder = %q", row.Folder)
	}
	if row.IsFolder {
		t.Error("a session row says it is a folder")
	}
	if row.Detail == "" {
		t.Error("no target text")
	}
	if f := v.Rows[FolderUID("Lab")]; !f.IsFolder || f.Detail != "2 sessions" {
		t.Errorf("folder row = %+v", f)
	}
}

func TestFolderRowCountsAreSingularAtOne(t *testing.T) {
	v := BuildTreeView(fixture(), "")
	if got := v.Rows[FolderUID("Core")].Detail; got != "1 session" {
		t.Errorf("Detail = %q", got)
	}
	if got := v.Rows[FolderUID("Empty")].Detail; got != "0 sessions" {
		t.Errorf("Detail = %q", got)
	}
}

func TestFolderOrderIsFileOrder(t *testing.T) {
	v := BuildTreeView(fixture(), "")
	want := []string{FolderUID("Lab"), FolderUID("Core"), FolderUID("Empty")}
	got := v.Children[TreeRootUID]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestAFilterOpensEveryFolderItLeaves(t *testing.T) {
	// A filtered tree with its branches shut shows folder names and none of
	// the matches that were just searched for.
	v := BuildTreeView(fixture(), "core")
	open := ExpandedFor(v, "core")
	if len(open) != len(v.Children[TreeRootUID]) || len(open) == 0 {
		t.Fatalf("open = %v", open)
	}
}

func TestNoFilterForcesNothingOpen(t *testing.T) {
	v := BuildTreeView(fixture(), "")
	if open := ExpandedFor(v, ""); len(open) != 0 {
		t.Fatalf("open = %v, want the person's own expansion left alone", open)
	}
}

func TestFolderNameHelpers(t *testing.T) {
	tr := fixture()
	if got := FolderNames(tr); got[0] != "Lab" || got[2] != "Empty" {
		t.Errorf("FolderNames = %v", got)
	}
	if got := SortedFolderNames(tr); got[0] != "Core" {
		t.Errorf("SortedFolderNames = %v", got)
	}
	// SortedFolderNames must not have reordered the caller's tree.
	if tr.Folders[0].Name != "Lab" {
		t.Error("sorting mutated the tree")
	}
}

func TestTheRootIsAlwaysABranch(t *testing.T) {
	// fyne.Tree walks from the root and descends only if IsBranch says so.
	// Answer false and the entire tree renders as one invisible leaf.
	for _, tr := range []sessions.Tree{fixture(), {}} {
		v := BuildTreeView(tr, "")
		if !v.IsBranch(TreeRootUID) {
			t.Fatal("the root is not a branch; nothing would render")
		}
	}
}

func TestFoldersAreBranchesAndSessionsAreNot(t *testing.T) {
	v := BuildTreeView(fixture(), "")
	if !v.IsBranch(FolderUID("Lab")) {
		t.Error("a folder is not a branch")
	}
	if !v.IsBranch(FolderUID("Empty")) {
		t.Error("an empty folder must still be a branch, or it cannot be filled")
	}
	if v.IsBranch(SessionUID("Lab", "eng-spine-1")) {
		t.Error("a session is a branch")
	}
	if v.IsBranch("s:no/such/row") {
		t.Error("an unknown uid is a branch")
	}
}

func TestNestedBuildTreeView(t *testing.T) {
	tr := sessions.Tree{}
	_ = tr.Add("3_Customers/PBB/Juniper", node("AER01", "10.1.1.1"))
	_ = tr.Add("3_Customers/PBB/Juniper", node("AER02", "10.1.1.2"))
	_ = tr.AddFolder("3_Customers/PBB/Arista")

	v := BuildTreeView(tr, "")
	rootKids := v.Children[TreeRootUID]
	if len(rootKids) != 1 || rootKids[0] != FolderUID("3_Customers") {
		t.Fatalf("root kids %v", rootKids)
	}
	cust := v.Children[FolderUID("3_Customers")]
	if len(cust) != 1 || cust[0] != FolderUID("3_Customers/PBB") {
		t.Fatalf("customers kids %v", cust)
	}
	pbb := v.Children[FolderUID("3_Customers/PBB")]
	if len(pbb) != 2 {
		t.Fatalf("PBB kids %v", pbb)
	}
	jun := v.Children[FolderUID("3_Customers/PBB/Juniper")]
	if len(jun) != 2 {
		t.Fatalf("Juniper kids %v", jun)
	}
	if v.Matched != 2 {
		t.Errorf("Matched=%d", v.Matched)
	}
}

func TestNestedFilterOpensAncestors(t *testing.T) {
	tr := sessions.Tree{}
	_ = tr.Add("3_Customers/PBB/Juniper", node("AER01", "10.1.1.1"))
	_ = tr.Add("Other/Site", node("core", "10.2.2.2"))

	v := BuildTreeView(tr, "aer01")
	if v.Matched != 1 {
		t.Fatalf("Matched=%d", v.Matched)
	}
	if _, ok := v.Rows[FolderUID("Other")]; ok {
		t.Error("Other should be filtered out")
	}
	exp := ExpandedFor(v, "aer01")
	if len(exp) < 3 {
		t.Fatalf("expected nested folders expanded, got %v", exp)
	}
}
