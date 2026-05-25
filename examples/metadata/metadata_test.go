package metadata

import "testing"

func TestResolveTierIndex_premium(t *testing.T) {
	if got := resolveTierIndex("premium"); got != 1 {
		t.Fatalf("want 1, got %d", got)
	}
}

func TestResolveTierIndex_standard(t *testing.T) {
	if got := resolveTierIndex("standard"); got != 0 {
		t.Fatalf("want 0, got %d", got)
	}
}

func TestResolveTierIndex_empty(t *testing.T) {
	if got := resolveTierIndex(""); got != 0 {
		t.Fatalf("want 0, got %d", got)
	}
}

func TestResolveTierIndex_unknown(t *testing.T) {
	if got := resolveTierIndex("unknown"); got != 0 {
		t.Fatalf("want 0, got %d", got)
	}
}
