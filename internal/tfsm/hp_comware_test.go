// internal/tfsm/hp_comware_test.go
//
// Regression coverage built from real `display lldp neighbor-information`
// captures off a live Comware device ("lab-core1"), confirmed 2026-08-21 —
// including a second pass that added a Management address section the
// first pass never saw, which is why this test carries two neighbors.
package tfsm

import "testing"

const comwareNeighborBlock = `
LLDP neighbor-information of port 47[GigabitEthernet1/0/47]:
  Neighbor index   : 1
  Update time      : 0 days,0 hours,23 minutes,2 seconds
  Chassis type     : MAC address
  Chassis ID       : 0200-0000-5130
  Port ID type     : Locally assigned
  Port ID          : 35
  Port description : 35
  System name        : lab-sw01
  System description : Aruba JL074A 3810M-48G-PoE+-1-slot Switch, revision KB.16.10.0024, ROM KB.16.01.0008 (/ws/swbuildm/rel_ajanta_qaoff/code/build/bom(swbuildm_rel_ajanta_qaoff_rel_ajanta))
  System capabilities supported : Bridge,Router
  System capabilities enabled   : Bridge

  Management address type           : ipv4
  Management address                : 192.0.2.74
  Management address interface type : IfIndex
  Management address interface ID   : Unknown
  Management address OID            : 0

  Port VLAN ID(PVID): 900

  Auto-negotiation supported : Yes
  Auto-negotiation enabled   : Yes
  OperMau                    : speed(1000)/duplex(Full)

  Power port class          : PSE
  PSE power supported       : Yes
  PSE power enabled         : Yes
  PSE pairs control ability : No
  Power pairs               : Signal
  Port power classification : Class 0

  Unknown organizationally-defined TLV
    TLV OUI         : 00-16-b9
    TLV subtype     : 2
    Index           : 1
    TLV information : 0001


LLDP neighbor-information of port 48[GigabitEthernet1/0/48]:
  Neighbor index   : 1
  Update time      : 0 days,0 hours,13 minutes,34 seconds
  Chassis type     : MAC address
  Chassis ID       : 0200-0000-5140
  Port ID type     : Interface name
  Port ID          : 1/1/2
  Port description : 1/1/2
  System name        : lab-sw2
  System description : Aruba JL726A  ML.10.13.1040
  System capabilities supported : Bridge,Router
  System capabilities enabled   : Bridge,Router

  Management address type           : ipv4
  Management address                : 192.0.2.101
  Management address interface type : IfIndex
  Management address interface ID   : 16777217
  Management address OID            : 0

  Port VLAN ID(PVID): 1

  VLAN name of VLAN 1: DEFAULT_VLAN_1

  Auto-negotiation supported : Yes
  Auto-negotiation enabled   : Yes
  OperMau                    : speed(1000)/duplex(Full)

  Maximum frame Size: 1500

  Unknown organizationally-defined TLV
    TLV OUI         : 88-3a-30
    TLV subtype     : 2
    Index           : 1
    TLV information : 0001

`

func TestHPComwareParsesRealNeighborsWithManagementAddresses(t *testing.T) {
	recs, err := Parse("hp_comware", "lldp_detail", comwareNeighborBlock)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}

	want := []map[string]string{
		{
			"LOCAL_INTERFACE": "GigabitEthernet1/0/47",
			"CHASSIS_ID":      "0200-0000-5130",
			"NEIGHBOR_PORT_ID": "35",
			"NEIGHBOR_NAME":   "lab-sw01",
			"CAPABILITIES":    "Bridge",
			"MGMT_ADDRESS":    "192.0.2.74",
		},
		{
			"LOCAL_INTERFACE":      "GigabitEthernet1/0/48",
			"CHASSIS_ID":           "0200-0000-5140",
			"NEIGHBOR_PORT_ID":     "1/1/2",
			"NEIGHBOR_NAME":        "lab-sw2",
			"NEIGHBOR_DESCRIPTION": "Aruba JL726A  ML.10.13.1040",
			"CAPABILITIES":         "Bridge,Router",
			"MGMT_ADDRESS":         "192.0.2.101",
		},
	}
	for i, w := range want {
		for field, wantVal := range w {
			if got := recs[i][field]; got != wantVal {
				t.Errorf("record %d: %s = %q, want %q", i, field, got, wantVal)
			}
		}
	}
}

// "Management address type", "Management address interface type", and
// "Management address interface ID" all also start with "Management
// address" -- confirming MGMT_ADDRESS's rule (which requires nothing but
// whitespace before the colon) doesn't accidentally capture one of those
// instead of the real value is the point of this test, not just that
// parsing succeeds.
func TestHPComwareMgmtAddressDoesNotCaptureTheWrongField(t *testing.T) {
	recs, err := Parse("hp_comware", "lldp_detail", comwareNeighborBlock)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for i, rec := range recs {
		if rec["MGMT_ADDRESS"] == "ipv4" || rec["MGMT_ADDRESS"] == "IfIndex" {
			t.Errorf("record %d: MGMT_ADDRESS = %q, captured a sibling field instead of the address", i, rec["MGMT_ADDRESS"])
		}
	}
}

