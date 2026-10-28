#!/bin/sh
# Install chuanshu for the current user on Omarchy/Arch:
# binary to ~/.local/bin, systemd user service enabled.
set -eu
cd "$(dirname "$0")/.."

GO="$(command -v go || true)"
if [ -z "$GO" ] && command -v mise >/dev/null 2>&1; then
  mise install go@latest >/dev/null 2>&1 || true
  GO="mise x go -- go"
fi
if [ -z "$GO" ]; then
  echo "需要先安装 Go: sudo pacman -S go" >&2
  exit 1
fi

mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user"
$GO build -o "$HOME/.local/bin/chuanshu" ./cmd/chuanshu
cp packaging/chuanshu.service "$HOME/.config/systemd/user/chuanshu.service"
systemctl --user daemon-reload
systemctl --user enable --now chuanshu.service

echo "安装完成。打开终端运行: chuanshu"
echo "提示: 若局域网看不到人，先确认防火墙放行 TCP 24247 与 UDP 24246，例如:"
echo "  sudo ufw allow 24247/tcp && sudo ufw allow 24246/udp"
