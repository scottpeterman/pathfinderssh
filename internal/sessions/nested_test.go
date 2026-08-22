package sessions

import (
	"testing"
)

func TestEnsurePathNestsFolders(t *testing.T) {
	var tr Tree
	f, err := tr.EnsurePath("3_Customers/PBB/Hagerstown MD/Juniper")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "Juniper" {
		t.Fatalf("leaf %q", f.Name)
	}
	if len(tr.Folders) != 1 || tr.Folders[0].Name != "3_Customers" {
		t.Fatalf("root %#v", tr.Folders)
	}
	pbb := tr.Folders[0].Folders[0]
	if pbb.Name != "PBB" || len(pbb.Folders) != 1 {
		t.Fatalf("PBB %#v", pbb)
	}
}

func TestAddIntoNestedPath(t *testing.T) {
	var tr Tree
	n := Node{Name: "AER01", Transport: TransportSSH, Host: "10.1.1.1"}.Normalize()
	if err := tr.Add("3_Customers/PBB/Juniper", n); err != nil {
		t.Fatal(err)
	}
	f, err := tr.FolderAt("3_Customers/PBB/Juniper")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Sessions) != 1 || f.Sessions[0].Name != "AER01" {
		t.Fatalf("%#v", f.Sessions)
	}
	if len(tr.Nodes()) != 1 {
		t.Fatalf("nodes %d", len(tr.Nodes()))
	}
}

func TestExpandEncodedPaths(t *testing.T) {
	n := Node{Name: "AER01", Transport: TransportSSH, Host: "1.1.1.1"}.Normalize()
	tr := Tree{Folders: []Folder{{
		Name:     "3_Customers / PBB / Juniper",
		Sessions: []Node{n},
	}}}
	tr.ExpandEncodedPaths()
	if len(tr.Folders) != 1 || tr.Folders[0].Name != "3_Customers" {
		t.Fatalf("got %#v", tr.Folders)
	}
	leaf, err := tr.FolderAt("3_Customers/PBB/Juniper")
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.Sessions) != 1 {
		t.Fatalf("sessions %#v", leaf.Sessions)
	}
}

func TestRoundTripNestedYAML(t *testing.T) {
	var tr Tree
	_ = tr.Add("Lab/Spines", Node{Name: "s1", Transport: TransportSSH, Host: "10.0.0.1"}.Normalize())
	data, err := MarshalTree(tr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalTree(data)
	if err != nil {
		t.Fatal(err)
	}
	f, err := got.FolderAt("Lab/Spines")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Sessions) != 1 {
		t.Fatalf("%#v", f)
	}
}
