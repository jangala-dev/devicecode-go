package registry

import "testing"

type dummyBuilder struct{}

func (dummyBuilder) Build(in BuildInput) (BuildOutput, error) { return BuildOutput{}, nil }

func TestRegisterAndLookup(t *testing.T) {
	const typ = "test_dummy_builder"
	if _, ok := Lookup(typ); ok {
		t.Skip("builder already registered by earlier test run")
	}
	RegisterBuilder(typ, dummyBuilder{})
	if _, ok := Lookup(typ); !ok {
		t.Fatalf("lookup failed for %q", typ)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	const typ = "test_duplicate_builder"
	if _, ok := Lookup(typ); !ok {
		RegisterBuilder(typ, dummyBuilder{})
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	RegisterBuilder(typ, dummyBuilder{})
}
