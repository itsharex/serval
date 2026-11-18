package main

import (
	"cmp"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"serval/core/sshconn"
	"serval/design"
	"serval/locales"

	"github.com/egoist/mygo/ui"
)

const (
	fileSortName = iota
	fileSortSize
	fileSortTime
)

// localEntry 是本机目录里的一项。路径是绝对路径，只在主线程读写。
type localEntry struct {
	Name    string
	Path    string
	IsDir   bool
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
}

// paneDrag 是从文件抽屉某一栏拖出来的一项。local 为真表示来自本机栏。
type paneDrag struct {
	local bool
	path  string
	name  string
	dir   bool
}

// ensureLocalList 在抽屉第一次打开时列出本机目录。测试会事先放好 localDir，避免去读用户的主目录。
func (a *app) ensureLocalList() {
	if a.localGen > 0 {
		return
	}
	dir := a.localDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			a.localErr = "读不到本机的主目录。"
			a.localGen = 1
			return
		}
		dir = home
	}
	a.openLocal(dir)
}

// openLocal 在后台读取本机目录，主线程只应用最后一次请求的结果。
func (a *app) openLocal(dir string) {
	if dir == "" {
		return
	}
	a.localGen++
	gen := a.localGen
	a.localDir = dir
	go func() {
		entries, err := listLocal(dir)
		a.post(func() {
			if gen != a.localGen {
				return
			}
			if err != nil {
				a.localEntries = nil
				a.localErr = "读不到这个本地目录。"
				return
			}
			a.localEntries = entries
			a.localErr = ""
		})
	}()
}

func listLocal(dir string) ([]localEntry, error) {
	dents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	entries := make([]localEntry, 0, len(dents))
	for _, d := range dents {
		info, err := d.Info()
		if err != nil {
			continue
		}
		// 不跟随符号链接，避免点进去绕回上级目录。
		entries = append(entries, localEntry{
			Name:    d.Name(),
			Path:    filepath.Join(dir, d.Name()),
			IsDir:   d.IsDir(),
			Size:    info.Size(),
			Mode:    info.Mode(),
			ModTime: info.ModTime(),
		})
	}
	slices.SortFunc(entries, func(a, b localEntry) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}
			return 1
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return entries, nil
}

func compareFileMeta(aDir, bDir bool, aName, bName string, aSize, bSize int64, aMod, bMod time.Time, sortBy int, desc bool) int {
	if aDir != bDir {
		if aDir {
			return -1
		}
		return 1
	}
	var c int
	switch sortBy {
	case fileSortSize:
		c = cmp.Compare(aSize, bSize)
	case fileSortTime:
		c = aMod.Compare(bMod)
	}
	if c == 0 {
		c = cmp.Compare(strings.ToLower(aName), strings.ToLower(bName))
	}
	if desc {
		return -c
	}
	return c
}

func filterLocalEntries(entries []localEntry, query string, sortBy int, desc, hideHidden bool) []localEntry {
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]localEntry, 0, len(entries))
	for _, e := range entries {
		if hideHidden && strings.HasPrefix(e.Name, ".") {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Name), q) {
			continue
		}
		out = append(out, e)
	}
	if sortBy != fileSortName || desc {
		slices.SortFunc(out, func(a, b localEntry) int {
			return compareFileMeta(a.IsDir, b.IsDir, a.Name, b.Name, a.Size, b.Size, a.ModTime, b.ModTime, sortBy, desc)
		})
	}
	return out
}

func filterRemoteEntries(entries []sshconn.Entry, query string, sortBy int, desc, hideHidden bool) []sshconn.Entry {
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]sshconn.Entry, 0, len(entries))
	for _, e := range entries {
		if hideHidden && strings.HasPrefix(e.Name, ".") {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Name), q) {
			continue
		}
		out = append(out, e)
	}
	if sortBy != fileSortName || desc {
		slices.SortFunc(out, func(a, b sshconn.Entry) int {
			return compareFileMeta(a.IsDir, b.IsDir, a.Name, b.Name, a.Size, b.Size, a.ModTime, b.ModTime, sortBy, desc)
		})
	}
	return out
}

func formatShortTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if t.Year() == time.Now().Year() {
		return t.Format("01-02 15:04")
	}
	return t.Format("2006-01-02")
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func (a *app) cdTerminalToRemoteDir(t *tab) {
	if t == nil || t.local || t.dir == "" {
		return
	}
	if t.focus == nil || t.focus.term == nil || t.focus.ended {
		a.toast(ui.Toast{Title: locales.RemoteCdDisconnected(), Type: "error"})
		return
	}
	t.followed = t.dir
	t.focus.term.Send([]byte("cd " + shellSingleQuote(t.dir) + "\n"))
	t.focus.termHandle.Focus()
	a.toast(ui.Toast{Title: locales.RemoteCdDone(path.Base(t.dir))})
}

type localQuickDir struct {
	label string
	path  string
}

func (a *app) localQuickDirs(t *tab) []localQuickDir {
	var out []localQuickDir
	seen := map[string]bool{}
	add := func(label, p string) {
		if p == "" {
			return
		}
		cleaned := filepath.Clean(p)
		if seen[cleaned] {
			return
		}
		info, err := os.Stat(cleaned)
		if err != nil || !info.IsDir() {
			return
		}
		seen[cleaned] = true
		out = append(out, localQuickDir{label: label, path: cleaned})
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(locales.LocalQuickHome(), home)
		add(locales.LocalQuickDownloads(), filepath.Join(home, "Downloads"))
		add(locales.LocalQuickDesktop(), filepath.Join(home, "Desktop"))
		add(locales.LocalQuickDocuments(), filepath.Join(home, "Documents"))
	}
	if t != nil && t.local && t.focus != nil && t.focus.term != nil {
		if cwd := t.focus.term.Dir(); cwd != "" {
			add(locales.LocalQuickTermDir(), cwd)
		}
	}
	return out
}

// acceptPaneDrop 把拖到另一栏的文件传过去。同一栏里松开什么都不做。
// 传到远端时沿用上传的替换确认；下到本机时若同名普通文件已存在则询问替换，目录冲突继续拒绝。
func (a *app) acceptPaneDrop(t *tab, destLocal bool, destDir string, item paneDrag) {
	if t == nil || item.local == destLocal || destDir == "" || item.name == "" || item.path == "" {
		return
	}
	if item.local {
		if t.local || t.files == nil {
			return
		}
		if item.dir {
			a.planFolderUpload(t, item.path, destDir)
			return
		}
		a.uploadTo(t, item.path, path.Join(destDir, item.name))
		return
	}
	if t.local || t.files == nil {
		return
	}
	if err := validateFileName(item.name); err != nil {
		a.toast(ui.Toast{Title: "无法下载这个文件", Description: err.Error(), Type: "error"})
		return
	}
	local := filepath.Join(destDir, item.name)
	if info, err := os.Lstat(local); err == nil {
		if item.dir || info.IsDir() {
			a.toast(ui.Toast{Title: "没有下载 " + item.name, Description: "本地已经有同名的文件或文件夹，请先把原来的移走。", Type: "error"})
			return
		}
		a.pendingDownloads = append(a.pendingDownloads, &pendingDownload{
			tab: t, remotePath: item.path, localPath: local, fileName: item.name,
		})
		return
	}
	if item.dir {
		a.runFolderDownload(t, item.path, local, item.name)
		return
	}
	// 拖放时先查一次是为了即时反馈；下载完成、改名落盘前 DownloadIfAbsent
	// 会再查一次，兜住“检查之后、落盘之前”本地出现同名文件的窗口。
	a.runTransfer(t, transferDownload, item.path, local, item.name, true)
}

