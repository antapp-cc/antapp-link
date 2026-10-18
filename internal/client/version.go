package client

import "github.com/antapp-cc/antapp-link/internal/version"

// Version 是客户端版本号，默认取 internal/version 里的那一个。
//
// 为什么要在这里再留一个 var：-ldflags -X 只能写包级变量，客户端自报版本走的是
// 这个符号。默认值直接引用共用版本，所以手搓构建（没带 -X）也不会自认旧版、
// 反复「发现新版本」；正式构建由 build.ps1 把同一个值同时注进客户端和服务端。
var Version = version.Version
