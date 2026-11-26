package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"unicode/utf16"
)

// picking 保证同时只弹一个系统目录选择框: 连点两下按钮不应该叠出两个模态框
var picking atomic.Bool

// isLoopbackRemote 判断请求是不是从本机页面发起的。
// 目录选择框弹在服务端所在的这台机器上, 从手机/别的电脑打开页面时弹框会出现在
// 用户看不到的地方, 所以只对本机请求开放, 其它来源继续走手动填写路径。
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handlePickDir 弹系统目录选择框, 选中即切换接收目录
func (r *receiver) handlePickDir(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, errors.New("需要 POST"))
		return
	}
	if !isLoopbackRemote(req.RemoteAddr) {
		httpError(w, http.StatusForbidden, errors.New("目录选择框只能在本机页面上打开, 这里请手动填写路径"))
		return
	}
	if !picking.CompareAndSwap(false, true) {
		httpError(w, http.StatusConflict, errors.New("已经有一个选择框打开了, 先在弹框里选完"))
		return
	}
	defer picking.Store(false)

	dir, err := pickDirectory(r.dir())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if dir == "" {
		// 用户直接关掉了弹框, 不算失败, 目录保持原样
		writeJSON(w, http.StatusOK, map[string]any{"dir": r.dir(), "cancelled": true})
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("无法使用目录 %s: %v", abs, err))
		return
	}
	r.setDir(abs)
	log.Printf("接收目录已切换为 %s", abs)
	writeJSON(w, http.StatusOK, map[string]any{"dir": abs})
}

// pickDirectory 按平台弹原生目录选择框, 返回选中的绝对路径; 用户取消返回空串
func pickDirectory(cur string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return pickDirDarwin(cur)
	case "windows":
		return pickDirWindows(cur)
	default:
		return pickDirLinux(cur)
	}
}

func pickDirDarwin(cur string) (string, error) {
	expr := `POSIX path of (choose folder with prompt "选择接收文件夹"`
	if cur != "" {
		expr += ` default location (POSIX file "` + escapeAppleScript(cur) + `")`
	}
	expr += `)`
	out, err := exec.Command("osascript", "-e", expr).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			msg := strings.TrimSpace(string(ee.Stderr))
			// -128 是用户取消 (AppleScript 的 userCanceledErr)
			if strings.Contains(msg, "-128") || msg == "" {
				return "", nil
			}
			return "", fmt.Errorf("打开目录选择框失败: %s", msg)
		}
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return "", nil
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	return p, nil
}

// escapeAppleScript 转义路径里的双引号和反斜杠, 防止路径把脚本撑破
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func pickDirWindows(cur string) (string, error) {
	// 选中的路径写成 UTF-8 临时文件再读回来, 免得踩上控制台的 ANSI 代码页
	seeded := ""
	if cur != "" {
		seeded = fmt.Sprintf("$d.SelectedPath=%s;", psQuote(cur))
	}
	script := `Add-Type -AssemblyName System.Windows.Forms;` +
		`$d = New-Object System.Windows.Forms.FolderBrowserDialog;` +
		`$d.Description = '选择接收文件夹';` +
		`$d.ShowNewFolderButton = $true;` +
		seeded +
		`if ($d.ShowDialog() -eq 'OK') { ` +
		`[IO.File]::WriteAllText($out, $d.SelectedPath, (New-Object Text.UTF8Encoding $false)) }`

	paths, err := runWindowsDialog(script)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", nil
	}
	p := strings.TrimSpace(paths[0])
	if p != filepath.VolumeName(p)+`\` {
		p = strings.TrimRight(p, `\`)
	}
	return p, nil
}

// runWindowsDialog 执行一段 System.Windows.Forms 对话框脚本, 选中内容写入 $out 临时文件并读回。
// 脚本里可直接引用变量 $out (临时文件路径), 由本函数在执行前注入。
func runWindowsDialog(scriptBody string) ([]string, error) {
	f, err := os.CreateTemp("", "lanfile-pick-*.txt")
	if err != nil {
		return nil, err
	}
	out := f.Name()
	f.Close()
	defer os.Remove(out)

	script := `Add-Type -AssemblyName System.Windows.Forms;$out=` + psQuote(out) + `;` + scriptBody
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encodeUTF16Base64(script))
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("打开选择框失败: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	b, err := os.ReadFile(out)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil // 没写文件说明用户取消
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

func encodeUTF16Base64(s string) string {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(u)*2)
	for _, r := range u {
		buf = append(buf, byte(r), byte(r>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func pickDirLinux(cur string) (string, error) {
	cands := [][]string{
		{"zenity", "--file-selection", "--directory", "--title=选择接收文件夹", "--filename=" + cur + "/"},
		{"kdialog", "--getexistingdirectory", cur, "--title", "选择接收文件夹"},
	}
	for _, c := range cands {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		out, err := cmd.Output()
		if err != nil {
			var ee *exec.ExitError
			// zenity/kdialog 取消时退出码是 1, 没有别的原因
			if errors.As(err, &ee) && ee.ExitCode() == 1 {
				return "", nil
			}
			return "", fmt.Errorf("打开目录选择框失败: %v", err)
		}
		p := strings.TrimSpace(string(out))
		if p != "" && p != "/" {
			p = strings.TrimRight(p, "/")
		}
		return p, nil
	}
	return "", errors.New("本机没有可用的目录选择框 (需要 zenity 或 kdialog), 请手动填写路径")
}
