// internal/capture/spec.go
//
// What to collect from a device, per platform.
//
// Capture's first use is config backup, but the shape is deliberately not
// config-shaped: a Spec is "one named kind of device state", and running-config
// is the first of them rather than the model. The key is two-dimensional —
// (type, platform) — where the crawler's plan table needed only one, because
// crawl collects exactly one thing and capture does not.
//
// # Why these are Go values and not a data file
//
// A YAML file of per-platform commands would be editable without a rebuild,
// which sounds like the better answer until two things are weighed against it.
// First, the read-only guarantee is supposed to be enforced by a test that
// FAILS THE BUILD, and a build-time test cannot cover a file the user edits at
// runtime. Second, the per-platform command knowledge is the accumulated field
// work; a data file is that work in copyable form.
//
// So built-in specs are values in this file. Adding a capture type is adding a
// var — no registry, no init() side effects, no interface for anyone to
// implement. If user-supplied commands are ever wanted, they belong on a
// separate and explicitly unverified path, not as an extension of this one.
package capture

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Cost is a declared estimate of what a command asks of the device.
//
// It is an estimate and cannot be anything else: the same command is trivial
// on a lab router and punishing on a loaded chassis, and nothing here knows
// which one it is talking to. What the declaration buys is a separate, much
// smaller concurrency lane — twenty devices can hand over a config at once
// while only two are asked for a tech-support. The actual protection against
// a device that answers forever is MaxBytes and Timeout below, not this.
type Cost int

const (
	// CostCheap is bounded output a device produces without effort:
	// a config, an inventory, a version banner.
	CostCheap Cost = iota

	// CostExpensive is output whose size and duration are properties of
	// the chassis rather than of the command. Runs in a narrow lane.
	//
	// NO BUILTIN USES THIS TODAY. tech-support was the one that did and it
	// was removed deliberately: shipping it meant a full diagnostic bundle
	// off every device in an estate was one click away, and a control that
	// makes that as easy as a config backup is a control that will
	// eventually be used that way by accident. The lane, the bounds and
	// this constant stay because the next expensive command will need
	// them, and because deleting the machinery would mean rebuilding it
	// under pressure the day something has to be collected during an
	// incident.
	CostExpensive
)

func (c Cost) String() string {
	if c == CostExpensive {
		return "expensive"
	}
	return "cheap"
}

// Command is one platform's answer to one capture type.
type Command struct {
	// Command is sent verbatim. One command — if a platform needs two,
	// that is two capture types, because two commands produce two
	// artifacts and one file cannot be diffed as either.
	Command string

	// MaxBytes overrides netexec's per-command limit. Zero takes the
	// netexec default. This is where a capture type declares that it
	// genuinely is huge, rather than every session being widened for the
	// benefit of the one command that needs it.
	MaxBytes int

	// Timeout overrides the session's command timeout. Zero takes the
	// session default.
	Timeout time.Duration

	// Cost selects the concurrency lane.
	Cost Cost

	// ModelMatch, when set, gates this command on the fingerprint's
	// version output. A device whose version text does not match is
	// reported not-applicable and the command is never sent.
	//
	// It exists because a platform key is a NOS, not a chassis. Every
	// Junos box answers to "juniper_junos" whether or not it has a bridge
	// table, so a forwarding-table command aimed at a switch would
	// otherwise be sent to every router in the estate.
	//
	// Use it only where the qualifying set is small and CLOSED, which in
	// practice means Junos: QFX and EX are the switches, everything else
	// is not, and that list does not move. Do NOT reach for it on Cisco
	// IOS. The Catalyst model space is large and still growing, so a
	// positive pattern there becomes a regex edited on every hardware
	// refresh — and its failure mode is the bad one. A switch the pattern
	// does not recognise is silently reported not-applicable and never
	// captured again, which is indistinguishable from a device that
	// legitimately has no such table. Where the set is open, send the
	// command and let the rejection check catch the refusal: a refusal is
	// self-correcting and a missing pattern is not.
	ModelMatch *regexp.Regexp
}

