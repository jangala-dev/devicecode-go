//go:build !qa_reactor

package reactor

import "testing"

func TestFirmwareIdentityDefaults(t *testing.T) {
	oldVersion := FirmwareVersion
	oldBuild := FirmwareBuild
	oldImageID := FirmwareImageID
	defer func() {
		FirmwareVersion = oldVersion
		FirmwareBuild = oldBuild
		FirmwareImageID = oldImageID
	}()

	FirmwareVersion = ""
	FirmwareBuild = ""
	FirmwareImageID = ""

	id := firmwareIdentity()
	if id.Version != "0.0.0-dev" {
		t.Fatalf("Version = %q, want default", id.Version)
	}
	if id.Build != "local" {
		t.Fatalf("Build = %q, want default", id.Build)
	}
	if id.ImageID != "img-dev" {
		t.Fatalf("ImageID = %q, want default", id.ImageID)
	}
}

func TestFirmwareIdentityUsesStampedValues(t *testing.T) {
	oldVersion := FirmwareVersion
	oldBuild := FirmwareBuild
	oldImageID := FirmwareImageID
	defer func() {
		FirmwareVersion = oldVersion
		FirmwareBuild = oldBuild
		FirmwareImageID = oldImageID
	}()

	FirmwareVersion = "15.7"
	FirmwareBuild = "fw-update-e2e-15.7"
	FirmwareImageID = "mcu-dev-15.7"

	id := firmwareIdentity()
	if id.Version != FirmwareVersion {
		t.Fatalf("Version = %q, want %q", id.Version, FirmwareVersion)
	}
	if id.Build != FirmwareBuild {
		t.Fatalf("Build = %q, want %q", id.Build, FirmwareBuild)
	}
	if id.ImageID != FirmwareImageID {
		t.Fatalf("ImageID = %q, want %q", id.ImageID, FirmwareImageID)
	}
}