// drawerPath 返回文件抽屉标题栏显示的路径；路径过长时保留末尾层级，避免右端截断遮住当前目录名。
func drawerPath(p string) string {
	if len(p) <= 38 {
		return p
	}
	return shortPath(p)
}

// toggleRemoteBookmark 收藏或取消收藏当前远程目录到主机记录。
func (a *app) toggleRemoteBookmark(t *tab) {
	if t == nil || t.local || t.dir == "" {
		return
	}
	h, ok := a.hostByID(t.host.ID)
	if !ok {
		return
	}
	dir := path.Clean(t.dir)
	next := slices.Clone(h.Bookmarks)
	if idx := slices.Index(next, dir); idx >= 0 {
		next = slices.Delete(next, idx, idx+1)
	} else {
		next = append(next, dir)
	}
	h.Bookmarks = next
	if err := a.apply(hostChange(h)); err != nil {
		a.toast(ui.Toast{Title: "保存目录书签失败", Description: err.Error(), Type: "error"})
		return
	}
	t.host.Bookmarks = slices.Clone(next)
}

func (a *app) removeRemoteBookmark(t *tab, dir string) {
	if t == nil {
		return
	}
	h, ok := a.hostByID(t.host.ID)
	if !ok {
		return
	}
	next := slices.DeleteFunc(slices.Clone(h.Bookmarks), func(b string) bool { return b == dir })
	h.Bookmarks = next
	if err := a.apply(hostChange(h)); err != nil {
		a.toast(ui.Toast{Title: "删除目录书签失败", Description: err.Error(), Type: "error"})
		return
	}
	t.host.Bookmarks = slices.Clone(next)
}

func fileToolIconButton(c *ui.Context, colors design.Colors, icon, label string, fn func()) ui.Element {
	t := c.Theme()
	b := ui.ButtonBase(c).Size(24, 24).Center().Radius(design.RadiusRow - 2).Label(label).Tooltip(label).Shrink(0)
	if b.Hovered() {
		b.Background(colors.Hover)
	}
	b.Children(func() {
		ui.Icon(c, design.Icon(icon)).FontSize(13).TextColor(t.TextMuted)
	})
	return b.OnClick(fn)
}

