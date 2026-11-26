package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func runSend(args []string) {
	// 不用 flag 包: 它遇到第一个位置参数就停止解析, 而 -name 通常写在最后
	var name string
	var pos []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-name" || args[i] == "--name":
			if i+1 < len(args) {
				i++
				name = args[i]
			}
		case strings.HasPrefix(args[i], "-name="):
			name = strings.TrimPrefix(args[i], "-name=")
		case strings.HasPrefix(args[i], "--name="):
			name = strings.TrimPrefix(args[i], "--name=")
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) < 2 {
		fmt.Fprintln(os.Stderr, "用法: lanfile send <文件> <主机[:端口]> [-name 目标文件名]")
		os.Exit(2)
	}
	src := pos[0]
	target := pos[1]
	// 不像 IP、也不带端口, 就当成设备别名去组播里找
	if net.ParseIP(target) == nil && !strings.Contains(target, ":") && target != "localhost" {
		dev, ok := resolveAlias(target)
		if !ok {
			fatal(fmt.Errorf("局域网内没找到设备 %q\n可用 lanfile devices 查看, 或直接写 IP: lanfile send <文件> 192.168.1.20", target))
		}
		target = net.JoinHostPort(dev.IP.String(), strconv.Itoa(dev.Port))
		fmt.Printf("已解析  %s -> %s\n", pos[1], target)
	}
	base := normalizeTarget(target)
	dstName := name
	if dstName == "" {
		dstName = filepath.Base(src)
	}

	st, err := os.Stat(src)
	if err != nil {
		fatal(err)
	}
	if st.IsDir() {
		runSendDir(base, src, dstName)
		return
	}

	fmt.Printf("源文件  %s (%s)\n目标    %s/%s\n", src, humanBytes(st.Size()), base, dstName)

	fmt.Fprint(os.Stderr, "计算 sha256 ...")
	sum, err := hashFile(src)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "\rsha256  %s\n", sum)

	res, err := transferFile(sendSpec{Base: base, Src: src, Name: dstName, Sum: sum},
		func(msg string) { fmt.Println(msg) },
		func(done, total int64) {
			if total <= 0 {
				return
			}
			fmt.Fprintf(os.Stderr, "\r  %5.1f%%  %s / %s",
				float64(done)*100/float64(total), humanBytes(done), humanBytes(total))
		})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fatal(err)
	}
	if res.Skipped {
		fmt.Println("目标端已存在同名且校验一致的文件, 无需传输")
		return
	}
	fmt.Printf("完成    %s -> %s\n耗时    %s\n本次速率 %s/s\n校验    sha256 双端一致\n",
		humanBytes(res.Size), res.Name, res.Elapsed.Round(time.Millisecond),
		humanBytes(int64(float64(res.Transferred)/res.Elapsed.Seconds())))
}

type sendResult struct {
	Name        string
	Size        int64
	Transferred int64
	Offset      int64
	Elapsed     time.Duration
	Skipped     bool
}

type sendSpec struct {
	Base   string       // http://host:port
	Src    string       // 本地文件路径
	Name   string       // 目标文件名
	Sum    string       // 源文件 sha256
	Root   string       // 文件夹名, 单文件为空
	Rel    string       // 相对根目录的路径, 单文件为空
	Preset *prepareResp // 已在清单里协商好时直接复用, 省一次往返
}

