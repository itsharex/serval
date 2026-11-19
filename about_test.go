package main

import (
	"testing"
)

func TestAboutDialogAndAttribution(t *testing.T) {
	h := newHarness(t)
	h.tt.Frame()

	var openedURL string
	h.a.openURL = func(u string) error {
		openedURL = u
		return nil
	}

	// 1. 从侧栏底栏按钮打开“关于”浮层
	h.click("关于")
	h.tt.Frame()
	if !h.a.about.open {
		t.Fatal("点击侧栏关于按钮后应打开关于浮层")
	}

	for _, want := range []string{"关于 Serval", "v" + appVersion, appAuthor, appRepoURL, appLicense, "MyGo"} {
		if !h.tt.HasText(want) {
			t.Fatalf("关于浮层缺少文本 %q，当前文本: %q", want, h.tt.Texts())
		}
	}

	// 2. 点击“访问 GitHub”打开仓库主页
	h.click("访问 GitHub")
	h.tt.Frame()
	if openedURL != appRepoURL {
		t.Fatalf("openURL=%q, want %q", openedURL, appRepoURL)
	}

	// 3. 点击“关闭”关闭浮层
	h.click("关闭")
	h.tt.Frame()
	if h.a.about.open {
		t.Fatal("点击关闭后关于浮层应当关闭")
	}

	// 4. 从设置面板打开“关于 Serval”，关闭后回到设置面板（不叠两层模态）
	h.click("设置")
	h.tt.Frame()
	h.click("关于 Serval")
	h.tt.Frame()
	if !h.a.about.open {
		t.Fatal("设置面板中点击关于 Serval 后应打开关于浮层")
	}
	h.click("关闭")
	h.tt.Frame()
	if h.a.about.open || !h.a.settingsPanel.open {
		t.Fatalf("关闭关于浮层后应恢复设置面板: about=%v, settings=%v", h.a.about.open, h.a.settingsPanel.open)
	}
}
