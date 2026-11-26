package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type receiver struct {
	// dirv 支持运行时切换接收目录 (页面上改), 原子指针避免与正在进行的传输产生数据竞争
	dirv atomic.Pointer[string]
	disc *Discovery
}

func (r *receiver) dir() string { return *r.dirv.Load() }

// setDir 切换接收目录; 调用方保证目录已存在
func (r *receiver) setDir(nd string) { r.dirv.Store(&nd) }

// handleRecvDir 供接收模式页面查询/修改接收目录
func (r *receiver) handleRecvDir(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"dir": r.dir()})
		return
	case http.MethodPost:
		var in struct {
			Dir string `json:"dir"`
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, 4096))
		if err != nil || json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.Dir) == "" {
			httpError(w, http.StatusBadRequest, fmt.Errorf("请提供接收目录路径"))
			return
		}
		nd, err := filepath.Abs(strings.TrimSpace(in.Dir))
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if err := os.MkdirAll(nd, 0o755); err != nil {
			httpError(w, http.StatusBadRequest, fmt.Errorf("无法创建目录 %s: %v", nd, err))
			return
		}
		r.setDir(nd)
		log.Printf("接收目录已切换为 %s", nd)
		writeJSON(w, http.StatusOK, map[string]string{"dir": nd})
	default:
		httpError(w, http.StatusMethodNotAllowed, fmt.Errorf("仅支持 GET/POST"))
	}
}

// ---------- 心跳保活 ----------

// 托管模式 (默认) 下服务随页面生死: 页面每 3 秒上报一次心跳,
// 全部页面关闭 (或浏览器崩溃) 后心跳停止, 服务自动退出, 不留后台进程。
// -keepalive 关闭后变为常驻模式, 后端一直运行, 页面只是操作入口。
var (
	lastHeartbeat atomic.Int64 // 最近一次心跳的 unix 秒
	gotHeartbeat  atomic.Bool  // 是否收到过至少一次心跳
)

