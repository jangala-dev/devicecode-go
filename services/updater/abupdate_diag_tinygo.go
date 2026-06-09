//go:build tinygo && rp2350

package updater

import (
	"devicecode-go/services/otadiag"
	"pico2-a-b/abupdate"
	"pico2-a-b/imagev1"
)

var abupdateDiagXferID string
var abupdateDiagGeneration uint64

func installABUpdateDiagHook(xferID string, generation uint64) {
	abupdateDiagXferID = xferID
	abupdateDiagGeneration = generation
	abupdate.SetDiagnosticHook(func(event string, kv ...string) {
		emitRawDiag(event, kv...)
	})
	imagev1.SetDiagnosticHook(func(event string, kv ...string) {
		emitRawDiag(event, kv...)
	})
}

func emitRawDiag(event string, kv ...string) {
	var fields [10]otadiag.Field
	n := 0
	fields[n] = otadiag.KV("generation", abupdateDiagGeneration)
	n++
	for i := 0; i+1 < len(kv) && n < len(fields); i += 2 {
		fields[n] = otadiag.KV(kv[i], kv[i+1])
		n++
	}
	otadiag.Event("[updater-stream]", event, abupdateDiagXferID, fields[:n]...)
}

func clearABUpdateDiagHook() {
	abupdate.ClearDiagnosticHook()
	imagev1.ClearDiagnosticHook()
	abupdateDiagXferID = ""
	abupdateDiagGeneration = 0
}

func clearABUpdateDiagHookFor(xferID string, generation uint64) {
	if abupdateDiagXferID != xferID || abupdateDiagGeneration != generation {
		return
	}
	clearABUpdateDiagHook()
}

func emitABUpdateDiag(event string, fields ...otadiag.Field) {
	var out [10]otadiag.Field
	n := 0
	out[n] = otadiag.KV("generation", abupdateDiagGeneration)
	n++
	for _, f := range fields {
		if n >= len(out) {
			break
		}
		out[n] = f
		n++
	}
	otadiag.Event("[updater-stream]", event, abupdateDiagXferID, out[:n]...)
}
