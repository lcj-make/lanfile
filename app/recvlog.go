package main

import (
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// recvItem 是一条"最近接收"记录。接收方原本对进来的文件毫无感知,
// 这里在落盘成功后记下是什么、谁发的、存到了哪, 页面上随时可回看。
type recvItem struct {
	Name   string `json:"name"`           // 文件名; 文件夹传输带 root/rel 前缀
	Root   string `json:"root,omitempty"` // 文件夹传输的根目录名, 单文件为空
	From   string `json:"from"`           // 发送方别名, 反查不到时退回 IP
	FromIP string `json:"from_ip"`
	Size   int64  `json:"size"`
	Dir    string `json:"dir"`   // 落盘目录, 提示用户去哪找文件
	AtMS   int64  `json:"at_ms"` // 完成时间
}

// 记录只存内存: 重启后列表清空, 但接收目录里的文件还在, 不做持久化
const recvLogMax = 50

var (
	recvMu    sync.Mutex
	recvItems []recvItem
)

func addRecvItem(item recvItem) {
	recvMu.Lock()
	recvItems = append(recvItems, item)
	if len(recvItems) > recvLogMax {
		recvItems = recvItems[len(recvItems)-recvLogMax:]
	}
	recvMu.Unlock()
	notifyReceived(item)
}

// 新的在前, 方便前端直接渲染
func listRecvItems() []recvItem {
	recvMu.Lock()
	defer recvMu.Unlock()
	out := make([]recvItem, len(recvItems))
	for i, it := range recvItems {
		out[len(recvItems)-1-i] = it
	}
	return out
}

func handleReceived(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, http.StatusOK, listRecvItems())
}

// remoteIP 从 RemoteAddr 里取出发送方 IP
func remoteIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// aliasByIP 在设备发现列表里反查发送方别名; 跨网段手填或已下线时反查不到, 退回显示 IP
func aliasByIP(devs []Device, ip string) string {
	for _, d := range devs {
		if d.IP.String() == ip {
			return d.Alias
		}
	}
	return ""
}

// notifyReceived 弹系统通知, 让正埋头干别的的用户也能第一时间知道收到了文件。
// 通知失败 (比如机器没装 osascript) 只影响提醒, 不影响传输, 静默忽略。
// 文件夹传输的每个文件都会走这里, 但 macOS 的通知是覆盖式的, 不会堆一屏;
// 为避免上百个文件连发上百条通知, 文件夹内的文件不单独弹通知, 页面列表里可见。
func notifyReceived(item recvItem) {
	if item.Root != "" {
		return
	}
	title := "lanfile 收到文件"
	text := fmt.Sprintf("%s (%s, 来自 %s)", item.Name, humanBytes(item.Size), item.From)
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf("display notification %q with title %q", text, title)
		exec.Command("osascript", "-e", script).Start()
	case "windows":
		// 气球通知走 PowerShell + WinForms, 不引第三方依赖
		ps := fmt.Sprintf(
			`[void][System.Reflection.Assembly]::LoadWithPartialName('System.Windows.Forms'); `+
				`$n=New-Object System.Windows.Forms.NotifyIcon; `+
				`$n.Icon=[System.Drawing.SystemIcons]::Information; $n.Visible=$true; `+
				`$n.ShowBalloonTip(5000,%s,%s,'Info'); Start-Sleep -m 900; $n.Dispose()`,
			psQuote(title), psQuote(text))
		exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", ps).Start()
	case "linux":
		exec.Command("notify-send", title, text).Start()
	}
}

// psQuote 把字符串包成 PowerShell 单引号字面量, 内部单引号翻倍转义
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
