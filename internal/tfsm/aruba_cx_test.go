// internal/tfsm/aruba_cx_test.go
//
// Regression coverage for a real parse failure hit live (2026-08-21)
// against an ArubaOS-CX 6300-series switch ("lab-cx1"): a neighbor with no
// management address at all reports the field as
// "Neighbor Management-Address    :" with nothing after the colon, which
// the original MGMT_ADDRESS pattern (\S+) could not match -- it requires
// at least one non-whitespace character.
package tfsm

import "testing"

const arubaCXEmptyMgmtAddressBlock = `
Port                           : 1/1/7
Neighbor Entries               : 1
Neighbor Entries Deleted       : 0
Neighbor Entries Dropped       : 0
Neighbor Entries Aged-Out      : 0
Neighbor System-Name           :
Neighbor System-Description    :
Neighbor Chassis-ID            : 02:00:00:00:51:10
Neighbor Management-Address    :
Chassis Capabilities Available :
Chassis Capabilities Enabled   :
Neighbor Port-ID               : 02:00:00:00:51:10
Neighbor Port-Desc             :
TTL                            : 121
`

func TestArubaCXParsesAnEmptyManagementAddress(t *testing.T) {
	recs, err := Parse("aruba_cx", "lldp_detail", arubaCXEmptyMgmtAddressBlock)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec["LOCAL_INTERFACE"] != "1/1/7" {
		t.Errorf("LOCAL_INTERFACE = %q, want %q", rec["LOCAL_INTERFACE"], "1/1/7")
	}
	if rec["MGMT_ADDRESS"] != "" {
		t.Errorf("MGMT_ADDRESS = %q, want empty", rec["MGMT_ADDRESS"])
	}
	if rec["CHASSIS_ID"] != "02:00:00:00:51:10" {
		t.Errorf("CHASSIS_ID = %q, want %q", rec["CHASSIS_ID"], "02:00:00:00:51:10")
	}
}
