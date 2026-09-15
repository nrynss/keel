// Package id generates and validates the unguessable public ids that
// Keel puts in URLs and storage names.
//
// An id is 128 bits of crypto/rand hex-encoded to 32 lowercase
// characters. Guessing one id reveals nothing about any other, so a
// resource named by id is shareable without being enumerable. The
// bits carry no timestamp and no counter.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
)

// idLen is the length of a generated id in characters: 128 bits of
// entropy, two hex characters per byte.
const idLen = 32

// New returns a fresh id drawn from crypto/rand. An entropy failure
// returns an error and never panics.
func New() (string, error) {
	return newFrom(rand.Reader)
}

// newFrom draws 16 bytes of entropy from r and hex-encodes them. It
// takes a reader so an in-package test can inject a failing entropy
// source.
func newFrom(r io.Reader) (string, error) {
	var b [idLen / 2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("id: read entropy: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Valid reports whether s has exactly the shape New produces: 32
// lowercase hex characters. A caller runs it before it looks an id up
// by name.
func Valid(s string) bool {
	if len(s) != idLen {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