func handleHeartbeat(w http.ResponseWriter, req *http.Request) {
	lastHeartbeat.Store(time.Now().Unix())
	gotHeartbeat.Store(true)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func startKeepaliveWatch() {
	lastHeartbeat.Store(time.Now().Unix())
	started := time.Now()
	go func() {
		for {
			time.Sleep(3 * time.Second)
			// 首次心跳前给 12 秒宽限: 留给浏览器打开页面的时间,
			// 超过仍无任何心跳就判定页面没起来, 直接退出, 不留后台进程。
			if !gotHeartbeat.Load() && time.Since(started) < 12*time.Second {
				continue
			}
			if time.Now().Unix()-lastHeartbeat.Load() > 12 {
				log.Printf("页面已全部关闭, 服务自动退出")
				os.Exit(0)
			}
		}
	}()
}

func runReceive(args []string) {
	fs := flag.NewFlagSet("receive", flag.ExitOnError)
	addr := fs.String("addr", fmt.Sprintf(":%d", defaultPort), "监听地址, 例如 :53318")
	dir := fs.String("dir", "./received", "接收目录")
	open := fs.Bool("open", false, "启动后打开界面窗口")
	useBrowser := fs.Bool("browser", false, "用浏览器(Chrome/Edge --app)打开, 而不是内嵌 WebView 窗口")
	keep := fs.Bool("keepalive", true, "托管模式(默认): 页面全部关闭后服务自动退出; 传 -keepalive=false 改为常驻")
	fs.Parse(args)
	// 命令行参数需要在闭包里读取, 用局部变量承接
	browserFallback := *useBrowser

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fatal(err)
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fatal(err)
	}
	r := &receiver{}
	r.setDir(abs)

	// 局域网工具: 允许任意来源跨域访问本机接口, 避免浏览器直连对端时被 CORS 拦截。
	cors := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if req.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
	inner := http.NewServeMux()
	mux := cors(inner)
	inner.HandleFunc("/api/prepare", r.handlePrepare)
	inner.HandleFunc("/api/prepare-dir", r.handlePrepareDir)
	inner.HandleFunc("/api/upload/", r.handleUpload)
	inner.HandleFunc("/api/reset/", r.handleReset)
	inner.HandleFunc("/api/info", r.handleInfo)
	inner.HandleFunc("/api/devices", r.handleDevices)
	inner.HandleFunc("/api/probe", r.handleProbe)
	inner.HandleFunc("/api/ping", r.handlePing)
	inner.HandleFunc("/api/send", r.handleSend)
	inner.HandleFunc("/api/received", handleReceived)
	inner.HandleFunc("/api/recvdir", r.handleRecvDir)
	inner.HandleFunc("/api/pickdir", r.handlePickDir)
	inner.HandleFunc("/api/pickfiles", r.handlePickFiles)
	inner.HandleFunc("/api/picksenddir", r.handlePickSendDir)
	inner.HandleFunc("/api/heartbeat", handleHeartbeat)
	inner.HandleFunc("/", r.handleIndex)
	inner.HandleFunc("/api/health", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	log.Printf("接收端已启动: 监听 %s, 目录 %s", *addr, abs)
	if disc, err := StartDiscovery(portFromAddr(*addr)); err == nil {
		r.disc = disc
		defer disc.Close()
		log.Printf("设备发现已启用: 别名 %s, 对端可运行 lanfile devices 查看", disc.self.Alias)
	} else {
		log.Printf("设备发现未启用 (%v), 不影响手动指定 IP 传输", err)
	}

	if *keep {
		startKeepaliveWatch()
	}

	srv := &http.Server{Addr: *addr, Handler: mux}
	url := browseURL(*addr)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		// 已经有一份在跑: 不再重复启动, 直接把页面顶到前台
		if isAddrInUse(err) {
			fmt.Printf("lanfile 已在运行: %s\n", url)
			if *open {
				openUI(url, browserFallback)
			}
			return
		}
		// 默认端口被别的程序占用时, 自动向后找一个空闲端口, 并在发现报文里如实上报
		host, p, perr := net.SplitHostPort(*addr)
		if perr == nil {
			if base, conv := strconv.Atoi(p); conv == nil {
				if fallback, lerr := pickFreePort(host, base); lerr == nil {
					*addr = net.JoinHostPort(host, strconv.Itoa(fallback))
					ln, err = net.Listen("tcp", *addr)
					url = browseURL(*addr)
					srv.Addr = *addr
				}
			}
		}
		if err != nil {
			fatal(err)
		}
	}
	// 内嵌 WebView 的事件循环必须在主线程跑: HTTP 服务放后台 goroutine,
	// 主线程交给 openUI 阻塞直到窗口关闭。
	go func() {
		if err := srv.Serve(ln); err != nil {
			fatal(err)
		}
	}()
	if *open {
		time.Sleep(300 * time.Millisecond)
		fmt.Printf("页面    %s\n", url)
		openUI(url, browserFallback)
	} else if err := srv.Serve(ln); err != nil {
		fatal(err)
	}
}

// openUI 打开界面。默认用内嵌原生 WebView (独立窗口, 不依赖浏览器);
// 传 -browser 时退回 Chrome/Edge --app 或默认浏览器。
func openUI(rawURL string, browserFallback bool) {
	if browserFallback {
		openBrowserApp(rawURL)
		return
	}
	openDesktop(rawURL)
}

// pickFreePort 从 base 开始依次向后尝试, 返回第一个能监听的端口。
// 仅在默认端口被非 lanfile 程序占用时调用, 让本机换端口而不是直接起不来。
func pickFreePort(host string, base int) (int, error) {
	for i := 0; i < portTryCount; i++ {
		p := base + i
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			l.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("端口 %d-%d 均被占用", base, base+portTryCount-1)
}

// browseURL 把监听地址换成能直接点开的本机地址
func browseURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Sprintf("http://127.0.0.1:%d/", defaultPort)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// handlePrepare 负责三件事:
//  1. 目标文件已存在且校验一致 -> 直接告诉发送端不用传
//  2. 存在上次中断留下的 .part -> 告诉发送端从第几字节续传
//  3. 全新传输 -> 记下 name/size/sha256 备查
func (r *receiver) handlePrepare(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, errors.New("需要 POST"))
		return
	}
	var in prepareReq
	if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	resp, err := r.planFile(in)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// planFile 判断单个文件该怎么传, 单文件和文件夹共用这一份逻辑:
