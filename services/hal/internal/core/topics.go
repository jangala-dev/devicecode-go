package core

import (
	"devicecode-go/bus"
	"devicecode-go/types"
)

// Opaque-topic helpers

func T(tokens ...bus.Token) bus.Topic { return bus.T(tokens...) }

func topicConfigHAL() bus.Topic { return T("config", "hal") }

// hal/cap/<domain>/<kind>/<name>/...
func capBase(domain string, kind types.Kind, name string) bus.Topic {
	return T("hal", "cap", domain, string(kind), name)
}

func capInfo(domain string, kind types.Kind, name string) bus.Topic {
	return capBase(domain, kind, name).Append("info")
}
func capStatus(domain string, kind types.Kind, name string) bus.Topic {
	return capBase(domain, kind, name).Append("status")
}
func capValue(domain string, kind types.Kind, name string) bus.Topic {
	return capBase(domain, kind, name).Append("value")
}
func capEvent(domain string, kind types.Kind, name string) bus.Topic {
	return capBase(domain, kind, name).Append("event")
}
func capEventTagged(domain string, kind types.Kind, name, tag string) bus.Topic {
	return capEvent(domain, kind, name).Append(tag)
}

// capability control
// hal/cap/<domain>/<kind>/<name>/control/<verb>
func parseCapCtrl(t bus.Topic) (CapAddr, string, bool) {
	if t.Len() < 7 {
		return CapAddr{}, "", false
	}
	d, ok1 := t.At(2).(string)
	k, ok2 := t.At(3).(string)
	n, ok3 := t.At(4).(string)
	v, ok4 := t.At(6).(string)
	if !(ok1 && ok2 && ok3 && ok4) {
		return CapAddr{}, "", false
	}
	return CapAddr{Domain: d, Kind: types.Kind(k), Name: n}, v, true
}

// hal/cap/+/+/+/control/+
func ctrlWildcard() bus.Topic {
	return T("hal", "cap", "+", "+", "+", "control", "+")
}
