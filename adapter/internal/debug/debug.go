// Package debug 提供受 UIPATH_DEBUG 环境变量控制的调试打印。
// 默认关闭——auth/client 里的 fmt.Println/fmt.Printf 调试转储（HTML、token、请求体等）
// 量极大，曾在水位补号循环里把 /tmp/adapter.log 撑到 13G。需要排障时 UIPATH_DEBUG=1 打开。
package debug

import (
	"fmt"
	"os"
)

var enabled = os.Getenv("UIPATH_DEBUG") != ""

// Enabled 返回调试打印是否开启。
func Enabled() bool { return enabled }

// Println 仅在 UIPATH_DEBUG 开启时打印（等价 fmt.Println）。
func Println(a ...any) {
	if enabled {
		fmt.Println(a...)
	}
}

// Printf 仅在 UIPATH_DEBUG 开启时打印（等价 fmt.Printf）。
func Printf(format string, a ...any) {
	if enabled {
		fmt.Printf(format, a...)
	}
}
