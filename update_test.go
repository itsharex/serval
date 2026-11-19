package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVersionComparison(t *testing.T) {
	cases := []struct {
		remote string
		local  string
		newer  bool
	}{
		{"0.1.0", "0.1.0", false},
		{"v0.1.0", "0.1.0", false},
		{"0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.0", false},
		{"0.2.0", "0.1.0", true},
		{"v0.2.0", "0.1.0", true},
		{"0.1.1", "0.1.0", true},
		{"1.0.0", "0.1.0", true},
		{"0.0.9", "0.1.0", false},
		{"0.1.0", "0.0.9", true},
		{"0.1.0", "0.1.0-beta.1", true},
		{"0.1.0-beta.1", "0.1.0", false},
		{"0.1.0-beta.2", "0.1.0-beta.1", true},
		{"0.2.0-alpha", "0.1.0", true},
		{"0.0.9-rc.1", "0.1.0", false},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_vs_%s", tc.remote, tc.local), func(t *testing.T) {
			got := isNewerVersion(tc.remote, tc.local)
			if got != tc.newer {
				t.Fatalf("isNewerVersion(%q, %q) = %v, want %v", tc.remote, tc.local, got, tc.newer)
			}
		})
	}
}

func TestFetchLatestRelease(t *testing.T) {
	pubTime := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("缺少 User-Agent 请求头")
		}
		switch r.URL.Path {
		case "/latest":
			_ = json.NewEncoder(w).Encode(githubReleaseResponse{
				TagName:     "v0.2.0",
				Name:        "Serval 0.2.0 发布",
				Body:        "## 新功能\n- 支持检查更新",
				HTMLURL:     "https://github.com/jhzhang09/serval/releases/tag/v0.2.0",
				PublishedAt: pubTime,
			})
		case "/notfound":
			http.NotFound(w, r)
		case "/invalid-json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{invalid-json"))
		case "/empty-tag":
			_ = json.NewEncoder(w).Encode(githubReleaseResponse{
				TagName: "",
				Name:    "Invalid",
			})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	ctx := context.Background()

	// 正常抓取
	info, err := fetchLatestRelease(ctx, ts.Client(), ts.URL+"/latest")
	if err != nil {
		t.Fatalf("fetchLatestRelease failed: %v", err)
	}
	if info.Version != "0.2.0" || info.Title != "Serval 0.2.0 发布" || info.URL != "https://github.com/jhzhang09/serval/releases/tag/v0.2.0" {
		t.Fatalf("解析 Release 字段不正确: %+v", info)
	}

	// 404
	if _, err := fetchLatestRelease(ctx, ts.Client(), ts.URL+"/notfound"); err == nil {
		t.Fatal("404 应该返回错误")
	}

	// 无效 JSON
	if _, err := fetchLatestRelease(ctx, ts.Client(), ts.URL+"/invalid-json"); err == nil {
		t.Fatal("无效 JSON 应该返回错误")
	}

	// 空 Tag
	if _, err := fetchLatestRelease(ctx, ts.Client(), ts.URL+"/empty-tag"); err == nil {
		t.Fatal("空 Tag 应该返回错误")
	}
}