//  1. 目标文件已存在且校验一致 -> 告诉发送端不用传
//  2. 存在上次中断留下的 .part -> 告诉发送端从第几字节续传
//  3. 全新传输 -> 记下 name/size/sha256 备查
func (r *receiver) planFile(in prepareReq) (prepareResp, error) {
	if in.Size < 0 || len(in.SHA256) != 64 {
		return prepareResp{}, errors.New("size 或 sha256 非法")
	}
	in.Name = safeName(in.Name)
	if in.Name == "" {
		return prepareResp{}, errors.New("文件名非法")
	}
	dst, err := r.destPath(in)
	if err != nil {
		return prepareResp{}, err
	}
	id := transferID(in.Root, in.Rel, in.SHA256)
	resp := prepareResp{ID: id, Name: in.Name}

	// 已经传过且内容一致, 直接跳过
	if st, err := os.Stat(dst); err == nil && st.Size() == in.Size {
		if sum, err := hashFile(dst); err == nil && strings.EqualFold(sum, in.SHA256) {
			log.Printf("跳过: %s 已存在且校验一致", displayName(in))
			resp.Complete = true
			return resp, nil
		}
	}

	if err := r.saveMeta(in); err != nil {
		return resp, err
	}

	part := r.partPath(id)
	var offset int64
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
		if offset > in.Size {
			// 比目标还长, 说明是脏数据, 丢掉重来
			os.Remove(part)
			offset = 0
		}
	}

	// 上次已经收满但没来得及改名
	if offset == in.Size && in.Size > 0 {
		if err := r.finalize(in); err == nil {
			resp.Complete = true
			return resp, nil
		}
		os.Remove(part)
		offset = 0
	}

	resp.Offset = offset
	if offset > 0 {
		// 把已落盘的那一段哈希一并返回, 让发送端先确认续传起点可信
		sum, err := hashPrefix(part, offset)
		if err != nil {
			return resp, err
		}
		resp.PrefixSHA256 = sum
		log.Printf("续传: %s 已有 %s / %s", displayName(in), humanBytes(offset), humanBytes(in.Size))
	} else {
		log.Printf("接收: %s (%s)", displayName(in), humanBytes(in.Size))
	}
	return resp, nil
}

func displayName(in prepareReq) string {
	if in.Rel == "" {
		return in.Name
	}
	return path.Join(in.Root, in.Rel)
}

