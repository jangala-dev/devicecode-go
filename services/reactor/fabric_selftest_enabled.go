//go:build !qa_reactor && fabric_uart_selftest && fabric_uart_hwtest

package reactor

import (
	"context"
	"time"

	"devicecode-go/services/fabric"
)

func (r *Reactor) addFabricSelfTestChild() {
	if r == nil || r.uiConn == nil || r.updaterSvc == nil {
		return
	}
	conn := r.uiConn.NewChildConnection("fabric-selftest")
	if conn == nil {
		return
	}
	r.children.Add("fabric-selftest", func(ctx context.Context) {
		log.Println("[fabric-selftest] starting in-process UART cross-wire transfer")
		res, err := fabric.RunUARTSelfTest(ctx, fabric.UARTSelfTestOptions{
			Conn:            conn,
			StageController: r.updaterSvc,
			PayloadSize:     1024,
			ChunkSize:       256,
			Timeout:         10 * time.Second,
		})
		if err != nil {
			log.Println("[fabric-selftest] failed err=", err.Error())
		} else {
			log.Println("[fabric-selftest] ok xfer=", res.XferID, " bytes=", int(res.PayloadSize), " chunk=", int(res.ChunkSize), " digest=", res.Digest)
		}
		<-ctx.Done()
	})
}