// comware7VerboseBlock is a real `display lldp neighbor-information
// verbose` capture off a Comware 7 device ("lab-core2"), confirmed live
// (2026-08-21). Distinct from the Comware 5 capture above in several
// confirmed ways: an "LLDP agent nearest-bridge:" line ahead of the
// neighbor's own fields, "LLDP neighbor index" instead of "Neighbor
// index", an added "Time to live" field, "IPv4" capitalized in Management
// address type (not parsed, so irrelevant, but noted), and a multi-line
// wrapped System description this template deliberately does not
// reconstruct -- see the truncation note in the .textfsm file itself.
const comware7VerboseBlock = `
LLDP neighbor-information of port 48[Ten-GigabitEthernet1/0/48]:
LLDP agent nearest-bridge:
 LLDP neighbor index : 1
 Update time         : 0 days, 0 hours, 17 minutes, 39 seconds
 Chassis type        : MAC address
 Chassis ID          : 0200-0000-5130
 Port ID type        : Locally assigned
 Port ID             : 57
 Time to live        : 120
 Port description    : A3
 System name         : lab-sw01
 System description  : Aruba JL074A 3810M-48G-PoE+-1-slot Switch, revision KB.16
                       .10.0024, ROM KB.16.01.0008 (/ws/swbuildm/rel_ajanta_qaof
                       f/code/build/bom(swbuildm_rel_ajanta_qaoff_rel_ajanta))
 System capabilities supported : Bridge, Router
 System capabilities enabled   : Bridge
 Management address type           : IPv4
 Management address                : 192.0.2.74
 Management address interface type : IfIndex
 Management address interface ID   : Unknown
 Management address OID            : 0
 Port VLAN ID(PVID)  : 1
 Auto-negotiation supported : Yes
 Auto-negotiation enabled   : Yes
 OperMau                    : Speed(10000)/Duplex(Full)
 Power port class           : PSE
 PSE power supported        : No
 PSE power enabled          : No
 PSE pairs control ability  : No
 Power pairs                : Signal
 Port power classification  : Class 0
 Unknown organizationally-defined TLV:
  TLV OUI            : 00-16-b9
  TLV subtype        : 2
  Index              : 1
  TLV information    : 0x0001

`

func TestHPComwareParsesComware7VerboseOutput(t *testing.T) {
	recs, err := Parse("hp_comware", "lldp_detail", comware7VerboseBlock)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]
	want := map[string]string{
		"LOCAL_INTERFACE":  "Ten-GigabitEthernet1/0/48",
		"CHASSIS_ID":       "0200-0000-5130",
		"NEIGHBOR_PORT_ID": "57",
		"NEIGHBOR_NAME":    "lab-sw01",
		"CAPABILITIES":     "Bridge",
		"MGMT_ADDRESS":     "192.0.2.74",
	}
	for field, wantVal := range want {
		if got := rec[field]; got != wantVal {
			t.Errorf("%s = %q, want %q", field, got, wantVal)
		}
	}
	// Deliberately truncated at the wrap point -- see the .textfsm file's
	// note on why this isn't reconstructed across the continuation lines.
	if want, got := "Aruba JL074A 3810M-48G-PoE+-1-slot Switch, revision KB.16", rec["NEIGHBOR_DESCRIPTION"]; got != want {
		t.Errorf("NEIGHBOR_DESCRIPTION = %q, want %q (truncated at the wrap point)", got, want)
	}
}

// Comware 7's PLAIN (non-verbose) form is a different, terser shape this
// template does not parse -- confirmed live. It must degrade to zero
// records through the permissive catch-all, not an error, since plan.go
// sends this form unconditionally as a fallback for Comware 5.
func TestHPComwareIgnoresComware7PlainFormOutput(t *testing.T) {
	const comware7PlainBlock = `
LLDP neighbor-information of port 48[Ten-GigabitEthernet1/0/48]:
LLDP agent nearest-bridge:
 LLDP neighbor index : 1
 ChassisID/subtype   : 0200-0000-5130/MAC address
 PortID/subtype      : 57/Locally assigned
 Capabilities        : Bridge
`
	recs, err := Parse("hp_comware", "lldp_detail", comware7PlainBlock)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("got %d records from the terse Comware 7 form, want 0 (LOCAL_INTERFACE is Required and this shape never sets it)", len(recs))
	}
}

// The pager text this device actually shows ("---- More ----") must not
// get treated as a second, spurious record boundary or leak into a field
// value -- it appears mid-stream in the raw session buffer, before
// StripEchoAndPrompt or the continue-prompt handling in session.go ever
// gets a chance to remove it, in the unusual case a caller feeds the raw
// text straight to Parse.
func TestHPComwareIgnoresThePagerLine(t *testing.T) {
	withPager := comwareNeighborBlock + "  ---- More ----\n"
	recs, err := Parse("hp_comware", "lldp_detail", withPager)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
}