type dirFile struct {
	Name   string `json:"name"`
	Rel    string `json:"rel"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type dirPrepareReq struct {
	Root  string    `json:"root"`
	Files []dirFile `json:"files"`
	Dirs  []string  `json:"dirs,omitempty"`
}

// handlePrepareDir 一次收下整棵目录的清单, 返回每个文件的续传起点。
// 一次往返就知道要传多少、跳过多少, 顺带把空目录先建出来。
func (r *receiver) handlePrepareDir(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, errors.New("需要 POST"))
		return
	}
	var in dirPrepareReq
	if err := json.NewDecoder(io.LimitReader(req.Body, 32<<20)).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	root := safeName(in.Root)
	if root == "" {
		httpError(w, http.StatusBadRequest, errors.New("根目录名非法"))
		return
	}
	if len(in.Files) > 200000 {
		httpError(w, http.StatusBadRequest, errors.New("文件数量过多"))
		return
	}
	os.MkdirAll(filepath.Join(r.dir(), root), 0o755)
	for _, d := range in.Dirs {
		rel, err := safeRel(d)
		if err != nil {
			continue
		}
		os.MkdirAll(filepath.Join(r.dir(), root, rel), 0o755)
	}

	out := make([]prepareResp, 0, len(in.Files))
	skipped := 0
	for _, f := range in.Files {
		resp, err := r.planFile(prepareReq{Name: f.Name, Size: f.Size, SHA256: f.SHA256, Root: root, Rel: f.Rel})
		if err != nil {
			httpError(w, http.StatusBadRequest, fmt.Errorf("%s: %w", f.Rel, err))
			return
		}
		resp.Name = f.Rel
		if resp.Complete {
			skipped++
		}
		out = append(out, resp)
	}
	log.Printf("目录清单: %s, %d 个文件, 其中 %d 个已存在", root, len(out), skipped)
	writeJSON(w, http.StatusOK, map[string]any{"root": root, "files": out})
}

// handleReset 丢弃已收的部分数据, 用于发送端发现前缀不符时从头重传
func (r *receiver) handleReset(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, errors.New("需要 POST"))
		return
	}
	id := strings.TrimPrefix(req.URL.Path, "/api/reset/")
	if len(id) != 64 {
		httpError(w, http.StatusBadRequest, errors.New("传输 id 非法"))
		return
	}
	if err := os.Remove(r.partPath(id)); err == nil {
		log.Printf("重置: %s… 已收数据已丢弃", id[:12])
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUpload 把请求体按字节追加到 .part, 结束后比对 sha256 再改名
func (r *receiver) handleUpload(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPut {
		httpError(w, http.StatusMethodNotAllowed, errors.New("需要 PUT"))
		return
	}
	id := strings.TrimPrefix(req.URL.Path, "/api/upload/")
	if len(id) != 64 {
		httpError(w, http.StatusBadRequest, errors.New("传输 id 非法"))
		return
	}
	meta, err := r.loadMeta(id)
	if err != nil {
		httpError(w, http.StatusNotFound, errors.New("未知传输, 请先调用 prepare"))
		return
	}

	part := r.partPath(id)
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	cur := st.Size()

	want, err := strconv.ParseInt(req.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		want = cur
	}
	if want != cur {
		f.Close()
		w.Header().Set("X-Actual-Offset", strconv.FormatInt(cur, 10))
		httpError(w, http.StatusConflict, fmt.Errorf("偏移不一致: 客户端 %d, 服务端 %d", want, cur))
		return
	}

	// 续传时先把已落盘的前缀读一遍, 重建哈希状态
	h := sha256.New()
	if cur > 0 {
		pf, err := os.Open(part)
		if err != nil {
			f.Close()
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		_, err = io.CopyBuffer(h, pf, make([]byte, chunkBuffer))
		pf.Close()
		if err != nil {
			f.Close()
			httpError(w, http.StatusInternalServerError, err)
			return
		}
	}

	if _, err := f.Seek(cur, io.SeekStart); err != nil {
		f.Close()
		httpError(w, http.StatusInternalServerError, err)
		return
	}

	start := time.Now()
	n, copyErr := io.CopyBuffer(io.MultiWriter(f, h), req.Body, make([]byte, chunkBuffer))
	if err := f.Sync(); err != nil {
		log.Printf("刷盘失败: %v", err)
	}
	f.Close()

	written := cur + n
	if copyErr != nil {
		w.Header().Set("X-Actual-Offset", strconv.FormatInt(written, 10))
		httpError(w, http.StatusBadRequest, fmt.Errorf("连接中断于 %s: %v", humanBytes(written), copyErr))
		return
	}
	if written > meta.Size {
		os.Remove(part)
		httpError(w, http.StatusBadRequest, fmt.Errorf("收到 %d 字节, 超过声明的 %d, 已丢弃", written, meta.Size))
		return
	}
	if written < meta.Size {
		w.Header().Set("X-Actual-Offset", strconv.FormatInt(written, 10))
		httpError(w, http.StatusBadRequest, fmt.Errorf("数据不完整: %s / %s", humanBytes(written), humanBytes(meta.Size)))
		return
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(sum, meta.SHA256) {
		os.Remove(part)
		httpError(w, http.StatusUnprocessableEntity,
			fmt.Errorf("sha256 不一致, 已丢弃; 期望 %s 实际 %s", meta.SHA256, sum))
		return
	}

	dst, err := r.destPath(meta)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if prev, err := os.Stat(dst); err == nil && prev.Size() != meta.Size {
		log.Printf("覆盖: %s (%s)", displayName(meta), humanBytes(prev.Size()))
	}
	if err := os.Rename(part, dst); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	r.removeMeta(id)

	// 记一条"最近接收", 让接收方能感知收到了什么、存在哪
	item := recvItem{
		Name: displayName(meta), Root: meta.Root, Size: meta.Size,
		FromIP: remoteIP(req), Dir: filepath.Dir(dst), AtMS: time.Now().UnixMilli(),
	}
	if r.disc != nil {
		item.From = aliasByIP(r.disc.List(), item.FromIP)
	}
	if item.From == "" {
		item.From = item.FromIP
	}
	addRecvItem(item)

	speed := float64(n) / time.Since(start).Seconds()
	log.Printf("完成: %s (%s, %.1f MB/s, 校验通过)", filepath.Base(dst), humanBytes(meta.Size), speed/(1<<20))
	writeJSON(w, http.StatusOK, uploadResp{OK: true, Name: filepath.Base(dst), SHA256: sum})
}

// finalize 校验已收满的 .part 并改名为正式文件
func (r *receiver) finalize(meta prepareReq) error {
	id := transferID(meta.Root, meta.Rel, meta.SHA256)
	part := r.partPath(id)
	sum, err := hashFile(part)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, meta.SHA256) {
		return fmt.Errorf("校验失败: 期望 %s 实际 %s", meta.SHA256, sum)
	}
	dst, err := r.destPath(meta)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	r.removeMeta(id)
	log.Printf("完成: %s (%s, 校验通过)", displayName(meta), humanBytes(meta.Size))
	return nil
}

// destPath 把清单里的路径映射成落盘位置。
// 单文件保持原来的"重名自动加序号"; 文件夹保留原结构, 直接覆盖旧文件。
func (r *receiver) destPath(in prepareReq) (string, error) {
	if in.Rel == "" {
		return uniquePath(filepath.Join(r.dir(), safeName(in.Name))), nil
	}
	rel, err := safeRel(in.Rel)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Root) == "" {
		return "", errors.New("文件夹传输缺少根目录名")
	}
	root := safeName(in.Root)
	return filepath.Join(r.dir(), root, rel), nil
}

// safeRel 拒绝绝对路径、上跳和盘符, 防止发送端写到接收目录之外
func safeRel(rel string) (string, error) {
	p := strings.ReplaceAll(rel, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return "", errors.New("非法相对路径: " + rel)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New("非法相对路径: " + rel)
		}
	}
	return filepath.FromSlash(p), nil
}

func (r *receiver) partPath(id string) string {
	return filepath.Join(r.dir(), ".lanfile-"+id+".part")
}

func (r *receiver) metaPath(id string) string {
	return filepath.Join(r.dir(), ".lanfile-"+id+".meta")
}

func (r *receiver) saveMeta(in prepareReq) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return os.WriteFile(r.metaPath(transferID(in.Root, in.Rel, in.SHA256)), b, 0o644)
}

func (r *receiver) loadMeta(id string) (prepareReq, error) {
	var m prepareReq
	b, err := os.ReadFile(r.metaPath(id))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

func (r *receiver) removeMeta(id string) {
	os.Remove(r.metaPath(id))
}

// safeName 只留文件名部分, 防止 ../ 之类的路径穿越
func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(ch rune) rune {
		if ch == 0 || ch == '/' || ch == '\\' {
			return -1
		}
		return ch
	}, name)
	if name == "" || name == "." || name == ".." {
		return "unnamed"
	}
	return name
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return p
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
	log.Printf("拒绝 (HTTP %d): %v", code, err)
}