func fileFilterSubbar(c *ui.Context, colors design.Colors, keyPrefix string, filter *string, filterLabel, clearLabel, hiddenLabel, sortLabel string, sortBy *int, sortDesc, hideHidden *bool, disabled bool) {
	t := c.Theme()
	ui.Row(c.Key(keyPrefix+"Subbar")).Height(28).Shrink(0).Padding(0, 10, 4, 10).Gap(4).AlignItems(ui.Center).
		BorderWidth(0, 0, 1, 0).BorderColor(colors.Hairline).Children(func() {
		ui.Row(c).Height(22).Grow(1).MinWidth(0).Padding(0, 4, 0, 6).Gap(4).AlignItems(ui.Center).
			Radius(4).Background(colors.Chip).Children(func() {
			ui.Icon(c, design.Icon("search")).FontSize(12).TextColor(colors.TextFaint).Shrink(0)
			c.Key(keyPrefix + "FilterInput")
			ui.TextInput(c, filter).Placeholder(locales.FileFilterPlaceholder()).Label(filterLabel).Disabled(disabled).
				Grow(1).MinWidth(0).FontSize(design.TextSmall-1).Border(0, ui.Transparent).Background(ui.Transparent)
			if *filter != "" {
				clr := ui.ButtonBase(c.Key(keyPrefix+"FilterClear")).Label(clearLabel).Tooltip(clearLabel).
					Size(18, 18).Center().Radius(4).Shrink(0)
				if clr.Hovered() {
					clr.Background(colors.Hover)
				}
				clr.Children(func() {
					ui.Icon(c, design.Icon("x")).FontSize(11).TextColor(colors.TextFaint)
				}).OnClick(func() { *filter = "" })
			}
		})
		hiddenIcon, hiddenTip := "eye", locales.FileHideDotfiles()
		if *hideHidden {
			hiddenIcon, hiddenTip = "eye-off", locales.FileShowDotfiles()
		}
		hBtn := ui.ButtonBase(c.Key(keyPrefix+"ToggleHidden")).Size(22, 22).Center().Radius(4).
			Label(hiddenLabel).Tooltip(hiddenTip).Shrink(0).Disabled(disabled)
		if hBtn.Hovered() {
			hBtn.Background(colors.Hover)
		}
		if *hideHidden {
			hBtn.Background(t.Accent.Alpha(0.14))
		}
		hBtn.Children(func() {
			ui.Icon(c, design.Icon(hiddenIcon)).FontSize(12).TextColor(t.TextMuted)
		}).OnClick(func() { *hideHidden = !*hideHidden })

		sortBtn := ui.ButtonBase(c.Key(keyPrefix+"SortBtn")).Size(22, 22).Center().Radius(4).
			Label(sortLabel).Tooltip(sortLabel).Shrink(0).Disabled(disabled)
		if sortBtn.Hovered() {
			sortBtn.Background(colors.Hover)
		}
		if *sortBy != fileSortName || *sortDesc {
			sortBtn.Background(t.Accent.Alpha(0.14))
		}
		sortBtn.Children(func() {
			ui.Icon(c, design.Icon("arrow-up-down")).FontSize(12).TextColor(t.TextMuted)
		}).PopoverMenuButton(func(m *ui.Menu) {
			nameLabel := locales.FileSortName()
			sizeLabel := locales.FileSortSize()
			timeLabel := locales.FileSortTime()
			if *sortBy == fileSortName {
				nameLabel = "✓ " + nameLabel
			}
			if *sortBy == fileSortSize {
				sizeLabel = "✓ " + sizeLabel
			}
			if *sortBy == fileSortTime {
				timeLabel = "✓ " + timeLabel
			}
			if m.Item(nameLabel).Chosen() {
				*sortBy = fileSortName
			}
			if m.Item(sizeLabel).Chosen() {
				*sortBy = fileSortSize
			}
			if m.Item(timeLabel).Chosen() {
				*sortBy = fileSortTime
			}
			m.Separator()
			descLabel := locales.FileSortDescending()
			if *sortDesc {
				descLabel = "✓ " + descLabel
			}
			if m.Item(descLabel).Chosen() {
				*sortDesc = !*sortDesc
			}
		}, menuButtonOptions(colors))
	})
}

