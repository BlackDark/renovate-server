package store

import "testing"

func TestMemorySemantics(t *testing.T) {
	testStoreSemantics(t, NewMemory())
}
