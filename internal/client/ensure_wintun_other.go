//go:build !windows

package client

func EnsureWintunDLL() (string, error) { return "", ErrNoWintun }
