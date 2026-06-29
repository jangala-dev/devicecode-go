package serial_raw

import (
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/x/shmring"
)

type uartxProbe struct{}

func (p *uartxProbe) start(id string, port core.SerialPort, rxR, txR *shmring.Ring)          {}
func (p *uartxProbe) afterRX(id string, port core.SerialPort, rxR, txR *shmring.Ring, n int) {}
func (p *uartxProbe) afterTX(id string, port core.SerialPort, rxR, txR *shmring.Ring, n int) {}
func (p *uartxProbe) rxRingFull(id string, port core.SerialPort, rxR, txR *shmring.Ring)     {}
func (p *uartxProbe) periodic(id string, port core.SerialPort, rxR, txR *shmring.Ring)       {}
