//go:build !darwin

package main

import "errors"

// errImmutabilityUnsupportedPlatform signals that this host's platform has
// no immutable-flag implementation wired up (the fleet is Darwin-only in
// practice; this stub exists so the package still builds elsewhere). The
// check reports itself as skipped rather than warning.
var errImmutabilityUnsupportedPlatform = errors.New("binary-immutability: no chflags-equivalent implementation for this platform")

func realBinaryIsImmutable(_ string) (bool, error) { return false, errImmutabilityUnsupportedPlatform }
func nameIsImmutable(_ string) (bool, error)       { return false, errImmutabilityUnsupportedPlatform }
func armRealBinaryImmutable(_ string) error        { return errImmutabilityUnsupportedPlatform }
func armNameImmutable(_ string) error              { return errImmutabilityUnsupportedPlatform }

func isPlatformUnsupported(err error) bool { return errors.Is(err, errImmutabilityUnsupportedPlatform) }