// Spec is one capture type across every platform that supports it.
type Spec struct {
	// Type names the artifact and is used verbatim as a directory level,
	// so it has to be a safe path element. Validate enforces that.
	Type string

	// Description is one line, for the harness and for a UI picker.
	Description string

	// Keep bounds how many distinct versions of this type the store holds
	// per device. Zero means unlimited, which is the only safe default: a
	// config backup that silently discards history is not a backup, and
	// every spec that existed before retention did stays at zero without
	// being edited.
	//
	// It is a property of the TYPE and not of the platform, because what
	// makes a capture worth keeping forever is what it is, not which
	// vendor produced it. A config is the record; an ARP table is a
	// sample of something that was already different by the time it was
	// stored.
	//
	// Retention is per (device, type) rather than global, because pruning
	// happens inside one device's type directory and a global count would
	// mean one busy device evicting another's history.
	Keep int

	// Commands maps a netexec platform name to its command. A platform
	// absent from this map is NOT AN ERROR — it is "not applicable", which
	// is a distinct outcome from failure and has to stay distinct all the
	// way to the display. A linux host has no running-config; reporting
	// that as a failed capture is how a run full of nothing-wrong reads as
	// a run full of problems.
	Commands map[string]Command
}

// For returns the command for a platform, and whether this spec applies to it
// at all.
func (s Spec) For(platform string) (Command, bool) {
	c, ok := s.Commands[platform]
	return c, ok
}

