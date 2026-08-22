// internal/sessions/tree.go
//
// The saved inventory: folders of nodes, as a file.
//
// This is the user's own list, hand-organized and hand-edited, and discovery
// imports INTO it rather than replacing it. That direction is the whole design.
// A crawl is a snapshot of what answered today; the tree is what someone
// decided was worth keeping, with the names and groupings they chose. An import
// that overwrote either would be throwing away the more expensive of the two.
//
// # Nested folders
//
// Folders hold sessions and child folders. Paths are slash-separated
// ("Customers/PBB/Hagerstown MD") so SecureCRT-style trees import without
// flattening. Version 1 files (flat only) still load; ExpandEncodedPaths
// upgrades legacy "A / B / C" names from older CRT imports.
//
// # Two file shapes, one loader
//
// The native file is a mapping with a version, so a later top-level key is an
// addition rather than a format break:
//
//	version: 2
//	folders:
//	  - folder_name: Lab
//	    sessions:
//	      - name: eng-spine-1
//	        transport: ssh
//	        host: 172.16.2.5
//	    folders:
//	      - folder_name: Spines
//	        sessions: []
//
// Load also accepts a bare top-level LIST of folders, which is how the older
// terminal wrote its file. Structure alone is not enough to open one of those
// usefully — its session keys are named differently and it has no transport
// field — so ImportTether is where that conversion lives. Accepting the shape
// here just means a file that is half-converted still opens instead of failing
// at the parser.
//
// # Nothing here is a secret
//
// A tree is Nodes, and Node keeps its secret fields out of YAML. The one thing
// this file adds is that an import must not smuggle them back in: a foreign
// session file carrying a plaintext password contributes everything EXCEPT that
// password, so importing someone's export cannot quietly re-introduce it.
package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/scottpeterman/pathfinderssh/internal/topo"
)

// FileVersion is written into every saved tree. Version 2 adds nested folders;
// v1 files still load (empty child lists).
const FileVersion = 2

// Folder is one group of sessions and optional child folders. YAML keys match
// the older terminal's file so existing trees stay recognizable.
type Folder struct {
	Name     string   `yaml:"folder_name"`
	Sessions []Node   `yaml:"sessions"`
	Folders  []Folder `yaml:"folders,omitempty"`
}

// Tree is a whole session file.
type Tree struct {
	Version int      `yaml:"version,omitempty"`
	Folders []Folder `yaml:"folders"`
}

// ─────────────────────────────────────────────────────────────────────
// Reading and writing
// ─────────────────────────────────────────────────────────────────────

// UnmarshalTree parses a session file. It accepts the native mapping and a bare
// list of folders, and normalizes every node it finds — so a hand-written file
// that omits a port opens with the right one rather than with zero.
//
// An empty file is an empty tree, not an error: that is what a first run looks
// like, and refusing it would mean the application could not start until
// somebody typed something.
func UnmarshalTree(data []byte) (Tree, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Tree{Version: FileVersion}, nil
	}

	var t Tree
	mapErr := yaml.Unmarshal(data, &t)
	if mapErr == nil && (len(t.Folders) > 0 || t.Version != 0) {
		out := t.normalized()
		out.ExpandEncodedPaths()
		return out, nil
	}

	// Bare list of folders — the older terminal's shape.
	var folders []Folder
	if err := yaml.Unmarshal(data, &folders); err == nil {
		out := Tree{Version: FileVersion, Folders: folders}.normalized()
		out.ExpandEncodedPaths()
		return out, nil
	}

	if mapErr != nil {
		return Tree{}, fmt.Errorf("parse session file: %w", mapErr)
	}
	out := t.normalized()
	out.ExpandEncodedPaths()
	return out, nil
}

// MarshalTree renders a tree as it would be written to disk.
func MarshalTree(t Tree) ([]byte, error) {
	t.Version = FileVersion
	out := t.normalized()
	trimFoldersForFile(out.Folders)
	return yaml.Marshal(out)
}

func trimFoldersForFile(folders []Folder) {
	for i := range folders {
		for j := range folders[i].Sessions {
			folders[i].Sessions[j] = trimForFile(folders[i].Sessions[j])
		}
		trimFoldersForFile(folders[i].Folders)
	}
}

