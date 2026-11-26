#!/bin/sh
# 打包船员用的安装包, 产物在 dist/。只能在 macOS 上跑 (用到 hdiutil / codesign)。
set -eu

cd "$(dirname "$0")"

APP=lanfile
VER=0.5
export PATH="$HOME/.local/go/bin:$PATH"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

rm -rf build dist
mkdir -p build dist
# 打包时 .app 会摊在 build/ 里, 一旦被 Spotlight 收录, 启动台和「打开」面板里
# 就会冒出好几个 lanfile 图标。让聚光灯忽略整个 build/ 目录, 顺便收尾时删干净。
: > build/.metadata_never_index

echo "== 编译 =="
# macOS 走内嵌 WebView (CGO 开, 链接系统 WKWebView); Windows 用纯 Go 交叉编译,
# 界面走 Edge --app 独立窗口兜底。
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o build/lanfile .
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o build/darwin-arm64/$APP .
CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o build/darwin-amd64/$APP .

# exe 的图标要写进 PE 资源段, 否则资源管理器里显示的是 Go 默认图标。
# rsrc 生成的 syso 必须放在包目录, 文件名带 _windows_amd64 后缀,
# 这样其它平台的编译会自动忽略它。
RSRC="$HOME/go/bin/rsrc"
rm -f rsrc_windows_amd64.syso
if [ -x "$RSRC" ]; then
	"$RSRC" -ico assets/icon.ico -o rsrc_windows_amd64.syso -arch amd64
else
	echo "   警告: 没找到 rsrc, exe 会用默认图标"
	echo "         go install github.com/akavel/rsrc@latest"
fi
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o build/lanfile.exe .
rm -f rsrc_windows_amd64.syso

# ---------------------------------------------------------------- macOS

# 生成 .app: 二进制 + 启动脚本。双击时系统只执行 CFBundleExecutable 且不带参数,
# 所以入口必须是脚本, 由它补上 receive -open。
make_app() {
	arch=$1
	app="$2"
	mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
	cp "build/darwin-$arch/$APP" "$app/Contents/MacOS/$APP"
	chmod 755 "$app/Contents/MacOS/$APP"
	cp assets/icon.icns "$app/Contents/Resources/icon.icns"

	cat > "$app/Contents/MacOS/run" <<'LAUNCHER'
#!/bin/sh
DIR=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$HOME/Library/Logs"
exec "$DIR/lanfile" receive -open -dir "$HOME/Desktop" >>"$HOME/Library/Logs/lanfile.log" 2>&1
LAUNCHER
	chmod 755 "$app/Contents/MacOS/run"

	cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>lanfile</string>
  <key>CFBundleDisplayName</key><string>lanfile</string>
  <key>CFBundleIdentifier</key><string>dev.lanfile.app</string>
  <key>CFBundleExecutable</key><string>run</string>
  <key>CFBundleIconFile</key><string>icon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$VER</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST
	printf 'APPL????' > "$app/Contents/PkgInfo"
}

make_dmg() {
	arch=$1
	stage="build/dmg-$arch"
	dmg="dist/$APP-$VER-macos-$arch.dmg"
	mkdir -p "$stage"
	make_app "$arch" "$stage/$APP.app"
	# 临时签名: 未签名的 arm64 包在别的机器上会被直接拒绝启动
	codesign --force --deep --sign - "$stage/$APP.app" >/dev/null 2>&1 || \
		echo "   警告: codesign 失败, 目标机器可能需要右键 -> 打开"
	ln -s /Applications "$stage/Applications"
	# UDBZ = 压缩 dmg
	hdiutil create -quiet -volname "$APP" -srcfolder "$stage" -ov -format UDBZ "$dmg"
	# 装完 dmg 就不再需要这份 .app 了, 立刻删掉, 免得留在项目里被系统当成第二个 lanfile
	rm -rf "$stage"
	echo "   $dmg"
}

echo "== macOS =="
make_dmg arm64
make_dmg amd64

# ---------------------------------------------------------------- Windows

echo "== Windows =="
win=build/win
mkdir -p "$win"
cp build/lanfile.exe "$win/lanfile.exe"
cp assets/icon.ico "$win/lanfile.ico"

cat > "$win/install.bat" <<'BAT'
@echo off
setlocal
title lanfile setup
set "DEST=%LOCALAPPDATA%\lanfile"
set "RECDIR=%USERPROFILE%\Desktop"
set "STARTUP=%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup"
set "MENU=%APPDATA%\Microsoft\Windows\Start Menu\Programs"

echo.
echo   lanfile setup
echo   install to : %DEST%
echo   receive to : %RECDIR%
echo.

if not exist "%~dp0lanfile.exe" (
  echo   [ERROR] lanfile.exe not found next to this script.
  echo           Extract the whole zip to a folder first, then run install.bat.
  echo.
  pause
  exit /b 1
)

if not exist "%DEST%" mkdir "%DEST%"
copy /y "%~dp0lanfile.exe" "%DEST%\lanfile.exe" >nul
if errorlevel 1 goto :fail
copy /y "%~dp0lanfile.ico" "%DEST%\lanfile.ico" >nul