// transferFile 是发送的核心: 握手取偏移 -> 前缀校验 -> 推流 -> 双端比对。
// CLI、文件夹传输和 Web UI 共用这一份实现, 避免多套逻辑走偏。
func transferFile(spec sendSpec, onEvent func(string), onProgress func(done, total int64)) (sendResult, error) {
	var res sendResult
	f, err := os.Open(spec.Src)
	if err != nil {
		return res, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return res, err
	}
	if st.IsDir() {
		return res, errors.New("暂只支持单文件")
	}
	size := st.Size()
	res.Size = size
	res.Name = spec.Name

	// 1. 握手: 目标端告知是否已存在、能否续传
	var pr prepareResp
	if spec.Preset != nil {
		pr = *spec.Preset
	} else {
		pr, err = doPrepare(spec.Base, prepareReq{
			Name: spec.Name, Size: size, SHA256: spec.Sum, Root: spec.Root, Rel: spec.Rel,
		})
		if err != nil {
			return res, err
		}
	}
	if pr.Complete {
		res.Skipped = true
		return res, nil
	}
	if pr.Offset > size {
		return res, fmt.Errorf("目标端偏移 %d 超过文件大小 %d", pr.Offset, size)
	}

	if pr.Offset > 0 {
		// 续传前先确认目标端已落盘的那一段和当前源文件前缀一致
		localPrefix, err := hashPrefix(spec.Src, pr.Offset)
		if err != nil {
			return res, err
		}
		if pr.PrefixSHA256 != "" && !strings.EqualFold(localPrefix, pr.PrefixSHA256) {
			emit(onEvent, fmt.Sprintf("前缀不符: 目标端已收的 %s 与当前源文件不一致, 丢弃后从头重传", humanBytes(pr.Offset)))
			if err := doReset(spec.Base, transferID(spec.Root, spec.Rel, spec.Sum)); err != nil {
				return res, err
			}
			pr.Offset = 0
		}
	}
	res.Offset = pr.Offset
	if pr.Offset > 0 {
		emit(onEvent, fmt.Sprintf("续传: 从 %s 处继续, 剩余 %s", humanBytes(pr.Offset), humanBytes(size-pr.Offset)))
	} else {
		emit(onEvent, "开始传输")
	}

	// 2. 从偏移处把剩余字节原样推过去
	remaining := size - pr.Offset
	if _, err := f.Seek(pr.Offset, io.SeekStart); err != nil {
		return res, err
	}
	body := &progressReader{r: io.LimitReader(f, remaining), total: remaining, started: time.Now(), onProgress: onProgress}
	url := fmt.Sprintf("%s/api/upload/%s?offset=%d", spec.Base, transferID(spec.Root, spec.Rel, spec.Sum), pr.Offset)
	req, err := http.NewRequest(http.MethodPut, url, body)
	if err != nil {
		return res, err
	}
	req.ContentLength = remaining
	req.Header.Set("Content-Type", "application/octet-stream")

	start := time.Now()
	hr, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, fmt.Errorf("传输中断: %w\n已接收的数据保留在目标端, 重新执行即可续传", err)
	}
	defer hr.Body.Close()

	if hr.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(hr.Body).Decode(&e)
		return res, fmt.Errorf("目标端拒绝 (HTTP %d): %s", hr.StatusCode, e.Error)
	}
	var out uploadResp
	if err := json.NewDecoder(hr.Body).Decode(&out); err != nil {
		return res, fmt.Errorf("解析目标端响应失败: %w", err)
	}
	body.report() // 进度补到 100%

	// 3. 双端哈希比对
	if !out.OK {
		return res, errors.New("目标端未确认接收")
	}
	if !strings.EqualFold(out.SHA256, spec.Sum) {
		return res, fmt.Errorf("校验失败: 本地 %s, 目标端 %s", spec.Sum, out.SHA256)
	}
	res.Transferred = body.done
	res.Elapsed = time.Since(start)
	res.Name = out.Name
	return res, nil
}

func emit(f func(string), msg string) {
	if f != nil {
		f(msg)
	}
}

type dirEntry struct {
	rel  string
	path string
	size int64
	sum  string
}

type dirPrepareResp struct {
	Root  string        `json:"root"`
	Files []prepareResp `json:"files"`
}

func doPrepareDir(base string, in dirPrepareReq) (dirPrepareResp, error) {
	var resp dirPrepareResp
	b, err := json.Marshal(in)
	if err != nil {
		return resp, err
	}
	hr, err := http.Post(base+"/api/prepare-dir", "application/json", bytes.NewReader(b))
	if err != nil {
		return resp, fmt.Errorf("连接目标失败: %w", err)
	}
	defer hr.Body.Close()
	if hr.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(hr.Body).Decode(&e)
		return resp, fmt.Errorf("目标端拒绝 (HTTP %d): %s", hr.StatusCode, e.Error)
	}
	if err := json.NewDecoder(hr.Body).Decode(&resp); err != nil {
		return resp, fmt.Errorf("解析目录清单响应失败: %w", err)
	}
	return resp, nil
}

// collectDir 走一遍目录, 收集普通文件和所有子目录。
// 符号链接不跟随: 跟过去就不再是"原样传输"了。
func collectDir(root string) ([]dirEntry, []string, error) {
	var entries []dirEntry
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			dirs = append(dirs, rel)
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			fmt.Printf("跳过符号链接: %s\n", rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			fmt.Printf("跳过特殊文件: %s\n", rel)
			return nil
		}
		entries = append(entries, dirEntry{rel: rel, path: p, size: info.Size()})
		return nil
	})
	return entries, dirs, err
}

