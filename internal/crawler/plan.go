// internal/crawler/plan.go
// Per-platform neighbor-collection plans: which CLI commands to run and
// which template key parses each. Encodes the validated field findings:
//   - EOS: lldp detail alone carries everything.
//   - IOS/IOS-XE: some builds omit Local Intf from lldp detail, so edges
//     come from the plain lldp table (+ cdp detail, which does carry the
//     local interface and a mgmt IP for crawl targets).
//   - NX-OS: cdp detail + lldp detail.
//   - Junos: newer builds take `show lldp neighbors detail`; older ones
//     reject it and need a per-interface loop — the plan starts with the
//     terse table (always valid) and treats detail as best-effort.
package crawler

import (
	"strings"

	"github.com/scottpeterman/pathfinderssh/internal/normalize"
	"github.com/scottpeterman/pathfinderssh/internal/topo"
)

// step is one command in a platform's plan.
type step struct {
	Command    string
	Key        string // tfsm command key
	Protocol   string // "cdp" | "lldp"
	BestEffort bool   // command may be rejected on some builds; not fatal
	EdgeSource bool   // records from this step create edges (vs enrich only)
}

var plans = map[string][]step{
	"arista_eos": {
		{Command: "show lldp neighbors detail", Key: "lldp_detail", Protocol: "lldp", EdgeSource: true},
	},
	"cisco_ios": {
		// Detail BEFORE summary, and an edge source in its own right.
		// Both forms describe the same links, and the first record to
		// claim a {local, remote, remote-port} key wins — so whichever
		// runs first decides whether the edge carries a management
		// address. The summary form has no address column at all and
		// truncates the neighbor name to 20 characters, so letting it
		// win meant a Cisco->Arista link (no CDP on the far end) mapped
		// as a bare, address-less name with nothing to fall back to.
		// The summary stays as the fallback for boxes where detail is
		// unsupported or errors out. nxos and junos were already
		// ordered this way; ios and iosxe were the outliers.
		{Command: "show lldp neighbors detail", Key: "lldp_detail", Protocol: "lldp", BestEffort: true, EdgeSource: true},
		{Command: "show lldp neighbors", Key: "lldp", Protocol: "lldp", EdgeSource: true},
		{Command: "show cdp neighbors detail", Key: "cdp_detail", Protocol: "cdp", BestEffort: true, EdgeSource: true},
	},
	"cisco_iosxe": {
		// Same ordering as cisco_ios, same reason.
		{Command: "show lldp neighbors detail", Key: "lldp_detail", Protocol: "lldp", BestEffort: true, EdgeSource: true},
		{Command: "show lldp neighbors", Key: "lldp", Protocol: "lldp", EdgeSource: true},
		{Command: "show cdp neighbors detail", Key: "cdp_detail", Protocol: "cdp", BestEffort: true, EdgeSource: true},
	},
	"cisco_nxos": {
		{Command: "show cdp neighbors detail", Key: "cdp_detail", Protocol: "cdp", EdgeSource: true},
		{Command: "show lldp neighbors detail", Key: "lldp_detail", Protocol: "lldp", BestEffort: true, EdgeSource: true},
	},
	"juniper_junos": {
		// detail first: it carries System Description (pre-dial exclusion
		// depends on it) and in-device dedup keeps the first record per
		// edge. Old Junos rejects detail (best-effort); terse then still
		// provides the edges, just without descriptions.
		{Command: "show lldp neighbors detail", Key: "lldp_detail", Protocol: "lldp", BestEffort: true, EdgeSource: true},
		{Command: "show lldp neighbors", Key: "lldp", Protocol: "lldp", EdgeSource: true},
	},
	// The three plans below are new and, unlike the ones above, have not
	// been run against real gear through this codebase's own test harness
	// (no Go toolchain in the authoring environment) -- only against the
	// TextFSM templates by hand. BestEffort is set unconditionally on all
	// three so a template gap degrades to "no neighbors found" rather than
	// aborting the device's crawl. See internal/tfsm/templates for the
	// per-template confidence notes.
	"aruba_procurve": {
		{Command: "show lldp info remote-device detail", Key: "lldp_detail", Protocol: "lldp", BestEffort: true, EdgeSource: true},
	},
	"aruba_cx": {
		{Command: "show lldp neighbor-info detail", Key: "lldp_detail", Protocol: "lldp", BestEffort: true, EdgeSource: true},
	},
	"extreme_exos": {
		{Command: "show lldp neighbors detailed", Key: "lldp_detail", Protocol: "lldp", BestEffort: true, EdgeSource: true},
	},
	"hp_comware": {
		// Confirmed live (2026-08-21) against two different Comware
		// devices: Comware 5's plain form already gives full per-field
		// detail, but the SAME command on Comware 7 gives a terse
		// summary (combined "ChassisID/subtype" and "PortID/subtype"
		// lines, one "Capabilities" line) the template below does not
		// parse -- Comware 7 needs the "verbose" form for the matching
		// shape. Both are sent, in the order most likely to match
		// first: a version that doesn't understand one command either
		// rejects it cleanly or produces non-matching output, and the
		// template's permissive catch-all means either failure mode
		// contributes zero records rather than corrupting the ones the
		// other command found.
		{Command: "display lldp neighbor-information verbose", Key: "lldp_detail", Protocol: "lldp", BestEffort: true, EdgeSource: true},
		{Command: "display lldp neighbor-information", Key: "lldp_detail", Protocol: "lldp", BestEffort: true, EdgeSource: true},
	},
}

// planFor returns the collection plan for a fingerprinted platform.
func planFor(platform string) ([]step, bool) {
	p, ok := plans[platform]
	return p, ok
}

// firstNonEmpty is a small helper for record-field fallbacks.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// recordToNeighbor maps a parsed template record (field names vary a little
// per template family) onto the topology model.
func recordToNeighbor(rec map[string]string, protocol string) topo.Neighbor {
	return topo.Neighbor{
		LocalInterface:  firstNonEmpty(rec["LOCAL_INTERFACE"]),
		RemoteDevice:    firstNonEmpty(rec["NEIGHBOR_NAME"], rec["SYSTEM_NAME"], rec["CHASSIS_ID"]),
		RemoteInterface: firstNonEmpty(rec["NEIGHBOR_INTERFACE"], rec["NEIGHBOR_PORT_ID"], rec["PORT_ID"]),
		RemoteIP:        firstNonEmpty(rec["MGMT_ADDRESS"], rec["REMOTE_IP"], rec["MANAGEMENT_IP"]),
		// CDP reports a platform directly; LLDP does not, and carries it as
		// prose inside the system description instead. Without the fallback
		// every LLDP-only device — which is every Arista and every Junos
		// here — reports no neighbor platform at all.
		RemotePlatform: firstNonEmpty(
			rec["PLATFORM"],
			normalize.PlatformFromDescription(
				firstNonEmpty(rec["NEIGHBOR_DESCRIPTION"], rec["SYSTEM_DESCRIPTION"]),
			),
		),
		RemoteDescr:  firstNonEmpty(rec["NEIGHBOR_DESCRIPTION"], rec["SYSTEM_DESCRIPTION"]),
		Capabilities: firstNonEmpty(rec["CAPABILITIES"]),
		Protocol:     protocol,
	}
}
