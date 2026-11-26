package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	discoveryGroup = "224.0.0.167" // 与 LocalSend 相同的发现组播地址
	discoveryPort  = 53317
	announceEvery  = 5 * time.Second
	deviceTTL      = 30 * time.Second
)

// announceMsg 字段名刻意对齐 LocalSend 的 register 报文,
// 后续做协议互通时只需换传输方式, 不用改数据结构。
type announceMsg struct {
	App         string `json:"app"`
	Alias       string `json:"alias"`
	Version     string `json:"version"`
	DeviceModel string `json:"deviceModel"`
	DeviceType  string `json:"deviceType"`
	Fingerprint string `json:"fingerprint"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
}

type Device struct {
	Alias       string
	IP          net.IP
	Port        int
	Fingerprint string
	DeviceType  string
	LastSeen    time.Time
}

func (d Device) Addr() string {
	return net.JoinHostPort(d.IP.String(), strconv.Itoa(d.Port))
}

// Discovery 维护一份局域网内的设备表, 靠组播心跳存活
type Discovery struct {
	mu      sync.RWMutex
	devices map[string]*Device
	self    announceMsg
	conn    *net.UDPConn
	send    net.PacketConn
	done    chan struct{}
	once    sync.Once
}

func StartDiscovery(httpPort int) (*Discovery, error) {
	alias, _ := os.Hostname()
	self := announceMsg{
		App:         "lanfile",
		Alias:       alias,
		Version:     "0.5",
		DeviceModel: runtime.GOOS + "/" + runtime.GOARCH,
		DeviceType:  "desktop",
		Fingerprint: fingerprint(alias),
		Port:        httpPort,
		Protocol:    "http",
	}

	group := &net.UDPAddr{IP: net.ParseIP(discoveryGroup), Port: discoveryPort}
	conn, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		return nil, err
	}
	conn.SetReadBuffer(1 << 20)

	send, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		conn.Close()
		return nil, err
	}

	d := &Discovery{
		devices: make(map[string]*Device),
		self:    self,
		conn:    conn,
		send:    send,
		done:    make(chan struct{}),
	}
	go d.readLoop()
	go d.announceLoop()
	return d, nil
}

func (d *Discovery) Close() {
	d.once.Do(func() {
		close(d.done)
		d.conn.Close()
		d.send.Close()
	})
}

func (d *Discovery) announceLoop() {
	group := &net.UDPAddr{IP: net.ParseIP(discoveryGroup), Port: discoveryPort}
	payload, _ := json.Marshal(d.self)
	t := time.NewTicker(announceEvery)
	defer t.Stop()
	for {
		d.send.WriteTo(payload, group)
		select {
		case <-d.done:
			return
		case <-t.C:
		}
	}
}

func (d *Discovery) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, src, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var m announceMsg
		if json.Unmarshal(buf[:n], &m) != nil || m.App != "lanfile" || m.Port <= 0 {
			continue
		}
		if m.Fingerprint == d.self.Fingerprint {
			continue // 自己发的包, 忽略
		}
		d.mu.Lock()
		d.devices[m.Fingerprint] = &Device{
			Alias:       m.Alias,
			IP:          src.IP,
			Port:        m.Port,
			Fingerprint: m.Fingerprint,
			DeviceType:  m.DeviceType,
			LastSeen:    time.Now(),
		}
		d.mu.Unlock()
	}
}

// List 返回未过期的设备, 按别名排序
func (d *Discovery) List() []Device {
	d.mu.RLock()
	defer d.mu.RUnlock()
	now := time.Now()
	out := make([]Device, 0, len(d.devices))
	for _, dev := range d.devices {
		if now.Sub(dev.LastSeen) > deviceTTL {
			continue
		}
		out = append(out, *dev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// Find 支持用完整主机名或去掉 .local 的短名匹配
func (d *Discovery) Find(name string) (Device, bool) {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	for _, dev := range d.List() {
		alias := strings.ToLower(dev.Alias)
		if alias == name || strings.TrimSuffix(alias, ".local") == name {
			return dev, true
		}
	}
	return Device{}, false
}

// ByFingerprint 按指纹找设备, Web UI 用它把选择结果映射回地址
func (d *Discovery) ByFingerprint(fp string) (Device, bool) {
	for _, dev := range d.List() {
		if dev.Fingerprint == fp {
			return dev, true
		}
	}
	return Device{}, false
}

// fingerprint 由主机名 + 网卡地址派生, 重启后不变, 后续可直接用于白名单
func fingerprint(alias string) string {
	mac := ""
	if ifs, err := net.Interfaces(); err == nil {
		for _, i := range ifs {
			if len(i.HardwareAddr) >= 6 && !strings.HasPrefix(i.Name, "lo") && !strings.HasPrefix(i.Name, "utun") {
				mac = i.HardwareAddr.String()
				break
			}
		}
	}
	sum := sha256.Sum256([]byte(alias + "|" + mac))
	return hex.EncodeToString(sum[:8])
}

func portFromAddr(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return defaultPort
	}
	if n, err := strconv.Atoi(p); err == nil && n > 0 {
		return n
	}
	return defaultPort
}

func runDevices(args []string) {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	wait := fs.Duration("t", 6*time.Second, "扫描时长")
	fs.Parse(args)

	d, err := StartDiscovery(defaultPort)
	if err != nil {
		fatal(fmt.Errorf("启动设备发现失败: %w", err))
	}
	defer d.Close()

	fmt.Printf("本机  %s  (端口 %d, 指纹 %s)\n扫描  %s ...\n\n", d.self.Alias, d.self.Port, d.self.Fingerprint, *wait)
	time.Sleep(*wait)

	list := d.List()
	if len(list) == 0 {
		fmt.Println("未发现其他设备")
		fmt.Println("  1. 对方需要运行 lanfile receive 或 lanfile devices")
		fmt.Println("  2. 双方都在运行仍看不到, 说明组播不通, 用 lanfile probe 复核")
		fmt.Println("  3. 也可以直接用 IP: lanfile send <文件> <对方IP>")
		return
	}
	fmt.Printf("%-26s %-24s %-6s %s\n", "别名", "地址", "端口", "最后发现")
	for _, dev := range list {
		fmt.Printf("%-26s %-24s %-6d %s前\n", dev.Alias, dev.IP, dev.Port, time.Since(dev.LastSeen).Round(time.Second))
	}
}

// resolveAlias 短暂监听组播, 把别名解析成地址
func resolveAlias(name string) (Device, bool) {
	d, err := StartDiscovery(defaultPort)
	if err != nil {
		return Device{}, false
	}
	defer d.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if dev, ok := d.Find(name); ok {
			return dev, true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return Device{}, false
}
