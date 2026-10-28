// chuanshu: LAN chat and file transfer for small teams on Omarchy.
// One binary: daemon (default background core), TUI, and CLI.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"chuanshu/internal/appcfg"
	"chuanshu/internal/daemon"
	"chuanshu/internal/identity"
	"chuanshu/internal/ipc"
	"chuanshu/internal/store"
	"chuanshu/internal/tui"
)

var version = "0.1.0"

// commit is stamped at build time via -ldflags "-X main.commit=...".
var commit = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "chuanshu:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	daemon.BuildInfo = commit
	paths, err := appcfg.ResolvePaths()
	if err != nil {
		return err
	}
	cfg, err := appcfg.Load(paths)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return tui.Run(paths, "")
	}
	switch args[0] {
	case "daemon":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		d, err := daemon.New(cfg, paths)
		if err != nil {
			return err
		}
		return d.Start(ctx)
	case "tui":
		openPeer := ""
		if len(args) >= 3 && args[1] == "--open" {
			openPeer = args[2]
		}
		return tui.Run(paths, openPeer)
	case "open", "focus":
		peer := ""
		if len(args) > 1 {
			peer = args[1]
		}
		return cmdOpen(paths, peer)
	case "unread":
		return cmdUnread(paths, args[1:])
	case "version", "--version", "-v":
		fmt.Printf("chuanshu %s (commit %s)\n", version, commit)
		// Also report the running daemon's build: replacing the
		// binary does not upgrade an already-running process.
		client := ipc.Client{Paths: paths}
		var me map[string]any
		if err := client.Call("me", nil, &me); err == nil {
			fmt.Printf("daemon build: %v (若与上面不一致，重启守护进程)\n", me["build"])
		} else {
			fmt.Println("daemon: 未运行（打开 chuanshu 时会自动启动）")
		}
		return nil
	case "id":
		id, err := identity.LoadOrCreate(paths.DataDir)
		if err != nil {
			return err
		}
		fmt.Printf("peer_id:     %s\nnickname:    %s\ncode:        %s\nfingerprint: %s\n",
			id.MyPeerID(), cfg.Nickname, id.MyCode(), identity.Fingerprint(id.NoisePublicBytes()))
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	}

	// Everything below talks to the daemon.
	if err := ipc.EnsureDaemon(paths); err != nil {
		return err
	}
	client := ipc.Client{Paths: paths}

	switch args[0] {
	case "peers":
		var peers []*store.PeerRecord
		if err := client.Call("peers", nil, &peers); err != nil {
			return err
		}
		if len(peers) == 0 {
			fmt.Println("还没有发现同事（确认对方已启动 chuanshu 且在同一局域网）")
			return nil
		}
		for _, p := range peers {
			state := "离线"
			if p.Online {
				state = "在线"
			}
			verified := ""
			if p.Verified {
				verified = " 已核对"
			}
			fmt.Printf("%s %s %s%s  %s\n", state, p.PeerID[:8], p.Nickname, verified, p.Addr)
		}
		return nil
	case "peer-add":
		if len(args) < 2 {
			return fmt.Errorf("用法: chuanshu peer-add <ip:端口>")
		}
		var rec store.PeerRecord
		if err := client.Call("peer_add", map[string]any{"addr": args[1]}, &rec); err != nil {
			return err
		}
		fmt.Printf("已连接: %s (%s)\n", rec.Nickname, rec.PeerID[:8])
		return nil
	case "send":
		if len(args) < 3 {
			return fmt.Errorf("用法: chuanshu send <同事> <消息...>")
		}
		raw, err := client.RawCall("send_text", map[string]any{"peer": args[1], "text": strings.Join(args[2:], " ")})
		if err != nil {
			return err
		}
		var cm store.ChatMessage
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &cm)
		}
		if cm.Status == "queued" {
			fmt.Println("对方不在线，消息已排队，待对方上线自动重发")
			return nil
		}
		fmt.Println("已送达")
		return nil
	case "file":
		if len(args) < 3 {
			return fmt.Errorf("用法: chuanshu file <同事> <文件或文件夹路径>")
		}
		abs, err := filepath.Abs(args[2])
		if err != nil {
			return err
		}
		var msgs []*store.ChatMessage
		if err := client.Call("send_file", map[string]any{"peer": args[1], "path": abs}, &msgs); err != nil {
			return err
		}
		fmt.Printf("已送达 %d 个文件\n", len(msgs))
		return nil
	case "offers":
		var offers []daemon.PendingOffer
		if err := client.Call("offers", nil, &offers); err != nil {
			return err
		}
		if len(offers) == 0 {
			fmt.Println("没有待确认的文件")
			return nil
		}
		for _, o := range offers {
			fmt.Printf("%s  %s 发来 %s (%d 字节)\n", o.TransferID[:8], o.FromName, o.File.Name, o.File.Size)
		}
		return nil
	case "accept", "reject":
		if len(args) < 2 {
			return fmt.Errorf("用法: chuanshu %s <传输ID前8位>", args[0])
		}
		var offers []daemon.PendingOffer
		if err := client.Call("offers", nil, &offers); err != nil {
			return err
		}
		id := ""
		for _, o := range offers {
			if strings.HasPrefix(o.TransferID, args[1]) {
				id = o.TransferID
				break
			}
		}
		if id == "" {
			return fmt.Errorf("找不到待确认文件 %q", args[1])
		}
		var ok bool
		if err := client.Call("respond_offer", map[string]any{"transfer_id": id, "accept": args[0] == "accept"}, &ok); err != nil {
			return err
		}
		fmt.Println("已处理")
		return nil
	case "code":
		if len(args) < 2 {
			return fmt.Errorf("用法: chuanshu code <同事>")
		}
		var out struct {
			Code string         `json:"code"`
			Me   map[string]any `json:"me"`
		}
		if err := client.Call("code", map[string]any{"peer": args[1]}, &out); err != nil {
			return err
		}
		fmt.Printf("对方核对码: %s\n你的核对码: %v\n当面或电话核对一致后运行: chuanshu trust %s\n", out.Code, out.Me["code"], args[1])
		return nil
	case "trust":
		if len(args) < 2 {
			return fmt.Errorf("用法: chuanshu trust <同事>")
		}
		var out struct {
			Code string `json:"code"`
		}
		if err := client.Call("verify", map[string]any{"peer": args[1]}, &out); err != nil {
			return err
		}
		fmt.Println("已标记为已核对:", out.Code)
		return nil
	case "shot":
		if len(args) < 2 {
			return fmt.Errorf("用法: chuanshu shot <同事>")
		}
		return sendScreenshot(client, paths, args[1])
	case "history":
		conv := store.BroadcastConv
		if len(args) > 1 {
			conv = args[1]
		}
		var out struct {
			Messages []*store.ChatMessage `json:"messages"`
		}
		if err := client.Call("history", map[string]any{"conv": conv}, &out); err != nil {
			return err
		}
		for _, m := range out.Messages {
			printMessage(m)
		}
		return nil
	default:
		usage()
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

