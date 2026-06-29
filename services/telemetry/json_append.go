package telemetry

import "strconv"

// AppendJSON methods let the Fabric exporter write common retained/event
// telemetry payloads directly into the outbound frame. MarshalJSON remains for
// ordinary encoding/json callers, but the hot path avoids the extra payload
// allocation and reflection pass.

func (f BatteryFact) AppendJSON(buf []byte) []byte {
	buf = append(buf, '{')
	buf = append(buf, `"presence":`...)
	buf = strconv.AppendQuote(buf, f.Presence)
	buf = append(buf, `,"measurements_valid":`...)
	buf = strconv.AppendBool(buf, f.MeasurementsValid)
	if f.Reason != "" {
		buf = append(buf, `,"reason":`...)
		buf = strconv.AppendQuote(buf, f.Reason)
	}
	if f.MeasurementsValid {
		buf = append(buf, `,"pack_mV":`...)
		buf = strconv.AppendInt(buf, int64(f.PackMV), 10)
		buf = append(buf, `,"per_cell_mV":`...)
		buf = strconv.AppendInt(buf, int64(f.PerCellMV), 10)
		buf = append(buf, `,"ibat_mA":`...)
		buf = strconv.AppendInt(buf, int64(f.IBatMA), 10)
		buf = append(buf, `,"temp_mC":`...)
		buf = strconv.AppendInt(buf, int64(f.TempMC), 10)
		buf = append(buf, `,"bsr_uohm_per_cell":`...)
		buf = strconv.AppendUint(buf, uint64(f.BSRUOhmPerCell), 10)
	}
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, uint64(f.Seq), 10)
	buf = append(buf, `,"uptime_ms":`...)
	buf = strconv.AppendInt(buf, f.UptimeMs, 10)
	buf = append(buf, '}')
	return buf
}

func (f ChargerFact) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"vin_mV":`...)
	buf = strconv.AppendInt(buf, int64(f.VinMV), 10)
	buf = append(buf, `,"vsys_mV":`...)
	buf = strconv.AppendInt(buf, int64(f.VsysMV), 10)
	buf = append(buf, `,"iin_mA":`...)
	buf = strconv.AppendInt(buf, int64(f.IinMA), 10)
	buf = append(buf, `,"state_bits":`...)
	buf = strconv.AppendUint(buf, uint64(f.StateBits), 10)
	buf = append(buf, `,"status_bits":`...)
	buf = strconv.AppendUint(buf, uint64(f.StatusBits), 10)
	buf = append(buf, `,"system_bits":`...)
	buf = strconv.AppendUint(buf, uint64(f.SystemBits), 10)
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, uint64(f.Seq), 10)
	buf = append(buf, `,"uptime_ms":`...)
	buf = strconv.AppendInt(buf, f.UptimeMs, 10)
	buf = append(buf, '}')
	return buf
}

func (f ChargerFact) MarshalJSON() ([]byte, error) {
	return f.AppendJSON(make([]byte, 0, 160)), nil
}

func (f ChargerConfigFact) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"schema":`...)
	buf = strconv.AppendQuote(buf, f.Schema)
	buf = append(buf, `,"source":`...)
	buf = strconv.AppendQuote(buf, f.Source)
	buf = append(buf, `,"thresholds":{"vin_lo_mV":`...)
	buf = strconv.AppendInt(buf, int64(f.Thresholds.VinLoMV), 10)
	buf = append(buf, `,"vin_hi_mV":`...)
	buf = strconv.AppendInt(buf, int64(f.Thresholds.VinHiMV), 10)
	buf = append(buf, `,"bsr_high_uohm_per_cell":`...)
	buf = strconv.AppendUint(buf, uint64(f.Thresholds.BSRHighUohmPerCell), 10)
	buf = append(buf, `},"alert_mask_bits":`...)
	buf = strconv.AppendUint(buf, uint64(f.AlertMaskBits), 10)
	buf = append(buf, `,"alert_mask":`...)
	buf = appendChargerAlertMaskJSON(buf, f.AlertMask)
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, uint64(f.Seq), 10)
	buf = append(buf, `,"uptime_ms":`...)
	buf = strconv.AppendInt(buf, f.UptimeMs, 10)
	buf = append(buf, '}')
	return buf
}

func (f ChargerConfigFact) MarshalJSON() ([]byte, error) {
	return f.AppendJSON(make([]byte, 0, 512)), nil
}

