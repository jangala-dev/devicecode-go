package telemetry

import (
	"encoding/json"
	"testing"
)

func requireValidJSON(t *testing.T, name string, b []byte) {
	t.Helper()
	if !json.Valid(b) {
		t.Fatalf("%s produced invalid JSON: %s", name, string(b))
	}
}

func TestAppendJSONPayloadsAreValid(t *testing.T) {
	requireValidJSON(t, "BatteryFact", BatteryFact{
		Presence:          "present",
		MeasurementsValid: true,
		PackMV:            13182,
		PerCellMV:         2197,
		IBatMA:            15,
		TempMC:            40745,
		BSRUOhmPerCell:    0,
		Seq:               5,
		UptimeMs:          97784,
	}.AppendJSON(nil))
	requireValidJSON(t, "ChargerFact", ChargerFact{VinMV: 24171, VsysMV: 24082, IinMA: 685, StateBits: 64, StatusBits: 1, SystemBits: 10951, Seq: 11, UptimeMs: 96342}.AppendJSON(nil))
	requireValidJSON(t, "ChargerConfigFact", ChargerConfigFact{Schema: "charger-config/1", Source: "ltc4015-programmed", Thresholds: ChargerThresholds{VinLoMV: 9000, VinHiMV: 11000, BSRHighUohmPerCell: 100000}, AlertMaskBits: 4, Seq: 3, UptimeMs: 1120}.AppendJSON(nil))
	requireValidJSON(t, "EnvTempFact", EnvTempFact{DeciC: 270, Seq: 3, UptimeMs: 122321}.AppendJSON(nil))
	requireValidJSON(t, "EnvHumFact", EnvHumFact{RHx100: 4540, Seq: 3, UptimeMs: 122321}.AppendJSON(nil))
	requireValidJSON(t, "RuntimeMemFact", RuntimeMemFact{AllocBytes: 327088, Seq: 4, UptimeMs: 120006}.AppendJSON(nil))
	requireValidJSON(t, "AlertEvent", AlertEvent{Kind: AlertBsrHigh, Severity: "warning", Source: "ltc4015", StateBits: 64, StatusBits: 1, SystemBits: 10951, Seq: 9, UptimeMs: 124035}.AppendJSON(nil))
}
