package main

import (
	"fmt"
	"runtime/debug"
	"strings"

	"serval/design"

	"github.com/egoist/mygo/ui"
)

const (
	appAuthor  = "jhzhang09"
	appRepoURL = "https://github.com/jhzhang09/serval"
	appLicense = "MIT License"
)

func mygoEngineVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "MyGo"
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/egoist/mygo" && dep.Version != "" {
			return "MyGo " + dep.Version
		}
	}
	return "MyGo"
}

// settingsAboutSection 在设置面板的“关于”标签页渲染署名、项目信息与更新检查。
func (a *app) settingsAboutSection(c *ui.Context, colors design.Colors) {
	t := c.Theme()
	ui.Column(c).Gap(14).Children(func() {
		// 品牌图标、版本号与简介
		ui.Row(c).Gap(14).AlignItems(ui.Center).Children(func() {
			design.BrandView(c, 48).Shadow(0, 8, 20, -6, colors.CardShadow)
			ui.Column(c).Gap(4).Grow(1).MinWidth(0).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "Serval").FontSize(design.TextDisplay - 6).FontWeight(700)
					ui.Box(c).Padding(2, 8).Radius(design.RadiusChip).Background(t.Accent.Alpha(0.14)).Children(func() {
						ui.Text(c, "v"+appVersion).FontSize(design.TextSmall).FontWeight(600).TextColor(t.Accent)
					})
				})
				ui.Text(c, "原生 GPU 渲染的轻量级跨平台 SSH 客户端，所有用户凭据均经端到端加密保护。").
					FontSize(design.TextSmall).TextColor(t.TextMuted)
			})
		})

		// 署名与项目信息卡片
		ui.Column(c).Radius(design.RadiusCard).Background(colors.Hover).Border(1, colors.CardBorder).Children(func() {
			aboutInfoRow(c, colors, "作者", true, func() {
				ui.Text(c, appAuthor).FontSize(design.TextBody).FontWeight(600).TextColor(t.Text)
			})
			aboutInfoRow(c, colors, "GitHub 仓库", true, func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Grow(1).MinWidth(0).Children(func() {
					ui.Text(c, appRepoURL).FontSize(design.TextSmall).TextColor(t.Accent).
						SingleLine().Ellipsis("…").Grow(1).MinWidth(0).Tooltip(appRepoURL)
					ui.Button(c.Key("aboutOpenGitHub"), "访问 GitHub").Height(26).Padding(0, 8).OnClick(func() {
						if a.openURL == nil || a.openURL(appRepoURL) != nil {
							c.WriteClipboard(appRepoURL)
							a.toast(ui.Toast{Title: "无法打开浏览器，已复制仓库链接"})
						}
					})
				})
			})
			aboutInfoRow(c, colors, "开源协议", true, func() {
				ui.Text(c, appLicense).FontSize(design.TextBody).TextColor(t.Text)
			})
			aboutInfoRow(c, colors, "界面引擎", false, func() {
				ui.Text(c, mygoEngineVersion()).FontSize(design.TextBody).TextColor(t.Text)
			})
		})

		// 更新检查与自动检查设置
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			checkBtnText := "检查更新"
			if a.update.checking {
				checkBtnText = "正在检查…"
			}
			ui.Button(c.Key("settingsCheckUpdate"), checkBtnText).Disabled(a.update.checking).OnClick(func() {
				a.checkForUpdates(true)
			})
			if a.update.state == updateAvailable && a.update.latest != nil {
				ui.Button(c.Key("settingsViewUpdate"), fmt.Sprintf("发现新版本 v%s", strings.TrimPrefix(a.update.latest.Version, "v"))).
					OnClick(func() {
						a.update.sheetOpen = true
						a.update.sheetFresh = true
					})
			}
		})

		autoCheck := !a.local.DisableAutoCheckUpdate
		c.Key("settingsAutoCheckUpdate")
		ui.Checkbox(c, &autoCheck, "启动时自动检查更新").OnChange(func() {
			next := a.local
			next.DisableAutoCheckUpdate = !autoCheck
			if err := a.saveLocal(next); err != nil {
				a.settingsPanel.err = err.Error()
			}
		})

		if a.update.checking {
			ui.Text(c, "正在连接 GitHub Releases 检查新版本…").FontSize(design.TextSmall).TextColor(t.TextMuted)
		} else if a.update.state == updateLatest {
			status := fmt.Sprintf("已是最新版本 (v%s)", appVersion)
			if !a.update.lastChecked.IsZero() {
				status += fmt.Sprintf(" · 上次检查于 %s", a.update.lastChecked.Format("15:04:05"))
			}
			ui.Text(c, status).FontSize(design.TextSmall).TextColor(t.Accent)
		} else if a.update.state == updateError && a.update.err != "" {
			ui.Text(c, fmt.Sprintf("检查更新失败：%s", a.update.err)).FontSize(design.TextSmall).TextColor(colors.Danger)
		}
	})
}

func aboutInfoRow(c *ui.Context, colors design.Colors, label string, divider bool, value func()) {
	t := c.Theme()
	row := ui.Row(c).Padding(10, 12).Gap(12).AlignItems(ui.Center)
	if divider {
		row.BorderWidth(0, 0, 1, 0).BorderColor(colors.Hairline)
	}
	row.Children(func() {
		ui.Text(c, label).Width(86).Shrink(0).FontSize(design.TextSmall).TextColor(t.TextMuted)
		ui.Row(c).Grow(1).MinWidth(0).AlignItems(ui.Center).Children(value)
	})
}
