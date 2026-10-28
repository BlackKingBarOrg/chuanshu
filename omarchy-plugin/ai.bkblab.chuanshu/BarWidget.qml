import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

// Chuanshu bar widget: envelope icon with an unread badge.
// Data comes from `chuanshu unread --json` (polled; the daemon is
// the single source of truth). Clicking runs `chuanshu open`,
// which focuses the running chat window or launches a dedicated
// terminal window, jumping to the latest unread conversation.
BarWidget {
  id: root
  moduleName: "ai.bkblab.chuanshu"

  property int unread: 0
  property bool daemonUp: false
  property string latestName: ""

  function refresh() {
    if (!poll.running) poll.running = true
  }

  function openChat() {
    Quickshell.execDetached(["sh", "-c", "chuanshu open"])
  }

  visible: true
  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  Process {
    id: poll
    command: ["sh", "-c", "chuanshu unread --json"]
    stdout: StdioCollector {
      onStreamFinished: {
        try {
          var data = JSON.parse(text)
          root.unread = data.total || 0
          root.latestName = data.latest_name || ""
          root.daemonUp = true
        } catch (e) {
          root.daemonUp = false
        }
      }
    }
    onExited: function (code) {
      if (code !== 0) root.daemonUp = false
    }
  }

  Timer {
    interval: 3000
    running: true
    repeat: true
    triggeredOnStart: true
    onTriggered: root.refresh()
  }

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    text: "\uf0e0"
    slotSize: Style.bar.statusSlot
    fontSize: Style.font.caption
    opacity: root.daemonUp ? 1.0 : 0.35
    tooltipText: root.unread > 0
      ? ("传书：未读 " + root.unread + " 条" + (root.latestName ? ("，最近来自 " + root.latestName) : "") + "（点击打开）")
      : "传书（点击打开）"
    onPressed: root.openChat()

    // Unread badge, top-right of the icon.
    Rectangle {
      visible: root.unread > 0
      anchors.top: parent.top
      anchors.right: parent.right
      anchors.topMargin: 1
      anchors.rightMargin: 1
      width: Math.max(13, badgeText.implicitWidth + 5)
      height: 13
      radius: 6.5
      color: "#e53935"

      Text {
        id: badgeText
        anchors.centerIn: parent
        text: root.unread > 99 ? "99+" : String(root.unread)
        color: "white"
        font.pixelSize: 9
        font.bold: true
      }
    }
  }
}
