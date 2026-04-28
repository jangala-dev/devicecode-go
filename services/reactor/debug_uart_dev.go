//go:build debug_uart && !qa_reactor

package reactor

import (
	"time"

	"devicecode-go/bus"
	"devicecode-go/types"
	"devicecode-go/x/shmring"
)

// debugUARTLog opens uart1 as a log mirror and routes log.Println output
// through it. Enabled with `-tags debug_uart`. The shmring write path
// inside utilities/Logger.logWrite is non-blocking and drops bytes on a
// full ring; that drop policy is the rate-limit for this debug stream.
//
// debug_uart MUST NOT be set in release builds — fabric on uart0
// (W7 acceptance) is the only allowed CM5-facing traffic, and any
// uart1 mirror is for development/bring-up only.
type debugUARTLog struct {
	subOpened *bus.Subscription
	subClosed *bus.Subscription
	retryAt   time.Time
}

const debugUARTLogID = "uart1"

func (d *debugUARTLog) init(uiConn *bus.Connection) {
	d.subOpened = uiConn.Subscribe(tSessOpened(debugUARTLogID))
	d.subClosed = uiConn.Subscribe(tSessClosed(debugUARTLogID))
	uiConn.Publish(uiConn.NewMessage(tSessOpen(debugUARTLogID), nil, false))
}

func (d *debugUARTLog) openedChan() <-chan *bus.Message {
	if d.subOpened == nil {
		return nil
	}
	return d.subOpened.Channel()
}

func (d *debugUARTLog) closedChan() <-chan *bus.Message {
	if d.subClosed == nil {
		return nil
	}
	return d.subClosed.Channel()
}

func (d *debugUARTLog) handleOpened(m *bus.Message) {
	if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
		log.SetUART1(shmring.Get(shmring.Handle(ev.TXHandle)))
		log.Println("[uart1] log session opened")
	}
}

func (d *debugUARTLog) handleClosed(uiConn *bus.Connection) {
	log.SetUART1(nil)
	log.Println("[uart1] log session closed")
	if time.Now().After(d.retryAt) {
		uiConn.Publish(uiConn.NewMessage(tSessOpen(debugUARTLogID), nil, false))
		d.retryAt = time.Now().Add(2 * time.Second)
	}
}
