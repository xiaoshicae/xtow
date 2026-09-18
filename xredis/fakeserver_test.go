package xredis

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRedis 一个够用的假 Redis：认 RESP 的命令帧，PING 回 PONG，其余回 OK。
//
// 自己写而不是引 miniredis：测试依赖会被 go mod tidy 记成直接依赖，
// 跟着进每个使用者的模块图（xtrace 上实测过，23 涨到 27）。
// 为了几十行的握手省这一笔是划算的。
type fakeRedis struct {
	ln net.Listener

	mu       sync.Mutex
	commands []string // 收到过的命令名，小写
	failPing bool     // 让 PING 返回错误，用来测连不上的分支
	live     int      // 当前还开着的连接数
}

func newFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{ln: ln}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeRedis) addr() string { return f.ln.Addr().String() }

func (f *fakeRedis) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *fakeRedis) setFailPing(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPing = v
}

// liveConns 当前还开着的连接数。
//
// 用它而不是数协程来验证「实例真的被关掉了」：协程数会被假服务端
// 自己的那些协程搅浑，而连接数是直接可观察的事实。
func (f *fakeRedis) liveConns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}

// waitConns 等连接数降到 want，超时返回实际值
func (f *fakeRedis) waitConns(want int) int {
	for i := 0; i < 100; i++ {
		if n := f.liveConns(); n == want {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
	return f.liveConns()
}

func (f *fakeRedis) serve(conn net.Conn) {
	f.mu.Lock()
	f.live++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.live--
		f.mu.Unlock()
		conn.Close()
	}()
	r := bufio.NewReader(conn)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		name := strings.ToLower(args[0])

		f.mu.Lock()
		f.commands = append(f.commands, name)
		fail := f.failPing
		f.mu.Unlock()

		var reply string
		switch {
		case name == "hello":
			// 回错误，go-redis 会退回 RESP2 继续——省掉实现 RESP3 握手
			reply = "-ERR unknown command 'HELLO'\r\n"
		case name == "ping" && fail:
			reply = "-ERR 假装挂了\r\n"
		case name == "ping":
			reply = "+PONG\r\n"
		default:
			reply = "+OK\r\n"
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

// readCommand 读一个 RESP 命令帧：*N\r\n 之后跟 N 个 $len\r\n<data>\r\n
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("不是命令帧: %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil || n < 0 {
		return nil, fmt.Errorf("参数个数不对: %q", line)
	}

	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		head, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimRight(head, "\r\n")[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2) // 连同结尾的 \r\n 一起读掉
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}
