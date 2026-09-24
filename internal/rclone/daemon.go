package rclone

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Daemon 代表一个后台运行的 `rclone rcd` 子进程。
type Daemon struct {
	cmd    *exec.Cmd
	Client *Client
	output *lockedBuffer
	// Exited 在子进程退出时收到退出错误（用于监控意外退出）
	Exited chan error
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// freePort 找一个当前空闲的本地端口。
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// StartRcd 以 RC API 模式启动 rclone 子进程并等待就绪。
// 子进程绑定 127.0.0.1 随机高位端口，随机强凭据（Basic Auth）。
func StartRcd(rcloneExe string) (*Daemon, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	user := "spike"
	pass := randomHex(16)

	cmd := exec.Command(rcloneExe,
		"rcd",
		"--rc-addr", addr,
		"--rc-user", user,
		"--rc-pass", pass,
		"--rc-serve", // 启用 GET /[fs]/path 直读（水合数据通道，支持 HTTP Range）
	)
	// 不弹出新的控制台窗口
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start rclone rcd: %w", err)
	}
	exited := make(chan error, 1)
	// 后台收割：Wait 只能调用一次，退出信息统一经 Exited 上报
	go func() { exited <- cmd.Wait() }()

	client := NewClient("http://"+addr, user, pass)
	// 就绪探测：最多 10 秒
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := client.Version(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill() // 收割由上面的 Wait goroutine 完成
			return nil, fmt.Errorf("rclone rcd 未在 10s 内就绪:\n%s", out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	return &Daemon{cmd: cmd, Client: client, output: out, Exited: exited}, nil
}

// Output 返回子进程的累计输出（诊断用）。
func (d *Daemon) Output() string { return d.output.String() }

// Stop 终止子进程（Wait 由 StartRcd 的收割 goroutine 完成，这里只 Kill）。
func (d *Daemon) Stop() error {
	if d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
	return nil
}
