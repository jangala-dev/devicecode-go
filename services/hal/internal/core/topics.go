package core

import "devicecode-go/bus"

// Opaque-topic helpers

func T(tokens ...bus.Token) bus.Topic { return bus.T(tokens...) }

func topicConfigHAL() bus.Topic { return T("config", "hal") }

// capability/<kind>/<id>/info|state|value|event
func capBase(kind string, id int) bus.Topic  { return T("hal", "capability", kind, id) }
func capInfo(kind string, id int) bus.Topic  { return capBase(kind, id).Append("info") }
func capState(kind string, id int) bus.Topic { return capBase(kind, id).Append("state") }
func capValue(kind string, id int) bus.Topic { return capBase(kind, id).Append("value") }

// capability/<kind>/<id>/control/<verb>
func capCtrl(kind string, id any, verb string) bus.Topic {
	return T("hal", "capability", kind, id, "control", verb)
}

// hal/capability/+/+/control/+ (use the default single-wildcard "+")
func ctrlWildcard() bus.Topic {
	return T("hal", "capability", "+", "+", "control", "+")
}
