// services/hal/internal/halcore/types_test.go

package halcore

import "testing"

func TestEdgeToString(t *testing.T) {
	if EdgeToString(EdgeRising) != "rising" ||
		EdgeToString(EdgeFalling) != "falling" ||
		EdgeToString(EdgeBoth) != "both" ||
		EdgeToString(EdgeNone) != "none" {
		t.Fatal("EdgeToString mapping incorrect")
	}
}
