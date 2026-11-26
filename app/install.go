package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// 常驻安装: 把当前二进制注册成登录自启的服务, 并在桌面/开始菜单留一个
// 双击入口。目标只有一个: 团队里的人不用开终端。
//
//   macOS   ~/Applications/lanfile.app + ~/Library/LaunchAgents/dev.lanfile.agent.plist
//   Linux   systemd --user 服务 + ~/.local/share/applications/lanfile.desktop
//   Windows 启动文件夹里的 lanfile.vbs (隐藏窗口) + 防火墙入站规则

const agentLabel = "dev.lanfile.agent"

func runInstall(args []string) {
	var dir string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-dir" || args[i] == "--dir":
			if i+1 < len(args) {
				i++
				dir = args[i]
			}
		case strings.HasPrefix(args[i], "-dir="):
			dir = strings.TrimPrefix(args[i], "-dir=")
		}
	}
	if dir == "" {
		dir = defaultRecvDir()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		fatal(err)
	}
	self, err := selfPath()
	if err != nil {
		fatal(err)
	}

	fmt.Printf("程序    %s\n接收目录 %s\n", self, abs)
	switch runtime.GOOS {
	case "darwin":
		err = installDarwin(self, abs)
	case "linux":
		err = installLinux(self, abs)
	case "windows":
		err = installWindows(self, abs)
	default:
		err = fmt.Errorf("暂不支持 %s", runtime.GOOS)
	}
	if err != nil {
		fatal(err)
	}
	fmt.Println("\n安装完成。之后开机会自动运行, 也可以双击 lanfile 图标手动启动。")
	fmt.Println("卸载: lanfile uninstall")
}

func runUninstall(args []string) {
	var errs []string
	// 先结束正在运行的进程: 否则 app 内的可执行文件被占用, macOS 会拒绝删除 .app
	stopRunning()
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		plist := filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist")
		exec.Command("launchctl", "unload", plist).Run()
		removeFile(plist, &errs)
		// 兼容两种安装位置: 手动拖进 /Applications, 或 install 装到 ~/Applications
		for _, app := range []string{
			filepath.Join("/Applications", "lanfile.app"),
			filepath.Join(home, "Applications", "lanfile.app"),
		} {
			if _, err := os.Stat(app); err == nil {
				if err := os.RemoveAll(app); err != nil {
					errs = append(errs, err.Error())
				} else {
					fmt.Println("删除", app)
				}
			}
		}
	case "linux":
		unit := filepath.Join(homeDir(), ".config", "systemd", "user", "lanfile.service")
		exec.Command("systemctl", "--user", "disable", "--now", "lanfile.service").Run()
		removeFile(unit, &errs)
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		removeFile(filepath.Join(homeDir(), ".local", "share", "applications", "lanfile.desktop"), &errs)
	case "windows":
		exec.Command("taskkill", "/f", "/im", "lanfile.exe").Run()
		removeFile(filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "lanfile.vbs"), &errs)
		exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name=lanfile").Run()
		fmt.Println("已删除防火墙规则 lanfile (若不存在可忽略)")
	}
	if len(errs) > 0 {
		fatal(errors.New(strings.Join(errs, "; ")))
	}
	fmt.Println("卸载完成, 已停止程序与开机自启。接收目录里的文件没有删除。")
}

// stopRunning 尽力结束本机正在运行的 lanfile 进程, 跨平台, 找不到进程不算错误。
func stopRunning() {
	switch runtime.GOOS {
	case "darwin", "linux":
		exec.Command("pkill", "-x", "lanfile").Run()
	case "windows":
		exec.Command("taskkill", "/f", "/im", "lanfile.exe").Run()
	}
}

/* ---------- macOS ---------- */

