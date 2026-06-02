package fabric

import "devicecode-go/bus"

// Topic remapping rules matching the shipped Lua fabric link contract.
//
// These rules are hardcoded and exact-match for v1. The Lua (CM5) side
// uses config-driven wildcard rules, but the MCU only needs a fixed set
// of routes. If new routes are required, add them here and on the Lua
// config side.
//
// The legacy MCU surface (config/device -> config/hal import, rpc/hal/dump
// inline handler, hal/cap/env/* and hal/cap/power/* exports, hal/state ->
// state/hal export, fabric/out/rpc/hal/dump call export) has been removed. The
// canonical surface is now:
//
// CM5 -> MCU wire call:
//   ["cap","self","updater","main","rpc","prepare-update"] -> rpc/updater/prepare
//   ["cap","self","updater","main","rpc","commit-update"]  -> rpc/updater/commit
//   xfer_begin target="updater/main" is handled by the transfer path
//   and routed to the local updater staging RPC after xfer_commit.
//
// MCU local bus publish -> wire:
//   state/self/#   -> state/self/...    (identity, telemetry, update facts)
//   event/self/#   -> event/self/...    (sparse charger alerts)

type importRule struct {
	wire  []string
	local []string
}

type busExportRule struct {
	localPrefix  []string
	remotePrefix []string
	suffix       bool
}

// importPublishRules is empty. Config-like data flows through the
// prepare-update metadata field instead of retained publishes.
var importPublishRules = []importRule{}

var (
	wireUpdaterPrepare = []string{"cap", "self", "updater", "main", "rpc", "prepare-update"}
	wireUpdaterCommit  = []string{"cap", "self", "updater", "main", "rpc", "commit-update"}
)

var criticalExportTopics = []bus.Topic{
	bus.T("state", "self", "software"),
	bus.T("state", "self", "updater"),
	bus.T("state", "self", "health"),
}

// cap/self/updater/main/rpc/{prepare-update,commit-update} land here from
// the wire and are routed to local rpc/updater/{prepare,commit} where the
// updater service binds. The updater package re-uses the same local topic
// strings (services/updater.TopicPrepareRPC / TopicCommitRPC) so callers
// stay consistent.
var importCallRules = []importRule{
	{
		wire:  wireUpdaterPrepare,
		local: []string{"rpc", "updater", "prepare"},
	},
	{
		wire:  wireUpdaterCommit,
		local: []string{"rpc", "updater", "commit"},
	},
}

// exportPublishRules is the minimal surface: local `state/self/*` retains and
// `event/self/*` events flow to the wire under the same name. Legacy HAL export
// topics are replaced by telemetry publishers under state/self/*.
var exportPublishRules = []busExportRule{
	{
		localPrefix:  []string{"state", "self"},
		remotePrefix: []string{"state", "self"},
		suffix:       true,
	},
	{
		localPrefix:  []string{"event", "self"},
		remotePrefix: []string{"event", "self"},
		suffix:       true,
	},
}

// exportCallRules is empty. The MCU does not originate outbound RPC calls for
// the current Fabric/update contract.
var exportCallRules = []busExportRule{}

func importPublishTopic(wire []string) bus.Topic {
	return importMatch(wire, importPublishRules)
}

func importCallTopic(wire []string) bus.Topic {
	return importMatch(wire, importCallRules)
}

func exportTopic(t bus.Topic) []string {
	return busExport(t, exportPublishRules)
}

func exportPatterns() []bus.Topic {
	return exportPatternsFor(exportPublishRules)
}

func isCriticalExportTopic(t bus.Topic) bool {
	if t == nil {
		return false
	}
	for _, want := range criticalExportTopics {
		if topicEquals(t, want) {
			return true
		}
	}
	return false
}

func exportCallTopic(t bus.Topic) []string {
	return busExport(t, exportCallRules)
}

func exportCallPatterns() []bus.Topic {
	return exportPatternsFor(exportCallRules)
}

func importMatch(wire []string, rules []importRule) bus.Topic {
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