rem start at login, window hidden
> "%STARTUP%\lanfile.vbs" echo Set sh = CreateObject("WScript.Shell")
>>"%STARTUP%\lanfile.vbs" echo sh.CurrentDirectory = "%DEST%"
>>"%STARTUP%\lanfile.vbs" echo sh.Run """%DEST%\lanfile.exe"" receive -dir ""%RECDIR%""", 0, False

rem double-click entry: wscript keeps the black console from appearing
> "%DEST%\lanfile-open.vbs" echo Set sh = CreateObject("WScript.Shell")
>>"%DEST%\lanfile-open.vbs" echo sh.CurrentDirectory = "%DEST%"
>>"%DEST%\lanfile-open.vbs" echo sh.Run """%DEST%\lanfile.exe"" receive -open -dir ""%RECDIR%""", 0, False

rem start menu shortcut
powershell -NoProfile -ExecutionPolicy Bypass -Command "$s=(New-Object -ComObject WScript.Shell).CreateShortcut('%MENU%\lanfile.lnk'); $s.TargetPath='%WINDIR%\System32\wscript.exe'; $s.Arguments='\"%DEST%\lanfile-open.vbs\"'; $s.IconLocation='%DEST%\lanfile.ico'; $s.Save()" >nul 2>&1

rem inbound rule (needs admin)
netsh advfirewall firewall add rule name="lanfile" dir=in action=allow program="%DEST%\lanfile.exe" enable=yes >nul 2>&1
if errorlevel 1 (
  echo   [WARN] firewall rule not added, other machines may not reach this one.
  echo          Right-click install.bat - Run as administrator.
) else (
  echo   [OK] firewall rule added
)

echo   [OK] autostart registered
echo   [OK] start menu shortcut created
echo.
echo   starting lanfile ...
wscript "%DEST%\lanfile-open.vbs"
echo.
echo   Done. The lanfile window should be open now.
echo   To stop: end lanfile.exe in Task Manager.
echo.
pause
exit /b 0

:fail
echo.
echo   [ERROR] cannot write to %DEST%
echo           Extract the zip to a normal folder and try again,
echo           or right-click install.bat - Run as administrator.
pause
exit /b 1
BAT

cat > "$win/uninstall.bat" <<'BAT'
@echo off
setlocal
title lanfile uninstall
taskkill /f /im lanfile.exe >nul 2>&1
del "%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\lanfile.vbs" >nul 2>&1
del "%APPDATA%\Microsoft\Windows\Start Menu\Programs\lanfile.lnk" >nul 2>&1
del "%LOCALAPPDATA%\lanfile\lanfile-open.vbs" >nul 2>&1
netsh advfirewall firewall delete rule name="lanfile" >nul 2>&1
echo.
echo   lanfile stopped.
echo   Removed: autostart entry, start menu shortcut, firewall rule.
echo   Program files kept in %LOCALAPPDATA%\lanfile
echo   Received files kept in %USERPROFILE%\Desktop
echo.
pause
BAT

# README 用 UTF-8 BOM, 记事本打开才不乱码
{
	printf '\xEF\xBB\xBF'
	cat <<'README'
lanfile — 局域网原样文件传输

一、安装
  1. 解压本压缩包到任意目录（不要放在压缩包里直接运行）
  2. 双击 install.bat
     · 首次运行 Windows 可能弹出防火墙提示，勾选「专用网络」并允许
     · 若提示防火墙规则未添加，右键 install.bat → 以管理员身份运行
  3. 安装完成后会自动弹出 lanfile 窗口（也可以从开始菜单打开）

二、使用
  · 页面右侧会列出同一局域网内其它运行了 lanfile 的电脑
  · 选中设备，把文件或文件夹拖进左侧区域即可
  · 收到的文件直接放在 用户目录的桌面 (Desktop)
  · 如果两台机器不在同一网段（IP 前三段不同），组播发现过不去：
    点设备栏右上角 ＋，手填对方 IP（如 172.20.40.102）添加即可

三、命令行（可选）
  lanfile.exe devices                        查看局域网内设备
  lanfile.exe send D:\a.zip 192.168.1.20    按 IP 直接发送
  lanfile.exe send D:\资料 192.168.1.20     发送整个目录，保留结构

四、卸载
  双击 uninstall.bat。已收到的文件不会被删除。

五、原理与保证
  按字节流原样传输，不压缩、不改名；接收端落地后重新比对 SHA-256，
  两端一致才算成功；中断后重跑同一条命令会从断点续传。
README
} > "$win/README.txt"

# 批处理必须是 CRLF + 纯 ASCII。之前是 LF + UTF-8 中文, cmd 按 GBK 解析这段字节时
# 会撞上引号/括号, 直接解析失败退出, 表现为窗口一闪就关。
for f in "$win/install.bat" "$win/uninstall.bat" "$win/README.txt"; do
	awk '{ sub(/\r$/, ""); printf "%s\r\n", $0 }' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
done

# zip 里统一用 ASCII 文件名, 避免旧版 Windows 解压中文名乱码
(cd "$win" && zip -q -r "../../dist/$APP-$VER-windows-x64.zip" .)
echo "   dist/$APP-$VER-windows-x64.zip"

echo
echo "== 完成 =="
ls -lh dist
