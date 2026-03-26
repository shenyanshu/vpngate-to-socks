package runner

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	socksVersion5       = 0x05
	socksMethodNoAuth   = 0x00
	socksMethodNotFound = 0xFF
	socksCommandConnect = 0x01

	socksReplySuccess           = 0x00
	socksReplyGeneralFailure    = 0x01
	socksReplyConnectionDenied  = 0x02
	socksReplyCommandNotSupport = 0x07
	socksReplyAddrNotSupport    = 0x08
)

type SOCKSServer struct {
	logger       *log.Logger
	listenAddr   string
	listener     net.Listener
	allowConnect func() bool
}

func newSOCKSServer(logger *log.Logger, listenAddr string, allowConnect func() bool) (*SOCKSServer, error) {
	if listenAddr == "" {
		listenAddr = "0.0.0.0:1080"
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("启动 SOCKS5 监听失败: %w", err)
	}

	s := &SOCKSServer{
		logger:       logger,
		listenAddr:   listenAddr,
		listener:     listener,
		allowConnect: allowConnect,
	}

	go s.serve()
	logger.Printf("SOCKS5 代理监听已启动：%s", listener.Addr().String())
	return s, nil
}

func (s *SOCKSServer) ListenAddr() string {
	return s.listenAddr
}

func (s *SOCKSServer) Close() error {
	if s.listener == nil {
		return nil
	}

	return s.listener.Close()
}

func (s *SOCKSServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}

			s.logger.Printf("SOCKS5 Accept 失败：%v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		s.logger.Printf("SOCKS5 收到连接：%s -> %s", conn.RemoteAddr().String(), conn.LocalAddr().String())

		go s.handleConn(conn)
	}
}

func (s *SOCKSServer) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := s.negotiate(conn); err != nil {
		s.logger.Printf("SOCKS5 连接处理失败：%v", err)
	}
}

func (s *SOCKSServer) negotiate(conn net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("读取握手头失败: %w", err)
	}
	s.logger.Printf("SOCKS5 握手开始：版本=%d 方法数=%d 来源=%s", header[0], header[1], conn.RemoteAddr().String())

	if header[0] != socksVersion5 {
		return fmt.Errorf("不支持的 SOCKS 版本: %d", header[0])
	}

	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("读取认证方法失败: %w", err)
	}

	accepted := slices.Contains(methods, byte(socksMethodNoAuth))
	s.logger.Printf("SOCKS5 客户端认证方法：%v", methods)

	if !accepted {
		_, _ = conn.Write([]byte{socksVersion5, socksMethodNotFound})
		return fmt.Errorf("客户端不支持无认证 SOCKS5")
	}

	if _, err := conn.Write([]byte{socksVersion5, socksMethodNoAuth}); err != nil {
		return fmt.Errorf("写入握手响应失败: %w", err)
	}

	requestHead := make([]byte, 4)
	if _, err := io.ReadFull(conn, requestHead); err != nil {
		return fmt.Errorf("读取请求头失败: %w", err)
	}

	if requestHead[0] != socksVersion5 {
		return fmt.Errorf("请求中的 SOCKS 版本不正确: %d", requestHead[0])
	}

	if requestHead[1] != socksCommandConnect {
		_ = writeSOCKSReply(conn, socksReplyCommandNotSupport)
		return fmt.Errorf("不支持的 SOCKS 命令: %d", requestHead[1])
	}

	target, err := readSOCKSTarget(conn, requestHead[3])
	if err != nil {
		_ = writeSOCKSReply(conn, socksReplyAddrNotSupport)
		return err
	}
	s.logger.Printf("SOCKS5 请求目标：%s", target)

	if s.allowConnect != nil && !s.allowConnect() {
		_ = writeSOCKSReply(conn, socksReplyConnectionDenied)
		return fmt.Errorf("当前 VPN 未连接，拒绝代理到 %s", target)
	}

	remoteConn, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		_ = writeSOCKSReply(conn, socksReplyGeneralFailure)
		return fmt.Errorf("连接目标 %s 失败: %w", target, err)
	}
	defer remoteConn.Close()
	s.logger.Printf("SOCKS5 CONNECT 已建立：%s -> %s", conn.RemoteAddr().String(), target)

	if _, err := conn.Write(buildSOCKSSuccessReply()); err != nil {
		return fmt.Errorf("写入成功响应失败: %w", err)
	}

	_ = conn.SetDeadline(time.Time{})
	_ = remoteConn.SetDeadline(time.Time{})

	errCh := make(chan error, 2)
	go relayTCP(errCh, remoteConn, conn)
	go relayTCP(errCh, conn, remoteConn)

	var firstErr error
	for i := 0; i < 2; i++ {
		if proxyErr := <-errCh; proxyErr != nil && !isIgnorableProxyError(proxyErr) && firstErr == nil {
			firstErr = proxyErr
		}
	}

	if firstErr != nil {
		return firstErr
	}

	return nil
}

func relayTCP(errCh chan<- error, dst net.Conn, src net.Conn) {
	_, err := io.Copy(dst, src)
	halfCloseWrite(dst)
	halfCloseRead(src)
	errCh <- err
}

func readSOCKSTarget(conn net.Conn, atyp byte) (string, error) {
	switch atyp {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", fmt.Errorf("读取 IPv4 地址失败: %w", err)
		}
		port, err := readSOCKSPort(conn)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(net.IP(buf).String(), strconv.Itoa(port)), nil
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", fmt.Errorf("读取域名长度失败: %w", err)
		}
		buf := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", fmt.Errorf("读取域名失败: %w", err)
		}
		port, err := readSOCKSPort(conn)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(string(buf), strconv.Itoa(port)), nil
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", fmt.Errorf("读取 IPv6 地址失败: %w", err)
		}
		port, err := readSOCKSPort(conn)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(net.IP(buf).String(), strconv.Itoa(port)), nil
	default:
		return "", fmt.Errorf("不支持的目标地址类型: %d", atyp)
	}
}

func readSOCKSPort(conn net.Conn) (int, error) {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return 0, fmt.Errorf("读取目标端口失败: %w", err)
	}

	return int(binary.BigEndian.Uint16(buf)), nil
}

func writeSOCKSReply(conn net.Conn, code byte) error {
	_, err := conn.Write([]byte{socksVersion5, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}

func buildSOCKSSuccessReply() []byte {
	return []byte{socksVersion5, socksReplySuccess, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
}

func halfCloseWrite(conn net.Conn) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}

	_ = tcpConn.CloseWrite()
}

func halfCloseRead(conn net.Conn) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}

	_ = tcpConn.CloseRead()
}

func isIgnorableProxyError(err error) bool {
	if err == nil || err == io.EOF {
		return true
	}

	message := strings.ToLower(strings.TrimSpace(err.Error()))
	ignorable := []string{
		"use of closed network connection",
		"broken pipe",
		"connection reset by peer",
	}

	for _, marker := range ignorable {
		if strings.Contains(message, marker) {
			return true
		}
	}

	return false
}
