// internal/netexec/fingerprint_test.go
// Unit tests for the classification half of fingerprinting (the probe loop
// itself needs a live session — lab work).
package netexec

import "testing"

func TestIsCLIError(t *testing.T) {
	errors := []string{
		"% Invalid input detected at '^' marker.",
		"                 ^\n% Invalid input detected",
		"syntax error, expecting <command>.",
		"ERROR: % Invalid input detected",
		"% Unrecognized command found at '^' position.",
		"Error: Ambiguous command",
		"bad command name set (line 1)",
		"bash: frobnicate: command not found",
	}
	for _, e := range errors {
		if !isCLIError(e) {
			t.Errorf("isCLIError(%q) = false, want true", e)
		}
	}
	notErrors := []string{
		"", // silent success
		"Arista DCS-7280SRA-48C6\nSoftware image version: 4.33.1.1F",
		"Cisco IOS Software, C2960 Software",
		"Screen length is set to 0", // some stacks confirm politely
	}
	for _, ok := range notErrors {
		if isCLIError(ok) {
			t.Errorf("isCLIError(%q) = true, want false", ok)
		}
	}
}

func TestClassifyVersions(t *testing.T) {
	cases := []struct {
		probeIdx int
		out      string
		want     string
	}{
		{0, "Arista DCS-7280SRA-48C6\nSoftware image version: 4.33.1.1F", "arista_eos"},
		{0, "Cisco Nexus Operating System (NX-OS) Software", "cisco_nxos"},
		{0, "Cisco IOS XE Software, Version 17.09.04a", "cisco_iosxe"},
		{0, "Cisco IOS Software, C2960X Software", "cisco_ios"},
		{0, "some future platform", ""},
		{1, "Hostname: lab-mx1\nModel: mx204\nJunos: 23.4R2.13", "juniper_junos"},
		{2, "Cisco Adaptive Security Appliance Software Version 9.18(4)", "cisco_asa"},
		{4, "ExtremeXOS (X440-G2-48p-10G4) version 22.7.2.4", "extreme_exos"},
		{5, "ArubaOS-CX Version : XL.10.00.0002C-1-g1b84ef2", "aruba_cx"},
		{6, "HP J9729A 2920-24G Switch, revision KA.16.09.0022", "aruba_procurve"},
		// Real `show version` output from an ArubaOS-Switch 3810M,
		// captured live (2026-08-21): no vendor name text anywhere,
		// only the "Image stamp:" label the classifier now also
		// matches on.
		{6, "Image stamp:\n /ws/swbuildm/rel_ajanta_qaoff/code/build/bom(swbuildm_rel_ajanta_qaoff_rel_ajanta)\n\t\tApr 10 2023 23:56:39\n\t\tKB.16.10.0024\n\t\t362\nBoot Image:     Secondary", "aruba_procurve"},
		{7, "version: 7.15.3 (stable)\nplatform: MikroTik", "mikrotik_routeros"},
		{8, "Linux lab-host 6.8.0 #1 SMP x86_64 GNU/Linux", "linux"},
	}
	for _, c := range cases {
		if got := classify(c.out, probes[c.probeIdx].classes); got != c.want {
			t.Errorf("classify(probe %d, %q) = %q, want %q", c.probeIdx, c.out, got, c.want)
		}
	}
}
