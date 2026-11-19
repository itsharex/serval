package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"serval/core/hosts"
	"serval/core/sshconn"

	"github.com/egoist/mygo/ui"
)

func TestBatch1ModalBlocksWorkspaceShortcuts(t *testing.T) {
	h := newHarness(t)
	h.a.openLocalTerminal()
	h.tt.Frame()
	if len(h.a.tabs) != 1 {
		t.Fatalf("应先打开 1 个本地标签页，实际 %d", len(h.a.tabs))
	}

	h.a.openAdd()
	h.tt.Frame()
	if !h.a.form.open {
		t.Fatal("添加主机弹窗应已打开")
	}

	// 弹窗打开时按工作区快捷键不应触发（KeyK、KeyJ、KeyI、KeyW）。
	h.tt.Key(chromeShortcut(), ui.KeyK)
	h.tt.Key(chromeShortcut(), ui.KeyJ)
	h.tt.Key(chromeShortcut(), ui.KeyI)
	h.tt.Key(chromeShortcut(), ui.KeyW)
	h.tt.Frame()

	if h.a.pal.open {
		t.Fatal("弹窗打开时 KeyK 不应打开命令面板")
	}
	if h.a.filesOpen {
		t.Fatal("弹窗打开时 KeyJ 不应打开文件抽屉")
	}
	if h.a.ai.open {
		t.Fatal("弹窗打开时 KeyI 不应打开 AI 面板")
	}
	if len(h.a.tabs) != 1 {
		t.Fatalf("弹窗打开时 KeyW 不应关闭标签页，实际 tabs=%d", len(h.a.tabs))
	}
}

func TestBatch1MoveTabRightAndHostDownAndGroupCleanup(t *testing.T) {
	h := newHarness(t)
	h.a.openLocalTerminal()
	h.a.openLocalTerminal()
	h.a.openLocalTerminal()
	h.tt.Frame()
	if len(h.a.tabs) != 3 {
		t.Fatalf("期望 3 个标签页，实际 %d", len(h.a.tabs))
	}
	aTab, bTab, cTab := h.a.tabs[0], h.a.tabs[1], h.a.tabs[2]
	h.a.moveTab(aTab.id, bTab.id)
	if h.a.tabs[0] != bTab || h.a.tabs[1] != aTab || h.a.tabs[2] != cTab {
		t.Fatalf("向右拖拽标签页顺序错误: [%d,%d,%d]", h.a.tabs[0].id, h.a.tabs[1].id, h.a.tabs[2].id)
	}

	h1 := hosts.Host{ID: "bh1", Name: "Alpha", Address: "10.0.0.1", Group: "BatchGroup"}
	h2 := hosts.Host{ID: "bh2", Name: "Beta", Address: "10.0.0.2", Group: "BatchGroup"}
	h3 := hosts.Host{ID: "bh3", Name: "Gamma", Address: "10.0.0.3", Group: "BatchGroup"}
	if err := h.a.apply(hostChange(h1), hostChange(h2), hostChange(h3)); err != nil {
		t.Fatal(err)
	}
	if err := h.a.moveHostOrder("bh1", "bh2", "BatchGroup"); err != nil {
		t.Fatal(err)
	}
	got := h.a.hostsInGroup("BatchGroup")
	if len(got) != 3 || got[0].ID != "bh2" || got[1].ID != "bh1" || got[2].ID != "bh3" {
		t.Fatalf("同组向下拖拽排序错误: %+v", got)
	}

	// 新建重复分组应报提示且不静默关闭；删除空分组应同步清理 CollapsedGroups。
	h.a.openNewGroup()
	h.a.groupSheet.name = "BatchEmpty"
	h.a.submitGroupDialog()
	h.a.toggleGroup("BatchEmpty")
	if !h.a.groupCollapsed("BatchEmpty") {
		t.Fatal("新分组应能折叠")
	}

	h.a.openNewGroup()
	h.a.groupSheet.name = "BatchEmpty"
	h.a.submitGroupDialog()
	if !h.a.groupSheet.open || h.a.groupSheet.err == "" {
		t.Fatalf("重复分组名称应提示错误并保持弹窗打开: open=%v err=%q", h.a.groupSheet.open, h.a.groupSheet.err)
	}
	h.a.groupSheet = groupDialogState{}

	h.a.deleteEmptyGroup("BatchEmpty")
	if h.a.groupCollapsed("BatchEmpty") {
		t.Fatalf("删除空分组后 CollapsedGroups 应同步清理: %+v", h.a.local.CollapsedGroups)
	}
}

