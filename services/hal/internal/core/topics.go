package core

import "devicecode-go/bus"

// Opaque-topic helpers

func T(tokens ...bus.Token) bus.Topic { return bus.T(tokens...) }

func topicConfigHAL() bus.Topic { return T("config", "hal") }

// hal/cap/<domain>/<kind>/<name>/...
func capBase(domain, kind, name string) bus.Topic { return T("hal", "cap", domain, kind, name) }

func capInfo(domain, kind, name string) bus.Topic { return capBase(domain, kind, name).Append("info") }
func capStatus(domain, kind, name string) bus.Topic {
	return capBase(domain, kind, name).Append("status")
}
func capValue(domain, kind, name string) bus.Topic {
	return capBase(domain, kind, name).Append("value")
}
func capEvent(domain, kind, name string) bus.Topic {
	return capBase(domain, kind, name).Append("event")
}
func capEventTagged(domain, kind, name, tag string) bus.Topic {
	return capEvent(domain, kind, name).Append(tag)
}

// capability control
// hal/cap/<domain>/<kind>/<name>/control/<verb>
func capCtrl(domain, kind, name, verb string) bus.Topic {
	return capBase(domain, kind, name).Append("control", verb)
}

// hal/cap/+/+/+/control/+
func ctrlWildcard() bus.Topic {
	return T("hal", "cap", "+", "+", "+", "control", "+")
}
