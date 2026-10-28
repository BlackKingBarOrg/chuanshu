// Package ipc is the client side of the daemon's unix-socket API,
// shared by the TUI and the CLI.
package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"chuanshu/internal/appcfg"
)

// Client is a short-lived request/response connection.
type Client struct {
	Paths appcfg.Paths
}

// Call performs one request and decodes the result into out.
func (c Client) Call(method string, params any, out any) error {
	conn, err := net.DialTimeout("unix", c.Paths.SocketPath(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("守护进程没有运行: %w", err)
	}
	defer conn.Close()
	return callOn(conn, method, params, out)
}

func callOn(conn net.Conn, method string, params any, out any) error {
	req := map[string]any{"id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return err
		}
		return fmt.Errorf("守护进程没有响应")
	}
	var resp struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return err
	}
	if !resp.OK {
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		return fmt.Errorf("请求失败")
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

// RawCall is Call without decoding, for error paths that still
// carry a result payload.
func (c Client) RawCall(method string, params any) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", c.Paths.SocketPath(), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("守护进程没有运行: %w", err)
	}
	defer conn.Close()
	req := map[string]any{"id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, _ := json.Marshal(req)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	if !sc.Scan() {
		return nil, fmt.Errorf("守护进程没有响应")
	}
	var resp struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return resp.Result, fmt.Errorf("%s", resp.Error)
	}
	return resp.Result, nil
}

// Event mirrors daemon.Event without importing the daemon package.
type Event struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Subscribe opens the event stream. The caller reads Events until
// the returned channel closes.
func (c Client) Subscribe() (<-chan Event, error) {
	conn, err := net.DialTimeout("unix", c.Paths.SocketPath(), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("守护进程没有运行: %w", err)
	}
	raw, _ := json.Marshal(map[string]any{"id": 1, "method": "subscribe"})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		_ = conn.Close()
		return nil, err
	}
	out := make(chan Event, 64)
	go func() {
		defer conn.Close()
		defer close(out)
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		first := true
		for sc.Scan() {
			if first { // subscribe ack
				first = false
				continue
			}
			var wrap struct {
				Event Event `json:"event"`
			}
			if err := json.Unmarshal(sc.Bytes(), &wrap); err == nil {
				out <- wrap.Event
			}
		}
	}()
	return out, nil
}

// EnsureDaemon starts the daemon detached if it is not running.
func EnsureDaemon(paths appcfg.Paths) error {
	if conn, err := net.DialTimeout("unix", paths.SocketPath(), 300*time.Millisecond); err == nil {
		_ = conn.Close()
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(paths.DataDir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	_ = logFile.Close()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", paths.SocketPath(), 300*time.Millisecond); err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(120 * time.Millisecond)
	}
	return fmt.Errorf("守护进程启动超时，查看日志: %s", logPath)
}
