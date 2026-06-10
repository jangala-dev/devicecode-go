//go:build ota_trace

package otadiag

func init() {
	verbose.Store(true)
}