// trimForFile drops the fields the node's transport does not use.
//
// Normalize deliberately fills every group regardless of transport, because a
// form that hides the serial group must not show 0 baud when it is unhidden.
// A FILE has the opposite requirement: an SSH session carrying baud, data bits,
// parity and stop bits is four lines of noise per device that mean nothing, and
// this file is meant to be opened in an editor. The rule is the one Validate
// already uses — only the fields that transport reads.
//
// Nothing is lost that was doing anything. Reloading runs Normalize, which puts
// the defaults straight back.
func trimForFile(n Node) Node {
	switch n.Transport {
	case TransportSerial:
		n.Host, n.Port = "", 0
		n.Username, n.Credential, n.AuthType, n.KeyPath = "", "", "", ""
		n.Jump = JumpSpec{}
		n.LegacyAlgorithms = false
		n.HostKeyPolicy, n.KnownHostsPath = "", ""
		n.ConnectTimeoutSec = 0
		n.TelnetCRLF = nil
		n.TermType = ""
	case TransportTelnet:
		n.Username, n.Credential, n.AuthType, n.KeyPath = "", "", "", ""
		n.Jump = JumpSpec{}
		n.LegacyAlgorithms = false
		n.HostKeyPolicy, n.KnownHostsPath = "", ""
		n.SerialPort, n.Baud, n.DataBits = "", 0, 0
		n.Parity, n.StopBits = "", ""
	default: // ssh
		n.SerialPort, n.Baud, n.DataBits = "", 0, 0
		n.Parity, n.StopBits = "", ""
		n.TelnetCRLF = nil
	}
	return n
}

// LoadFile reads a session file. A file that is not there is an empty tree
// rather than an error — the first run of a new install has no inventory yet,
// and that is not a condition worth a dialog.
func LoadFile(path string) (Tree, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Tree{Version: FileVersion}, nil
	}
	if err != nil {
		return Tree{}, err
	}
	return UnmarshalTree(data)
}

