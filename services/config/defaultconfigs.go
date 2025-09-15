package config

// -----------------------------------------------------------------------------
// Embedded configuration
//
// Populate embeddedConfigs at build time (e.g. via code generation) or
// manually during development.
// Key: device ID (same value placed in ctx under ctxDeviceKey)
// Val: raw JSON bytes for that device
// -----------------------------------------------------------------------------

const cfgPico = `{
  "power": [
  ],
  "bridge": [
  ],
  "hal": [
  ],
  "heartbeat": {
      "interval": 2
  }
}`

var embeddedConfigs = map[string][]byte{
	"pico": []byte(cfgPico),
}
