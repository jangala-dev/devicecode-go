package ltc4015

// Snapshot collects commonly used telemetry and status.
// Zero values remain where individual reads fail.
type Snapshot struct {
	Vin_mV, Vsys_mV     int32
	IBat_mA, IIn_mA     int32
	Pack_mV, PerCell_mV int32
	Die_mC              int32
	BSR_uOhmPerCell     uint32
	State               ChargerState
	Status              ChargeStatus
	System              SystemStatus
	NTCRatio            uint16
}

func (d *Device) Snapshot() Snapshot {
	var s Snapshot
	d.SnapshotInto(&s)
	return s
}

func (d *Device) SnapshotInto(out *Snapshot) {
	var s Snapshot
	if v, e := d.Vin_mV(); e == nil {
		s.Vin_mV = v
	}
	if v, e := d.Vsys_mV(); e == nil {
		s.Vsys_mV = v
	}
	if v, e := d.Battery_mVPack(); e == nil {
		s.Pack_mV = v
	}
	if v, e := d.Battery_mVPerCell(); e == nil {
		s.PerCell_mV = v
	}
	if v, e := d.Ibat_mA(); e == nil {
		s.IBat_mA = v
	}
	if v, e := d.Iin_mA(); e == nil {
		s.IIn_mA = v
	}
	if v, e := d.Die_mC(); e == nil {
		s.Die_mC = v
	}
	if v, e := d.BSR_uOhmPerCell(); e == nil {
		s.BSR_uOhmPerCell = v
	}
	if v, e := d.ChargerState(); e == nil {
		s.State = v
	}
	if v, e := d.ChargeStatus(); e == nil {
		s.Status = v
	}
	if v, e := d.SystemStatus(); e == nil {
		s.System = v
	}
	if v, e := d.NTCRatio(); e == nil {
		s.NTCRatio = v
	}
	*out = s
}
