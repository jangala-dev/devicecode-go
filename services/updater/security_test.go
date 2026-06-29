package updater

import "testing"

func TestSignedImagePolicyDefaultsTarget(t *testing.T) {
	oldProduct := SignedImageProductFamily
	oldProfile := SignedImageHardwareProfile
	oldBoard := SignedImageMCUBoardFamily
	defer func() {
		SignedImageProductFamily = oldProduct
		SignedImageHardwareProfile = oldProfile
		SignedImageMCUBoardFamily = oldBoard
	}()

	SignedImageProductFamily = ""
	SignedImageHardwareProfile = ""
	SignedImageMCUBoardFamily = ""

	target := SignedImagePolicy().Target
	if target.ProductFamily != "bigbox" {
		t.Fatalf("ProductFamily = %q, want default", target.ProductFamily)
	}
	if target.HardwareProfile != "bb-v1-cm5-2" {
		t.Fatalf("HardwareProfile = %q, want default", target.HardwareProfile)
	}
	if target.MCUBoardFamily != "rp2354a" {
		t.Fatalf("MCUBoardFamily = %q, want default", target.MCUBoardFamily)
	}
}

func TestSignedImagePolicyUsesStampedTarget(t *testing.T) {
	oldProduct := SignedImageProductFamily
	oldProfile := SignedImageHardwareProfile
	oldBoard := SignedImageMCUBoardFamily
	defer func() {
		SignedImageProductFamily = oldProduct
		SignedImageHardwareProfile = oldProfile
		SignedImageMCUBoardFamily = oldBoard
	}()

	SignedImageProductFamily = "product-test"
	SignedImageHardwareProfile = "profile-test"
	SignedImageMCUBoardFamily = "board-test"

	target := SignedImagePolicy().Target
	if target.ProductFamily != SignedImageProductFamily {
		t.Fatalf("ProductFamily = %q, want %q", target.ProductFamily, SignedImageProductFamily)
	}
	if target.HardwareProfile != SignedImageHardwareProfile {
		t.Fatalf("HardwareProfile = %q, want %q", target.HardwareProfile, SignedImageHardwareProfile)
	}
	if target.MCUBoardFamily != SignedImageMCUBoardFamily {
		t.Fatalf("MCUBoardFamily = %q, want %q", target.MCUBoardFamily, SignedImageMCUBoardFamily)
	}
}
