package config

import (
	"encoding/json"
	"testing"
)

func TestHALConfigJSONRoundTrip(t *testing.T) {
	cfg := HALConfig{
		Devices: []Device{
			{ID: "d1", Type: "gpio", Params: map[string]any{"pin": 3}, BusRef: BusRef{Type: "i2c", ID: "i2c0"}},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got HALConfig
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Devices) != 1 || got.Devices[0].ID != "d1" || got.Devices[0].BusRef.ID != "i2c0" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
