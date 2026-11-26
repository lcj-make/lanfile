package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

type deviceDTO struct {
	Alias       string `json:"alias"`
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Fingerprint string `json:"fingerprint"`
	LastSeenMS  int64  `json:"last_seen_ms"`
}

func (r *receiver) handleIndex(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(page)
}

func (r *receiver) handleInfo(w http.ResponseWriter, req *http.Request) {
	info := map[string]any{
		"alias":   "",
		"port":    defaultPort,
		"dir":     r.dir(),
		"version": "0.5",
		"ips":     localIPv4(),
	}
	if r.disc != nil {
		info["alias"] = r.disc.self.Alias
		info["port"] = r.disc.self.Port
		info["fingerprint"] = r.disc.self.Fingerprint
	}
	writeJSON(w, http.StatusOK, info)
}

func (r *receiver) handleDevices(w http.ResponseWriter, req *http.Request) {
	out := []deviceDTO{}
	if r.disc != nil {
		for _, dev := range r.disc.List() {
			out = append(out, deviceDTO{
				Alias:       dev.Alias,
				IP:          dev.IP.String(),
				Port:        dev.Port,
				Fingerprint: dev.Fingerprint,
				LastSeenMS:  time.Since(dev.LastSeen).Milliseconds(),
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// parseAddr 把 "1.2.3.4" 或 "1.2.3.4:端口" 规整成 host:port。
// 只给 IP 时不强行假定端口: 返回的端口为 0, 由调用方先探测对端真实端口,
// 探测不到再回退默认端口。跨网段时组播过不去, 只能靠用户手填地址。
func parseAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("地址为空")
	}
	if !strings.Contains(s, ":") {
		if net.ParseIP(s) == nil {
			return "", errors.New("不是合法 IP: " + s)
		}
		return net.JoinHostPort(s, "0"), nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", errors.New("地址格式不对: " + s + " (要 IP 或 IP:端口)")
	}
	if net.ParseIP(host) == nil {
		return "", errors.New("不是合法 IP: " + host)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", errors.New("端口不合法: " + port)
	}
	return net.JoinHostPort(host, port), nil
}

// probePeer 先连候选端口取回对端 /api/info。端口为 0 时依次尝试
// 默认端口和其后一小段, 适配对端换过端口的情况。返回可用地址与别名。
func probePeer(addr string) (string, string, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", err
	}
	port, _ := strconv.Atoi(portStr)
	candidates := []int{}
	if port > 0 {
		candidates = append(candidates, port)
	} else {
		for i := 0; i < portTryCount; i++ {
			candidates = append(candidates, defaultPort+i)
		}
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	var lastErr error
	for _, p := range candidates {
		target := net.JoinHostPort(host, strconv.Itoa(p))
		resp, err := cl.Get("http://" + target + "/api/info")
		if err != nil {
			lastErr = err
			continue
		}
		var info struct {
			Alias string `json:"alias"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&info)
		resp.Body.Close()
		if decodeErr != nil || info.Alias == "" {
			lastErr = errors.New(target + " 上没有在跑的 lanfile")
			continue
		}
		return target, info.Alias, nil
	}
	if lastErr == nil {
		lastErr = errors.New("连不上 " + host)
	}
	return "", "", lastErr
}

// handlePing 探活。不带 addr 时返回本机别名; 带 addr 时由本机后端代理探测对端,
// 避免浏览器从 127.0.0.1 直连局域网 IP 被跨域 (CORS) 拦截而误判离线。
func (r *receiver) handlePing(w http.ResponseWriter, req *http.Request) {
	if addr := req.URL.Query().Get("addr"); addr != "" {
		target, _, err := probePeer(addr)
		if err != nil {
			httpError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"addr": target, "online": true})
		return
	}
	alias := ""
	if r.disc != nil {
		alias = r.disc.self.Alias
	}
	writeJSON(w, http.StatusOK, map[string]any{"alias": alias})
}

// handleProbe 在添加手填设备前先连一次, 确认对端确实是 lanfile 并取回它的别名
func (r *receiver) handleProbe(w http.ResponseWriter, req *http.Request) {
	addr, err := parseAddr(req.URL.Query().Get("addr"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	realAddr, alias, err := probePeer(addr)
	if err != nil {
		httpError(w, http.StatusBadGateway, fmt.Errorf("连不上 %s: %v", addr, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"addr": realAddr, "alias": alias})
}

// handleSend 接收浏览器上传的文件, 落到临时文件算出 sha256, 再转给目标设备。
// 响应是 NDJSON 流, 每行一个阶段事件, 前端据此画真实进度。
func (r *receiver) handleSend(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, errors.New("需要 POST"))
		return
	}
	if r.disc == nil {
		httpError(w, http.StatusServiceUnavailable, errors.New("设备发现未启用, 无法发送"))
		return
	}
	// to 优先当设备指纹解析; 解析不到再当 IP[:端口], 支持跨网段手填的设备
	to := req.URL.Query().Get("to")
	addr, alias := "", ""
	if dev, ok := r.disc.ByFingerprint(to); ok {
		addr, alias = dev.Addr(), dev.Alias
	} else if a, err := parseAddr(to); err == nil {
		addr, alias = a, a
	} else {
		httpError(w, http.StatusNotFound, errors.New("设备不在线, 刷新后重试"))
		return
	}
	name := safeName(req.URL.Query().Get("name"))
	rawRoot := strings.TrimSpace(req.URL.Query().Get("root"))
	rel := req.URL.Query().Get("rel")
	if rel != "" {
		// safeName 会把空串兜底成 unnamed, 所以这里必须校验原始值
		if rawRoot == "" {
			httpError(w, http.StatusBadRequest, errors.New("缺 root"))
			return
		}
		if _, err := safeRel(rel); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
	}
	root := safeName(rawRoot)

	// 原生模式: 前端只传本机绝对路径, 后端直接读盘, 不走浏览器文件句柄。
	// 这样内嵌 WebView 里选文件也能正常发送。
	localPath := strings.TrimSpace(req.URL.Query().Get("localPath"))
	if localPath != "" {
		r.handleSendLocal(w, req, addr, alias, name, root, rel, localPath)
		return
	}

	tmp, err := os.CreateTemp("", "lanfile-up-*")
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	emitLine := func(v any) {
		json.NewEncoder(w).Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// 1. 先收完再发: 协议要求发送前就拿到 sha256
	total := req.ContentLength
	h := sha256.New()
	buf := make([]byte, chunkBuffer)
	var received int64
	last := time.Time{}
	for {
		n, rerr := req.Body.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				emitLine(map[string]any{"phase": "error", "error": werr.Error()})
				return
			}
			h.Write(buf[:n])
			received += int64(n)
			if time.Since(last) > 200*time.Millisecond {
				last = time.Now()
				emitLine(map[string]any{"phase": "receiving", "done": received, "total": total})
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			emitLine(map[string]any{"phase": "error", "error": "接收上传失败: " + rerr.Error()})
			return
		}
	}
	tmp.Sync()
	sum := hex.EncodeToString(h.Sum(nil))
	emitLine(map[string]any{"phase": "receiving", "done": received, "total": received})

	// 2. 走和 CLI 完全相同的链路转发
	res, err := transferFile(sendSpec{
		Base: "http://" + addr, Src: tmp.Name(), Name: name, Sum: sum, Root: root, Rel: rel,
	}, nil, func(done, tot int64) {
		emitLine(map[string]any{"phase": "sending", "done": done, "total": tot})
	})
	if err != nil {
		emitLine(map[string]any{"phase": "error", "error": err.Error()})
		return
	}
	log.Printf("发送完成: %s -> %s (%s, 校验通过, 耗时 %s)", name, alias, humanBytes(res.Size), res.Elapsed.Round(time.Millisecond))
	emitLine(map[string]any{
		"phase":      "done",
		"name":       res.Name,
		"sha256":     sum,
		"skipped":    res.Skipped,
		"bytes":      res.Size,
		"elapsed_ms": res.Elapsed.Milliseconds(),
	})
}

// handleSendLocal 后端直接读取本机路径发送, 用于内嵌 WebView 原生选文件。
func (r *receiver) handleSendLocal(w http.ResponseWriter, req *http.Request, addr, alias, name, root, rel, localPath string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	emitLine := func(v any) {
		json.NewEncoder(w).Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}

	st, err := os.Stat(localPath)
	if err != nil {
		emitLine(map[string]any{"phase": "error", "error": "读不到本地文件: " + err.Error()})
		return
	}
	if st.IsDir() {
		emitLine(map[string]any{"phase": "error", "error": "请选择文件, 不要选文件夹"})
		return
	}
	sum, err := hashFile(localPath)
	if err != nil {
		emitLine(map[string]any{"phase": "error", "error": "计算校验失败: " + err.Error()})
		return
	}
	emitLine(map[string]any{"phase": "receiving", "done": st.Size(), "total": st.Size()})

	res, err := transferFile(sendSpec{
		Base: "http://" + addr, Src: localPath, Name: name, Sum: sum, Root: root, Rel: rel,
	}, nil, func(done, tot int64) {
		emitLine(map[string]any{"phase": "sending", "done": done, "total": tot})
	})
	if err != nil {
		emitLine(map[string]any{"phase": "error", "error": err.Error()})
		return
	}
	log.Printf("发送完成: %s -> %s (%s, 校验通过, 耗时 %s)", name, alias, humanBytes(res.Size), res.Elapsed.Round(time.Millisecond))
	emitLine(map[string]any{
		"phase":      "done",
		"name":       res.Name,
		"sha256":     sum,
		"skipped":    res.Skipped,
		"bytes":      res.Size,
		"elapsed_ms": res.Elapsed.Milliseconds(),
	})
}