func installDarwin(self, dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	app := filepath.Join(home, "Applications", "lanfile.app")
	macosDir := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		return err
	}

	// app 自带一份二进制, 原程序被移动或删除也不影响双击启动
	inner := filepath.Join(macosDir, "lanfile")
	if err := copyFile(self, inner, 0o755); err != nil {
		return fmt.Errorf("复制程序到 app 内失败: %w", err)
	}

	// 双击 app 时系统只启动 CFBundleExecutable, 不带任何参数,
	// 所以真正的入口是一个 shell 脚本, 由它补上 receive -open。
	launcher := fmt.Sprintf(`#!/bin/sh
mkdir -p "$HOME/Library/Logs"
exec %q receive -open -dir %q >>"$HOME/Library/Logs/lanfile.log" 2>&1
`, inner, dir)
	runScript := filepath.Join(macosDir, "lanfile-run")
	if err := os.WriteFile(runScript, []byte(launcher), 0o755); err != nil {
		return err
	}

	info := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>lanfile</string>
  <key>CFBundleDisplayName</key><string>lanfile</string>
  <key>CFBundleIdentifier</key><string>` + agentLabel + `.app</string>
  <key>CFBundleExecutable</key><string>lanfile-run</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.5</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(info), 0o644); err != nil {
		return err
	}
	fmt.Println("创建", app)

	logPath := filepath.Join(home, "Library", "Logs", "lanfile.log")
	agent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>receive</string>
    <string>-dir</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, agentLabel, xmlEscape(inner), xmlEscape(dir), xmlEscape(logPath), xmlEscape(logPath))

	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		return err
	}
	plistPath := filepath.Join(agents, agentLabel+".plist")
	if err := os.WriteFile(plistPath, []byte(agent), 0o644); err != nil {
		return err
	}
	fmt.Println("创建", plistPath)

	// 先卸再装, 保证重复执行也是同一份配置
	exec.Command("launchctl", "unload", plistPath).Run()
	if out, err := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err != nil {
		fmt.Printf("注意: launchctl load 失败 (%v): %s\n", err, strings.TrimSpace(string(out)))
		fmt.Println("      开机自启可能不生效, 但双击 lanfile.app 仍可用")
	} else {
		fmt.Println("已注册开机自启, 并在后台启动")
	}
	return nil
}

/* ---------- Linux ---------- */

func installLinux(self, dir string) error {
	home := homeDir()
	unitPath := filepath.Join(home, ".config", "systemd", "user", "lanfile.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=lanfile 局域网原样文件传输
After=network-online.target

[Service]
ExecStart="%s" receive -dir "%s"
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
`, self, dir)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	fmt.Println("创建", unitPath)

	desktopPath := filepath.Join(home, ".local", "share", "applications", "lanfile.desktop")
	if err := os.MkdirAll(filepath.Dir(desktopPath), 0o755); err != nil {
		return err
	}
	desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=lanfile
Comment=局域网原样文件传输
Exec="%s" receive -open -dir "%s"
Terminal=false
Categories=Utility;Network;
`, self, dir)
	if err := os.WriteFile(desktopPath, []byte(desktop), 0o644); err != nil {
		return err
	}
	fmt.Println("创建", desktopPath)

	exec.Command("systemctl", "--user", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", "lanfile.service").CombinedOutput(); err != nil {
		fmt.Printf("注意: systemctl 启用失败 (%v): %s\n", err, strings.TrimSpace(string(out)))
		fmt.Println("      可手动执行: systemctl --user enable --now lanfile.service")
	} else {
		fmt.Println("已注册开机自启, 并在后台启动")
	}
	return nil
}

/* ---------- Windows ---------- */

func installWindows(self, dir string) error {
	startup := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	if err := os.MkdirAll(startup, 0o755); err != nil {
		return err
	}
	// 0 = 隐藏窗口, 登录时不弹黑框; 需要看页面时双击 vbs 或访问下面的地址
	vbs := fmt.Sprintf(`Set sh = CreateObject("WScript.Shell")
sh.CurrentDirectory = "%s"
sh.Run """%s"" receive -dir ""%s""", 0, False
`, filepath.Dir(self), self, dir)
	vbsPath := filepath.Join(startup, "lanfile.vbs")
	if err := os.WriteFile(vbsPath, []byte(vbs), 0o644); err != nil {
		return err
	}
	fmt.Println("创建", vbsPath)

	// 防火墙规则需要管理员权限, 失败也不影响局域网内访问提示
	out, err := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name=lanfile", "dir=in", "action=allow", "program="+self, "enable=yes").CombinedOutput()
	if err != nil {
		fmt.Printf("注意: 添加防火墙规则失败 (%v), 请在管理员终端执行:\n", err)
		fmt.Printf("      netsh advfirewall firewall add rule name=lanfile dir=in action=allow program=\"%s\" enable=yes\n", self)
		if s := strings.TrimSpace(string(out)); s != "" {
			fmt.Println("     ", s)
		}
	} else {
		fmt.Println("已添加防火墙入站规则 lanfile")
	}
	fmt.Println("下次登录会自动在后台启动; 现在手动启动一次: 双击 Start Menu/Programs/Startup/lanfile.vbs")
	return nil
}