func printMessage(m *store.ChatMessage) {
	who := m.FromName
	if m.Mine {
		who = "我"
	}
	if m.Attachment != nil {
		fmt.Printf("%s %s [文件 %s %d字节] %s\n", m.TS.Format("01-02 15:04"), who, m.Attachment.Name, m.Attachment.Size, m.Attachment.LocalPath)
		return
	}
	fmt.Printf("%s %s %s\n", m.TS.Format("01-02 15:04"), who, m.Text)
}

// cmdOpen focuses the running TUI window (jumping to a
// conversation when asked) or launches a dedicated terminal
// window for chuanshu. This backs the Omarchy bar widget.
func cmdOpen(paths appcfg.Paths, peer string) error {
	if err := ipc.EnsureDaemon(paths); err != nil {
		return err
	}
	client := ipc.Client{Paths: paths}
	target := peer
	if target == "" {
		var sum daemon.UnreadSummary
		if err := client.Call("unread", nil, &sum); err == nil {
			target = sum.LatestConv
		}
	}
	var win struct {
		Address string `json:"address"`
	}
	_ = client.Call("window", nil, &win)
	if win.Address == "" {
		// A TUI may be running without having registered (older
		// build): adopt its window by title and register it now.
		if addr := findChuanshuWindow(); addr != "" {
			var ok bool
			if err := client.Call("register_window", map[string]any{"address": addr}, &ok); err == nil {
				win.Address = addr
			}
		}
	}
	if win.Address != "" && !windowExists(win.Address) {
		// Stale registration: that window is gone (a TUI killed
		// with its terminal never gets to unregister). Focusing a
		// dead address silently "succeeds", so clear it and fall
		// through to launching a fresh window.
		var ok bool
		_ = client.Call("unregister_window", map[string]any{"address": win.Address}, &ok)
		win.Address = ""
	}
	if win.Address != "" {
		// Omarchy's Hyprland uses the Lua dispatcher form; stock
		// Hyprland uses focuswindow. Try both.
		focused := exec.Command("hyprctl", "dispatch", fmt.Sprintf("hl.dsp.focus({ window = \"address:%s\" })", win.Address)).Run() == nil ||
			exec.Command("hyprctl", "dispatch", "focuswindow", "address:"+win.Address).Run() == nil
		if focused {
			if target != "" {
				var out map[string]any
				_ = client.Call("focus_conv", map[string]any{"conv": target}, &out)
			}
			fmt.Println("已切到传书窗口")
			return nil
		}
		var ok bool
		_ = client.Call("unregister_window", map[string]any{"address": win.Address}, &ok)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	termArgs := []string{"--app-id=chuanshu", "--title=Chuanshu", exe}
	if target != "" {
		termArgs = append(termArgs, "tui", "--open", target)
	}
	cmd := exec.Command("xdg-terminal-exec", termArgs...)
	logFile, err := os.OpenFile(filepath.Join(paths.DataDir, "open.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("打开终端失败: %w", err)
	}
	fmt.Println("已打开传书窗口")
	return nil
}

// windowExists reports whether a Hyprland window address is
// still mapped.
func windowExists(addr string) bool {
	out, err := exec.Command("hyprctl", "clients", "-j").Output()
	if err != nil {
		return false
	}
	var clients []struct {
		Address string `json:"address"`
	}
	if json.Unmarshal(out, &clients) != nil {
		return false
	}
	for _, c := range clients {
		if c.Address == addr {
			return true
		}
	}
	return false
}

// findChuanshuWindow locates a dedicated chuanshu terminal
// window (title "Chuanshu") via hyprctl.
func findChuanshuWindow() string {
	out, err := exec.Command("hyprctl", "clients", "-j").Output()
	if err != nil {
		return ""
	}
	var clients []struct {
		Address      string `json:"address"`
		Title        string `json:"title"`
		InitialTitle string `json:"initialTitle"`
	}
	if json.Unmarshal(out, &clients) != nil {
		return ""
	}
	for _, c := range clients {
		if c.InitialTitle == "Chuanshu" || c.Title == "Chuanshu" {
			return c.Address
		}
	}
	return ""
}

// cmdUnread prints the unread summary (for the bar widget: --json).
func cmdUnread(paths appcfg.Paths, args []string) error {
	if err := ipc.EnsureDaemon(paths); err != nil {
		return err
	}
	client := ipc.Client{Paths: paths}
	var sum daemon.UnreadSummary
	if err := client.Call("unread", nil, &sum); err != nil {
		return err
	}
	for _, a := range args {
		if a == "--json" {
			raw, _ := json.Marshal(sum)
			fmt.Println(string(raw))
			return nil
		}
	}
	if sum.Total == 0 {
		fmt.Println("没有未读消息")
		return nil
	}
	fmt.Printf("未读 %d 条，最近来自 %s：%s\n", sum.Total, sum.LatestName, sum.LatestText)
	return nil
}

func sendScreenshot(client ipc.Client, paths appcfg.Paths, peer string) error {
	// Region select with slurp when available, full screen otherwise.
	imgPath := filepath.Join(paths.CacheDir, "shot-"+time.Now().Format("20060102-150405")+".png")
	var cmd *exec.Cmd
	if _, err := exec.LookPath("slurp"); err == nil {
		geo, err := exec.Command("slurp").Output()
		if err != nil {
			return fmt.Errorf("已取消截图")
		}
		cmd = exec.Command("grim", "-g", strings.TrimSpace(string(geo)), imgPath)
	} else {
		cmd = exec.Command("grim", imgPath)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("截图失败: %v %s", err, out)
	}
	var msgs []*store.ChatMessage
	if err := client.Call("send_file", map[string]any{"peer": peer, "path": imgPath}, &msgs); err != nil {
		return err
	}
	fmt.Println("截图已发送:", filepath.Base(imgPath))
	return nil
}

func usage() {
	fmt.Print(`chuanshu — 局域网聊天与传文件（Omarchy 小团队版）

用法:
  chuanshu                  打开 TUI（自动启动后台服务）
  chuanshu open [同事]      打开/聚焦传书窗口（顶栏组件用；有未读直达最近未读）
  chuanshu unread [--json]  未读汇总（顶栏组件用 JSON 输出）
  chuanshu daemon           前台运行守护进程（供 systemd 调用）
  chuanshu peers            列出同事与在线状态
  chuanshu peer-add <ip:端口>  手动连接（广播被屏蔽时）
  chuanshu send <同事> <消息...>  发消息（对方不在线自动排队，上线补发）
  chuanshu file <同事> <路径>    发文件或文件夹
  chuanshu shot <同事>      框选截图并发送
  chuanshu offers           待确认接收的文件
  chuanshu accept <ID>      接受文件 / chuanshu reject <ID> 拒绝
  chuanshu code <同事>      查看身份核对码
  chuanshu trust <同事>     核对一致后标记已验证
  chuanshu history [同事]   查看聊天记录（缺省为全员广播）
  chuanshu id               本机身份信息
  chuanshu version          版本

TUI 操作: 打开先选人（↑↓/jk 选择，打字搜索，Enter 进入聊天，Esc 回列表换人）
           聊天中 @ 模糊选文件 · Ctrl+V 粘贴图片/文件 · 拖文件进终端也可加附件
           Ctrl+N/P 快速切换 · Ctrl+O 打开附件 · Ctrl+X 清空附件 · Ctrl+C 退出
`)
}

var _ = json.Marshal
