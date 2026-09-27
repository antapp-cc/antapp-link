//go:build windows && arm64

package client

import _ "embed"

//go:embed assets/wintun/wintun_arm64.dll
var wintunDLL []byte
