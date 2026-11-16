package sshconn

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

// ProbeOptions 描述一次主机连通性探测的参数。
type ProbeOptions struct {
	Addr      string
	Proxy     *Proxy
	Timeout   time.Duration
	ExpectSSH bool
}

// Probe 探测目标地址的连通性与往返延迟。
// 当 ExpectSSH 为 true 时，在 TCP 建连（含可选代理隧道）后还会等待服务端返回
// RFC 4253 的 "SSH-" 版本横幅，避免透明代理/TUN（如 Surge 增强模式）在本地直接
// 完成 TCP 握手造成的假阳性“可达”。
func Probe(ctx context.Context, opts ProbeOptions) (time.Duration, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	start := time.Now()
	conn, err := dialFirst(ctx, opts.Addr, opts.Proxy, timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if !opts.ExpectSSH {
		return time.Since(start), nil
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	remaining := timeout - time.Since(start)
	if remaining < 300*time.Millisecond {
		remaining = 300 * time.Millisecond
	}
	_ = conn.SetReadDeadline(time.Now().Add(remaining))

	br := bufio.NewReader(io.LimitReader(conn, 4096))
	for range 16 {
		line, readErr := br.ReadString('\n')
		if strings.HasPrefix(line, "SSH-") {
			return time.Since(start), nil
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			var netErr net.Error
			if errors.As(readErr, &netErr) && netErr.Timeout() {
				return 0, errors.New("等待 SSH 服务响应超时")
			}
			if errors.Is(readErr, io.EOF) {
				return 0, errors.New("连接已关闭，目标端口未返回 SSH 服务响应")
			}
			return 0, errors.New("未收到有效的 SSH 服务响应")
		}
	}
	return 0, errors.New("未收到有效的 SSH 服务响应")
}
