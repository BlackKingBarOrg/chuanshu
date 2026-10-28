#!/bin/sh
# Install the Omarchy integration for chuanshu:
#   - bar widget (unread badge) into ~/.config/omarchy/plugins
#   - desktop entry + icon so the launcher can open the chat window
# The daemon and binary themselves come from scripts/install.sh.
set -eu
cd "$(dirname "$0")/.."

PLUGIN_ID="ai.bkblab.chuanshu"
PLUGIN_SRC="omarchy-plugin/$PLUGIN_ID"
PLUGIN_DST="$HOME/.config/omarchy/plugins/$PLUGIN_ID"
SHELL_JSON="$HOME/.config/omarchy/shell.json"

if ! command -v chuanshu >/dev/null 2>&1; then
  echo "先运行 scripts/install.sh 安装传书本体" >&2
  exit 1
fi

mkdir -p "$PLUGIN_DST" "$HOME/.local/share/applications" \
  "$HOME/.local/share/icons/hicolor/scalable/apps"
cp "$PLUGIN_SRC/manifest.json" "$PLUGIN_SRC/BarWidget.qml" "$PLUGIN_DST/"
cp packaging/chuanshu.desktop "$HOME/.local/share/applications/chuanshu.desktop"
cp packaging/chuanshu.svg "$HOME/.local/share/icons/hicolor/scalable/apps/chuanshu.svg"
command -v update-desktop-database >/dev/null 2>&1 && \
  update-desktop-database "$HOME/.local/share/applications" >/dev/null 2>&1 || true

# Add the widget to the right section of the bar (idempotent).
if [ -f "$SHELL_JSON" ] && command -v python3 >/dev/null 2>&1; then
  cp "$SHELL_JSON" "$SHELL_JSON.bak.chuanshu.$(date +%s)"
  python3 - "$SHELL_JSON" "$PLUGIN_ID" <<'PY'
import json, sys
path, pid = sys.argv[1], sys.argv[2]
with open(path) as f:
    data = json.load(f)
right = data.setdefault("bar", {}).setdefault("layout", {}).setdefault("right", [])
if not any(isinstance(x, dict) and x.get("id") == pid for x in right):
    right.insert(0, {"id": pid})
    with open(path, "w") as f:
        json.dump(data, f, indent=1, ensure_ascii=False)
    print("已把顶栏组件加入 shell.json 右侧")
else:
    print("顶栏组件已在 shell.json 中，跳过")
PY
else
  echo "未找到 shell.json 或 python3，请手动把 $PLUGIN_ID 加入顶栏右侧"
fi

echo "Omarchy 集成安装完成。顶栏右侧会出现传书信封图标，未读时带红色角标。"
echo "可选：想让传书窗口独占/固定，可在 ~/.config/hypr/looknfeel.lua 追加:"
echo '  o.window("chuanshu", { fullscreen = true })'
echo "若顶栏没立刻出现，运行: omarchy restart shell"