func (a *app) localColumn(c *ui.Context, colors design.Colors, t *tab) {
	theme := c.Theme()
	col := ui.Column(c.Key("localPane")).Grow(1).MinWidth(0).MinHeight(0)
	if item, ok := ui.Drop[paneDrag](col); ok {
		a.acceptPaneDrop(t, true, a.localDir, item)
	}
	if over, ok := ui.DragOver[paneDrag](col); ok && !over.local {
		col.Background(theme.Accent.Alpha(0.08))
	}
	col.Children(func() {
		ui.Row(c).Height(32).Shrink(0).Padding(4, 6, 0, 14).Gap(2).AlignItems(ui.Center).Children(func() {
			ui.Text(c, locales.ViewLocal()).FontSize(design.TextSmall).TextColor(theme.TextMuted).Grow(1).MinWidth(0).SingleLine()
			fileToolIconButton(c.Key("localNewFile"), colors, "file-plus", locales.LocalNewFile(), a.openLocalNewFile).
				Disabled(a.localDir == "")
			fileToolIconButton(c.Key("localNewFolder"), colors, "folder-plus", locales.LocalNewFolder(), a.openLocalNewFolder).
				Disabled(a.localDir == "")
			parent := filepath.Dir(a.localDir)
			fileToolIconButton(c, colors, "arrow-up", locales.LocalParentDirectory(), func() { a.openLocal(parent) }).
				Disabled(a.localDir == "" || parent == a.localDir)
			fileToolIconButton(c, colors, "refresh-cw", locales.LocalRefresh(), func() { a.openLocal(a.localDir) }).
				Disabled(a.localDir == "")
		})
		ui.Row(c).Height(26).Shrink(0).Padding(0, 6, 2, 14).Gap(2).AlignItems(ui.Center).Children(func() {
			if a.editingLocalPath {
				c.Key("localPathInput")
				in := ui.TextInput(c, &a.localPathDraft).Height(22).Grow(1).MinWidth(0).
					FontSize(design.TextSmall).AutoFocus()
				in.OnShortcut(0, ui.KeyEscape, func() { a.editingLocalPath = false })
				if in.Submitted() {
					target := strings.TrimSpace(a.localPathDraft)
					a.editingLocalPath = false
					if target != "" {
						a.openLocal(target)
					}
				}
				return
			}
			title := drawerPath(filepath.ToSlash(a.localDir))
			if a.localDir == "" || a.localDir == "." {
				title = locales.ThisMachine()
			}
			b := ui.ButtonBase(c.Key("editLocalPath")).Label(locales.LocalEditPath()).Tooltip(a.localDir).
				Height(22).Grow(1).MinWidth(0).Justify(ui.Start).AlignItems(ui.Center).Radius(4).
				Disabled(a.localDir == "")
			if b.Hovered() {
				b.Background(colors.Hover)
			}
			b.Children(func() {
				ui.Text(c.Key("localTitle"), title).FontSize(design.TextSmall).FontWeight(600).
					Grow(1).MinWidth(0).SingleLine().TruncateStart().Ellipsis("…")
			}).OnClick(func() {
				a.editingLocalPath = true
				a.localPathDraft = a.localDir
			})
			quickDirs := a.localQuickDirs(t)
			quickBtn := ui.ButtonBase(c.Key("localBookmarks")).Size(22, 22).Center().Radius(4).
				Label(locales.LocalQuickDirs()).Tooltip(locales.LocalQuickDirsTooltip()).Shrink(0).Disabled(len(quickDirs) == 0)
			if quickBtn.Hovered() {
				quickBtn.Background(colors.Hover)
			}
			quickBtn.Children(func() {
				ui.Icon(c, design.Icon("bookmark")).FontSize(13).TextColor(theme.TextMuted)
			}).PopoverMenuButton(func(m *ui.Menu) {
				for _, q := range quickDirs {
					dir := q.path
					if m.Item(q.label + "  " + drawerPath(filepath.ToSlash(dir))).Chosen() {
						a.openLocal(dir)
					}
				}
			}, menuButtonOptions(colors))
		})
		fileFilterSubbar(c, colors, "local", &a.localFilter, "筛选本地文件", "清除本地筛选", "本地隐藏文件", "本地排序方式",
			&a.localSortBy, &a.localSortDesc, &a.localHideHidden, a.localDir == "")
		if a.localErr != "" {
			ui.Text(c, a.localErr).TextColor(colors.Danger).FontSize(design.TextSmall).Padding(8, 14).Selectable()
		}
		entries := filterLocalEntries(a.localEntries, a.localFilter, a.localSortBy, a.localSortDesc, a.localHideHidden)
		ui.List(c, nil, len(entries), func(i int) {
			a.localFileRow(c, colors, t, entries[i])
		}).Grow(1).MinHeight(0).Padding(6, 6, 8, 6).Children(func() {
			if len(entries) == 0 && a.localErr == "" && a.localGen > 0 {
				msg := locales.LocalEmptyDirectory()
				if len(a.localEntries) > 0 {
					msg = locales.FileNoMatches()
				}
				ui.Text(c, msg).TextColor(theme.TextMuted).Padding(8, 8)
			}
		})
	})
}

