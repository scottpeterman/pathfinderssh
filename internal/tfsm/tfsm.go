// internal/tfsm/tfsm.go
// Embedded, pre-vetted TextFSM templates and exact selection: the platform
// string from netexec's fingerprint plus a command key maps to exactly one
// template — no scoring engine. The vendored folder is frozen and validated
// against captured device output (variant choice and EOF-record behavior
// included), so a lookup miss is a configuration error, not a fallback case.
package tfsm

import (
	"embed"
	"fmt"
	"sync"

	"github.com/sirikothe/gotextfsm"
)

//go:embed templates/*.textfsm
var templateFS embed.FS

// selection: platform (fingerprint string) -> command key -> template file.
// Command keys are logical ("lldp_detail", "lldp", "cdp", "cdp_detail"),
// decoupled from the exact CLI string, which lives in the crawler's plan.
var selection = map[string]map[string]string{
	"arista_eos": {
		// native EOS format — the _detail2 variant, validated at scale;
		// the _detail variant matches an older output style and is kept
		// in the folder only for reference.
		"lldp_detail": "arista_eos_show_lldp_neighbors_detail2.textfsm",
		"lldp":        "arista_eos_show_lldp_neighbors.textfsm",
	},
	"cisco_ios": {
		// on some IOS builds lldp_detail omits Local Intf entirely —
		// edges must come from the plain table, detail is enrichment.
		"lldp_detail": "cisco_ios_show_lldp_neighbors_detail.textfsm",
		"lldp":        "cisco_ios_show_lldp_neighbors.textfsm",
		"cdp_detail":  "cisco_ios_show_cdp_neighbors_detail.textfsm",
		"cdp":         "cisco_ios_show_cdp_neighbors.textfsm",
	},
	"cisco_iosxe": { // same output family as classic IOS
		"lldp_detail": "cisco_ios_show_lldp_neighbors_detail.textfsm",
		"lldp":        "cisco_ios_show_lldp_neighbors.textfsm",
		"cdp_detail":  "cisco_ios_show_cdp_neighbors_detail.textfsm",
		"cdp":         "cisco_ios_show_cdp_neighbors.textfsm",
	},
	"cisco_nxos": {
		"lldp_detail": "cisco_nxos_show_lldp_neighbors_detail.textfsm",
		"lldp":        "cisco_nxos_show_lldp_neighbors.textfsm",
		"cdp_detail":  "cisco_nxos_show_cdp_neighbors_detail.textfsm",
		"cdp":         "cisco_nxos_show_cdp_neighbors.textfsm",
	},
	"juniper_junos": {
		// per-interface detail format (the collector loops interfaces;
		// concatenated blocks parse in one pass)
		"lldp_detail": "juniper_junos_show_lldp_neighbors_detail.textfsm",
		"lldp":        "juniper_junos_show_lldp_neighbors.textfsm",
	},
	// The following three are built from real device captures but, unlike
	// every other entry above, have not been run through go test in this
	// repo (no Go toolchain in the authoring environment). See the comment
	// at the top of each .textfsm file for what specifically is unverified.
	"aruba_procurve": {
		"lldp_detail": "aruba_procurve_show_lldp_info_remote_device_detail.textfsm",
	},
	"aruba_cx": {
		"lldp_detail": "aruba_cx_show_lldp_neighbor_info_detail.textfsm",
	},
	"extreme_exos": {
		"lldp_detail": "extreme_exos_show_lldp_neighbors_detailed.textfsm",
	},
	"hp_comware": {
		"lldp_detail": "hp_comware_display_lldp_neighbor_information.textfsm",
	},
}

var (
	mu    sync.Mutex
	cache = map[string]string{} // file -> template SOURCE, never a parsed TextFSM
)

// load returns the template source, reading it from the embedded FS once.
//
// It deliberately caches the TEXT and not a parsed gotextfsm.TextFSM. That
// looks wasteful and is not: gotextfsm keeps per-parse working state inside
// the template's own Values map — see clearRecord and checkLine, which both
// write fsm.Values[name] mid-parse — so a TextFSM is the parser's scratch
// space, not a reusable compiled template. Sharing one across goroutines is
// a data race whatever you do with the pointer, because copying the struct
// copies the map header and leaves the storage shared.
//
// This cost us a production-shaped crash: two devices at the same crawl
// depth parsing the same template at once produced "concurrent map iteration
// and map write" inside appendRecord. The bug was always there; it needed
// two devices to reach the parse step simultaneously to show up.
func load(file string) (string, error) {
	mu.Lock()
	if src, ok := cache[file]; ok {
		mu.Unlock()
		return src, nil
	}
	mu.Unlock()

	b, err := templateFS.ReadFile("templates/" + file)
	if err != nil {
		return "", err
	}
	src := string(b)

	mu.Lock()
	cache[file] = src
	mu.Unlock()
	return src, nil
}

// Parse runs the exact template for (platform, commandKey) over
// command-scoped output (echo and trailing prompt already stripped by
// netexec) and returns one map per record.
func Parse(platform, commandKey, output string) ([]map[string]string, error) {
	cmds, ok := selection[platform]
	if !ok {
		return nil, fmt.Errorf("no templates for platform %q", platform)
	}
	file, ok := cmds[commandKey]
	if !ok {
		return nil, fmt.Errorf("platform %q has no template for %q", platform, commandKey)
	}
	src, err := load(file)
	if err != nil {
		return nil, err
	}
	// A fresh TextFSM per call. This is the concurrency boundary: every
	// caller gets its own Values map, so nothing is shared between the
	// goroutines the crawler runs per depth batch. Templates are small and
	// the alternative — one lock around every parse — would serialize the
	// only CPU work in a pipeline that is otherwise waiting on SSH.
	var fsm gotextfsm.TextFSM
	if err := fsm.ParseString(src); err != nil {
		return nil, fmt.Errorf("template %s: %w", file, err)
	}
	p := gotextfsm.ParserOutput{}
	if err := p.ParseTextString(output, fsm, true); err != nil {
		return nil, fmt.Errorf("parse with %s: %w", file, err)
	}
	recs := make([]map[string]string, 0, len(p.Dict))
	for _, row := range p.Dict {
		m := make(map[string]string, len(row))
		for k, v := range row {
			if s, ok := v.(string); ok {
				m[k] = s
			} else {
				m[k] = fmt.Sprint(v)
			}
		}
		recs = append(recs, m)
	}
	return recs, nil
}

// Supported reports whether a platform has any neighbor templates.
func Supported(platform string) bool { _, ok := selection[platform]; return ok }
