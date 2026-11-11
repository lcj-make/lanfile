# lanfile 官网

`lanfile` 是一个跑在局域网里的文件互传工具：一台开接收，另一台把文件扔进来。不压缩、不改名、不覆盖已有文件；传完两端各算一次 SHA-256，对得上才算完。

本站是这个工具的产品落地页（单页，纯静态）。

## 目录

- `index.html` — 站点唯一页面（自包含 CSS/JS）
- `assets/` — 发送端 / 接收端的真实界面截图（900px JPEG）
- `downloads/` — 三个安装包（macOS arm64 / macOS amd64 / Windows x64）

## 安装包指纹（SHA-256 实算）

```
7d2732c96120c5b93a8fcc52bf8e4b131eadf37b5d47f99425f5da1163b37dc5  lanfile-0.4-macos-arm64.dmg
8e54b526dd55f28c706454c9a717cc84f6f55e71ff7713a8b1efe24338271fb4  lanfile-0.4-macos-amd64.dmg
047ccc678c12abdd16b6854b5f07feaedc455530433fa2ea9b866c1d278c2d64  lanfile-0.4-windows-x64.zip
```

站点通过 GitHub Pages 自动部署（`.github/workflows/deploy.yml`，push 到 `main` 即生效）。
