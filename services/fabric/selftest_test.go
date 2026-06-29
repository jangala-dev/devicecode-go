package fabric

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/updater"
)

type selfTestAcceptVerifier struct{}

func (selfTestAcceptVerifier) Verify(r io.Reader, sink updater.SlotSink) (updater.Manifest, error) {
	n, err := io.Copy(sink, r)
	if err != nil {
		_ = sink.Abort()
		return updater.Manifest{}, err
	}
	if err := sink.Commit(); err != nil {
		return updater.Manifest{}, err
	}
	return updater.Manifest{Version: "selftest", BuildID: "host", ImageID: "hwtest-image", PayloadSHA256: strings.Repeat("a", 64), PayloadLength: uint32(n)}, nil
}

func TestRunUARTSelfTest(t *testing.T) {
	b := bus.NewBus(8, "+", "#")
	conn := b.NewConnection("fabric-selftest")
	updaterConn := b.NewConnection("updater")
	mem := updater.NewMemoryMetadata()
	svc := updater.New(updater.Options{
		Conn:          updaterConn,
		Verifier:      selfTestAcceptVerifier{},
		Metadata:      mem,
		MetadataWrite: mem,
		Identity:      updater.Identity{Version: "test", Build: "build", ImageID: "old-image"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)
	res, err := RunUARTSelfTest(ctx, UARTSelfTestOptions{Conn: conn, StageController: svc, PayloadSize: 512, ChunkSize: 128, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("RunUARTSelfTest: %v", err)
	}
	if !res.OK() || res.PayloadSize != 512 || res.ChunkSize != 128 {
		t.Fatalf("bad result: %+v", res)
	}
}
