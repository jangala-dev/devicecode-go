package telemetry

import (
	"time"

	"devicecode-go/types"
)

// Policy controls telemetry-side de-chatter for retained state facts.
//
// HAL remains responsible for sampling and safety cadence. Telemetry is
// responsible for deciding whether a new HAL observation is meaningful
// enough to republish onto the canonical state/self/* surface, which is
// then exported by Fabric.
//
// Thresholds are intentionally file-local constants for now. Once the
// runtime has a central config service, these values can move into
// retained config without changing the HAL boundary.
type Policy struct {
	// KeepaliveInterval republishes a retained fact after this interval
	// even when values have not changed materially. This preserves a
	// liveness/stall signal for CM5 importers without emitting every HAL
	// sample.
	KeepaliveInterval time.Duration

	BatteryPackMinDeltaMV         uint32
	BatteryPackMinDeltaPct        uint32
	BatteryPerCellMinDeltaMV      uint32
	BatteryPerCellMinDeltaPct     uint32
	BatteryIBatMinDeltaMA         uint32
	BatteryIBatMinDeltaPct        uint32
	BatteryTempMinDeltaMC         uint32
	BatteryTempMinDeltaPct        uint32
	BatteryBSRMinDeltaUOhmPerCell uint32
	BatteryBSRMinDeltaPct         uint32

	ChargerVinMinDeltaMV   uint32
	ChargerVinMinDeltaPct  uint32
	ChargerVsysMinDeltaMV  uint32
	ChargerVsysMinDeltaPct uint32
	ChargerIinMinDeltaMA   uint32
	ChargerIinMinDeltaPct  uint32

	EnvTempMinDeltaDeciC uint32
	EnvTempMinDeltaPct   uint32
	EnvHumMinDeltaRHx100 uint32
	EnvHumMinDeltaPct    uint32
}

// DefaultPolicy is deliberately conservative: first observation always
// publishes, bitfield changes always publish, analogue drift is suppressed
// unless it crosses a modest absolute or percentage threshold, and every
// retained fact is refreshed periodically.
var DefaultPolicy = Policy{
	KeepaliveInterval: 60 * time.Second,

	BatteryPackMinDeltaMV:         100,
	BatteryPackMinDeltaPct:        1,
	BatteryPerCellMinDeltaMV:      25,
	BatteryPerCellMinDeltaPct:     1,
	BatteryIBatMinDeltaMA:         100,
	BatteryIBatMinDeltaPct:        0,  // absolute threshold only; near-zero current jitters by 1mA
	BatteryTempMinDeltaMC:         500,
	BatteryTempMinDeltaPct:        2,
	BatteryBSRMinDeltaUOhmPerCell: 500,
	BatteryBSRMinDeltaPct:         10,

	ChargerVinMinDeltaMV:   100,
	ChargerVinMinDeltaPct:  1,
	ChargerVsysMinDeltaMV:  100,
	ChargerVsysMinDeltaPct: 1,
	ChargerIinMinDeltaMA:   100,
	ChargerIinMinDeltaPct:  0, // absolute threshold only; avoid near-zero percentage chatter

	EnvTempMinDeltaDeciC: 5,   // 0.5 C
	EnvTempMinDeltaPct:   0,   // absolute threshold is clearer near ambient
	EnvHumMinDeltaRHx100: 100, // 1.00 %RH
	EnvHumMinDeltaPct:    0,
}

func (p Policy) withDefaults() Policy {
	if p.KeepaliveInterval <= 0 {
		p.KeepaliveInterval = DefaultPolicy.KeepaliveInterval
	}
	return p
}

type publishGate struct {
	seen bool
	last time.Time
}

func (g *publishGate) shouldPublish(now time.Time, keepalive time.Duration) bool {
	if !g.seen {
		return true
	}
	return keepalive > 0 && now.Sub(g.last) >= keepalive
}

func (g *publishGate) markPublished(now time.Time) {
	g.seen = true
	g.last = now
}

func batteryMeaningful(curr, prev types.BatteryValue, p Policy) bool {
	return meaningfulDeltaI32(curr.PackMilliV, prev.PackMilliV, p.BatteryPackMinDeltaMV, p.BatteryPackMinDeltaPct) ||
		meaningfulDeltaI32(curr.PerCellMilliV, prev.PerCellMilliV, p.BatteryPerCellMinDeltaMV, p.BatteryPerCellMinDeltaPct) ||
		meaningfulDeltaI32(curr.IBatMilliA, prev.IBatMilliA, p.BatteryIBatMinDeltaMA, p.BatteryIBatMinDeltaPct) ||
		meaningfulDeltaI32(curr.TempMilliC, prev.TempMilliC, p.BatteryTempMinDeltaMC, p.BatteryTempMinDeltaPct) ||
		meaningfulDeltaU32(curr.BSR_uOhmPerCell, prev.BSR_uOhmPerCell, p.BatteryBSRMinDeltaUOhmPerCell, p.BatteryBSRMinDeltaPct)
}

func chargerMeaningful(curr, prev types.ChargerValue, p Policy) bool {
	if curr.State != prev.State || curr.Status != prev.Status || curr.Sys != prev.Sys {
		return true
	}
	return meaningfulDeltaI32(curr.VIN_mV, prev.VIN_mV, p.ChargerVinMinDeltaMV, p.ChargerVinMinDeltaPct) ||
		meaningfulDeltaI32(curr.VSYS_mV, prev.VSYS_mV, p.ChargerVsysMinDeltaMV, p.ChargerVsysMinDeltaPct) ||
		meaningfulDeltaI32(curr.IIn_mA, prev.IIn_mA, p.ChargerIinMinDeltaMA, p.ChargerIinMinDeltaPct)
}

func meaningfulDeltaI32(curr, prev int32, minAbs uint32, minPct uint32) bool {
	diff := absDiffI32(curr, prev)
	if minAbs > 0 && diff >= minAbs {
		return true
	}
	if minPct == 0 {
		return false
	}
	base := absI32(prev)
	if base == 0 {
		return false
	}
	return diff*100 >= base*minPct
}

func meaningfulDeltaU32(curr, prev uint32, minAbs uint32, minPct uint32) bool {
	diff := absDiffU32(curr, prev)
	if minAbs > 0 && diff >= minAbs {
		return true
	}
	if minPct == 0 || prev == 0 {
		return false
	}
	return diff*100 >= prev*minPct
}

func absDiffI32(a, b int32) uint32 {
	diff := int64(a) - int64(b)
	if diff < 0 {
		diff = -diff
	}
	return uint32(diff)
}

func absI32(v int32) uint32 {
	if v < 0 {
		return uint32(-int64(v))
	}
	return uint32(v)
}

func absDiffU32(a, b uint32) uint32 {
	if a >= b {
		return a - b
	}
	return b - a
}
