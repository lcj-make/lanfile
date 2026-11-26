package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	probeTCP = 53319 // 专用于探测的 TCP 端口, 避开正式的 53318
)

type probeMsg struct {
	App   string `json:"app"`
	ID    string `json:"id"`
	Alias string `json:"alias"`
	Port  int    `json:"port"`
}

// runProbe 用来判断局域网是否开启 AP 隔离 / 组播是否可用。
// 两台机器都执行一次, 谁也没收到对方, 就是被隔离或组播被禁。
func runProbe(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	peer := fs.String("peer", "", "只做 TCP 单播测试, 直接填对方 IP")
	dur := fs.Duration("t", 20*time.Second, "等待时长")
	fs.Parse(args)

	alias, _ := os.Hostname()
	myID := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

	// 开一个临时 TCP 端口, 让对方的单播测试有一个可连的目标
	var ln net.Listener
	if l, err := net.Listen("tcp", fmt.Sprintf(":%d", probeTCP)); err == nil {
		ln = l
		defer ln.Close()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	} else {
		fmt.Printf("提示: TCP %d 被占用 (%v), 单播测试结果可能不准\n", probeTCP, err)
	}

	if *peer != "" {
		fmt.Printf("本机     %s\n对方     %s\n\n", strings.Join(localIPv4(), ", "), *peer)
		if testUnicast(*peer) {
			fmt.Println("\n结论: TCP 单播可用, 对方可达 (未做组播测试)")
		} else {
			fmt.Println("\n结论: TCP 单播不通 —— 对方没运行 probe、被防火墙拦截, 或已开启 AP 隔离")
		}
		return
	}

	group := &net.UDPAddr{IP: net.ParseIP(discoveryGroup), Port: discoveryPort}
	conn, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		fatal(fmt.Errorf("加入组播失败: %w", err))
	}
	defer conn.Close()
	conn.SetReadBuffer(1 << 20)

	send, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		fatal(fmt.Errorf("创建发送套接字失败: %w", err))
	}
	defer send.Close()

	fmt.Printf("本机     %s  (%s)\n", strings.Join(localIPv4(), ", "), alias)
	fmt.Printf("组播组   %s:%d\n等待     %s\n\n", discoveryGroup, discoveryPort, *dur)
	fmt.Println("请在另一台机器上执行: lanfile probe")
	fmt.Println("----------------------------------------")

	deadline := time.Now().Add(*dur)
	payload, _ := json.Marshal(probeMsg{App: "lanfile-probe", ID: myID, Alias: alias, Port: probeTCP})

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			if time.Now().After(deadline) {
				return
			}
			send.WriteTo(payload, group)
			<-t.C
		}
	}()

	seen := map[string]bool{}
	uniOK := 0
	conn.SetReadDeadline(deadline)
	buf := make([]byte, 2048)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // 读超时即结束
		}
		var m probeMsg
		if json.Unmarshal(buf[:n], &m) != nil || m.App != "lanfile-probe" || m.ID == myID {
			continue
		}
		if seen[src.IP.String()] {
			continue
		}
		seen[src.IP.String()] = true
		fmt.Printf("[组播可达] %-15s  %s\n", src.IP, m.Alias)
		if testUnicast(src.IP.String()) {
			fmt.Printf("[单播可达] %-15s  TCP %d\n", src.IP, probeTCP)
			uniOK++
		}
	}

	fmt.Println("----------------------------------------")
	switch {
	case len(seen) == 0:
		fmt.Println("结论: 没有收到任何组播发现包")
		fmt.Println("  1. 先确认对方也在运行 probe (单方运行看不到结果)")
		fmt.Println("  2. 双方都在跑仍无输出, 基本可判定 AP 隔离或组播被禁")
		fmt.Println("  3. 再用 lanfile probe -peer <对方IP> 确认单播是否通")
	case uniOK == len(seen):
		fmt.Printf("结论: 发现 %d 台设备, 组播与单播都通 —— 未开启 AP 隔离, M2 可直接做自动发现\n", len(seen))
	default:
		fmt.Printf("结论: 发现 %d 台设备, 其中 %d 台单播不通 —— 组播可用但 TCP 被拦\n", len(seen), len(seen)-uniOK)
	}
}

func testUnicast(ip string) bool {
	target := net.JoinHostPort(ip, strconv.Itoa(probeTCP))
	c, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		fmt.Printf("[单播不通] %-15s  TCP %d (%v)\n", ip, probeTCP, err)
		return false
	}
	c.Close()
	return true
}

// localIPv4 列出本机可用于局域网通信的 IPv4 地址
func localIPv4() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return []string{"未知"}
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ip.String())
	}
	if len(out) == 0 {
		return []string{"未知"}
	}
	return out
}