func TestBatch1HostFormProtocolDistroAndForwardMissingHost(t *testing.T) {
	h := newHarness(t)
	telnetHost := hosts.Host{ID: "h-telnet", Name: "Switch-Telnet", Protocol: hosts.ProtocolTelnet, Address: "10.0.0.8", Port: 23, User: "admin"}
	telnetCustomPort := hosts.Host{ID: "h-telnet22", Name: "Switch-22", Protocol: hosts.ProtocolTelnet, Address: "10.0.0.18", Port: 22, User: "admin"}
	serialHost := hosts.Host{ID: "h-serial", Name: "Router-COM", Protocol: hosts.ProtocolSerial, Address: "/dev/ttyS0", BaudRate: 115200}
	if err := h.a.apply(hostChange(telnetHost), hostChange(telnetCustomPort), hostChange(serialHost)); err != nil {
		t.Fatal(err)
	}

	if got := h.a.formFrom(telnetHost).port; got != "" {
		t.Fatalf("Telnet 默认端口 23 不应作为自定义端口填入，实际 %q", got)
	}
	if got := h.a.formFrom(telnetCustomPort).port; got != "22" {
		t.Fatalf("Telnet 使用非默认 22 端口时应保留显示 22，实际 %q", got)
	}
	if got := h.a.formFrom(serialHost).port; got != "" {
		t.Fatalf("Serial 不应预设 TCP 端口，实际 %q", got)
	}

	// 系统标识从非空切回自动（空）时应真实保存为空。
	existing := hosts.Host{ID: "h-distro", Name: "UbuntuBox", Address: "10.0.0.9", User: "root", Port: 22, Distro: "ubuntu"}
	if err := h.a.apply(hostChange(existing)); err != nil {
		t.Fatal(err)
	}
	h.a.openEdit(existing)
	h.a.form.distro = "" // 重置为自动
	built, _, _, ok := h.a.buildHostFromForm(true)
	if !ok {
		t.Fatalf("buildHostFromForm 失败: %q", h.a.form.err)
	}
	if built.Distro != "" {
		t.Fatalf("重置系统标识为自动后应保存为空，实际 %q", built.Distro)
	}
	h.a.form = hostForm{}

	// 编辑端口转发时，非 SSH 主机不应出现，且已删除的绑定主机应明确提示。
	if err := h.a.apply(forwardChange(hosts.Forward{
		ID:         "f-missing",
		Name:       "fwd-orphan",
		Kind:       hosts.ForwardLocal,
		HostID:     "deleted-host-id",
		BindHost:   "127.0.0.1",
		BindPort:   19090,
		TargetHost: "127.0.0.1",
		TargetPort: 80,
	})); err != nil {
		t.Fatal(err)
	}
	h.click("端口转发")
	h.tt.Frame()
	h.click("fwd-orphan")
	h.tt.Frame()
	if !h.tt.HasText("原绑定主机已删除") {
		t.Fatalf("绑定主机已删除时下拉框应明确展示提示: %q", h.tt.Texts())
	}
	h.a.saveForward()
	h.tt.Frame()
	if h.a.forwardPanel.err == "" {
		t.Fatal("绑定主机已删除直接保存时应报错提醒重新选择")
	}
	h.a.forwardPanel = forwardPanel{}
	h.tt.Frame()
}

func TestBatch1EnsureFilesStopsRetryAndAISessionBehavior(t *testing.T) {
	h := newHarness(t)
	tb := h.connectDev()

	// 模拟 SFTP 通道出错时，ensureFiles 不应死循环重复发起请求。
	tb.files = nil
	tb.filesBusy = false
	tb.filesError = "连接超时"
	h.a.ensureFiles(tb)
	if tb.filesBusy {
		t.Fatal("存在 filesError 时 ensureFiles 不应在未点击重试前自动重启")
	}

	// 渲染失败横幅与重试按钮，点击重试应清错并重新发起建连。
	tb.files = &sshconn.Files{} // 先允许渲染抽屉，再置空验证点击重试走 ensureFiles
	h.a.toggleFiles()
	h.tt.Frame()
	if !h.tt.HasText("重试") {
		t.Fatalf("filesError 非空时应显示重试按钮: %q", h.tt.Texts())
	}
	tb.files = nil
	h.click("重试")
	h.pump("点击重试重新建连 SFTP", func() bool {
		return tb.files != nil && tb.filesError == ""
	})

	// AI 面板：删除当前会话时不能中止其它后台会话；打开后第二帧不再强抢输入框焦点。
	h.a.openAI()
	h.tt.Frame()
	if h.a.ai.fresh {
		t.Fatal("首次渲染 AI 抽屉后 fresh 应复位为 false")
	}

	// 构造两个会话，其中后台会话正在运行。
	_, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()
	bgConv := &aiConversation{
		ID:        "conv-bg",
		Title:     "后台排查任务",
		UpdatedAt: time.Now(),
		Loading:   true,
		Cancel:    cancelBg,
		Turns:     []aiTurn{{query: "bg", running: true}},
	}
	activeConv := &aiConversation{
		ID:        "conv-active",
		Title:     "当前待删会话",
		UpdatedAt: time.Now(),
		Turns:     []aiTurn{{query: "cur"}},
	}
	h.a.ai.convs = []*aiConversation{activeConv, bgConv}
	loadConversationInto(&h.a.ai, activeConv)

	h.a.deleteAIConversation("conv-active")
	if !bgConv.Loading || len(bgConv.Turns) == 0 || !bgConv.Turns[0].running {
		t.Fatalf("删除当前会话不应中断其它后台会话: bg=%+v", bgConv)
	}

	// 用带输入和长内容的渲染检查 fresh=false 时可正常聚焦到其他按钮而不被每帧抢回焦点。
	h.a.ai.turns = []aiTurn{{query: fmt.Sprintf("检查状态 %d", 1), prompt: "检查状态", err: "示例报错"}}
	h.tt.Frame()
	if !h.tt.HasText("重试") {
		t.Fatalf("AI 失败轮次应提供重试按钮: %q", h.tt.Texts())
	}
	histBox, ok := h.tt.Find("历史会话")
	if !ok {
		t.Fatal("找不到 AI 历史会话按钮")
	}
	h.tt.Move(histBox.X+histBox.W/2, histBox.Y+histBox.H/2)
	h.tt.Frame()
}
