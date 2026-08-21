// internal/tfsm/aruba_procurve_test.go
//
// Regression coverage for a real parse failure hit live (2026-08-21)
// against an ArubaOS-Switch 3810M: a neighbor whose Remote Management
// Address Type is "all802" reports the address as six space-separated hex
// byte pairs rather than a dotted-quad IP, which the original MGMT_ADDRESS
// pattern (\S+) could not match.
package tfsm

import "testing"

const arubaProcurveAll802Block = `
  Local Port   : 48
  ChassisType  : mac-address
  ChassisId    : 020000-005120
  PortType     : interface-name
  PortId       : 3:8
  SysName      : lab-exos1
  System Descr : ExtremeXOS (Stack) version 32.6.3.127 32.6.3.127-patch1-8
  PortDescr    :
  Pvid         :

  System Capabilities Supported  : bridge, router
  System Capabilities Enabled    : bridge

  Remote Management Address
     Type    : all802
     Address : 02 00 00 00 51 20

  Poe Plus Information Detail

    Poe Device Type         : Type2 PSE
    Power Source            : Unknown
    Power Priority          : Unknown
    PD Requested Power Value   : 0.0 Watts
    PSE Allocated Power Value  : 0.0 Watts
`

func TestArubaProcurveParsesAnAll802ManagementAddress(t *testing.T) {
	recs, err := Parse("aruba_procurve", "lldp_detail", arubaProcurveAll802Block)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec["LOCAL_INTERFACE"] != "48" {
		t.Errorf("LOCAL_INTERFACE = %q, want %q", rec["LOCAL_INTERFACE"], "48")
	}
	if rec["MGMT_ADDRESS"] != "02 00 00 00 51 20" {
		t.Errorf("MGMT_ADDRESS = %q, want %q", rec["MGMT_ADDRESS"], "02 00 00 00 51 20")
	}
	if rec["NEIGHBOR_NAME"] != "lab-exos1" {
		t.Errorf("NEIGHBOR_NAME = %q, want %q", rec["NEIGHBOR_NAME"], "lab-exos1")
	}
}