// runSendDir 先算指纹、交清单, 再逐文件推流。
// 单个文件失败不中断整体, 最后统一汇报。
func runSendDir(base, src, rootName string) {
	if rootName == "" {
		rootName = filepath.Base(src)
	}
	entries, dirs, err := collectDir(src)
	if err != nil {
		fatal(err)
	}
	if len(entries) == 0 {
		fatal(errors.New("目录里没有可传的普通文件"))
	}
	var total int64
	for _, e := range entries {
		total += e.size
	}
	fmt.Printf("目录    %s -> %s/ (%d 个文件, %s)\n", src, rootName, len(entries), humanBytes(total))

	hashFailed := 0
	for i := range entries {
		e := &entries[i]
		fmt.Fprintf(os.Stderr, "\r计算 sha256  %d/%d  %s", i+1, len(entries), e.rel)
		sum, err := hashFile(e.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n跳过 %s: %v\n", e.rel, err)
			hashFailed++
			continue
		}
		e.sum = sum
	}
	fmt.Fprintln(os.Stderr)

	req := dirPrepareReq{Root: rootName, Dirs: dirs}
	for _, e := range entries {
		if e.sum == "" {
			continue
		}
		req.Files = append(req.Files, dirFile{
			Name: filepath.Base(e.rel), Rel: e.rel, Size: e.size, SHA256: e.sum,
		})
	}
	plan, err := doPrepareDir(base, req)
	if err != nil {
		fatal(err)
	}

	// plan.Files 与 req.Files 一一对应, 按 rel 建索引方便回查
	preset := make(map[string]prepareResp, len(plan.Files))
	for i, f := range req.Files {
		if i < len(plan.Files) {
			preset[f.Rel] = plan.Files[i]
		}
	}

	var skipped, failed, sentCount int
	var sentBytes int64
	start := time.Now()
	for i, e := range entries {
		if e.sum == "" {
			failed++
			continue
		}
		p, ok := preset[e.rel]
		if ok && p.Complete {
			skipped++
			continue
		}
		var presetPtr *prepareResp
		if ok {
			pp := p
			presetPtr = &pp
		}
		res, err := transferFile(sendSpec{
			Base: base, Src: e.path, Name: filepath.Base(e.rel), Sum: e.sum,
			Root: rootName, Rel: e.rel, Preset: presetPtr,
		}, nil, func(done, tot int64) {
			if tot <= 0 {
				return
			}
			fmt.Fprintf(os.Stderr, "\r[%d/%d] %s  %5.1f%%", i+1, len(entries), e.rel, float64(done)*100/float64(tot))
		})
		fmt.Fprint(os.Stderr, "\r")
		if err != nil {
			failed++
			fmt.Printf("[%d/%d] 失败 %s: %v\n", i+1, len(entries), e.rel, err)
			continue
		}
		sentCount++
		sentBytes += res.Transferred
	}

	el := time.Since(start)
	fmt.Printf("完成    %d 个已传, %d 个跳过, %d 个失败\n", sentCount, skipped, failed+hashFailed)
	if el.Seconds() > 0.2 {
		fmt.Printf("耗时    %s, 平均 %s/s\n", el.Round(time.Millisecond),
			humanBytes(int64(float64(sentBytes)/el.Seconds())))
	}
	if failed+hashFailed > 0 {
		os.Exit(1)
	}
}

func doPrepare(base string, in prepareReq) (prepareResp, error) {
	var out prepareResp
	b, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	hr, err := http.Post(base+"/api/prepare", "application/json", bytes.NewReader(b))
	if err != nil {
		return out, fmt.Errorf("无法连接 %s: %w", base, err)
	}
	defer hr.Body.Close()
	if hr.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(hr.Body).Decode(&e)
		return out, fmt.Errorf("目标端拒绝 (HTTP %d): %s", hr.StatusCode, e.Error)
	}
	err = json.NewDecoder(hr.Body).Decode(&out)
	return out, err
}

// doReset 让目标端丢弃已收的部分数据
func doReset(base, id string) error {
	req, err := http.NewRequest(http.MethodPost, base+"/api/reset/"+id, nil)
	if err != nil {
		return err
	}
	hr, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer hr.Body.Close()
	if hr.StatusCode != http.StatusOK {
		return fmt.Errorf("重置失败 (HTTP %d)", hr.StatusCode)
	}
	return nil
}

// normalizeTarget 允许传 192.168.1.20、192.168.1.20:53318 或完整 URL。
// 只给 IP 时先探测对端真实端口, 探测成功用对端端口, 失败再回退默认端口。
func normalizeTarget(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "http://" + s
	}
	hostPart := strings.SplitN(strings.SplitN(s, "://", 2)[1], "/", 2)[0]
	scheme := strings.SplitN(s, "://", 2)[0]
	if !strings.Contains(hostPart, ":") {
		if real, _, err := probePeer(net.JoinHostPort(hostPart, "0")); err == nil {
			hostPart = real
		} else {
			hostPart = net.JoinHostPort(hostPart, strconv.Itoa(defaultPort))
		}
	}
	return scheme + "://" + strings.TrimSuffix(hostPart, "/")
}

// progressReader 边传边回调进度, 具体怎么显示由调用方决定
type progressReader struct {
	r          io.Reader
	total      int64
	done       int64
	started    time.Time
	last       time.Time
	onProgress func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if time.Since(p.last) > 150*time.Millisecond {
		p.last = time.Now()
		p.report()
	}
	return n, err
}

func (p *progressReader) report() {
	if p.total <= 0 || p.onProgress == nil {
		return
	}
	p.onProgress(p.done, p.total)
}
