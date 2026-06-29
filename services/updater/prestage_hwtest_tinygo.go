//go:build tinygo && rp2350 && fabric_uart_hwtest

package updater

import (
	"errors"

	"devicecode-go/x/xxhash"
)

// Hardware UART/Fabric interconnection tests must exercise the Fabric receiver
// without writing the Pico2's inactive A/B slot.  Under fabric_uart_hwtest the
// streamed stage is therefore a fixed-state digest/count sink.  The production
// rp2350 prestage path remains in prestage_tinygo.go and is excluded by the
// build tag above.
var hwtestStreamedStage struct {
	active     bool
	ready      bool
	xferID     string
	generation uint64
	declared   uint32
	written    uint32
	hasher     xxhash.Hasher
	desc       streamedStage
}

func startStreamedStage(xferID string, generation uint64, size uint32) error {
	hwtestStreamedStage.active = true
	hwtestStreamedStage.ready = false
	hwtestStreamedStage.xferID = xferID
	hwtestStreamedStage.generation = generation
	hwtestStreamedStage.declared = size
	hwtestStreamedStage.written = 0
	hwtestStreamedStage.hasher = *xxhash.New(0)
	hwtestStreamedStage.desc = streamedStage{}
	return nil
}

func writeStreamedStage(xferID string, generation uint64, data []byte) error {
	if !hwtestStreamedStage.active {
		return errors.New("streamed_stage_not_started")
	}
	if hwtestStreamedStage.xferID != xferID || hwtestStreamedStage.generation != generation {
		return errors.New("streamed_stage_generation_mismatch")
	}
	if len(data) == 0 {
		return errors.New("empty_chunk")
	}
	hwtestStreamedStage.written += uint32(len(data))
	_, _ = hwtestStreamedStage.hasher.Write(data)
	return nil
}

func commitStreamedStage(svc *Service, xferID string, generation uint64) (streamedStage, error) {
	_ = svc
	if !hwtestStreamedStage.active {
		return streamedStage{}, errors.New("streamed_stage_not_started")
	}
	if hwtestStreamedStage.xferID != xferID || hwtestStreamedStage.generation != generation {
		return streamedStage{}, errors.New("streamed_stage_generation_mismatch")
	}
	if hwtestStreamedStage.written != hwtestStreamedStage.declared {
		hwtestStreamedStage.active = false
		return streamedStage{}, errors.New("streamed_stage_size_mismatch")
	}
	desc := streamedStage{
		Version:       "uart-crosswire-test",
		BuildID:       "fabric-uart-crosswire",
		ImageID:       "hwtest-image",
		Length:        hwtestStreamedStage.written,
		PayloadSHA256: "xxhash32:" + hwtestXXHashHex(hwtestStreamedStage.hasher.Sum32()),
	}
	hwtestStreamedStage.desc = desc
	hwtestStreamedStage.ready = true
	hwtestStreamedStage.active = false
	return desc, nil
}

func abortStreamedStage() {
	hwtestStreamedStage.active = false
	hwtestStreamedStage.ready = false
	hwtestStreamedStage.desc = streamedStage{}
}

func consumeStreamedStageResult() (streamedStage, bool) {
	if !hwtestStreamedStage.ready {
		return streamedStage{}, false
	}
	out := hwtestStreamedStage.desc
	hwtestStreamedStage.ready = false
	hwtestStreamedStage.desc = streamedStage{}
	return out, true
}

func discardStreamedStageResult() {
	abortStreamedStage()
	clearABUpdateDiagHook()
}

func hwtestXXHashHex(v uint32) string {
	const digits = "0123456789abcdef"
	var buf [8]byte
	for i := 7; i >= 0; i-- {
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}
