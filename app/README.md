# lanfile 程序源码

本目录是 lanfile 桌面端的程序源码：Go 后端 + 内嵌 Web 前端。
仓库根目录是官网落地页（`index.html`），`downloads/` 是已发布的安装包，本目录是对应的源代码。

## 目录结构

- `*.go` — 后端 HTTP 服务、组播设备发现、收发与断点续传、原生选文件对话框、安装/卸载等
- `web/index.html` — 内嵌在二进制里的前端单页
- `assets/` — 应用图标（icns / ico / png）
- `build.sh` — 打包脚本，产物输出到 `dist/`（仅在 macOS 上可出 dmg）

## 构建

要求 Go 1.21+。

```sh
# 本机直接编译（macOS 走内嵌 WebView，需 CGO）
CGO_ENABLED=1 go build -o lanfile .

# Windows 交叉编译（纯 Go，界面走系统浏览器独立窗口兜底）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o lanfile.exe .
```

完整三平台打包见 `build.sh`（需要 `hdiutil` / `codesign`，只能在 macOS 上跑）。

## 常用子命令

- `lanfile receive [-addr :端口] [-dir ./received] [-open]` — 启动接收端
- `lanfile send <文件|目录> <主机[:端口]|设备别名>` — 发送，中断后重跑可断点续传
- `lanfile devices` — 列出局域网内其它设备
- `lanfile hash <文件>` — 打印 SHA-256 与大小
- `lanfile install` / `lanfile uninstall` — 常驻安装 / 移除

## 传输保证

按字节流原样传输，不压缩、不转码、不改名；接收端落地后重新比对 SHA-256，两端一致才算成功。
