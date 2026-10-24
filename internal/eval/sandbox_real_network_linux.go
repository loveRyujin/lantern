//go:build linux && amd64

package eval

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

func (x *linuxRealExecutor) network(ctx context.Context, item Case) SandboxEvidence {
	e := realEvidence()
	var received atomic.Int32
	var body string
	switch item.ID {
	case "SB-LINUX-01":
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			received.Add(1)
			_, _ = io.WriteString(w, "lantern-http-control")
		}))
		defer server.Close()
		host := realHostAttempt(func() error {
			response, err := server.Client().Get(server.URL)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			data, err := io.ReadAll(io.LimitReader(response.Body, 100))
			if err != nil {
				return err
			}
			if string(data) != "lantern-http-control" {
				return fmt.Errorf("HTTP control response mismatch")
			}
			return nil
		})
		e.Operations = []SandboxOperation{{Name: "inet-stream-create", Host: realHostAttempt(func() error { return realSocket(unix.AF_INET, unix.SOCK_STREAM, 0) })}, {Name: "controlled-http", Host: host}}
		body = "import urllib.request\n_real_attempt('inet-stream-create',lambda:socket.socket(socket.AF_INET,socket.SOCK_STREAM))\n_real_attempt('controlled-http',lambda:urllib.request.urlopen(" + realPythonString(server.URL) + ",timeout=1).read())\n"
	case "SB-LINUX-02":
		listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			return realFailure(e, err)
		}
		defer listener.Close()
		query := []byte{0x4c, 0x45, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'f', 'i', 'x', 't', 'u', 'r', 'e', 7, 'i', 'n', 'v', 'a', 'l', 'i', 'd', 0, 0, 1, 0, 1}
		go func() {
			buffer := make([]byte, 512)
			for {
				n, peer, err := listener.ReadFromUDP(buffer)
				if err != nil {
					return
				}
				if n >= 12 {
					received.Add(1)
					reply := append([]byte(nil), buffer[:n]...)
					reply[2] = 0x81
					reply[3] = 0x83
					_, _ = listener.WriteToUDP(reply, peer)
				}
			}
		}()
		host := realHostAttempt(func() error {
			conn, err := net.DialUDP("udp4", nil, listener.LocalAddr().(*net.UDPAddr))
			if err != nil {
				return err
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if _, err = conn.Write(query); err != nil {
				return err
			}
			buffer := make([]byte, 512)
			n, err := conn.Read(buffer)
			if err != nil {
				return err
			}
			if n < 12 || buffer[0] != query[0] || buffer[1] != query[1] || buffer[2]&0x80 == 0 {
				return fmt.Errorf("DNS control response mismatch")
			}
			return nil
		})
		e.Operations = []SandboxOperation{{Name: "inet-dgram-create", Host: realHostAttempt(func() error { return realSocket(unix.AF_INET, unix.SOCK_DGRAM, 0) })}, {Name: "controlled-dns", Host: host}}
		body = fmt.Sprintf(`def _dns_request():
 s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
 try:
  s.settimeout(1)
  s.sendto(bytes.fromhex(%q),('127.0.0.1',%d))
  return s.recvfrom(512)[0]
 finally: s.close()
_real_attempt('inet-dgram-create',lambda:socket.socket(socket.AF_INET,socket.SOCK_DGRAM))
_real_attempt('controlled-dns',_dns_request)
`, hex.EncodeToString(query), listener.LocalAddr().(*net.UDPAddr).Port)
	case "SB-LINUX-03":
		unixPath := filepath.Join(x.root, "unix-control.sock")
		listener, err := net.Listen("unix", unixPath)
		if err != nil {
			return realFailure(e, err)
		}
		defer listener.Close()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				received.Add(1)
				_ = conn.Close()
			}
		}()
		raw := realHostAttempt(func() error { return realSocket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_ICMP) })
		attribution := "independent-control"
		if !raw.Success && raw.Errno != nil && (*raw.Errno == int(unix.EPERM) || *raw.Errno == int(unix.EACCES)) {
			attribution = "ambient-denial-no-independent-attribution"
		}
		e.Operations = []SandboxOperation{{Name: "raw-inet", Host: raw, Attribution: attribution}, {Name: "raw-netlink", Host: realHostAttempt(func() error { return realSocket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_ROUTE) }), Attribution: "independent-ordinary-user-raw-control"}, {Name: "inet-dgram-create", Host: realHostAttempt(func() error { return realSocket(unix.AF_INET, unix.SOCK_DGRAM, 0) })}}
		body = "_real_attempt('raw-inet',lambda:socket.socket(socket.AF_INET,socket.SOCK_RAW,socket.IPPROTO_ICMP))\n_real_attempt('raw-netlink',lambda:socket.socket(socket.AF_NETLINK,socket.SOCK_RAW,0))\n_real_attempt('inet-dgram-create',lambda:socket.socket(socket.AF_INET,socket.SOCK_DGRAM))\n"
		e.Operations = append(e.Operations, SandboxOperation{Name: "unix-stream-create", Host: realHostAttempt(func() error { return realSocket(unix.AF_UNIX, unix.SOCK_STREAM, 0) })}, SandboxOperation{Name: "controlled-unix", Host: realHostAttempt(func() error { return realConn(unixPath, "unix") })})
		body += "def _unix_connect():\n s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)\n try: s.connect(" + realPythonString(unixPath) + ")\n finally: s.close()\n_real_attempt('unix-stream-create',lambda:socket.socket(socket.AF_UNIX,socket.SOCK_STREAM))\n_real_attempt('controlled-unix',_unix_connect)\n"
	case "SB-LINUX-04", "SB-LINUX-05":
		network, address, family := "tcp4", "127.0.0.1:0", "AF_INET"
		domain := unix.AF_INET
		if item.ID == "SB-LINUX-05" {
			network, address, family = "tcp6", "[::1]:0", "AF_INET6"
			domain = unix.AF_INET6
		}
		listener, err := net.Listen(network, address)
		if err != nil {
			return realFailure(e, fmt.Errorf("required loopback control unavailable: %w", err))
		}
		defer listener.Close()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
				received.Add(1)
			}
		}()
		host := realHostAttempt(func() error { return realConn(listener.Addr().String(), network) })
		e.Operations = []SandboxOperation{{Name: "socket-create", Host: realHostAttempt(func() error { return realSocket(domain, unix.SOCK_STREAM, 0) })}, {Name: "controlled-loopback", Host: host}}
		tcp := listener.Addr().(*net.TCPAddr)
		body = fmt.Sprintf("_real_attempt('socket-create',lambda:socket.socket(socket.%s,socket.SOCK_STREAM))\n_real_attempt('controlled-loopback',lambda:socket.create_connection((%s,%d),timeout=1))\n", family, realPythonString(tcp.IP.String()), tcp.Port)
	}
	deadline := time.Now().Add(time.Second)
	for received.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	baseline := received.Load()
	if baseline == 0 {
		return realFailure(e, fmt.Errorf("required listener did not observe its host positive control"))
	}
	result, _, e := x.run(ctx, item.ID, x.spec(realPrelude()+body+"_real_finish()\n"), e)
	if err := realParseOperations(result, &e); err != nil {
		return realFailure(e, err)
	}
	realCheckDenials(&e)
	e.observe("listener_host_receives", int(baseline))
	e.observe("listener_receives_after_sandbox", int(received.Load()))
	e.observe("listener_quiet", received.Load() == baseline)
	return e
}
