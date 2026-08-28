package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gotd/td/telegram/dcs"
	"golang.org/x/net/proxy"
)

// DialFunc 根据代理 URL 构建 MTProto 拨号函数（dcs.DialFunc）
// 支持: http:// https:// socks5:// socks5h:// socks4://
func DialFunc(proxyURL string) (dcs.DialFunc, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("代理 URL 解析失败 %q: %w", proxyURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return httpConnectDialer(u), nil
	case "socks5", "socks5h", "socks4", "socks4a":
		d, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks 代理初始化: %w", err)
		}
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			if cd, ok := d.(proxy.ContextDialer); ok {
				return cd.DialContext(ctx, network, addr)
			}
			return d.Dial(network, addr)
		}, nil
	}
	return nil, fmt.Errorf("不支持的代理协议 %q（支持 http/https/socks5/socks4）", u.Scheme)
}

// httpConnectDialer HTTP 代理 CONNECT 隧道拨号
func httpConnectDialer(u *url.URL) dcs.DialFunc {
	proxyAddr := u.Host
	if u.Port() == "" {
		proxyAddr = net.JoinHostPort(u.Hostname(), "80")
	}
	auth := ""
	if u.User != nil {
		pwd, _ := u.User.Password()
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pwd))
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("连接代理 %s: %w", proxyAddr, err)
		}
		req := &http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Opaque: addr},
			Host:   addr,
			Header: make(http.Header),
		}
		if auth != "" {
			req.Header.Set("Proxy-Authorization", auth)
		}
		if err := req.Write(conn); err != nil {
			conn.Close()
			return nil, fmt.Errorf("发送 CONNECT: %w", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("代理响应异常: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("代理 CONNECT 被拒绝: %s", resp.Status)
		}
		return conn, nil
	}
}