func TestAppCheckForUpdatesManualLatest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(githubReleaseResponse{
			TagName: "v" + appVersion,
			Name:    "Serval v" + appVersion,
			Body:    "已经是最新版",
			HTMLURL: "https://github.com/jhzhang09/serval/releases/tag/v" + appVersion,
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.updateURL = ts.URL
	h.a.httpClient = ts.Client()

	h.a.openSettings()
	h.pump("打开设置面板", func() bool { return h.a.settingsPanel.open })
	h.tt.Frame()

	// 滚动到关于与更新区域
	if box, ok := h.tt.Find("外观"); ok {
		h.tt.Scroll(box.X+50, box.Y+50, 0, 450)
		h.tt.Frame()
	}

	// 点击检查更新按钮
	h.click("检查更新")
	h.pump("检查更新完成", func() bool { return h.a.update.state == updateLatest })
	h.tt.Frame()

	if !h.tt.HasText(fmt.Sprintf("已是最新版本 (v%s)", appVersion)) {
		t.Fatalf("应该显示已是最新版本: texts=%q", h.tt.Texts())
	}
}

func TestAppCheckForUpdatesDiscoversNewVersion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(githubReleaseResponse{
			TagName:     "v99.0.0",
			Name:        "Serval 99.0.0 新版本",
			Body:        "## 更新日志\n- 支持检查更新与 Homebrew 升级引导",
			HTMLURL:     "https://github.com/jhzhang09/serval/releases/tag/v99.0.0",
			PublishedAt: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC),
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.updateURL = ts.URL
	h.a.httpClient = ts.Client()

	h.a.openSettings()
	h.pump("打开设置面板", func() bool { return h.a.settingsPanel.open })
	h.tt.Frame()

	// 滚动到关于与更新区域
	if box, ok := h.tt.Find("外观"); ok {
		h.tt.Scroll(box.X+50, box.Y+50, 0, 450)
		h.tt.Frame()
	}

	// 点击检查更新
	h.click("检查更新")
	h.pump("发现新版本并弹出更新面板", func() bool { return h.a.update.sheetOpen })
	h.tt.Frame()

	if !h.tt.HasText("发现新版本 v99.0.0") {
		t.Fatalf("更新浮层未显示版本号: texts=%q", h.tt.Texts())
	}
	if !h.tt.HasText("更新日志") {
		t.Fatalf("更新浮层未展示更新日志: texts=%q", h.tt.Texts())
	}

	if !h.tt.HasText("当前 v" + appVersion) {
		t.Fatalf("更新浮层未显示当前版本号: texts=%q", h.tt.Texts())
	}

	// 按平台复制升级命令或下载链接
	if runtime.GOOS == "darwin" {
		h.click("复制升级命令")
		h.tt.Frame()
		if h.tt.Clipboard() != "brew upgrade serval" {
			t.Fatalf("剪贴板内容不正确: got %q, want %q", h.tt.Clipboard(), "brew upgrade serval")
		}
	} else {
		h.click("复制下载链接")
		h.tt.Frame()
		wantURL := "https://github.com/jhzhang09/serval/releases/tag/v99.0.0"
		if h.tt.Clipboard() != wantURL {
			t.Fatalf("剪贴板内容不正确: got %q, want %q", h.tt.Clipboard(), wantURL)
		}
	}

	// 点击前往 GitHub 下载
	h.click("前往 GitHub 下载")
	h.tt.Frame()
	if h.a.update.sheetOpen {
		t.Fatal("点击下载后浮层应关闭")
	}
	if len(h.openedURLs) != 1 || h.openedURLs[0] != "https://github.com/jhzhang09/serval/releases/tag/v99.0.0" {
		t.Fatalf("打开的下载链接不匹配: got %v", h.openedURLs)
	}
}

func TestAppAutoCheckUpdatesOnVaultReady(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(githubReleaseResponse{
			TagName: "v0.9.9",
			Name:    "Serval 0.9.9",
			Body:    "自动检测到的新版本",
			HTMLURL: "https://github.com/jhzhang09/serval/releases/tag/v0.9.9",
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.updateURL = ts.URL
	h.a.httpClient = ts.Client()

	h.a.onVaultReady()
	h.pump("启动自动检查弹出新版本浮层", func() bool { return h.a.update.sheetOpen })
	h.tt.Frame()

	if !h.tt.HasText("发现新版本 v0.9.9") {
		t.Fatalf("自动检查未弹出新版本浮层: texts=%q", h.tt.Texts())
	}
}

func TestAppAutoCheckUpdateToggle(t *testing.T) {
	h := newHarness(t)
	h.a.openSettings()
	h.pump("打开设置面板", func() bool { return h.a.settingsPanel.open })
	h.tt.Frame()

	// 滚动到关于与更新区域
	if box, ok := h.tt.Find("外观"); ok {
		h.tt.Scroll(box.X+50, box.Y+50, 0, 450)
		h.tt.Frame()
	}

	// 默认状态：DisableAutoCheckUpdate == false (即开启)
	if h.a.local.DisableAutoCheckUpdate {
		t.Fatal("默认应为自动检查更新")
	}

	// 取消勾选
	h.click("启动时自动检查更新")
	h.tt.Frame()
	if !h.a.local.DisableAutoCheckUpdate {
		t.Fatal("点击后应禁用自动检查更新")
	}

	// 确认写入 device.json
	raw, err := os.ReadFile(filepath.Join(h.dir, "device.json"))
	if err != nil {
		t.Fatalf("读取 device.json 失败: %v", err)
	}
	var dev deviceSettings
	if err := json.Unmarshal(raw, &dev); err != nil {
		t.Fatalf("解析 device.json 失败: %v", err)
	}
	if !dev.DisableAutoCheckUpdate {
		t.Fatalf("device.json 应记录 disableAutoCheckUpdate=true: %s", string(raw))
	}
}

func TestPaletteCheckUpdate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(githubReleaseResponse{
			TagName: "v" + appVersion,
			Name:    "Serval v" + appVersion,
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.updateURL = ts.URL
	h.a.httpClient = ts.Client()

	h.a.openPalette()
	h.tt.Frame()

	h.a.pal.query = "更新"
	h.tt.Frame()

	h.click("检查更新")
	h.pump("命令面板触发检查更新", func() bool { return h.a.update.state == updateLatest })
	h.tt.Frame()

	if h.a.update.state != updateLatest {
		t.Fatalf("命令面板触发检查更新状态不正确: got %v", h.a.update.state)
	}
}

func TestUpdateDialogRendersMarkdownNotes(t *testing.T) {
	body := `## Downloads · 下载

| Platform · 平台 | Desktop App · 桌面应用 |
|---|---|
| macOS 13+ | ` + "`serval-99.0.0.dmg`" + ` |
| Windows 10 / 11 | Installer · 安装版<br>` + "`serval-setup-99.0.0-amd64.exe`" + ` |

> **macOS:** ad-hoc signed, use Homebrew.

<!-- Release notes generated using configuration in .github/release.yml -->

- 修复更新日志 Markdown 表格渲染`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(githubReleaseResponse{
			TagName:     "v99.0.0",
			Name:        "Serval v99.0.0",
			Body:        body,
			HTMLURL:     "https://github.com/jhzhang09/serval/releases/tag/v99.0.0",
			PublishedAt: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC),
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.updateURL = ts.URL
	h.a.httpClient = ts.Client()
	h.a.checkForUpdates(true)
	h.pump("弹出带 Markdown 更新日志的新版本浮层", func() bool { return h.a.update.sheetOpen })
	h.tt.Frame()

	texts := h.tt.Texts()
	for _, rawMarker := range []string{"## ", "|---|", "<br>", "<!--", "**macOS:**"} {
		for _, txt := range texts {
			if strings.Contains(txt, rawMarker) {
				t.Fatalf("更新日志仍显示未渲染的原始标记 %q: %q", rawMarker, txt)
			}
		}
	}
	if !h.tt.HasText("Downloads · 下载") {
		t.Fatalf("未渲染 Markdown 标题: texts=%q", texts)
	}
	if !h.tt.HasText("Platform · 平台") || !h.tt.HasText("serval-99.0.0.dmg") {
		t.Fatalf("未渲染 Markdown 表格单元: texts=%q", texts)
	}
	if !h.tt.HasText("• 修复更新日志 Markdown 表格渲染") {
		t.Fatalf("未渲染 Markdown 列表项: texts=%q", texts)
	}
}

func TestUpdateSkipVersionAndOpenURLFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(githubReleaseResponse{
			TagName: "v99.1.0",
			Name:    "Serval v99.1.0",
			Body:    "- 跳过版本与浏览器回退测试",
			HTMLURL: "https://github.com/jhzhang09/serval/releases/tag/v99.1.0",
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.updateURL = ts.URL
	h.a.httpClient = ts.Client()

	// 1. 首次自动检查弹出更新浮层，点击“跳过此版本”
	h.a.checkForUpdates(false)
	h.pump("首次自动检查弹窗", func() bool { return h.a.update.sheetOpen })
	h.tt.Frame()

	h.click("跳过此版本")
	h.tt.Frame()
	if h.a.update.sheetOpen {
		t.Fatal("点击跳过此版本后浮层应关闭")
	}
	if h.a.local.SkippedVersion != "99.1.0" {
		t.Fatalf("应记录跳过的版本号 99.1.0，实际=%q", h.a.local.SkippedVersion)
	}

	// 2. 再次静默检查同一版本不应自动弹窗
	h.a.update.state = updateIdle
	h.a.checkForUpdates(false)
	h.pump("静默检查完成", func() bool { return !h.a.update.checking && h.a.update.state == updateAvailable })
	h.tt.Frame()
	if h.a.update.sheetOpen {
		t.Fatal("已跳过的版本不应在静默检查时自动弹窗")
	}

	// 3. 用户主动检查仍应弹窗并清除跳过标记
	h.a.checkForUpdates(true)
	h.pump("主动检查弹窗", func() bool { return h.a.update.sheetOpen })
	h.tt.Frame()
	if h.a.local.SkippedVersion != "" {
		t.Fatalf("主动检查发现新版本后应清除 SkippedVersion，实际=%q", h.a.local.SkippedVersion)
	}

	// 4. 打开浏览器失败时回退复制下载链接
	h.a.openURL = func(string) error { return errors.New("no browser") }
	h.click("前往 GitHub 下载")
	h.tt.Frame()
	wantURL := "https://github.com/jhzhang09/serval/releases/tag/v99.1.0"
	if h.tt.Clipboard() != wantURL {
		t.Fatalf("openURL 失败时应复制下载链接，实际=%q want=%q", h.tt.Clipboard(), wantURL)
	}
	if !h.tt.HasText("无法打开浏览器，已复制下载链接") {
		t.Fatalf("openURL 失败时应弹出复制提示: texts=%q", h.tt.Texts())
	}
}
