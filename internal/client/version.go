package client

// Version 是客户端版本号。服务端有同名变量，发版时一起改。
//
// 必须是 var 而不是 const：build.ps1 要用 -ldflags -X 把它注进去。
// 默认值跟着当前版本走 —— 手搓构建没带 -X 时也不至于自认旧版、反复「发现新版本」。
var Version = "0.4.1"
