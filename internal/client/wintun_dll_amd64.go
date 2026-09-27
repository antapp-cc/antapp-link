//go:build windows && amd64

package client

import _ "embed"

//go:embed assets/wintun/wintun_amd64.dll
var wintunDLL []byte
