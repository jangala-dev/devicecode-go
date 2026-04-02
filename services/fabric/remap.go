package fabric

import "devicecode-go/bus"

// Topic remapping rules matching the shipped Lua fabric link contract.
//
// These rules are hardcoded and exact-match for v1. The Lua (CM5) side
// uses config-driven wildcard rules, but the MCU only needs a fixed set
// of routes. If new routes are required, add them here and on the Lua
// config side.
//
// CM5 -> MCU wire publish:
//   ["config","device"] -> config/device
//
// CM5 -> MCU wire call:
//   ["rpc","hal","dump"] -> rpc/hal/dump
//
// MCU local bus publish -> wire:
//   hal/cap/env/#   -> ["state","env",...]
//   hal/cap/power/# -> ["state","power",...]
//   hal/state       -> ["state","hal"]

type wireImportRule struct {
	wire  []string
	local []string
}

type busExportRule struct {
	localPrefix  []string
	remotePrefix []string
	suffix       bool
}

var importPublishRules = []wireImportRule{
	{
		wire:  []string{"config", "device"},
		local: []string{"config", "device"},
	},
}

var importCallRules = []wireImportRule{
	{
		wire:  []string{"rpc", "hal", "dump"},
		local: []string{"rpc", "hal", "dump"},
	},
}

var exportPublishRules = []busExportRule{
	{
		localPrefix:  []string{"hal", "cap", "env"},
		remotePrefix: []string{"state", "env"},
		suffix:       true,
	},
	{
		localPrefix:  []string{"hal", "cap", "power"},
		remotePrefix: []string{"state", "power"},
		suffix:       true,
	},
	{
		localPrefix:  []string{"hal", "state"},
		remotePrefix: []string{"state", "hal"},
	},
}

var exportCallRules = []busExportRule{
	{
		localPrefix:  []string{"fabric", "out", "rpc", "hal", "dump"},
		remotePrefix: []string{"rpc", "hal", "dump"},
	},
}

func importPublishTopic(wire []string) bus.Topic {
	return wireImport(wire, importPublishRules)
}

func importCallTopic(wire []string) bus.Topic {
	return wireImport(wire, importCallRules)
}

func exportTopic(t bus.Topic) []string {
	return busExport(t, exportPublishRules)
}

func exportPatterns() []bus.Topic {
	return exportPatternsFor(exportPublishRules)
}

func exportCallTopic(t bus.Topic) []string {
	return busExport(t, exportCallRules)
}

func exportCallPatterns() []bus.Topic {
	return exportPatternsFor(exportCallRules)
}

func wireImport(wire []string, rules []wireImportRule) bus.Topic {
	for _, rule := range rules {
		if slicesEqualStrings(wire, rule.wire) {
			return stringsToTopic(rule.local)
		}
	}
	return nil
}

func busExport(t bus.Topic, rules []busExportRule) []string {
	for _, rule := range rules {
		out, ok := applyBusExportRule(t, rule)
		if ok {
			return out
		}
	}
	return nil
}

func applyBusExportRule(t bus.Topic, rule busExportRule) ([]string, bool) {
	if t.Len() < len(rule.localPrefix) {
		return nil, false
	}
	for i, want := range rule.localPrefix {
		if str(t, i) != want {
			return nil, false
		}
	}
	if !rule.suffix && t.Len() != len(rule.localPrefix) {
		return nil, false
	}

	out := make([]string, 0, len(rule.remotePrefix)+maxInt(0, t.Len()-len(rule.localPrefix)))
	out = append(out, rule.remotePrefix...)
	if rule.suffix {
		for i := len(rule.localPrefix); i < t.Len(); i++ {
			s := str(t, i)
			if s == "" {
				return nil, false
			}
			out = append(out, s)
		}
	}
	return out, true
}

func exportPatternsFor(rules []busExportRule) []bus.Topic {
	out := make([]bus.Topic, 0, len(rules))
	for _, rule := range rules {
		tokens := make([]bus.Token, 0, len(rule.localPrefix)+1)
		for _, s := range rule.localPrefix {
			tokens = append(tokens, s)
		}
		if rule.suffix {
			tokens = append(tokens, "#")
		}
		out = append(out, bus.T(tokens...))
	}
	return out
}

func stringsToTopic(parts []string) bus.Topic {
	tokens := make([]bus.Token, len(parts))
	for i, s := range parts {
		tokens[i] = s
	}
	return bus.T(tokens...)
}

func slicesEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func str(t bus.Topic, i int) string {
	s, _ := t.At(i).(string)
	return s
}
