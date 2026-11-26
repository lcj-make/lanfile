//go:build darwin

package main

import (
	"log"
	"sync"

	"github.com/webview/webview_go"
)

// 独立桌面窗口: 用 macOS 系统自带的 WKWebView 渲染本机页面, 不依赖用户安装 Chrome/Edge。
// 后端 HTTP 服务在另一个 goroutine 跑, 窗口关闭后由心跳看门狗退出进程。

var (
	desktopOnce sync.Once
	desktopW    webview.WebView
)

// openDesktop 打开原生桌面窗口并阻塞到窗口关闭。
// 必须在主线程调用 (macOS Cocoa 要求主线程跑事件循环)。
func openDesktop(url string) {
	desktopOnce.Do(func() {
		w := webview.New(true)
		desktopW = w
		w.SetTitle("lanfile")
		w.SetSize(1180, 780, webview.HintNone)
		w.Navigate(url)
	})
	desktopW.Run()
	desktopW.Destroy()
	log.Printf("桌面窗口已关闭")
}
