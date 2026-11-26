//go:build (!cgo && !darwin) || osusergo

package main

import "errors"

func passwdHome() (string, error) {
	// Without libc, os/user can fall back to the caller's environment.
	return "", errors.New("katl-dev-evidence requires native passwd lookup")
}
