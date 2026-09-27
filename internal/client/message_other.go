//go:build !windows

package client

import "fmt"

func ShowMessage(title, text string) {
	fmt.Printf("%s: %s\n", title, text)
}
