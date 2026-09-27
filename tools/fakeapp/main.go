// Command fakeapp 是给更新流程测试用的假程序：只打印自己的版本就退出。
//
// 用它当「新版」的好处是替换完成后启动起来就走，不会像真客户端那样弹个
// 「还没有连接码」的框出来干扰测试。
package main

import "fmt"

// Version 由构建命令用 -ldflags -X 注入。
var Version = "0.0.0"

func main() {
	fmt.Println("fakeapp", Version)
}