func (a *app) localFileRow(c *ui.Context, colors design.Colors, t *tab, entry localEntry) {
	theme := c.Theme()
	canUpload := t != nil && !t.local && t.files != nil && t.dir != ""
	menu := func(m *ui.Menu) {
		if entry.IsDir {
			if m.Item(locales.ViewOpen()).Chosen() {
				a.openLocal(entry.Path)
			}
			if m.Item(locales.ViewUploadFolder()).Disabled(!canUpload).Chosen() {
				a.planFolderUpload(t, entry.Path, t.dir)
			}
		} else {
			if m.Item(locales.Edit()).Chosen() {
				a.openLocalEditor(entry.Path, entry.Name)
			}
			if m.Item(locales.ViewUploadFile()).Disabled(!canUpload).Chosen() {
				a.uploadTo(t, entry.Path, path.Join(t.dir, entry.Name))
			}
		}
		m.Separator()
		if m.Item(locales.Rename()).Chosen() {
			a.openLocalRenameFile(entry.Path, entry.Name)
		}
		if m.Item(locales.ViewCopyPath()).Chosen() {
			c.WriteClipboard(entry.Path)
			a.toast(ui.Toast{Title: locales.ViewPathCopied()})
		}
		if m.Item(locales.Delete()).Danger().Chosen() {
			a.askDeleteLocalFile(entry.Path, entry.Name, entry.IsDir)
		}
	}
	line := ui.Row(c.Key("localLine:"+entry.Path)).AlignItems(ui.Center).Radius(design.RadiusRow-2).Padding(0, 2, 0, 0)
	hovered := line.Hovered()
	if hovered {
		line.Background(colors.Hover)
	}
	line.Children(func() {
		// 标签带上“本地”，和远端同名文件同时出现时，点击和读屏不会撞在一起。
		row := ui.ButtonBase(c.Key("local:"+entry.Path)).Label("本地 "+entry.Name).Height(28).Grow(1).MinWidth(0).
			Padding(0, 6, 0, 8).Gap(6).Justify(ui.Start).AlignItems(ui.Center).Radius(design.RadiusRow-2).PopoverMenu(menu, menuOptions(colors))
		row.Drag(paneDrag{local: true, path: entry.Path, name: entry.Name, dir: entry.IsDir})
		if entry.IsDir {
			if item, ok := ui.Drop[paneDrag](row); ok {
				a.acceptPaneDrop(t, true, entry.Path, item)
			}
			row.OnClick(func() { a.openLocal(entry.Path) })
		} else if row.DoubleClicked() {
			a.openLocalEditor(entry.Path, entry.Name)
		}
		row.Children(func() {
			icon := "file"
			color := colors.TextFaint
			if entry.IsDir {
				icon, color = "folder", theme.Accent
			}
			ui.Icon(c, design.Icon(icon)).FontSize(14).TextColor(color).Shrink(0)
			ui.Text(c, entry.Name).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
			if !entry.IsDir {
				ui.Text(c, formatSize(entry.Size)).FontSize(design.TextSmall - 1).TextColor(colors.TextFaint).FontFeatures("tnum").Shrink(0)
			}
			if a.effectiveFilesWidth() >= 480 && !entry.ModTime.IsZero() {
				ui.Text(c, formatShortTime(entry.ModTime)).FontSize(design.TextSmall - 1).TextColor(colors.TextFaint).FontFeatures("tnum").Shrink(0)
			}
		})
		if hovered {
			more := ui.ButtonBase(c.Key("localFileMore:"+entry.Path)).Label("更多本地操作 "+entry.Name).
				Size(22, 22).Center().Radius(4).Shrink(0)
			if more.Hovered() {
				more.Background(colors.Hover)
			}
			more.Children(func() {
				ui.Icon(c, design.Icon("ellipsis")).FontSize(13).TextColor(theme.TextMuted)
			}).PopoverMenuButton(menu, menuButtonOptions(colors))
		}
	})
}

func (a *app) effectiveFilesWidth() float32 {
	if a.filesWidth <= 0 {
		return 560
	}
	return a.filesWidth
}