// Platforms lists the platforms this spec applies to, sorted.
func (s Spec) Platforms() []string {
	out := make([]string, 0, len(s.Commands))
	for p := range s.Commands {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// safeType is the character set allowed in a capture type, since the type
// becomes a directory name on three operating systems.
var safeType = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Validate reports whether a spec is usable. Called by the builtin test, and
// worth calling on anything constructed elsewhere.
func (s Spec) Validate() error {
	if !safeType.MatchString(s.Type) {
		return fmt.Errorf("capture type %q is not a safe directory name", s.Type)
	}
	if len(s.Commands) == 0 {
		return fmt.Errorf("capture type %q has no commands", s.Type)
	}
	if s.Keep < 0 {
		return fmt.Errorf("capture type %q has negative Keep; zero means unlimited", s.Type)
	}
	for platform, c := range s.Commands {
		if strings.TrimSpace(c.Command) == "" {
			return fmt.Errorf("%s/%s: empty command", s.Type, platform)
		}
		if strings.ContainsAny(c.Command, "\r\n") {
			return fmt.Errorf("%s/%s: command contains a line break", s.Type, platform)
		}
		if c.MaxBytes < 0 {
			return fmt.Errorf("%s/%s: negative MaxBytes", s.Type, platform)
		}
		if c.Timeout < 0 {
			return fmt.Errorf("%s/%s: negative Timeout", s.Type, platform)
		}
	}
	return nil
}

// RunningConfig is the reason capture exists first.
//
// Junos takes `| display set` rather than the hierarchical form: set-style
// output is line-oriented, so a diff between two captures names the statement
// that changed instead of the brace level it lives under.
var RunningConfig = Spec{
	Type:        "running-config",
	Description: "the active configuration",
	Commands: map[string]Command{
		"cisco_ios":     {Command: "show running-config"},
		"cisco_iosxe":   {Command: "show running-config"},
		"cisco_nxos":    {Command: "show running-config"},
		"arista_eos":    {Command: "show running-config"},
		"juniper_junos": {Command: "show configuration | display set"},
		"aruba_cx":       {Command: "show running-config"},
		"aruba_procurve": {Command: "show running-config"},
		"hp_comware":     {Command: "display current-configuration"},
		"extreme_exos":   {Command: "show configuration"},
	},
}

// StartupConfig is what the device will come back as. Captured separately
// from RunningConfig on purpose: the interesting question about these two is
// whether they differ, and that question cannot be asked if they share a file.
var StartupConfig = Spec{
	Type:        "startup-config",
	Description: "the saved configuration (differs from running when someone forgot to write)",
	Commands: map[string]Command{
		"cisco_ios":   {Command: "show startup-config"},
		"cisco_iosxe": {Command: "show startup-config"},
		"cisco_nxos":  {Command: "show startup-config"},
		"arista_eos":  {Command: "show startup-config"},
		"aruba_procurve": {Command: "show config"},
		"hp_comware":     {Command: "display saved-configuration"},
		// aruba_cx: likely has an equivalent (the `boot system`
		// prompts offer to save running to startup), but no exact
		// command was confirmed against vendor docs, so it is left
		// out rather than guessed.
		// extreme_exos: like Junos, EXOS has no separate
		// running/startup split to read back via a show command —
		// the saved config lives as a file, compared by copying it
		// off rather than displaying it inline.
		// Junos has no startup/running split — the committed
		// configuration IS the startup one. Absent rather than
		// duplicated, so the report says "not applicable" instead of
		// storing the same bytes twice under two names.
	},
}

// Inventory exists mostly to prove the model is not config-shaped: same
// storage, same history, same diff, different kind of state entirely.
var Inventory = Spec{
	Type:        "inventory",
	Description: "chassis, modules and serial numbers",
	Commands: map[string]Command{
		"cisco_ios":     {Command: "show inventory"},
		"cisco_iosxe":   {Command: "show inventory"},
		"cisco_nxos":    {Command: "show inventory"},
		"arista_eos":    {Command: "show inventory"},
		"juniper_junos": {Command: "show chassis hardware"},
		"aruba_cx":       {Command: "show system"},
		"aruba_procurve": {Command: "show system information"},
		"hp_comware":     {Command: "display device manuinfo"},
		"extreme_exos":   {Command: "show switch"},
	},
}

// ARPTable is layer-3 to layer-2 resolution, as the device currently sees it.
//
// The first of the two BOUNDED types, and the reason Keep exists. A config
// changes when someone changes it; an ARP table changes because time passed.
// Every capture of one is a new file, so an unbounded history of them is a
// directory that grows without ever answering a question the newest few could
// not — while the config history beside it stays small precisely because it
// only grows when something happened.
//
// Junos takes `no-resolve`: without it the device attempts a reverse DNS
// lookup per entry, which on a table of any size turns a fast read into a
// slow one bounded by a resolver rather than by the chassis.
var ARPTable = Spec{
	Type:        "arp-table",
	Description: "IP to MAC resolution as currently held (rolling history)",
	Keep:        5,
	Commands: map[string]Command{
		"cisco_ios":     {Command: "show ip arp"},
		"cisco_iosxe":   {Command: "show ip arp"},
		"cisco_nxos":    {Command: "show ip arp"},
		"arista_eos":    {Command: "show ip arp"},
		"juniper_junos": {Command: "show arp no-resolve"},
		"aruba_cx":       {Command: "show arp"},
		"aruba_procurve": {Command: "show arp"},
		"hp_comware":     {Command: "display arp"},
		"extreme_exos":   {Command: "show iparp"},
	},
}

// junosSwitch matches the version output of a Junos box that has a bridge
// table. QFX and EX are the switching families; MX, ACX, PTX and SRX are not,
// and all of them answer to the same "juniper_junos" platform key.
//
// Anchored on a digit so it cannot be satisfied by the word "ex" appearing in
// a hostname or a description line.
var junosSwitch = regexp.MustCompile(`(?i)\b(qfx|ex)[0-9]`)

// MACTable is the forwarding table — which MAC was last seen on which port.
//
// The two vendors here need opposite treatment, and that asymmetry is the
// point rather than an inconsistency.
//
// Junos gets a ModelMatch. The switching families are QFX and EX and that set
// is closed, so gating is exact and an MX is never sent a command it cannot
// answer.
//
// Cisco IOS deliberately gets NO gate even though it has the same router/
// switch split, because the Catalyst model space is open-ended. A positive
// pattern would silently skip any switch it failed to recognise, and a capture
// type that quietly reports "not applicable" for a device that does have a MAC
// table is a worse failure than one that tries and is refused. A refusal is
// caught by the rejection check, costs one command, and corrects itself the
// moment the device changes.
//
// NX-OS needs neither: every Nexus is a switch.
var MACTable = Spec{
	Type:        "mac-table",
	Description: "MAC address to port forwarding entries (rolling history)",
	Keep:        5,
	Commands: map[string]Command{
		"cisco_ios":     {Command: "show mac address-table"},
		"cisco_iosxe":   {Command: "show mac address-table"},
		"cisco_nxos":    {Command: "show mac address-table"},
		"arista_eos":    {Command: "show mac address-table"},
		"juniper_junos": {Command: "show ethernet-switching table brief", ModelMatch: junosSwitch},
		"aruba_cx":       {Command: "show mac-address-table"},
		"aruba_procurve": {Command: "show mac-address"},
		"hp_comware":     {Command: "display mac-address"},
		"extreme_exos":   {Command: "show fdb"},
	},
}

// Builtin returns every shipped spec, in a stable order.
//
// Adding a capture type means adding a var above and a line here. That is the
// whole extension mechanism, and it is deliberately not more than that.
func Builtin() []Spec {
	return []Spec{RunningConfig, StartupConfig, Inventory, ARPTable, MACTable}
}

// Lookup finds a builtin spec by type name.
func Lookup(typ string) (Spec, bool) {
	for _, s := range Builtin() {
		if s.Type == typ {
			return s, true
		}
	}
	return Spec{}, false
}
