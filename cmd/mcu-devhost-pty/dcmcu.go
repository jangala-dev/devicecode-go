//go:build !tinygo

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"devicecode-go/services/updater"
)

const (
	dcmcuMagic          = "DCMCUIMG"
	dcmcuFormatVersion  = uint16(1)
	dcmcuMinHeaderLen   = 16
	dcmcuMaxHeaderLen   = 4096
	dcmcuMaxManifestLen = 1024 * 1024
)

type dcmcuManifest struct {
	Schema    int    `json:"schema"`
	Component string `json:"component"`
	Build     struct {
		Version string `json:"version"`
		BuildID string `json:"build_id"`
		ImageID string `json:"image_id"`
	} `json:"build"`
	Payload struct {
		Length uint32 `json:"length"`
		SHA256 string `json:"sha256"`
	} `json:"payload"`
}

type devhostDCMCUVerifier struct{}

func (devhostDCMCUVerifier) Verify(r io.Reader, sink updater.SlotSink) (updater.Manifest, error) {
	if sink == nil {
		return updater.Manifest{}, errors.New("devhost_dcmcu: nil sink")
	}
	manifest, payload, err := readDCMCU(r)
	if err != nil {
		_ = sink.Abort()
		return updater.Manifest{}, err
	}
	if _, err := sink.Write(payload); err != nil {
		_ = sink.Abort()
		return updater.Manifest{}, err
	}
	if err := sink.Commit(); err != nil {
		_ = sink.Abort()
		return updater.Manifest{}, err
	}
	return updater.Manifest{
		Version:       manifest.Build.Version,
		BuildID:       manifest.Build.BuildID,
		ImageID:       manifest.Build.ImageID,
		PayloadSHA256: manifest.Payload.SHA256,
		PayloadLength: manifest.Payload.Length,
	}, nil
}

func readDCMCU(r io.Reader) (dcmcuManifest, []byte, error) {
	var zero dcmcuManifest
	header16 := make([]byte, dcmcuMinHeaderLen)
	if _, err := io.ReadFull(r, header16); err != nil {
		return zero, nil, fmt.Errorf("dcmcu_header: %w", err)
	}
	if string(header16[:len(dcmcuMagic)]) != dcmcuMagic {
		return zero, nil, errors.New("dcmcu_bad_magic")
	}
	version := binary.LittleEndian.Uint16(header16[8:10])
	headerLen := binary.LittleEndian.Uint16(header16[10:12])
	manifestLen := binary.LittleEndian.Uint32(header16[12:16])
	if version != dcmcuFormatVersion {
		return zero, nil, errors.New("dcmcu_version_unsupported")
	}
	if headerLen < dcmcuMinHeaderLen || headerLen > dcmcuMaxHeaderLen {
		return zero, nil, errors.New("dcmcu_header_len_invalid")
	}
	if manifestLen == 0 || manifestLen > dcmcuMaxManifestLen {
		return zero, nil, errors.New("dcmcu_manifest_len_invalid")
	}
	if extra := int(headerLen) - len(header16); extra > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(extra)); err != nil {
			return zero, nil, fmt.Errorf("dcmcu_header_rest: %w", err)
		}
	}
	manifestRaw := make([]byte, manifestLen)
	if _, err := io.ReadFull(r, manifestRaw); err != nil {
		return zero, nil, fmt.Errorf("dcmcu_manifest: %w", err)
	}
	var manifest dcmcuManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return zero, nil, fmt.Errorf("dcmcu_manifest_json_invalid: %w", err)
	}
	if err := validateDCMCUManifest(manifest); err != nil {
		return zero, nil, err
	}
	payload := make([]byte, manifest.Payload.Length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return zero, nil, fmt.Errorf("dcmcu_payload: %w", err)
	}
	sum := sha256.Sum256(payload)
	if got := hex.EncodeToString(sum[:]); got != manifest.Payload.SHA256 {
		return zero, nil, fmt.Errorf("dcmcu_payload_sha256_mismatch: got %s want %s", got, manifest.Payload.SHA256)
	}
	return manifest, payload, nil
}

func validateDCMCUManifest(m dcmcuManifest) error {
	if m.Schema != 1 {
		return errors.New("dcmcu_manifest_schema_unsupported")
	}
	if m.Component != "mcu" {
		return errors.New("dcmcu_component_not_mcu")
	}
	if m.Build.ImageID == "" {
		return errors.New("dcmcu_image_id_required")
	}
	if len(m.Payload.SHA256) != 64 {
		return errors.New("dcmcu_payload_sha256_invalid")
	}
	if _, err := hex.DecodeString(m.Payload.SHA256); err != nil {
		return errors.New("dcmcu_payload_sha256_invalid")
	}
	return nil
}
