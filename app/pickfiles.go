package main

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// 后端原生选择文件/文件夹。
// 内嵌 WebView (WKWebView) 里 <input type="file"> 打不开系统文件框,
// 所以原生模式下由后端弹 osascript / PowerShell 原生对话框, 拿到本机绝对路径,
// 再由后端直接读盘发送, 不经过浏览器的文件句柄。

type pickedEntry struct {
	Path string `json:"path"`
	Rel  string `json:"rel"`
}

type pickedGroup struct {
	Name  string        `json:"name"`
	Root  string        `json:"root"`
	Files []pickedEntry `json:"files"`
}

// handlePickFiles 弹原生文件选择框, 支持多选; 返回分组清单 (单文件 root 为空)。
func (r *receiver) handlePickFiles(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, errors.New("需要 POST"))
		return
	}
	if !isLoopbackRemote(req.RemoteAddr) {
		httpError(w, http.StatusForbidden, errors.New("文件选择框只能在本机页面上打开"))
		return
	}
	if !picking.CompareAndSwap(false, true) {
		httpError(w, http.StatusConflict, errors.New("已经有一个选择框打开了, 先在弹框里选完"))
		return
	}
	defer picking.Store(false)
	paths, err := pickFilesNative(false)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if len(paths) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"groups": []pickedGroup{}})
		return
	}
	groups, err := groupsFromPaths(paths)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

// handlePickSendDir 弹原生文件夹选择框, 递归收集目录内文件。
func (r *receiver) handlePickSendDir(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, errors.New("需要 POST"))
		return
	}
	if !isLoopbackRemote(req.RemoteAddr) {
		httpError(w, http.StatusForbidden, errors.New("文件夹选择框只能在本机页面上打开"))
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
		writeJSON(w, http.StatusOK, map[string]any{"groups": []pickedGroup{}})
		return
	}
	group, err := groupFromDir(dir)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": []pickedGroup{group}})
}

// groupsFromPaths 把选中的文件/目录整理成分组: 目录单独成组, 文件归入单文件组。
func groupsFromPaths(paths []string) ([]pickedGroup, error) {
	var files []pickedEntry
	var dirs []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			dirs = append(dirs, p)
		} else {
			files = append(files, pickedEntry{Path: p, Rel: filepath.Base(p)})
		}
	}
	var out []pickedGroup
	if len(files) > 0 {
		out = append(out, pickedGroup{Name: "已选文件", Files: files})
	}
	for _, d := range dirs {
		g, err := groupFromDir(d)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// groupFromDir 递归收集目录内文件, rel 为相对该目录的路径。
func groupFromDir(dir string) (pickedGroup, error) {
	base := filepath.Base(dir)
	var files []pickedEntry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, pickedEntry{Path: path, Rel: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return pickedGroup{}, err
	}
	return pickedGroup{Name: base, Root: base, Files: files}, nil
}

// pickFilesNative 按平台弹原生多选文件框。folders=true 时只选文件夹。
func pickFilesNative(folders bool) ([]string, error) {
	switch runtime.GOOS {
	case "darwin":
		return pickFilesDarwin(folders)
	case "windows":
		return pickFilesWindows(folders)
	default:
		return nil, errors.New("当前平台不支持原生选择框, 请用浏览器模式")
	}
}

func pickFilesDarwin(folders bool) ([]string, error) {
	var expr string
	if folders {
		expr = `POSIX path of (choose folder with prompt "选择要发送的文件夹")`
	} else {
		expr = `set f to choose file with prompt "选择要发送的文件" with multiple selections allowed
set out to {}
repeat with p in f
	set end of out to POSIX path of p
end repeat
set delimiter to linefeed
set AppleScript's text item delimiters to delimiter
out as text`
	}
	out, err := exec.Command("osascript", "-e", expr).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			msg := strings.TrimSpace(string(ee.Stderr))
			if strings.Contains(msg, "-128") || msg == "" {
				return nil, nil // 用户取消
			}
			return nil, errors.New("打开文件选择框失败: " + msg)
		}
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

func pickFilesWindows(folders bool) ([]string, error) {
	var dialog string
	if folders {
		dialog = `$d = New-Object System.Windows.Forms.FolderBrowserDialog;
if ($d.ShowDialog() -eq 'OK') { [IO.File]::WriteAllText($out, $d.SelectedPath, (New-Object Text.UTF8Encoding $false)) }`
	} else {
		dialog = `$d = New-Object System.Windows.Forms.OpenFileDialog;
$d.Multiselect = $true;
if ($d.ShowDialog() -eq 'OK') { [IO.File]::WriteAllText($out, ($d.FileNames -join "` + "\n" + `"), (New-Object Text.UTF8Encoding $false)) }`
	}
	return runWindowsDialog(dialog)
}

// groupsToFrontend 转成前端 pending 结构: 带本地路径, 发送时后端直接读盘。
func groupsToFrontend(groups []pickedGroup) []map[string]any {
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		files := make([]map[string]any, 0, len(g.Files))
		for _, f := range g.Files {
			files = append(files, map[string]any{
				"localPath": f.Path,
				"rel":       f.Rel,
				"name":      filepath.Base(f.Path),
			})
		}
		out = append(out, map[string]any{
			"name":  g.Name,
			"root":  g.Root,
			"files": files,
		})
	}
	return out
}
