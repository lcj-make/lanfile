//go:build !darwin

package main

// Windows / Linux 没有内嵌 WebView 时, 走浏览器兜底。
// Windows 可用系统自带 Edge 的 --app 模式, 仍以独立窗口呈现, 不占标签页。
func openDesktop(url string) {
	openBrowserApp(url)
}
