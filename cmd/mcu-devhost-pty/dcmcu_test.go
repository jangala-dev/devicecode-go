//go:build !tinygo

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func makeDCMCUForTest(t *testing.T, imageID string, payload []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(payload)
	manifest, err := json.Marshal(map[string]any{
		"schema":    1,
		"component": "mcu",
		"build": map[string]any{
			"version":  "15.0",
			"build_id": "test-build",
			"image_id": imageID,
		},
		"payload": map[string]any{
			"length": len(payload),
			"sha256": hex.EncodeToString(sum[:]),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	headerLen := uint16(32)
	header := make([]byte, headerLen)
	copy(header, []byte(dcmcuMagic))
	binary.LittleEndian.PutUint16(header[8:10], dcmcuFormatVersion)
	binary.LittleEndian.PutUint16(header[10:12], headerLen)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(manifest)))
	out := append(header, manifest...)
	out = append(out, payload...)
	return out
}

func TestReadDCMCUExtractsManifestAndVerifiesPayload(t *testing.T) {
	payload := []byte(strings.Repeat("payload-", 64))
	m, got, err := readDCMCU(bytes.NewReader(makeDCMCUForTest(t, "mcu-dev-15.0", payload)))
	if err != nil {
		t.Fatalf("readDCMCU: %v", err)
	}
	if m.Build.ImageID != "mcu-dev-15.0" || m.Build.Version != "15.0" || m.Build.BuildID != "test-build" {
		t.Fatalf("manifest build = %+v", m.Build)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestReadDCMCURejectsPayloadHashMismatch(t *testing.T) {
	blob := makeDCMCUForTest(t, "mcu-dev-15.0", []byte("payload"))
	blob[len(blob)-1] ^= 0xff
	_, _, err := readDCMCU(bytes.NewReader(blob))
	if err == nil || !strings.Contains(err.Error(), "dcmcu_payload_sha256_mismatch") {
		t.Fatalf("err = %v, want payload hash mismatch", err)
	}
}

type recordingSink struct {
	bytes.Buffer
	committed, aborted bool
}

func (s *recordingSink) Commit() error { s.committed = true; return nil }
func (s *recordingSink) Abort() error  { s.aborted = true; return nil }

func TestDevhostVerifierStreamsPayloadIntoSink(t *testing.T) {
	payload := []byte("verified-payload")
	sink := &recordingSink{}
	manifest, err := (devhostDCMCUVerifier{}).Verify(bytes.NewReader(makeDCMCUForTest(t, "mcu-dev-16.0", payload)), sink)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if manifest.ImageID != "mcu-dev-16.0" || manifest.PayloadLength != uint32(len(payload)) {
		t.Fatalf("manifest = %+v", manifest)
	}
	if !sink.committed || sink.aborted || sink.String() != string(payload) {
		t.Fatalf("sink committed=%v aborted=%v payload=%q", sink.committed, sink.aborted, sink.String())
	}
}
