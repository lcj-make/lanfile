# lanfile 官网

`lanfile` 是一个跑在局域网里的文件互传工具：一台开接收，另一台把文件扔进来。不压缩、不改名、不覆盖已有文件；传完两端各算一次 SHA-256，对得上才算完。

本站是这个工具的产品落地页（单页，纯静态）。

## 目录

- `index.html` — 站点唯一页面（自包含 CSS/JS）
- `assets/` — 发送端 / 接收端的真实界面截图（900px JPEG）
- `downloads/` — 三个安装包（macOS arm64 / macOS amd64 / Windows x64）
- `app/` — 桌面端程序源码（Go 后端 + 内嵌 Web 前端，构建说明见 `app/README.md`）

## 安装包指纹（SHA-256 实算）

```
1477240ceccf64c6fe7c3eb9e76397a40faaf1bd69a09da7f4b1cc9ac339b03c  lanfile-0.5-macos-arm64.dmg
fa9ac85627c9353b6b69318a3b970df62f7a0a5a0cb08017f69776a9a17ca31e  lanfile-0.5-macos-amd64.dmg
ff4781bb76bd53c163fd602fbe91efe511709e019ea9b303469058b45e41a517  lanfile-0.5-windows-x64.zip
```

站点通过 GitHub Pages 自动部署（`.github/workflows/deploy.yml`，push 到 `main` 即生效）。