/* ---------- 公共 ---------- */

// defaultRecvDir 默认收进桌面, 文件一落地对方就能看见, 不用翻下载目录
func defaultRecvDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "received"
	}
	return filepath.Join(home, "Desktop")
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func removeFile(path string, errs *[]string) {
	if err := os.Remove(path); err == nil {
		fmt.Println("删除", path)
	} else if !os.IsNotExist(err) {
		*errs = append(*errs, err.Error())
	}
}

// xmlEscape 防止路径里的 & < > 破坏 plist
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// openBrowserApp 用 Chrome/Edge 的 --app 模式打开, 没有再退回默认浏览器。
// 这是 -browser 兜底路径; 默认走内嵌 WebView 独立窗口, 不依赖浏览器。
func openBrowserApp(rawURL string) {
	// macOS 上直接 exec 浏览器二进制有个坑: 浏览器没在跑时, 直接执行二进制
	// 不算 LaunchServices 启动, 新窗口经常开在别的 Space 或干脆不激活,
	// 用户看起来就是"双击了没反应"。走 open -na 交给系统拉起, 窗口一定会到前台。
	if runtime.GOOS == "darwin" {
		if exe := appBrowser(); exe != "" {
			name := browserAppName(exe)
			if err := exec.Command("open", "-na", name, "--args", "--app="+rawURL, "--window-size=1180,780").Run(); err == nil {
				log.Printf("已打开窗口: %s (--app 模式, %s)", rawURL, name)
				return
			} else {
				log.Printf("open -na %s 失败 (%v), 改为直接执行浏览器", name, err)
			}
		}
	}
	if exe := appBrowser(); exe != "" {
		c := exec.Command(exe, "--app="+rawURL, "--window-size=1180,780")
		if err := c.Start(); err == nil {
			log.Printf("已打开窗口: %s (直接执行 %s)", rawURL, exe)
			return
		} else {
			log.Printf("直接执行 %s 失败 (%v), 退回默认浏览器", exe, err)
		}
	}
	log.Printf("退回默认浏览器打开 %s", rawURL)
	openBrowser(rawURL)
}

// browserAppName 从 .../Foo.app/Contents/MacOS/Foo 里取出 "Foo.app" 这段名字,
// open -na 需要的是应用名而不是可执行文件路径
func browserAppName(exe string) string {
	for _, seg := range strings.Split(exe, string(filepath.Separator)) {
		if strings.HasSuffix(seg, ".app") {
			return seg
		}
	}
	return filepath.Base(exe)
}

// appBrowser 返回可以做 --app 模式的浏览器可执行文件路径, 找不到返回空
func appBrowser() string {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
			candidates = append(candidates,
				filepath.Join(dir, "Google Chrome.app", "Contents", "MacOS", "Google Chrome"),
				filepath.Join(dir, "Microsoft Edge.app", "Contents", "MacOS", "Microsoft Edge"),
				filepath.Join(dir, "Chromium.app", "Contents", "MacOS", "Chromium"),
			)
		}
	case "windows":
		for _, root := range []string{
			os.Getenv("ProgramFiles"),
			os.Getenv("ProgramFiles(x86)"),
			os.Getenv("LOCALAPPDATA"),
		} {
			if root == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(root, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(root, `Google\Chrome\Application\chrome.exe`),
			)
		}
	default:
		for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge"} {
			if p, err := exec.LookPath(name); err == nil {
				candidates = append(candidates, p)
			}
		}
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// openBrowser 拉起系统默认浏览器 (没有 Chrome/Edge 时的兜底)
func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	c.Start()
}

// isAddrInUse 判断端口是否被占用, 用于区分"已经有一份在跑"和真正的启动失败
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "address already in use") ||
		strings.Contains(s, "Only one usage of each socket address")
}