func appendChargerAlertMaskJSON(buf []byte, m ChargerAlertMask) []byte {
	buf = append(buf, `{"vin_lo":`...)
	buf = strconv.AppendBool(buf, m.VinLo)
	buf = append(buf, `,"vin_hi":`...)
	buf = strconv.AppendBool(buf, m.VinHi)
	buf = append(buf, `,"bsr_high":`...)
	buf = strconv.AppendBool(buf, m.BSRHigh)
	buf = append(buf, `,"bat_missing":`...)
	buf = strconv.AppendBool(buf, m.BatMissing)
	buf = append(buf, `,"bat_short":`...)
	buf = strconv.AppendBool(buf, m.BatShort)
	buf = append(buf, `,"max_charge_time_fault":`...)
	buf = strconv.AppendBool(buf, m.MaxChargeTimeFault)
	buf = append(buf, `,"absorb":`...)
	buf = strconv.AppendBool(buf, m.Absorb)
	buf = append(buf, `,"equalize":`...)
	buf = strconv.AppendBool(buf, m.Equalize)
	buf = append(buf, `,"cccv":`...)
	buf = strconv.AppendBool(buf, m.CCCV)
	buf = append(buf, `,"precharge":`...)
	buf = strconv.AppendBool(buf, m.Precharge)
	buf = append(buf, `,"iin_limited":`...)
	buf = strconv.AppendBool(buf, m.IinLimited)
	buf = append(buf, `,"uvcl_active":`...)
	buf = strconv.AppendBool(buf, m.UvclActive)
	buf = append(buf, `,"cc_phase":`...)
	buf = strconv.AppendBool(buf, m.CcPhase)
	buf = append(buf, `,"cv_phase":`...)
	buf = strconv.AppendBool(buf, m.CvPhase)
	buf = append(buf, '}')
	return buf
}

func (f EnvTempFact) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"deci_c":`...)
	buf = strconv.AppendInt(buf, int64(f.DeciC), 10)
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, uint64(f.Seq), 10)
	buf = append(buf, `,"uptime_ms":`...)
	buf = strconv.AppendInt(buf, f.UptimeMs, 10)
	buf = append(buf, '}')
	return buf
}

func (f EnvTempFact) MarshalJSON() ([]byte, error) {
	return f.AppendJSON(make([]byte, 0, 64)), nil
}

func (f EnvHumFact) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"rh_x100":`...)
	buf = strconv.AppendInt(buf, int64(f.RHx100), 10)
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, uint64(f.Seq), 10)
	buf = append(buf, `,"uptime_ms":`...)
	buf = strconv.AppendInt(buf, f.UptimeMs, 10)
	buf = append(buf, '}')
	return buf
}

func (f EnvHumFact) MarshalJSON() ([]byte, error) {
	return f.AppendJSON(make([]byte, 0, 64)), nil
}

func (f RuntimeMemFact) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"alloc_bytes":`...)
	buf = strconv.AppendUint(buf, f.AllocBytes, 10)
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, uint64(f.Seq), 10)
	buf = append(buf, `,"uptime_ms":`...)
	buf = strconv.AppendInt(buf, f.UptimeMs, 10)
	buf = append(buf, '}')
	return buf
}

func (f RuntimeMemFact) MarshalJSON() ([]byte, error) {
	return f.AppendJSON(make([]byte, 0, 80)), nil
}

func (e AlertEvent) AppendJSON(buf []byte) []byte {
	buf = append(buf, `{"kind":`...)
	buf = strconv.AppendQuote(buf, string(e.Kind))
	buf = append(buf, `,"severity":`...)
	buf = strconv.AppendQuote(buf, e.Severity)
	buf = append(buf, `,"source":`...)
	buf = strconv.AppendQuote(buf, e.Source)
	buf = append(buf, `,"state_bits":`...)
	buf = strconv.AppendUint(buf, uint64(e.StateBits), 10)
	buf = append(buf, `,"status_bits":`...)
	buf = strconv.AppendUint(buf, uint64(e.StatusBits), 10)
	buf = append(buf, `,"system_bits":`...)
	buf = strconv.AppendUint(buf, uint64(e.SystemBits), 10)
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, uint64(e.Seq), 10)
	buf = append(buf, `,"uptime_ms":`...)
	buf = strconv.AppendInt(buf, e.UptimeMs, 10)
	buf = append(buf, '}')
	return buf
}

func (e AlertEvent) MarshalJSON() ([]byte, error) {
	return e.AppendJSON(make([]byte, 0, 160)), nil
}
