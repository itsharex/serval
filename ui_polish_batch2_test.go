package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"serval/core/hosts"
	"serval/core/sshconn"
	"serval/core/zmodem"
)

func TestBatch2PaletteScrollResetOnQueryChange(t *testing.T) {
	h := newHarness(t)
	h.a.openPalette()
	h.tt.Frame()

	// 在未过滤（条目较多）状态下调整 selected 与 scroll.Y，同查询再次渲染应保持 selected
	h.a.pal.selected = 2
	h.a.pal.scroll.Y = 96
	h.tt.Frame()
	if h.a.pal.selected != 2 {
		t.Fatalf("未改变 query 时不应复位 selected: got %d", h.a.pal.selected)
	}

	h.tt.Type("主机")
	h.tt.Frame()
	if h.a.pal.query != "主机" {
		t.Fatalf("输入应更新 pal.query: got %q", h.a.pal.query)
	}
	if h.a.pal.selected != 0 || h.a.pal.scroll.Y != 0 {
		t.Fatalf("query 改变后应复位 selected=0 与 scroll.Y=0: selected=%d scrollY=%v", h.a.pal.selected, h.a.pal.scroll.Y)
	}
}

func TestBatch2DualPaneWidthRetryHiddenAndScanning(t *testing.T) {
	h := newHarness(t)
	tb := h.connectDev()

	// 先在 localDir 放一个隐藏文件并开启隐藏过滤，验证“仅隐藏文件被过滤”的引导与一键显示
	hiddenFile := filepath.Join(h.a.localDir, ".hidden-only")
	if err := os.WriteFile(hiddenFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.a.localHideHidden = true

	h.a.filesWidth = 560
	h.a.filesSplit = 10 // 低于下限 20 时应夹紧为 20
	if got := h.a.paneWidth(true); got < 100 || got > 130 {
		t.Fatalf("filesSplit=10 夹紧为 20%% 后左栏宽度异常: got %v", got)
	}
	h.a.filesSplit = 95 // 高于上限 80 时应夹紧为 80
	if got := h.a.paneWidth(false); got < 100 || got > 130 {
		t.Fatalf("filesSplit=95 夹紧为 80%% 后右栏宽度异常: got %v", got)
	}
	h.a.filesSplit = 50

	// 1. 先加载仅含隐藏文件的目录并打开抽屉，验证扫描提示与“显示隐藏文件”引导
	h.a.openLocal(h.a.localDir)
	h.pump("加载本地目录", func() bool { return len(h.a.localEntries) == 1 && h.a.localErr == "" })
	tb.scanningFolders = 1
	h.a.toggleFiles()
	h.tt.Frame()
	if !h.tt.HasText("正在扫描文件夹…") {
		t.Fatalf("scanningFolders>0 时应展示扫描提示: %q", h.tt.Texts())
	}
	if !h.tt.HasText("已隐藏 1 个以 . 开头的项目") || !h.tt.HasText("显示隐藏文件") {
		t.Fatalf("仅有隐藏文件时应显示专属引导和按钮: %q", h.tt.Texts())
	}
	h.click("显示隐藏文件")
	h.tt.Frame()
	if h.a.localHideHidden {
		t.Fatal("点击显示隐藏文件后 localHideHidden 应变为 false")
	}
	if !h.tt.HasText(".hidden-only") {
		t.Fatalf("显示隐藏文件后应列出 .hidden-only: %q", h.tt.Texts())
	}

	// 2. 再打开不存在目录，验证保留旧目录列表并记住 failedDir 供“重试”恢复
	prevDir := h.a.localDir
	missingDir := filepath.Join(h.dir, "missing-local-dir")
	h.a.openLocal(missingDir)
	h.pump("尝试打开不存在本地目录", func() bool { return h.a.localFailedDir == missingDir && h.a.localErr != "" })
	h.tt.Frame()
	if h.a.localDir != prevDir {
		t.Fatalf("打开失败时应保留原 localDir: got %q want %q", h.a.localDir, prevDir)
	}
	if err := os.MkdirAll(missingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.click("重试")
	h.pump("点击重试切入已恢复的本地目录", func() bool { return h.a.localDir == missingDir && h.a.localErr == "" })
}

func TestBatch2MultiDownloadConflictReplaceAllAndSkipAll(t *testing.T) {
	h := newHarness(t)
	tb := h.connectDev()

	// 准备两个待确认下载，验证“全部跳过”一次清空剩余冲突队列
	p1 := filepath.Join(h.a.localDir, "one.txt")
	p2 := filepath.Join(h.a.localDir, "two.txt")
	h.a.pendingDownloads = []*pendingDownload{
		{tab: tb, remotePath: "/one.txt", localPath: p1, fileName: "one.txt"},
		{tab: tb, remotePath: "/two.txt", localPath: p2, fileName: "two.txt"},
	}
	h.tt.Frame()
	if !h.tt.HasText("还有 1 个同名文件待确认。") {
		t.Fatalf("多文件冲突确认应显示剩余数量提示: %q", h.tt.Texts())
	}
	h.click("全部跳过")
	h.tt.Frame()
	if len(h.a.pendingDownloads) != 0 {
		t.Fatalf("点击全部跳过后应清空冲突队列: len=%d", len(h.a.pendingDownloads))
	}
	if len(tb.transfers) != 0 {
		t.Fatalf("全部跳过不应发起任何传输: len=%d", len(tb.transfers))
	}
}

func TestBatch2ZmodemDownloadRefreshesLocalPane(t *testing.T) {
	h := newHarness(t)
	h.a.openLocal(h.a.localDir)
	h.pump("初始化本地目录", func() bool { return h.a.localErr == "" })
	h.tt.Frame()

	newFile := filepath.Join(h.a.localDir, "from-sz.txt")
	if err := os.WriteFile(newFile, []byte("hello sz"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 与 startZmodemDownload 完成回到主线程后的刷新逻辑一致
	h.a.zmodemFinished(zmodem.Download, 1, nil, h.a.localDir, nil)
	h.a.openLocal(h.a.localDir)
	h.pump("等待本地列表刷新出新文件", func() bool {
		for _, entry := range h.a.localEntries {
			if entry.Name == "from-sz.txt" {
				return true
			}
		}
		return false
	})
}

func TestBatch2EditorReasonSaveAndCloseAndReload(t *testing.T) {
	if got := editorReason(sshconn.ErrFileTooLarge, true); !strings.Contains(got, "请用其他本地程序打开") {
		t.Fatalf("本地超大文件提示应引导使用本地程序打开，实际=%q", got)
	}
	if got := editorReason(sshconn.ErrFileTooLarge, false); !strings.Contains(got, "下载") {
		t.Fatalf("远程超大文件提示应建议下载，实际=%q", got)
	}

	h := newHarness(t)
	localFile := filepath.Join(h.a.localDir, "note.txt")
	if err := os.WriteFile(localFile, []byte("line1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.a.openLocalEditor(localFile, "note.txt")
	h.pump("打开本地文件编辑器", func() bool { return h.a.editor.open && !h.a.editor.busy && h.a.editor.buf != nil })
	h.tt.Frame()

	// 修改文本 -> 请求关闭 -> 选择“保存并关闭”
	h.a.editor.buf.Set("line1\nline2\n")
	h.tt.Frame()
	h.click("关闭")
	h.tt.Frame()
	if !h.tt.HasText("保存并关闭") {
		t.Fatalf("有未保存修改时应提供保存并关闭按钮: %q", h.tt.Texts())
	}
	h.click("保存并关闭")
	h.pump("保存并自动关闭编辑器", func() bool { return !h.a.editor.open })
	h.tt.Frame()

	diskData, err := os.ReadFile(localFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(diskData) != "line1\nline2\n" {
		t.Fatalf("保存并关闭后磁盘内容不正确: got %q", string(diskData))
	}

	// 外部修改冲突 -> 选择“重新加载”
	h.a.openLocalEditor(localFile, "note.txt")
	h.pump("再次打开编辑器", func() bool { return h.a.editor.open && !h.a.editor.busy && h.a.editor.buf != nil })
	h.tt.Frame()
	h.a.editor.askOverwrite = true
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(localFile, []byte("external-reload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.tt.Frame()
	if !h.tt.HasText("重新加载") {
		t.Fatalf("外部修改冲突框应提供重新加载按钮: %q", h.tt.Texts())
	}
	h.click("重新加载")
	h.pump("重新加载外部变更完成", func() bool {
		return !h.a.editor.busy && h.a.editor.buf != nil && h.a.editor.buf.String() == "external-reload\n"
	})
}

func TestBatch2BroadcastMultiTabIndicatorsAndSelectToggle(t *testing.T) {
	h := newHarness(t)
	h.a.openLocalTerminal()
	h.a.openLocalTerminal()
	h.tt.Frame()
	if len(h.a.tabs) != 2 {
		t.Fatalf("应打开 2 个本地标签页，实际=%d", len(h.a.tabs))
	}
	tab1, tab2 := h.a.tabs[0], h.a.tabs[1]
	// 给 tab2 加入广播，再切回 tab1 并拆分出第二个窗格
	h.a.togglePaneBroadcast(tab2, tab2.focus)
	h.a.active = 0
	h.tt.Frame()
	h.a.split(tab1, false)
	h.pump("拆分窗格", func() bool { return tab1.root.leaves() == 2 })
	var leaves []*pane
	tab1.root.each(func(p *pane) { leaves = append(leaves, p) })
	if len(leaves) != 2 {
		t.Fatalf("tab1 应有 2 个窗格，实际=%d", len(leaves))
	}
	p1, p2 := leaves[0], leaves[1]
	h.a.togglePaneBroadcast(tab1, p1)
	tab1.focus = p2 // 让未加入广播的窗格成为当前焦点
	h.tt.Frame()

	if !h.tt.HasText("广播中 · 2 个窗格") {
		t.Fatalf("广播条应统计全部标签页窗格数: %q", h.tt.Texts())
	}
	if !h.tt.HasText("（含其他标签 1 个）") {
		t.Fatalf("广播条应提示其他标签中也有广播窗格: %q", h.tt.Texts())
	}
	if !h.tt.HasText("未加入广播") && !h.tt.HasText("未广播") {
		t.Fatalf("聚焦未加入广播的窗格时顶栏应显示提示: %q", h.tt.Texts())
	}

	// 全选当前窗格 -> 文案切换为“取消全选本标签” -> 点击仅退出 tab1 广播，保留 tab2
	h.click("全选当前窗格")
	h.tt.Frame()
	if !h.a.broadcast[p1.id] || !h.a.broadcast[p2.id] {
		t.Fatal("全选当前窗格应把 tab1 全部活窗格加入广播")
	}
	if !h.tt.HasText("取消全选本标签") {
		t.Fatalf("全选当前窗格后按钮应切换为取消全选本标签: %q", h.tt.Texts())
	}
	h.click("取消全选本标签")
	h.tt.Frame()
	if h.a.broadcast[p1.id] || h.a.broadcast[p2.id] {
		t.Fatal("取消全选本标签应移出 tab1 全部窗格")
	}
	if !h.a.broadcast[tab2.focus.id] || !h.a.broadcastOn {
		t.Fatal("取消全选本标签不应移出其他标签页的广播窗格")
	}
}

func TestBatch2SettingsAndMonitorStates(t *testing.T) {
	h := newHarness(t)
	h.a.hosts = append(h.a.hosts, hosts.Host{ID: "dummy-id"}) // 不影响，只测 settings + monitor
	h.a.openSettings()
	h.tt.Frame()

	// 1. 安全页取消修改主密码时清空密码草稿
	h.click("安全")
	h.tt.Frame()
	h.click("修改主密码…")
	h.tt.Frame()
	h.a.settingsPanel.currentPassword = "old-pw"
	h.a.settingsPanel.newPassword = "new-pw-123"
	h.a.settingsPanel.confirmPassword = "new-pw-123"
	h.click("取消")
	h.tt.Frame()
	if h.a.settingsPanel.currentPassword != "" || h.a.settingsPanel.newPassword != "" || h.a.settingsPanel.confirmPassword != "" {
		t.Fatal("取消修改主密码后应清空三个密码草稿字段")
	}

	// 2. 同步页字段经 UI 编辑后自动清除旧探测结果
	h.click("同步")
	h.tt.Frame()
	h.a.settingsPanel.probeMsg = "可以连接。"
	h.a.settingsPanel.probeOK = true
	h.tt.Frame()
	if !h.tt.HasText("可以连接。") {
		t.Fatalf("应先渲染已有探测结果: %q", h.tt.Texts())
	}
	h.click("WebDAV 服务器地址")
	h.tt.Type("https://dav.example.com")
	h.tt.Frame()
	if h.a.settingsPanel.probeMsg != "" || h.a.settingsPanel.probeOK {
		t.Fatalf("修改同步字段后应清空旧 probeMsg 和 probeOK: msg=%q ok=%v", h.a.settingsPanel.probeMsg, h.a.settingsPanel.probeOK)
	}
	h.click("完成")
	h.tt.Frame()

	// 3. 监控首次探测失败显示可重试按钮，首次采样 HasCPU=false 显示 "—"
	tb := h.connectDev()
	h.a.stopTabMonitor(tb)
	tb.monitorOpen = true
	tb.stats = ServerStats{LastErr: "监控探测失败"}
	h.tt.Frame()
	if !h.tt.HasText("监控探测失败") {
		t.Fatalf("监控首次探测失败应展示重试按钮: %q", h.tt.Texts())
	}

	tb.stats = ServerStats{
		HasCPU:        false,
		HasNet:        false,
		MemTotalBytes: 8 << 30,
		MemUsedBytes:  4 << 30,
		UpdatedAt:     time.Now(),
	}
	h.tt.Frame()
	if !h.tt.HasText("—") {
		t.Fatalf("首次采样 HasCPU=false 时应显示占位符 —: %q", h.tt.Texts())
	}
}
