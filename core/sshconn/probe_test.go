package sshconn

import (
	"context"
	"net"
	"testing"
	"time"

	"serval/internal/devproxy"
	"serval/internal/devsshd"
)

func TestProbeSSHRequiresBanner(t *testing.T) {
	server := startServer(t, devsshd.Options{})

	rtt, err := Probe(context.Background(), ProbeOptions{
		Addr:      server.Addr().String(),
		Timeout:   2 * time.Second,
		ExpectSSH: true,
	})
	if err != nil {
		t.Fatalf("真实 SSH 服务应探测成功: %v", err)
	}
	if rtt <= 0 {
		t.Fatalf("RTT 应大于 0，实际 %v", rtt)
	}

	// 模拟 Surge / 透明代理：TCP 握手能通，但服务端/上游立即关闭且不返回 SSH 横幅。
	silentLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silentLn.Close()
	go func() {
		for {
			c, err := silentLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	if _, err := Probe(context.Background(), ProbeOptions{
		Addr:      silentLn.Addr().String(),
		Timeout:   time.Second,
		ExpectSSH: true,
	}); err == nil {
		t.Fatal("未返回 SSH 横幅的 TCP 端口不应判定为可达")
	}

	// 非 SSH（如 Telnet）只要求 TCP 建连时仍应成功。
	if rtt, err := Probe(context.Background(), ProbeOptions{
		Addr:      silentLn.Addr().String(),
		Timeout:   time.Second,
		ExpectSSH: false,
	}); err != nil {
		t.Fatalf("ExpectSSH=false 应只校验 TCP 连通: %v", err)
	} else if rtt <= 0 {
		t.Fatalf("ExpectSSH=false 成功时 RTT 也应大于 0，实际 %v", rtt)
	}

	// 非 SSH 协议响应（例如 HTTP）也应被识别为不可达。
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer httpLn.Close()
	go func() {
		for {
			c, err := httpLn.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
			_ = c.Close()
		}
	}()
	if _, err := Probe(context.Background(), ProbeOptions{
		Addr:      httpLn.Addr().String(),
		Timeout:   time.Second,
		ExpectSSH: true,
	}); err == nil {
		t.Fatal("返回非 SSH 横幅的端口不应判定为可达")
	}
}

func TestProbeThroughProxyChecksTargetPort(t *testing.T) {
	server := startServer(t, devsshd.Options{})
	socks, err := devproxy.StartSOCKS5("", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { socks.Close() })

	if _, err := Probe(context.Background(), ProbeOptions{
		Addr:      server.Addr().String(),
		Proxy:     &Proxy{Kind: "socks5", Addr: socks.Addr()},
		Timeout:   2 * time.Second,
		ExpectSSH: true,
	}); err != nil {
		t.Fatalf("经代理探测真实 SSH 目标失败: %v", err)
	}

	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := deadLn.Addr().String()
	_ = deadLn.Close()

	// 即使代理本身在线，目标端口不可达也必须失败。
	if _, err := Probe(context.Background(), ProbeOptions{
		Addr:      deadAddr,
		Proxy:     &Proxy{Kind: "socks5", Addr: socks.Addr()},
		Timeout:   time.Second,
		ExpectSSH: true,
	}); err == nil {
		t.Fatal("代理在线但目标端口不可达时，探测必须失败")
	}
}
