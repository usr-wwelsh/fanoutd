package store

import (
	"crypto/rand"
	"encoding/hex"
)

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewID mints an identifier for something that has no table of its own — a
// breakdown's group, or the workspace its subtasks share. Both are only ever
// foreign keys on a task row, so nothing else allocates them.
func NewID() string { return generateID() }
