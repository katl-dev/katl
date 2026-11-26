//go:build (cgo || darwin) && !osusergo

package main

import "os/user"

func passwdHome() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	return account.HomeDir, nil
}
