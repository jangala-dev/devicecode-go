package updater

import (
	"errors"
	"io"

	"github.com/jangala-dev/pico2-a-b/signedimage"
)

var (
	SignedImageProductFamily    string
	SignedImageHardwareProfile  string
	SignedImageMCUBoardFamily   string
	SignedImageTrustedKeyID     string
	SignedImageTrustedPublicKey string
)

const (
	defaultSignedImageProductFamily   = "bigbox"
	defaultSignedImageHardwareProfile = "bb-v1-cm5-2"
	defaultSignedImageMCUBoardFamily  = "rp2354a"
)

type signedImageVerifier struct{}

func SignedImageVerifier() Verifier {
	return signedImageVerifier{}
}

func SignedImagePolicy() signedimage.Policy {
	var keys []signedimage.TrustedKey
	if SignedImageTrustedKeyID != "" && SignedImageTrustedPublicKey != "" {
		pub, err := signedimage.ParsePublicKeyHex(SignedImageTrustedPublicKey)
		if err == nil {
			keys = append(keys, signedimage.TrustedKey{
				KeyID:     SignedImageTrustedKeyID,
				PublicKey: pub,
			})
		}
	}
	return signedimage.Policy{
		Target: signedimage.Target{
			ProductFamily:   signedImageStringOr(SignedImageProductFamily, defaultSignedImageProductFamily),
			HardwareProfile: signedImageStringOr(SignedImageHardwareProfile, defaultSignedImageHardwareProfile),
			MCUBoardFamily:  signedImageStringOr(SignedImageMCUBoardFamily, defaultSignedImageMCUBoardFamily),
		},
		Keys: keys,
	}
}

func signedImageStringOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func (signedImageVerifier) Verify(r io.Reader, sink SlotSink) (Manifest, error) {
	if sink == nil {
		return Manifest{}, errors.New("signed_image: nil sink")
	}
	res, err := signedimage.Verify(r, SignedImagePolicy(), func(uint32) (signedimage.PayloadSink, error) {
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
