// Package netinfo 提供真实的网络探测能力：局域网地址枚举、端口占用检测、
// 安全建议生成。所有结论都来自实际系统调用，不做任何推测性断言。
package netinfo

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// Address 是一个可供其他设备访问的地址。
type Address struct {
	IP         string `json:"ip"`
	Family     string `json:"family"` // ipv4 | ipv6
	Iface      string `json:"iface"`
	URL        string `json:"url"`
	IsLoopback bool   `json:"isLoopback"`
	// Recommended 表示推荐的手机扫码地址：优先私有网段 IPv4。
	Recommended bool `json:"recommended"`
}

// LANAddresses 枚举本机所有可用于局域网访问的地址。
//
// 只返回已启用、非虚拟回环接口上的全局单播地址；链路本地地址
// （169.254.x.x / fe80::）对普通用户没有意义，排除以免误导。
func LANAddresses(port int) []Address {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := []Address{}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		loopback := ifi.Flags&net.FlagLoopback != 0
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			if !loopback && !ip.IsGlobalUnicast() {
				continue
			}
			family := "ipv6"
			if ip.To4() != nil {
				family = "ipv4"
				ip = ip.To4()
			}
			out = append(out, Address{
				IP:         ip.String(),
				Family:     family,
				Iface:      ifi.Name,
				URL:        BuildURL(ip.String(), family, port),
				IsLoopback: loopback,
			})
		}
	}
	// 推荐地址排序：私有 IPv4 > 其它 IPv4 > IPv6，回环永远排最后。
	sort.SliceStable(out, func(i, j int) bool {
		return rank(out[i]) < rank(out[j])
	})
	for i := range out {
		if i == 0 && !out[i].IsLoopback {
			out[i].Recommended = true
			break
		}
	}
	return out
}

// BuildURL 构造访问地址，IPv6 需要方括号。
func BuildURL(ip, family string, port int) string {
	if family == "ipv6" {
		return fmt.Sprintf("http://[%s]:%d", ip, port)
	}
	return fmt.Sprintf("http://%s:%d", ip, port)
}

func rank(a Address) int {
	if a.IsLoopback {
		return 3
	}
	if a.Family != "ipv4" {
		return 2
	}
	if isPrivateV4(a.IP) {
		return 0
	}
	return 1
}

func isPrivateV4(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil {
		return false
	}
	return p.IsPrivate()
}

// PrimaryURL 返回最适合展示给用户扫描/复制的地址。
func PrimaryURL(port int) string {
	list := LANAddresses(port)
	for _, a := range list {
		if !a.IsLoopback {
			return a.URL
		}
	}
	return BuildURL("127.0.0.1", "ipv4", port)
}

// IsPortFree 检测端口是否可绑定。仅返回事实，不推断占用者身份。
func IsPortFree(addr string, port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", addr, port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// DescribePortUse 生成端口占用的可读说明（不猜测具体进程）。
func DescribePortUse(addr string, port int) string {
	if IsPortFree(addr, port) {
		return fmt.Sprintf("端口 %d 当前可用", port)
	}
	return fmt.Sprintf("端口 %d 已被占用。请关闭占用该端口的程序，或在设置中改用其它端口（例如 %d）。", port, port+1)
}

// Tips 按真实检测结果生成针对性的排查建议。
// 任何一条建议都对应页面上的实际检测项，不输出无法验证的结论。
func Tips(port int, listening bool, tcpReachable bool) []string {
	tips := []string{}
	if !listening {
		tips = append(tips, "服务未在监听：请确认进程仍在运行且端口未被其它程序占用。")
	}
	if len(LANAddresses(port)) == 0 {
		tips = append(tips, "未检测到局域网地址：设备可能没有连接到任何网络（仅有回环地址）。")
	}
	if !tcpReachable {
		tips = append(tips, "本机自检未能连上服务端口：可能是防火墙拦截了入站连接。请在系统防火墙中允许本程序的专用网络访问。")
	}
	tips = append(tips,
		"如果手机扫描二维码后打不开：请确认手机与这台电脑连的是同一个 Wi-Fi，且该 Wi-Fi 未开启「AP 隔离 / 访客网络隔离」。",
		"如果两台设备在不同网段（例如一个 192.168.1.x、一个 192.168.0.x）：请把它们接入同一路由器，或使用其中一个地址手动访问。",
		"如果公司网络限制设备互访：请改用手机热点，让发送方与接收方都连到该热点后重试。",
		"如果页面能打开但传输中断：请检查是否开启了代理或 VPN，它们可能改变局域网内的路由。",
	)
	return tips
}

// SelfTest 执行本机自检：能否连上自己的服务端口。
//
// 这是真实拨号，不是模拟。返回 false 说明即使本机也无法建立 TCP 连接，
// 通常意味着监听地址绑定到了非回环地址而防火墙拦截了回环之外的路径。
func SelfTest(host string, port int) (bool, string) {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	conn, err := net.DialTimeout("tcp", addr, 1500*time.Millisecond)
	if err != nil {
		return false, err.Error()
	}
	_ = conn.Close()
	return true, ""
}

// NormalizeListenAddr 把监听地址整理成可用于 net.Listen 的形式。
func NormalizeListenAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "0.0.0.0"
	}
	return addr
}

// InterfaceSummary 返回接口概览字符串，供诊断信息复制。
func InterfaceSummary(port int) string {
	var b strings.Builder
	for _, a := range LANAddresses(port) {
		fmt.Fprintf(&b, "%s (%s, %s)\n", a.URL, a.Family, a.Iface)
	}
	if b.Len() == 0 {
		return "无可用网络接口\n"
	}
	return b.String()
}
