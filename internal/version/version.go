// Package version 是客户端与服务端共用的版本号。
//
// 分开维护必然有一天忘改一边（0.4.4 那轮就是这样：客户端改了、服务端漏了，
// 发出去两端自报不同版本，排查时很误导）。这里只留一份默认值，构建时用
// -ldflags -X 统一注入 —— build.ps1 把同一个值同时注进 client.Version 与
// server.Version，两边永远一致。
package version

// Version 是当前版本号。发版时改这里一处。
//
// 必须是 var 而不是 const：-X 只能改包级 var。
var Version = "0.4.11"
