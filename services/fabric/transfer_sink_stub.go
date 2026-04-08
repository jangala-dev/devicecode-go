//go:build !(tinygo && rp2350)

package fabric

import "errors"

var errTransferUnsupported = errors.New("unsupported")

type unsupportedTransferFactory struct{}

func newTransferFactory() transferFactory {
	return unsupportedTransferFactory{}
}

func (unsupportedTransferFactory) Begin(meta transferMeta) (transferSink, error) {
	_ = meta
	return nil, errTransferUnsupported
}
