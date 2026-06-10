//go:build !fabric_xfer_probe

package fabric

const xferProbeEnabled = false

func xferProbe(args ...any) {}
