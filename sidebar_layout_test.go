package main

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"serval/core/hosts"
)

// 侧栏主机名和地址太长时以“…”结尾，不能画出自己的布局框、压到行尾的“更多操作”按钮上。
// 布局框本身一直是对的，出错的是绘制：只用 NoWrap 时文字不会被截断，所以这里检查像素。
func TestHostRowLongTextIsCutWithEllipsis(t *testing.T) {
	long := hosts.Host{ID: "long", Name: "build-server-with-a-very-long-name", Address: "very-long-hostname.internal.example.com",
		Port: 22, User: "deploy-automation", Group: defaultGroup}
	h := newHarness(t, long)
	h.tt.Frame()
	name, ok := h.tt.Find(long.Name)
	if !ok {
		t.Fatalf("找不到主机名: %q", h.tt.Texts())
	}
	h.tt.Move(name.X+4, name.Y+name.H/2) // 悬停，显示“更多操作”
	for range 5 {                        // 等悬停背景的过渡动画结束
		h.tt.Advance(100 * time.Millisecond)
		h.tt.Frame()
	}
	if _, ok := h.tt.Find("更多操作"); !ok {
		t.Fatalf("悬停后应显示“更多操作”: %q", h.tt.Texts())
	}
	sub, ok := h.tt.Find(long.Target())
	if !ok {
		t.Fatalf("找不到地址: %q", h.tt.Texts())
	}

	// 测试窗口缩放比例是 1，图像像素和布局坐标一致。
	img := h.tt.Image()
	// 地址布局框右侧、同一行文字高度内只应是行背景。参考色取自布局框右侧、文字上方的行背景。
	ref := img.RGBAAt(int(sub.X+sub.W+4), int(name.Y+3))
	for x := int(sub.X + sub.W + 2); x < int(sub.X+sub.W+40); x++ {
		for y := int(sub.Y + 3); y < int(sub.Y+sub.H-2); y++ {
			c := img.RGBAAt(x, y)
			if absDiff(c.R, ref.R)+absDiff(c.G, ref.G)+absDiff(c.B, ref.B) > 90 {
				t.Fatalf("地址文字画出了布局框（%d,%d 处有文字像素），应以省略号截断", x, y)
			}
		}
	}
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func TestSidebarWidthResizeAndPersist(t *testing.T) {
	h := newHarness(t)
	h.a.rememberOnDevice(true)
	h.tt.Frame()

	grip, ok := h.tt.Find("调整侧栏宽度")
	if !ok {
		t.Fatal("找不到侧栏调宽把手")
	}
	if h.a.effectiveSidebarWidth() != 240 {
		t.Fatalf("默认侧栏宽度应为 240，实际 %v", h.a.effectiveSidebarWidth())
	}

	// 向右拖动 50px 并释放，应持久化到 device.json。
	gx, gy := grip.X+grip.W/2, grip.Y+grip.H/2
	h.tt.Press(gx, gy)
	h.tt.Move(gx+50, gy)
	h.tt.Frame()
	h.tt.Release(gx+50, gy)
	h.tt.Frame()
	if h.a.effectiveSidebarWidth() != 290 || h.a.local.SidebarWidth != 290 {
		t.Fatalf("拖动后侧栏宽度应为 290，实际 width=%v local=%v", h.a.effectiveSidebarWidth(), h.a.local.SidebarWidth)
	}

	// 拖超上限应夹紧在 360。
	grip, _ = h.tt.Find("调整侧栏宽度")
	gx, gy = grip.X+grip.W/2, grip.Y+grip.H/2
	h.tt.Press(gx, gy)
	h.tt.Move(gx+200, gy)
	h.tt.Frame()
	h.tt.Release(gx+200, gy)
	h.tt.Frame()
	if h.a.effectiveSidebarWidth() != 360 {
		t.Fatalf("最大宽度应限制在 360，实际 %v", h.a.effectiveSidebarWidth())
	}

	// 重启后应恢复自定义侧栏宽度。
	h.reopen()
	if h.a.effectiveSidebarWidth() != 360 {
		t.Fatalf("重启后侧栏宽度应恢复为 360，实际 %v", h.a.effectiveSidebarWidth())
	}

	// 键盘左移与双击恢复默认 240。
	grip, _ = h.tt.Find("调整侧栏宽度")
	h.tt.ClickAt(grip.X+grip.W/2, grip.Y+grip.H/2)
	h.tt.Key(0, ui.KeyLeft)
	h.tt.Frame()
	if h.a.effectiveSidebarWidth() != 344 {
		t.Fatalf("左方向键应缩窄 16px 到 344，实际 %v", h.a.effectiveSidebarWidth())
	}
	grip, _ = h.tt.Find("调整侧栏宽度")
	cx, cy := grip.X+grip.W/2, grip.Y+grip.H/2
	h.tt.ClickAt(cx, cy)
	h.tt.Advance(50 * time.Millisecond)
	h.tt.ClickAt(cx, cy)
	h.tt.Frame()
	if h.a.effectiveSidebarWidth() != 240 {
		t.Fatalf("双击调宽把手应恢复 240，实际 %v", h.a.effectiveSidebarWidth())
	}
}

func TestNarrowWindowDrawerBudgetAndFilesClose(t *testing.T) {
	h := newHarness(t)
	h.connectDev()
	h.tt.SetSize(820, 600)

	h.a.toggleFiles()
	h.tt.Frame()
	if !h.a.filesOpen {
		t.Fatal("文件抽屉应打开")
	}

	// 窄窗口下同时打开 AI 抽屉：后打开的 AI 应保留，文件抽屉自动让位，不再把终端压碎。
	h.a.openAI()
	h.tt.Frame()
	if !h.a.ai.open || h.a.filesOpen {
		t.Fatalf("窄窗口打开 AI 后应隐藏文件抽屉: ai=%v files=%v", h.a.ai.open, h.a.filesOpen)
	}

	// 再点文件抽屉：文件抽屉保留，AI 让位。
	h.a.toggleFiles()
	h.tt.Frame()
	if !h.a.filesOpen || h.a.ai.open {
		t.Fatalf("窄窗口切到文件抽屉后应隐藏 AI: files=%v ai=%v", h.a.filesOpen, h.a.ai.open)
	}

	// 点文件抽屉标题栏的关闭按钮可关闭。
	if !h.tt.HasText("文件管理") {
		t.Fatalf("文件抽屉应显示“文件管理”标题: %q", h.tt.Texts())
	}
	h.click("关闭文件抽屉")
	h.tt.Frame()
	if h.a.filesOpen {
		t.Fatal("点关闭按钮后文件抽屉应关闭")
	}
}

func TestDismissNoticeCollapsedSidebarAndClearFilter(t *testing.T) {
	h := newHarness(t)
	h.a.notice = "同步配置需要注意"
	h.a.hostsError = "主机列表读取异常"
	h.tt.Frame()
	if !h.tt.HasText("同步配置需要注意") || !h.tt.HasText("主机列表读取异常") {
		t.Fatalf("侧栏应显示通知与系统错误: %q", h.tt.Texts())
	}

	// 折叠侧栏后，工作区顶部仍能看到横幅并关闭。
	h.a.sidebarOpen = false
	h.tt.Frame()
	if !h.tt.HasText("同步配置需要注意") || !h.tt.HasText("主机列表读取异常") {
		t.Fatalf("侧栏收起后工作区仍应显示横幅: %q", h.tt.Texts())
	}
	h.click("关闭通知")
	h.click("关闭系统错误")
	h.tt.Frame()
	if h.a.notice != "" || h.a.hostsError != "" {
		t.Fatalf("横幅关闭后应清空: notice=%q err=%q", h.a.notice, h.a.hostsError)
	}

	// 侧栏搜索框的清除过滤按钮。
	h.a.sidebarOpen = true
	h.a.hostFilter = "dev"
	h.tt.Frame()
	h.click("清除过滤")
	h.tt.Frame()
	if !strings.EqualFold(h.a.hostFilter, "") {
		t.Fatalf("点清除过滤后应清空搜索关键字，实际 %q", h.a.hostFilter)
	}
}

func TestSidebarTagsCollapsedGroupAndShortcutNavigation(t *testing.T) {
	h := newHarness(t)
	dev := h.devHost()
	dev.Tags = []string{"prod", "web", "edge", "db"}
	if err := h.a.apply(hostChange(dev)); err != nil {
		t.Fatal(err)
	}
	h.tt.Frame()
	for _, want := range []string{"#prod", "#web", "#edge", "+1"} {
		if !h.tt.HasText(want) {
			t.Fatalf("侧栏主机行应展示标签微徽章 %q: %q", want, h.tt.Texts())
		}
	}

	// 折叠分组后显示数量并隐藏分组内主机行
	h.click("折叠 " + defaultGroup)
	h.tt.Frame()
	if !h.tt.HasText("1") || h.tt.HasText("+1") || h.tt.HasText("dev") {
		t.Fatalf("折叠分组后应显示主机计数并隐藏主机行: %q", h.tt.Texts())
	}
	h.click("展开 " + defaultGroup)
	h.tt.Frame()

	tb := h.connectDev()
	mods := chromeShortcut()

	// Ctrl+Shift+T / Cmd+T 新建本地终端，Ctrl+Shift+1/2 / Cmd+1/2 切标签
	h.tt.Key(mods, ui.KeyT)
	h.pump("快捷键新建本地终端", func() bool { return len(h.a.tabs) == 2 && h.a.active == 1 })

	h.tt.Key(mods, ui.Key1)
	h.tt.Frame()
	if h.a.active != 0 {
		t.Fatalf("快捷键 1 应切回第一个标签，实际 active=%d", h.a.active)
	}
	h.tt.Key(mods, ui.Key9)
	h.tt.Frame()
	if h.a.active != 1 {
		t.Fatalf("快捷键 9 应切到最后一个标签，实际 active=%d", h.a.active)
	}

	// 切回 SSH 标签拆分窗格并用 Alt+左右方向键在窗格间切换焦点
	h.tt.Key(mods, ui.Key1)
	h.tt.Frame()
	firstPane := tb.focus
	h.a.split(tb, false)
	h.pump("拆出第二个窗格", func() bool { return tb.root.leaves() == 2 && tb.focus != firstPane })
	secondPane := tb.focus

	h.tt.Key(mods|ui.Alt, ui.KeyLeft)
	h.tt.Frame()
	if tb.focus != firstPane {
		t.Fatalf("Alt+Left 应切回第一个窗格: got=%v want=%v", tb.focus.id, firstPane.id)
	}
	h.tt.Key(mods|ui.Alt, ui.KeyRight)
	h.tt.Frame()
	if tb.focus != secondPane {
		t.Fatalf("Alt+Right 应切回第二个窗格: got=%v want=%v", tb.focus.id, secondPane.id)
	}

	// Ctrl+Shift+N / Cmd+N 打开新建主机表单
	h.tt.Key(mods, ui.KeyN)
	h.tt.Frame()
	if !h.a.form.open {
		t.Fatal("快捷键 N 应打开新建主机表单")
	}
}
