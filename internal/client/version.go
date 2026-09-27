package client

// Version 是客户端版本号。服务端有同名常量，发版时一起改。
//
// 必须是 var 而不是 const：build.ps1 要用 -ldflags -X 把它注进去。
var Version = "0.1.0"
