//go:build uartx_probe

package serial_raw

import (
	"time"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/x/shmring"
)

type uartxProbe struct {
	armed        bool
	nextPeriodic time.Time
	nextLoss     time.Time
	last         core.SerialDebugStats
	lastLoss     uint32
}

func (p *uartxProbe) start(id string, port core.SerialPort, rxR, txR *shmring.Ring) {
	p.print("start", id, port, rxR, txR)
	p.nextPeriodic = time.Now().Add(2 * time.Second)
	p.armed = true
}

func (p *uartxProbe) afterRX(id string, port core.SerialPort, rxR, txR *shmring.Ring, n int) {
	if n <= 0 {
		return
	}
	p.printIfLossChanged("rx", id, port, rxR, txR)
}

func (p *uartxProbe) afterTX(id string, port core.SerialPort, rxR, txR *shmring.Ring, n int) {
	if n <= 0 {
		return
	}
	p.printIfLossChanged("tx", id, port, rxR, txR)
}

func (p *uartxProbe) rxRingFull(id string, port core.SerialPort, rxR, txR *shmring.Ring) {
	// This is the HAL session ring, not the UARTX ISR ring. It matters because
	// if it stays full, the session worker cannot drain the UARTX RX ring.
	if rxR.Space() == 0 {
		p.print("session_rx_ring_full", id, port, rxR, txR)
	}
}

func (p *uartxProbe) periodic(id string, port core.SerialPort, rxR, txR *shmring.Ring) {
	now := time.Now()
	if !p.armed || now.After(p.nextPeriodic) {
		p.print("periodic", id, port, rxR, txR)
		p.nextPeriodic = now.Add(2 * time.Second)
		p.armed = true
	}
}

func (p *uartxProbe) printIfLossChanged(reason, id string, port core.SerialPort, rxR, txR *shmring.Ring) {
	s, ok := debugStats(port)
	if !ok {
		return
	}
	loss := totalLoss(s)
	// Keep the probe diagnostic but not self-defeating. Printing every dropped
	// byte can itself stall the serial pump and create more drops. Emit promptly
	// for the first loss edge, then coalesce further loss changes by count or
	// time; max-occupancy changes are still visible in periodic snapshots.
	now := time.Now()
	if loss != p.lastLoss && (p.lastLoss == 0 || loss-p.lastLoss >= 128 || now.After(p.nextLoss)) {
		p.print(reason, id, port, rxR, txR)
		p.nextLoss = now.Add(500 * time.Millisecond)
	}
}

func totalLoss(s core.SerialDebugStats) uint32 {
	return s.RXRingDrops + s.RXOverrun + s.RXBreak + s.RXParity + s.RXFraming
}

func debugStats(port core.SerialPort) (core.SerialDebugStats, bool) {
	d, ok := port.(core.SerialDiagnostics)
	if !ok {
		return core.SerialDebugStats{}, false
	}
	return d.DebugStats(), true
}

func (p *uartxProbe) print(reason, id string, port core.SerialPort, rxR, txR *shmring.Ring) {
	s, ok := debugStats(port)
	if !ok {
		return
	}
	loss := totalLoss(s)
	println("[uartx-probe]", id,
		"reason", reason,
		"rx_hw", s.RXHWBytes,
		"rx_enq", s.RXEnqueued,
		"rx_read", s.RXReadBytes,
		"rx_drop", s.RXRingDrops,
		"rx_oe", s.RXOverrun,
		"rx_fe", s.RXFraming,
		"rx_pe", s.RXParity,
		"rx_be", s.RXBreak,
		"rx_max", s.RXRingMax,
		"rx_notify_drop", s.RXNotifyDrop,
		"tx_acc", s.TXAccepted,
		"tx_hw", s.TXHWBytes,
		"tx_full", s.TXRingFull,
		"tx_max", s.TXRingMax,
		"tx_notify_drop", s.TXNotifyDrop,
		"sess_rx_avail", rxR.Available(),
		"sess_rx_space", rxR.Space(),
		"sess_tx_avail", txR.Available(),
		"sess_tx_space", txR.Space(),
		"loss", loss,
	)
	p.last = s
	p.lastLoss = loss
}