// SaveFile writes a session file through a temporary file in the same
// directory, then renames it over the target.
//
// The rename is the point. This file is the user's whole inventory, it is
// rewritten on every edit, and a process that dies partway through a direct
// write leaves a truncated one — losing months of organizing to a crash during
// a folder rename. A rename within a directory is atomic on every platform this
// ships to, so the file on disk is either the old tree or the new one.
func SaveFile(path string, t Tree) error {
	data, err := MarshalTree(t)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sessions-*.yaml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 0600: a session file names hosts, usernames and key paths. None of it
	// is a secret, and all of it is a map of what to attack.
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (t Tree) normalized() Tree {
	out := Tree{Version: FileVersion, Folders: normalizeFolders(t.Folders)}
	return out
}

func normalizeFolders(in []Folder) []Folder {
	out := make([]Folder, 0, len(in))
	for _, f := range in {
		nf := Folder{
			Name:     strings.TrimSpace(f.Name),
			Sessions: make([]Node, 0, len(f.Sessions)),
			Folders:  normalizeFolders(f.Folders),
		}
		for _, n := range f.Sessions {
			nf.Sessions = append(nf.Sessions, n.Normalize())
		}
		out = append(out, nf)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────
// Identity
// ─────────────────────────────────────────────────────────────────────

// Key is what makes two nodes the same device for the purpose of an import.
//
// It is the address, not the name: a device renamed in the tree is still that
// device, and re-importing a crawl must not add it a second time under the name
// the crawler happened to report. Serial nodes key on the port, which is the
// only address they have.
//
// An empty key means "no address" — such a node never matches anything,
// including another node with no address, because there is nothing to compare.
func (n Node) Key() string {
	switch n.Transport {
	case TransportSerial:
		p := strings.TrimSpace(n.SerialPort)
		if p == "" {
			return ""
		}
		return "serial:" + p
	default:
		h := strings.ToLower(strings.TrimSpace(n.Host))
		if h == "" {
			return ""
		}
		nn := n.Normalize()
		return string(nn.Transport) + ":" + h + ":" + strconv.Itoa(nn.Port)
	}
}

// Keys returns every address already in the tree.
func (t Tree) Keys() map[string]bool {
	seen := map[string]bool{}
	var walk func([]Folder)
	walk = func(folders []Folder) {
		for _, f := range folders {
			for _, n := range f.Sessions {
				if k := n.Key(); k != "" {
					seen[k] = true
				}
			}
			walk(f.Folders)
		}
	}
	walk(t.Folders)
	return seen
}

// Nodes flattens the tree. Order is depth-first folder order, then session
// order — what the file says, not sorted, because the order is something the
// user arranged.
func (t Tree) Nodes() []Node {
	out := make([]Node, 0)
	var walk func([]Folder)
	walk = func(folders []Folder) {
		for _, f := range folders {
			out = append(out, f.Sessions...)
			walk(f.Folders)
		}
	}
	walk(t.Folders)
	return out
}

// ─────────────────────────────────────────────────────────────────────
// Editing
// ─────────────────────────────────────────────────────────────────────

// FolderIndex returns the position of a folder, or -1. Names are compared
// case-insensitively: two folders called "Lab" and "lab" in one tree is a
// mistake nobody makes on purpose.
func (t Tree) FolderIndex(name string) int {
	want := strings.ToLower(strings.TrimSpace(name))
	for i, f := range t.Folders {
		if strings.ToLower(strings.TrimSpace(f.Name)) == want {
			return i
		}
	}
	return -1
}

// AddFolder appends an empty folder. name may be a nested path ("A/B"); every
// missing segment is created. The leaf must not already exist under its parent.
func (t *Tree) AddFolder(name string) error {
	parts := SplitPath(name)
	if len(parts) == 0 {
		return fmt.Errorf("folder name is required")
	}
	leaf := parts[len(parts)-1]
	if strings.Contains(leaf, PathSep) {
		return fmt.Errorf("folder name %q must not contain %q", leaf, PathSep)
	}
	if len(parts) == 1 {
		if t.FolderIndex(leaf) >= 0 {
			return fmt.Errorf("a folder called %q already exists", leaf)
		}
		t.Folders = append(t.Folders, Folder{Name: leaf})
		return nil
	}
	parentPath := JoinPath(parts[:len(parts)-1]...)
	parent, err := t.EnsurePath(parentPath)
	if err != nil {
		return err
	}
	want := strings.ToLower(leaf)
	for _, c := range parent.Folders {
		if strings.ToLower(strings.TrimSpace(c.Name)) == want {
			return fmt.Errorf("a folder called %q already exists under %q", leaf, parentPath)
		}
	}
	parent.Folders = append(parent.Folders, Folder{Name: leaf})
	return nil
}

// RenameFolder changes a folder's leaf name (path or top-level name).
func (t *Tree) RenameFolder(oldPath, newName string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("folder name is required")
	}
	if strings.Contains(newName, PathSep) || strings.Contains(newName, " / ") {
		return fmt.Errorf("folder name must be a single segment, not a path")
	}
	parts := SplitPath(oldPath)
	if len(parts) == 0 {
		return fmt.Errorf("no folder called %q", oldPath)
	}
	if len(parts) == 1 {
		i := t.FolderIndex(parts[0])
		if i < 0 {
			return fmt.Errorf("no folder called %q", oldPath)
		}
		if j := t.FolderIndex(newName); j >= 0 && j != i {
			return fmt.Errorf("a folder called %q already exists", newName)
		}
		t.Folders[i].Name = newName
		return nil
	}
	parentPath := JoinPath(parts[:len(parts)-1]...)
	parent, err := t.FolderAt(parentPath)
	if err != nil {
		return fmt.Errorf("no folder called %q", oldPath)
	}
	wantOld := strings.ToLower(parts[len(parts)-1])
	idx := -1
	for i, c := range parent.Folders {
		if strings.ToLower(strings.TrimSpace(c.Name)) == wantOld {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no folder called %q", oldPath)
	}
	wantNew := strings.ToLower(newName)
	for i, c := range parent.Folders {
		if i != idx && strings.ToLower(strings.TrimSpace(c.Name)) == wantNew {
			return fmt.Errorf("a folder called %q already exists under %q", newName, parentPath)
		}
	}
	parent.Folders[idx].Name = newName
	return nil
}

// RemoveFolder deletes a folder path and everything in it. A folder with
// sessions or child folders is refused unless force is set.
func (t *Tree) RemoveFolder(path string, force bool) error {
	parts := SplitPath(path)
	if len(parts) == 0 {
		return fmt.Errorf("no folder called %q", path)
	}
	if len(parts) == 1 {
		i := t.FolderIndex(parts[0])
		if i < 0 {
			return fmt.Errorf("no folder called %q", path)
		}
		f := t.Folders[i]
		n := f.CountSessions()
		if (n > 0 || len(f.Folders) > 0) && !force {
			return fmt.Errorf("folder %q still has %d session(s)", f.Name, n)
		}
		t.Folders = append(t.Folders[:i], t.Folders[i+1:]...)
		return nil
	}
	parentPath := JoinPath(parts[:len(parts)-1]...)
	parent, err := t.FolderAt(parentPath)
	if err != nil {
		return fmt.Errorf("no folder called %q", path)
	}
	want := strings.ToLower(parts[len(parts)-1])
	idx := -1
	for i, c := range parent.Folders {
		if strings.ToLower(strings.TrimSpace(c.Name)) == want {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no folder called %q", path)
	}
	f := parent.Folders[idx]
	n := f.CountSessions()
	if (n > 0 || len(f.Folders) > 0) && !force {
		return fmt.Errorf("folder %q still has %d session(s)", f.Name, n)
	}
	parent.Folders = append(parent.Folders[:idx], parent.Folders[idx+1:]...)
	return nil
}

// SessionIndex returns the position of a session within a folder, or -1.
func (f Folder) SessionIndex(name string) int {
	want := strings.ToLower(strings.TrimSpace(name))
	for i, n := range f.Sessions {
		if strings.ToLower(strings.TrimSpace(n.Label())) == want {
			return i
		}
	}
	return -1
}

// Add puts a node in a folder path, creating every missing folder along the way.
func (t *Tree) Add(folder string, n Node) error {
	n = n.Normalize()
	if n.Label() == "" {
		return fmt.Errorf("session needs a name or a host")
	}
	f, err := t.EnsurePath(folder)
	if err != nil {
		return err
	}
	if f.SessionIndex(n.Label()) >= 0 {
		return fmt.Errorf("%q already has a session called %q", folder, n.Label())
	}
	f.Sessions = append(f.Sessions, n)
	return nil
}

// Replace overwrites a session in place, keeping its position in the folder.
// Position is not decoration: an edit that moved the row to the bottom would
// lose an ordering somebody chose.
func (t *Tree) Replace(folder, name string, n Node) error {
	f, err := t.FolderAt(folder)
	if err != nil {
		return err
	}
	j := f.SessionIndex(name)
	if j < 0 {
		return fmt.Errorf("no session called %q in %q", name, folder)
	}
	n = n.Normalize()
	if n.Label() == "" {
		return fmt.Errorf("session needs a name or a host")
	}
	if k := f.SessionIndex(n.Label()); k >= 0 && k != j {
		return fmt.Errorf("%q already has a session called %q", folder, n.Label())
	}
	f.Sessions[j] = n
	return nil
}

// Remove deletes one session.
func (t *Tree) Remove(folder, name string) error {
	f, err := t.FolderAt(folder)
	if err != nil {
		return err
	}
	j := f.SessionIndex(name)
	if j < 0 {
		return fmt.Errorf("no session called %q in %q", name, folder)
	}
	f.Sessions = append(f.Sessions[:j], f.Sessions[j+1:]...)
	return nil
}

// Move relocates a session between folder paths, keeping it identical.
func (t *Tree) Move(fromFolder, name, toFolder string) error {
	from, err := t.FolderAt(fromFolder)
	if err != nil {
		return err
	}
	j := from.SessionIndex(name)
	if j < 0 {
		return fmt.Errorf("no session called %q in %q", name, fromFolder)
	}
	if JoinPath(SplitPath(fromFolder)...) == JoinPath(SplitPath(toFolder)...) {
		return nil
	}
	node := from.Sessions[j]
	// Add first: if the destination refuses the name, the session is still
	// where it was rather than gone.
	if err := t.Add(toFolder, node); err != nil {
		return err
	}
	return t.Remove(fromFolder, name)
}

// ─────────────────────────────────────────────────────────────────────
// Import
// ─────────────────────────────────────────────────────────────────────

// ImportResult is what an import did, in the terms the person who ran it
// thinks in. Skipped is not a failure — it is the answer to "how much of this
// did I already have", which is the question after a re-crawl.
type ImportResult struct {
	Folder   string
	Added    int
	Skipped  []string // already in the tree, by address
	Renamed  []string // added, but under a different name to avoid a clash
	Rejected []string // no address to connect to
}

// Import merges nodes into one folder, skipping anything whose address is
// already anywhere in the tree.
//
// Skipping is deliberate and it is the whole reason an import is safe to re-run.
// A second crawl of the same estate contributes only what is new, and every
// name, folder and setting already edited by hand is left alone — the import
// never has an opinion about a device it has seen before.
//
// A name that collides inside the destination folder is disambiguated rather
// than dropped: the device is new, so losing it to a naming coincidence would
// be the wrong trade.
func (t *Tree) Import(folder string, nodes []Node) ImportResult {
	res := ImportResult{Folder: folder}
	existing := t.Keys()

	for _, n := range nodes {
		n = n.Normalize()
		key := n.Key()
		if key == "" {
			res.Rejected = append(res.Rejected, n.Label())
			continue
		}
		if existing[key] {
			res.Skipped = append(res.Skipped, n.Label())
			continue
		}

		want := n.Label()
		if f, err := t.FolderAt(folder); err == nil && f.SessionIndex(want) >= 0 {
			n.Name = want + " (" + n.Host + ")"
			res.Renamed = append(res.Renamed, want)
		}
		if err := t.Add(folder, n); err != nil {
			res.Rejected = append(res.Rejected, n.Label())
			continue
		}
		existing[key] = true
		res.Added++
	}
	return res
}

// NodesFromMap turns a crawl's map.json into importable sessions.
//
// Every crawled device becomes an SSH node addressed by IP where the crawl
// found one, because that is what the crawler itself dialled — a name that
// resolved for the neighbour reporting it does not necessarily resolve here,
// and a session that cannot connect is worse than one with an ugly host field.
// The reported name stays as the display name.
//
// Leaves — devices a neighbour named but nothing ever dialled — are excluded by
// default. They are the bulk of a real map (hundreds of servers behind a filter)
// and nothing is known about them beyond a name and sometimes an address, so
// importing them turns a session tree into a list of things that mostly are not
// sessions. Ask for them explicitly when the leaf IS the target.
func NodesFromMap(data []byte, includeLeaves bool) ([]Node, error) {
	// encoding/json, not yaml. map.json's field names live in `json:` tags on
	// topo's structs, and yaml.v3 does not read those — it lowercases the Go
	// field name instead, so NodeDetails would be looked up as "nodedetails"
	// and every device would silently arrive with no IP and no platform.
	var m map[string]topo.MapNode
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse map: %w", err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("map contains no devices")
	}

	seen := map[string]bool{}
	out := make([]Node, 0, len(m))

	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	// Map iteration order is random; an import that produced a different
	// folder order every run would look like it had done something.
	sort.Strings(names)

	for _, name := range names {
		node := m[name]
		seen[name] = true
		out = append(out, mapNode(name, node.NodeDetails.IP, node.NodeDetails.Platform))
	}

	if !includeLeaves {
		return out, nil
	}

	leaves := map[string]topo.PeerEntry{}
	for _, node := range m {
		for peer, entry := range node.Peers {
			if seen[peer] {
				continue
			}
			if _, ok := leaves[peer]; !ok {
				leaves[peer] = entry
			}
		}
	}
	leafNames := make([]string, 0, len(leaves))
	for name := range leaves {
		leafNames = append(leafNames, name)
	}
	sort.Strings(leafNames)
	for _, name := range leafNames {
		out = append(out, mapNode(name, leaves[name].IP, leaves[name].Platform))
	}
	return out, nil
}

func mapNode(name, ip, platform string) Node {
	host := strings.TrimSpace(ip)
	if host == "" {
		host = name
	}
	return Node{
		Name:      name,
		Transport: TransportSSH,
		Host:      host,
		// The platform string is a label, never a code path. It is here so a
		// person reading the tree knows what they are about to connect to;
		// anything the automation acts on comes from a live fingerprint.
		DeviceType: strings.TrimSpace(platform),
	}.Normalize()
}

// ─────────────────────────────────────────────────────────────────────
// The older terminal's file
// ─────────────────────────────────────────────────────────────────────

// tetherSession is one entry in the older terminal's session file. Its keys
// are its own — display_name rather than name, a port written as a string, and
// three metadata keys that are capitalized while everything around them is not.
// They are reproduced exactly rather than tidied, because the point of this
// struct is to read files that already exist.
type tetherSession struct {
	DisplayName   string `yaml:"display_name"`
	Host          string `yaml:"host"`
	Port          string `yaml:"port"`
	Username      string `yaml:"username"`
	AuthType      string `yaml:"auth_type"`
	KeyPath       string `yaml:"key_path"`
	CredsID       string `yaml:"credsid"`
	TerminalTheme string `yaml:"terminal_theme"`
	DeviceType    string `yaml:"DeviceType"`
	Vendor        string `yaml:"Vendor"`
	Model         string `yaml:"Model"`
}

type tetherFolder struct {
	Name     string          `yaml:"folder_name"`
	Sessions []tetherSession `yaml:"sessions"`
}

// ImportTether converts the older terminal's session file into folders.
//
// The conversion is lossy in one direction only, and the loss is on purpose:
//
//   - a password present in the source is DROPPED. Importing a colleague's
//     exported file must not put their password into this process, and it could
//     not be saved anyway
//   - port is a string there and an int here; anything unparseable becomes zero,
//     which Normalize turns into the transport's default rather than an error.
//     A file is not worth refusing over a typo in one port
//   - every session becomes SSH, because that file has no transport field and
//     the tool that wrote it only ever saved SSH
//
// Everything else maps across: credsid is a credential name, which is what this
// model stores too, and terminal_theme means the same thing in both.
func ImportTether(data []byte) ([]Folder, error) {
	var raw []tetherFolder
	if err := yaml.Unmarshal(data, &raw); err != nil {
		// The native shape is a mapping, so this is the likely mistake.
		return nil, fmt.Errorf("parse session file (expected a list of folders): %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("no folders in that file")
	}

	// yaml.v3 matches tags case-sensitively, so a hand-edited file that wrote
	// devicetype instead of DeviceType would silently lose it. Read the same
	// documents a second time as plain maps and fill in only what the struct
	// left empty.
	var loose []map[string]any
	_ = yaml.Unmarshal(data, &loose)

	out := make([]Folder, 0, len(raw))
	for fi, rf := range raw {
		f := Folder{Name: strings.TrimSpace(rf.Name), Sessions: make([]Node, 0, len(rf.Sessions))}
		if f.Name == "" {
			f.Name = "Imported"
		}
		for si, s := range rf.Sessions {
			n := Node{
				Name:          strings.TrimSpace(s.DisplayName),
				Transport:     TransportSSH,
				Host:          strings.TrimSpace(s.Host),
				Port:          atoiOrZero(s.Port),
				Username:      strings.TrimSpace(s.Username),
				Credential:    strings.TrimSpace(s.CredsID),
				AuthType:      strings.TrimSpace(s.AuthType),
				KeyPath:       strings.TrimSpace(s.KeyPath),
				TerminalTheme: strings.TrimSpace(s.TerminalTheme),
				DeviceType:    strings.TrimSpace(s.DeviceType),
				Vendor:        strings.TrimSpace(s.Vendor),
				Model:         strings.TrimSpace(s.Model),
			}
			fillLooseMetadata(&n, loose, fi, si)
			f.Sessions = append(f.Sessions, n.Normalize())
		}
		out = append(out, f)
	}
	return out, nil
}

// fillLooseMetadata recovers the three case-sensitive metadata keys when the
// file wrote them in a different case. It only ever fills a field the typed
// decode left empty, so it cannot override what the file actually said.
func fillLooseMetadata(n *Node, loose []map[string]any, fi, si int) {
	if fi >= len(loose) {
		return
	}
	list, ok := loose[fi]["sessions"].([]any)
	if !ok || si >= len(list) {
		return
	}
	entry, ok := list[si].(map[string]any)
	if !ok {
		return
	}
	get := func(key string) string {
		for k, v := range entry {
			if !strings.EqualFold(k, key) {
				continue
			}
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	if n.DeviceType == "" {
		n.DeviceType = get("devicetype")
	}
	if n.Vendor == "" {
		n.Vendor = get("vendor")
	}
	if n.Model == "" {
		n.Model = get("model")
	}
}

func atoiOrZero(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v < 0 {
		return 0
	}
	return v
}
