package updater

import (
	"errors"
	"io"

	"pico2-a-b/imagev1"
)

var (
	SignedImageProductFamily    = "bigbox"
	SignedImageHardwareProfile  = "bb-v1-cm5-2"
	SignedImageMCUBoardFamily   = "rp2354a"
	SignedImageTrustedKeyID     = ""
	SignedImageTrustedPublicKey = ""
)

type signedImageVerifier struct{}

func SignedImageVerifier() Verifier {
	return signedImageVerifier{}
}

func SignedImagePolicy() imagev1.Policy {
	var keys []imagev1.TrustedKey
	if SignedImageTrustedKeyID != "" && SignedImageTrustedPublicKey != "" {
		pub, err := imagev1.ParsePublicKeyHex(SignedImageTrustedPublicKey)
		if err == nil {
			keys = append(keys, imagev1.TrustedKey{
				KeyID:     SignedImageTrustedKeyID,
				PublicKey: pub,
			})
		}
	}
	return imagev1.Policy{
		Target: imagev1.Target{
			ProductFamily:   SignedImageProductFamily,
			HardwareProfile: SignedImageHardwareProfile,
			MCUBoardFamily:  SignedImageMCUBoardFamily,
		},
		Keys: keys,
	}
}

func (signedImageVerifier) Verify(r io.Reader, sink SlotSink) (Manifest, error) {
	if sink == nil {
		return Manifest{}, errors.New("signed_image: nil sink")
	}
	res, err := imagev1.Verify(r, SignedImagePolicy(), func(uint32) (imagev1.PayloadSink, error) {
		return sink, nil
	})
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{
		Version:       res.Version,
		BuildID:       res.BuildID,
		ImageID:       res.ImageID,
		PayloadSHA256: res.PayloadSHA256,
		PayloadLength: res.PayloadLength,
	}, nil
}
