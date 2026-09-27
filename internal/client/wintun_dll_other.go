//go:build !(windows && (amd64 || arm64))

package client

// 既不是 Windows、也不是我们内嵌了 dll 的架构。留一个空值，
// 让 EnsureWintunDLL 给出「请自行放置 wintun.dll」的明确提示，而不是编译不过。
var wintunDLL []byte