func (a *app) remoteColumn(c *ui.Context, colors design.Colors, t *tab) {
	theme := c.Theme()
	curHost, saved := a.hostByID(t.host.ID)
	var bookmarks []string
	if saved {
		bookmarks = curHost.Bookmarks
	} else {
		bookmarks = t.host.Bookmarks
	}
	col := ui.Column(c.Key("remotePane")).Grow(1).MinWidth(0).MinHeight(0)
	if item, ok := ui.Drop[paneDrag](col); ok {
		a.acceptPaneDrop(t, false, t.dir, item)
	}
	if over, ok := ui.DragOver[paneDrag](col); ok && over.local {
		col.Background(theme.Accent.Alpha(0.08))
	}
	col.Children(func() {
		ui.Row(c).Height(32).Shrink(0).Padding(4, 6, 0, 14).Gap(2).AlignItems(ui.Center).Children(func() {
			ui.Text(c, locales.ViewRemote()).FontSize(design.TextSmall).TextColor(theme.TextMuted).Grow(1).MinWidth(0).SingleLine()
			fileToolIconButton(c, colors, "plus", locales.ViewUploadFile(), func() { a.startUpload(t) }).Disabled(t.dir == "" || t.local)
			fileToolIconButton(c, colors, "folder-up", locales.ViewUploadFolder(), func() { a.startUploadFolder(t) }).Disabled(t.dir == "" || t.local)
			fileToolIconButton(c.Key("remoteNewFile"), colors, "file-plus", locales.NewFile(), func() { a.openNewFile(t) }).Disabled(t.dir == "" || t.local)
			fileToolIconButton(c, colors, "folder-plus", locales.NewFolder(), func() { a.openNewFolder(t) }).Disabled(t.dir == "" || t.local)
			fileToolIconButton(c, colors, "arrow-up", locales.ViewParentDirectory(), func() { a.openDir(t, path.Dir(t.dir)) }).Disabled(t.dir == "" || t.dir == "/")
			fileToolIconButton(c, colors, "refresh-cw", locales.ViewRefresh(), func() { a.openDir(t, t.dir) }).Disabled(t.dir == "")
		})
		ui.Row(c).Height(26).Shrink(0).Padding(0, 6, 2, 14).Gap(2).AlignItems(ui.Center).Children(func() {
			if a.editingRemotePath && !t.local && t.files != nil {
				c.Key("remotePathInput")
				in := ui.TextInput(c, &a.remotePathDraft).Height(22).Grow(1).MinWidth(0).
					FontSize(design.TextSmall).AutoFocus()
				in.OnShortcut(0, ui.KeyEscape, func() { a.editingRemotePath = false })
				if in.Submitted() {
					target := strings.TrimSpace(a.remotePathDraft)
					a.editingRemotePath = false
					if target != "" {
						a.openDir(t, target)
					}
				}
				return
			}
			title := drawerPath(t.dir)
			if t.dir == "" {
				title = locales.ViewFiles()
			}
			b := ui.ButtonBase(c.Key("editRemotePath")).Label(locales.RemoteEditPath()).Tooltip(t.dir).
				Height(22).Grow(1).MinWidth(0).Justify(ui.Start).AlignItems(ui.Center).Radius(4).
				Disabled(t.dir == "" || t.local || t.files == nil)
			if b.Hovered() {
				b.Background(colors.Hover)
			}
			b.Children(func() {
				ui.Text(c.Key("filesTitle"), title).FontSize(design.TextSmall).FontWeight(600).
					Grow(1).MinWidth(0).SingleLine().TruncateStart().Ellipsis("…")
			}).OnClick(func() {
				a.editingRemotePath = true
				a.remotePathDraft = t.dir
			})
			bmBtn := ui.ButtonBase(c.Key("remoteBookmarks")).Size(22, 22).Center().Radius(4).
				Label("远程目录书签").Tooltip("远程目录书签").Shrink(0).Disabled(t.dir == "" || t.local || !saved)
			if bmBtn.Hovered() {
				bmBtn.Background(colors.Hover)
			}
			if saved && t.dir != "" && slices.Contains(bookmarks, path.Clean(t.dir)) {
				bmBtn.Background(theme.Accent.Alpha(0.14))
			}
			bmBtn.Children(func() {
				ui.Icon(c, design.Icon("bookmark")).FontSize(13).TextColor(theme.TextMuted)
			}).PopoverMenuButton(func(m *ui.Menu) {
				curDir := path.Clean(t.dir)
				toggleLabel := "收藏当前目录"
				if slices.Contains(bookmarks, curDir) {
					toggleLabel = "取消收藏当前目录"
				}
				if m.Item(toggleLabel).Disabled(t.dir == "").Chosen() {
					a.toggleRemoteBookmark(t)
				}
				if len(bookmarks) > 0 {
					m.Separator()
					for _, bm := range bookmarks {
						dir := bm
						if m.Item(dir).Chosen() {
							a.openDir(t, dir)
						}
					}
					m.Separator()
					m.Submenu("删除书签", func(sub *ui.Menu) {
						for _, bm := range bookmarks {
							dir := bm
							if sub.Item(dir).Chosen() {
								a.removeRemoteBookmark(t, dir)
							}
						}
					})
				}
			}, menuButtonOptions(colors))

			pinIcon, pinLabel := "pin", locales.RemotePinDir()
			if t.filesPinned {
				pinIcon, pinLabel = "pin-off", locales.RemoteUnpinDir()
			}
			pinBtn := ui.ButtonBase(c.Key("remotePin")).Size(22, 22).Center().Radius(4).
				Label(pinLabel).Tooltip(pinLabel).Shrink(0).Disabled(t.local || t.files == nil)
			if pinBtn.Hovered() {
				pinBtn.Background(colors.Hover)
			}
			if t.filesPinned {
				pinBtn.Background(theme.Accent.Alpha(0.14))
			}
			pinBtn.Children(func() {
				ui.Icon(c, design.Icon(pinIcon)).FontSize(13).TextColor(theme.TextMuted)
			}).OnClick(func() {
				t.filesPinned = !t.filesPinned
				if !t.filesPinned {
					a.followCwd(t)
				}
			})

			fileToolIconButton(c.Key("remoteCdTerm"), colors, "terminal", locales.RemoteCdTerminal(), func() {
				a.cdTerminalToRemoteDir(t)
			}).Size(22, 22).Disabled(t.local || t.dir == "" || t.focus == nil || t.focus.ended)
		})
		fileFilterSubbar(c, colors, "remote", &t.filesFilter, "筛选远程文件", "清除远程筛选", "远程隐藏文件", "远程排序方式",
			&t.filesSortBy, &t.filesSortDesc, &t.filesHideHidden, t.local || t.dir == "")
		if t.filesError != "" {
			ui.Text(c, t.filesError).TextColor(colors.Danger).FontSize(design.TextSmall).Padding(8, 14).Selectable()
		}
		if t.filesBusy {
			ui.Text(c, locales.ViewOpeningFiles()).TextColor(theme.TextMuted).Padding(10, 14)
		}
		if t.local {
			ui.Text(c, locales.ViewLocalFilesHint()).
				TextColor(theme.TextMuted).FontSize(design.TextSmall).Padding(10, 14)
		}
		entries := filterRemoteEntries(t.entries, t.filesFilter, t.filesSortBy, t.filesSortDesc, t.filesHideHidden)
		ui.List(c, nil, len(entries), func(i int) {
			a.fileRow(c, colors, t, entries[i])
		}).Grow(1).MinHeight(0).Padding(6, 6, 8, 6).Children(func() {
			if len(entries) == 0 && t.files != nil && t.filesError == "" && !t.local {
				msg := locales.ViewEmptyDirectory()
				if len(t.entries) > 0 {
					msg = locales.FileNoMatches()
				}
				ui.Text(c, msg).TextColor(theme.TextMuted).Padding(8, 8)
			}
		})
	})
}
