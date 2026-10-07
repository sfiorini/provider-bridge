package api

import "testing"

func TestVersionAccessorReturnsStampedValue(t *testing.T) {
	orig := version
	defer func() { version = orig }()
	version = "9.9.9-test"
	if got := Version(); got != "9.9.9-test" {
		t.Fatalf("Version() = %q, want %q", got, "9.9.9-test")
	}
}

func TestVersionDefaultsToDev(t *testing.T) {
	orig := version
	defer func() { version = orig }()
	version = "dev"
	if got := Version(); got != "dev" {
		t.Fatalf("Version() = %q, want %q", got, "dev")
	}
}
