package ltc4015

// AlertEvent summarises latched alert sources read from the device.
// A zero value means "no alerts latched" for that group.
type AlertEvent struct {
	Limit     LimitAlerts        // 0x36: LIMIT_ALERTS
	ChgState  ChargerStateAlerts // 0x37: CHARGER_STATE_ALERTS
	ChgStatus ChargeStatusAlerts // 0x38: CHARGE_STATUS_ALERTS
}

// Empty reports whether no alerts were latched in any group.
func (e AlertEvent) Empty() bool {
	return e.Limit == 0 && e.ChgState == 0 && e.ChgStatus == 0
}

// ServiceSMBAlert performs the SMBus ARA handshake and drains/clears alert
// registers. It must be called from non-ISR context (it performs I2C I/O).
//
// Returns (event, true, nil) if the LTC4015 identified itself on ARA.
// Returns (_, false, nil) if some other device asserted SMBALERT.
// Returns (_, _, err) on bus/IO errors.
//
// Clears are best-effort: failure to clear does not prevent returning the event.
// Callers may choose to retry on a clear error.
func (d *Device) ServiceSMBAlert() (AlertEvent, bool, error) {
	ok, err := d.AcknowledgeAlert() // SMBus ARA (0x19 but in 7-bit practice 0x0C)
	if err != nil || !ok {
		return AlertEvent{}, false, err
	}

	ev, err := d.DrainAlerts()
	return ev, true, err
}

// drainAlerts reads the three alert groups and then issues clear writes.
// It is used by ServiceSMBAlert but may also be called directly by polling code.
func (d *Device) DrainAlerts() (AlertEvent, error) {
	var ev AlertEvent

	lim, err := d.ReadLimitAlerts()
	if err != nil {
		return AlertEvent{}, err
	}
	csa, err := d.ReadChargerStateAlerts()
	if err != nil {
		return AlertEvent{}, err
	}
	css, err := d.ReadChargeStatusAlerts()
	if err != nil {
		return AlertEvent{}, err
	}

	ev.Limit = lim
	ev.ChgState = csa
	ev.ChgStatus = css

	// Best-effort clears; do not mask the event on clear failures.
	var clearErr error
	if err := d.ClearLimitAlerts(); err != nil {
		clearErr = err
	}
	if err := d.ClearChargerStateAlerts(); err != nil && clearErr == nil {
		clearErr = err
	}
	if err := d.ClearChargeStatusAlerts(); err != nil && clearErr == nil {
		clearErr = err
	}
	return ev, clearErr
}
