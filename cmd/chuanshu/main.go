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

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "chuanshu:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	paths, err := appcfg.ResolvePaths()
	if err != nil {
		return err
	}
	cfg, err := appcfg.Load(paths)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return tui.Run(paths)
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
		return tui.Run(paths)
	case "version", "--version", "-v":
		fmt.Println("chuanshu", version)
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
		_ = raw
		if err != nil {
			return err
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
  chuanshu daemon           前台运行守护进程（供 systemd 调用）
  chuanshu peers            列出同事与在线状态
  chuanshu peer-add <ip:端口>  手动连接（广播被屏蔽时）
  chuanshu send <同事> <消息...>  发消息（对方不在线立即失败）
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
           聊天中 Ctrl+N/P 快速切换 · Ctrl+V 粘贴图片 · /file <路径> 加附件
           Ctrl+O 打开最近附件 · y/n 确认或拒绝收文件 · Ctrl+C 退出
`)
}

var _ = json.Marshal
