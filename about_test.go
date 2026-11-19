package main

import (
	"testing"
)

func TestSettingsAboutTabAndAttribution(t *testing.T) {
	h := newHarness(t)
	h.tt.Frame()

	var openedURL string
	h.a.openURL = func(u string) error {
		openedURL = u
		return nil
	}

	// 1. 打开设置面板，默认“通用”页不再包含冗余的“关于与更新”
	h.click("设置")
	h.tt.Frame()
	if h.tt.HasText("关于与更新") {
		t.Fatal("通用设置页不应再保留冗余的“关于与更新”区块")
	}

	// 2. 切换到“关于”标签页，验证署名、仓库、协议与引擎信息
	h.click("关于")
	h.tt.Frame()

	for _, want := range []string{"v" + appVersion, appAuthor, appRepoURL, appLicense, "MyGo", "检查更新"} {
		if !h.tt.HasText(want) {
			t.Fatalf("设置“关于”页缺少文本 %q，当前文本: %q", want, h.tt.Texts())
		}
	}

	// 3. 点击“访问 GitHub”打开仓库主页
	h.click("访问 GitHub")
	h.tt.Frame()
	if openedURL != appRepoURL {
		t.Fatalf("openURL=%q, want %q", openedURL, appRepoURL)
	}
}
