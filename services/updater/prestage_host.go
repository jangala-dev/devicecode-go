//go:build !tinygo || !rp2350

package updater

import (
	"errors"
	"io"
	"os"
)

type streamedStage struct {
	Version       string
	BuildID       string
	ImageID       string
	Length        uint32
	PayloadSHA256 string
}

var hostStreamedStage struct {
	file  *os.File
	path  string
	desc  streamedStage
	ready bool
}

func startStreamedStage(xferID string, generation uint64, size uint32) error {
	_, _, _ = xferID, generation, size
	abortStreamedStage()
	f, err := os.CreateTemp("", "dcgo-streamed-stage-*")
	if err != nil {
		return err
	}
	hostStreamedStage.file = f
	hostStreamedStage.path = f.Name()
	return nil
}

func writeStreamedStage(xferID string, generation uint64, data []byte) error {
	_, _ = xferID, generation
	if len(data) == 0 {
		return errors.New("empty_chunk")
	}
	if hostStreamedStage.file == nil {
		return errors.New("streamed_stage_not_started")
	}
	_, err := hostStreamedStage.file.Write(data)
	return err
}

func commitStreamedStage(xferID string, generation uint64) (streamedStage, error) {
	_, _ = xferID, generation
	f := hostStreamedStage.file
	if f == nil {
		return streamedStage{}, errors.New("streamed_stage_not_started")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		abortStreamedStage()
		return streamedStage{}, err
	}
	svc := currentService()
	if svc == nil {
		abortStreamedStage()
		return streamedStage{}, errors.New("updater_not_running")
	}
	sink, err := newSlotSink(0)
	if err != nil {
		abortStreamedStage()
		return streamedStage{}, err
	}
	manifest, err := svc.verifier.Verify(f, sink)
	if err != nil {
		abortStreamedStage()
		return streamedStage{}, err
	}
	desc := streamedStage{
		Version:       manifest.Version,
		BuildID:       manifest.BuildID,
		ImageID:       manifest.ImageID,
		Length:        manifest.PayloadLength,
		PayloadSHA256: manifest.PayloadSHA256,
	}
	hostStreamedStage.desc = desc
	hostStreamedStage.ready = true
	_ = f.Close()
	_ = os.Remove(hostStreamedStage.path)
	hostStreamedStage.file = nil
	hostStreamedStage.path = ""
	return desc, nil
}

func abortStreamedStage() {
	if hostStreamedStage.file != nil {
		_ = hostStreamedStage.file.Close()
	}
	if hostStreamedStage.path != "" {
		_ = os.Remove(hostStreamedStage.path)
	}
	hostStreamedStage.file = nil
	hostStreamedStage.path = ""
	hostStreamedStage.desc = streamedStage{}
	hostStreamedStage.ready = false
}

func consumeStreamedStageResult() (streamedStage, bool) {
	if !hostStreamedStage.ready {
		return streamedStage{}, false
	}
	out := hostStreamedStage.desc
	hostStreamedStage.desc = streamedStage{}
	hostStreamedStage.ready = false
	return out, true
}

func discardStreamedStageResult() {
	abortStreamedStage()
	clearABUpdateDiagHook()
}
