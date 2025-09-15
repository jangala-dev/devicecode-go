package halerr

import "testing"

func TestErrorsAreStableStrings(t *testing.T) {
	cases := map[string]error{
		"busy":                       ErrBusy,
		"invalid_period":             ErrInvalidPeriod,
		"invalid_capability_address": ErrInvalidCapAddr,
		"unknown_capability":         ErrUnknownCap,
		"no_adaptor":                 ErrNoAdaptor,
		"missing_bus_ref":            ErrMissingBusRef,
		"unknown_bus":                ErrUnknownBus,
		"invalid_mode":               ErrInvalidMode,
		"unknown_pin":                ErrUnknownPin,
		"unsupported":                ErrUnsupported,
	}
	for want, e := range cases {
		if e == nil || e.Error() != want {
			t.Fatalf("error %q mismatch: got %#v", want, e)
		}
	}
}
