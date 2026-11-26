package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const (
	// defaultPort 是我们的 HTTP 端口。刻意避开 LocalSend 的 53317,
	// 两者同时运行时不会抢端口; 发现报文里会带上本端口, 对端按声明值连接。
	defaultPort = 53318
	// portTryCount 本机启动时若默认端口被占用, 依次向后尝试的端口个数
	portTryCount = 20
	// chunkBuffer 留空表示用 8MB 缓冲搬运字节流
	chunkBuffer = 8 << 20
)

// prepareReq / prepareResp / uploadResp 是两端共享的 JSON 结构。
// 后续要兼容 LocalSend 协议时, 在这里对齐字段即可。
type prepareReq struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// 文件夹传输时: Root 是根目录名, Rel 是文件相对根目录的路径。
	// 单文件传输时两者都为空, 沿用原来的行为。
	Root string `json:"root,omitempty"`
	Rel  string `json:"rel,omitempty"`
}

// transferID 是传输的幂等标识。单文件沿用 sha256 (改名也能续传);
// 文件夹内按 (root, rel, sha256) 派生, 否则同内容的两个文件会抢同一个 .part。
func transferID(root, rel, sum string) string {
	if rel == "" {
		return sum
	}
	h := sha256.Sum256([]byte(root + "\x00" + rel + "\x00" + sum))
	return hex.EncodeToString(h[:])
}

type prepareResp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Offset   int64  `json:"offset"`
	Complete bool   `json:"complete"`
	// PrefixSHA256 是目标端已落盘那一段的哈希, 发送端据此判断续传起点是否可信
	PrefixSHA256 string `json:"prefix_sha256,omitempty"`
}

type uploadResp struct {
	OK     bool   `json:"ok"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

func main() {
	if len(os.Args) < 2 {
		// 双击启动: Windows 上双击 exe 不会带参数, 以前只打印用法就退出,
		// 表现为"窗口闪一下就没了"。这里直接当接收端跑起来并打开窗口。
		fmt.Println("lanfile 启动中, 关闭本窗口即停止服务")
		runReceive([]string{"-dir", defaultRecvDir(), "-open"})
		return
	}
	switch os.Args[1] {
	case "receive", "recv", "serve":
		runReceive(os.Args[2:])
	case "send":
		runSend(os.Args[2:])
	case "hash":
		runHash(os.Args[2:])
	case "probe":
		runProbe(os.Args[2:])
	case "devices", "ls":
		runDevices(os.Args[2:])
	case "install":
		runInstall(os.Args[2:])
	case "uninstall":
		runUninstall(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`lanfile - 局域网原样文件传输 (M1)

用法:
  lanfile receive [-addr :端口] [-dir ./received] [-open]
        启动接收端, 把文件收进 -dir, 同时在局域网内广播自己;
        -open 启动后自动打开操作窗口。已在运行时再执行一次只会把窗口调到前台

  lanfile install [-dir 接收目录]
        常驻安装: 注册开机自启, 并留一个双击入口
          macOS   ~/Applications/lanfile.app + LaunchAgent
          Linux   systemd --user 服务 + 应用菜单项
          Windows 启动文件夹脚本 + 防火墙入站规则
        默认接收目录 ~/Desktop (桌面)

  lanfile uninstall
        移除开机自启和双击入口 (不动已经收到的文件)

  lanfile devices [-t 6s]
        列出局域网内正在运行的其它设备

  lanfile send <文件|目录> <主机[:端口]|设备别名> [-name 目标名]
        发送文件或整个目录; 中断后重跑同一条命令即从断点续传。
        目录会保留相对路径, 逐文件校验, 坏一个不影响其它文件

  lanfile hash <文件>
        打印 sha256 与大小, 用于人工核对

  lanfile probe [-t 20s] [-peer 对方IP]
        检测局域网是否开启 AP 隔离:
        两台机器各跑一次, 会分别报告组播与 TCP 单播是否可达

保证:
  按字节流原样传输, 不压缩、不转码、不改名;
  接收端落地后重新比对 sha256, 两端一致才算成功。
`)
}

func runHash(args []string) {
	if len(args) != 1 {
		usage()
		os.Exit(2)
	}
	st, err := os.Stat(args[0])
	if err != nil {
		fatal(err)
	}
	sum, err := hashFile(args[0])
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%s  %d  %s\n", sum, st.Size(), args[0])
}

// hashFile 流式计算 sha256, 不把整个文件读进内存
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, chunkBuffer)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashPrefix 只对文件前 n 个字节算 sha256, 用于校验续传起点
func hashPrefix(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, io.LimitReader(f, n), make([]byte, chunkBuffer)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
