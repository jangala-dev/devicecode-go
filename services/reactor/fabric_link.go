//go:build !qa_reactor

package reactor

import (
	"context"
	"time"

	"devicecode-go/services/fabric"
	"devicecode-go/types"
	"devicecode-go/x/shmring"
)

const (
	fabricUART            = "uart1"
	fabricLogPrefix       = "[" + fabricUART + "] "
	fabricStopWaitTimeout = 500 * time.Millisecond
)

// fabricBuffers is allocated once at package scope so the UART/Fabric hot path
// does not construct line or transfer-sized buffers on demand. It is shared by
// at most one active Fabric session; the Reactor tears any old session down
// before starting a replacement.
var fabricBuffers fabric.FabricBuffers

func waitFabricDone(done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (r *Reactor) startPassiveFabric(ctx context.Context, ev types.SerialSessionOpened) {
	if r == nil || r.uiConn == nil {
		return
	}
	// Only one Fabric session may own the UART rings. A fresh HAL session_opened
	// event replaces the previous session explicitly.
	r.stopFabricLink()

	rx := shmring.Get(shmring.Handle(ev.RXHandle))
	tx := shmring.Get(shmring.Handle(ev.TXHandle))
	if rx == nil || tx == nil {
		log.Println(fabricLogPrefix + "fabric session missing rings")
		return
	}

	tr := fabric.NewShmringTransportWithBuffers(rx, tx, &fabricBuffers)
	fabricConn := r.uiConn.NewChildConnection("fabric")
	if fabricConn == nil {
		_ = tr.Close()
		log.Println(fabricLogPrefix + "fabric session missing bus")
		return
	}

	stageController := r.fabricStageController()
	transferMode := fabricTransferMode()

	fabricCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.fabricCancel = cancel
	r.fabricDone = done
	r.fabricSessionOpen = true

	log.Println(fabricLogPrefix+"fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=", transferMode)
	go func() {
		defer close(done)
		defer tr.Close()
		// The transfer policy is selected by build tag. The default firmware build
		// uses a rejecting controller, so an unexpected xfer_begin cannot enter
		// flash staging. fabric_uart_hwtest/fabric_stage_enabled explicitly opt in
		// to the updater-owned stage controller.
		fabric.RunWithOptions(fabricCtx, tr, fabricConn, "mcu", "bigbox-cm5", fabric.DefaultLinkConfig(), fabric.RunOptions{Buffers: &fabricBuffers, StageController: stageController})
	}()
	log.Println(fabricLogPrefix+"fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0 transfer=", transferMode)
}

func (r *Reactor) stopFabricLink() {
	if r == nil || r.fabricCancel == nil {
		return
	}
	done := r.fabricDone
	r.fabricCancel()
	r.fabricCancel = nil
	r.fabricDone = nil
	r.fabricSessionOpen = false
	if !waitFabricDone(done, fabricStopWaitTimeout) {
		log.Println(fabricLogPrefix + "fabric session stop timed out")
	}
}
