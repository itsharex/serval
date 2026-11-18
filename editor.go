package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"serval/core/sshconn"
	"serval/design"
	"serval/locales"

	"github.com/egoist/mygo/ui"
)

// maxEditorBytes 是内置编辑器愿意读进内存的上限。
// 文本框要留着全文才能编辑，再大的文件就请用户下载后用别的程序打开。
const maxEditorBytes = 1 << 20

var errLocalFileChanged = errors.New("本地文件已变化")

// editorState 是正在编辑的远程或本地文本文件。字段只在主线程读写。
type editorState struct {
	open, fresh bool
	loading     bool
	local       bool
	tab         *tab
	path, name  string
	buf         *ui.TextBuffer
	// size 和 modUnix 是打开时（或上次成功保存时）文件的大小和修改时间，精确到秒。
	size    int64
	modUnix int64
	// version 是上次和磁盘/远端一致时的文本版本。和 buf.Version() 不同就表示还没保存。
	version uint64
	dirty   bool
	busy    bool
	err     string

	askDiscard   bool
	askOverwrite bool
	// closeAfterDiscard 是“放弃修改”确认后要一并关闭的标签页。
	// 从标签页触发确认时记录；取消或从编辑器自己的关闭按钮触发时保持为空。
	closeAfterDiscard *tab
}

func (a *app) openEditor(t *tab, remotePath, name string) {
	if t == nil || t.local || t.files == nil || a.editor.loading {
		return
	}
	if a.editor.open && a.editor.dirty {
		a.toast(ui.Toast{Title: "请先处理正在编辑的 " + a.editor.name})
		return
	}
	if err := validateFileName(name); err != nil {
		a.toast(ui.Toast{Title: "不能编辑这个文件", Description: err.Error(), Type: "error"})
		return
	}
	a.editor.loading = true
	files := t.files
	go func() {
		data, mod, err := files.ReadLimited(context.Background(), remotePath, maxEditorBytes)
		a.post(func() {
			a.editor.loading = false
			if !slices.Contains(a.tabs, t) {
				return
			}
			if err != nil {
				a.toast(ui.Toast{Title: "不能编辑 " + name, Description: editorReason(err), Type: "error"})
				return
			}
			buf := ui.NewTextBuffer(string(data))
			a.editor = editorState{
				open: true, fresh: true, tab: t, path: remotePath, name: name,
				buf: buf, size: int64(len(data)), modUnix: mod.Unix(), version: buf.Version(),
			}
		})
	}()
}

// openLocalEditor 在后台检查并读取本地文本文件，打开内置编辑器。
func (a *app) openLocalEditor(localPath, name string) {
	if localPath == "" || a.editor.loading {
		return
	}
	if a.editor.open && a.editor.dirty {
		a.toast(ui.Toast{Title: "请先处理正在编辑的 " + a.editor.name})
		return
	}
	if err := validateFileName(name); err != nil {
		a.toast(ui.Toast{Title: "不能编辑这个文件", Description: err.Error(), Type: "error"})
		return
	}
	a.editor.loading = true
	go func() {
		data, mod, err := readLocalLimited(localPath, maxEditorBytes)
		a.post(func() {
			a.editor.loading = false
			if err != nil {
				a.toast(ui.Toast{Title: "不能编辑 " + name, Description: editorReason(err), Type: "error"})
				return
			}
			buf := ui.NewTextBuffer(string(data))
			a.editor = editorState{
				open: true, fresh: true, local: true, path: localPath, name: name,
				buf: buf, size: int64(len(data)), modUnix: mod.Unix(), version: buf.Version(),
			}
		})
	}()
}

func readLocalLimited(localPath string, limit int64) ([]byte, editorMod, error) {
	info, err := os.Lstat(localPath)
	if err != nil {
		return nil, editorMod{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, editorMod{}, errors.New("只能编辑普通文件")
	}
	if info.Size() > limit {
		return nil, editorMod{}, sshconn.ErrFileTooLarge
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return nil, editorMod{}, err
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return nil, editorMod{}, sshconn.ErrNotText
	}
	return data, editorMod{size: int64(len(data)), unix: info.ModTime().Unix()}, nil
}

func writeLocalFile(localPath string, data []byte, expectSize, expectUnix int64, force bool) (editorMod, error) {
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return editorMod{}, sshconn.ErrNotText
	}
	info, err := os.Lstat(localPath)
	if err != nil {
		return editorMod{}, err
	}
	if !info.Mode().IsRegular() {
		return editorMod{}, errors.New("本地路径已经不是普通文件，没有保存")
	}
	if !force && (info.Size() != expectSize || info.ModTime().Unix() != expectUnix) {
		return editorMod{}, errLocalFileChanged
	}
	dir := filepath.Dir(localPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(localPath)+".*.tmp")
	if err != nil {
		return editorMod{}, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return editorMod{}, fmt.Errorf("写入临时文件失败: %w", err)
	}
	_ = tmp.Chmod(info.Mode().Perm())
	if err := tmp.Close(); err != nil {
		return editorMod{}, fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, localPath); err != nil {
		return editorMod{}, fmt.Errorf("保存本地文件失败: %w", err)
	}
	cleanup = false
	after, err := os.Lstat(localPath)
	if err != nil {
		return editorMod{}, err
	}
	return editorMod{size: after.Size(), unix: after.ModTime().Unix()}, nil
}

func editorReason(err error) string {
	switch {
	case errors.Is(err, sshconn.ErrFileTooLarge):
		return fmt.Sprintf("文件超过 %d MB，编辑器打不开。请下载后再用别的程序编辑。", maxEditorBytes>>20)
	case errors.Is(err, sshconn.ErrNotText):
		return "这不是 UTF-8 文本（或含有二进制数据），编辑器打不开。"
	default:
		return transferReason(err)
	}
}

func (a *app) requestCloseEditor() {
	if a.editor.dirty {
		a.editor.askDiscard = true
		a.editor.closeAfterDiscard = nil
		return
	}
	a.editor = editorState{}
}

func (a *app) saveEditor(force bool) {
	ed := &a.editor
	if !ed.open || ed.busy || ed.buf == nil {
		return
	}
	if !ed.local {
		if ed.tab == nil || ed.tab.files == nil {
			return
		}
		if !slices.Contains(a.tabs, ed.tab) {
			a.editor = editorState{}
			return
		}
	}
	text := ed.buf.String()
	if strings.ContainsRune(text, 0) || !utf8.ValidString(text) {
		ed.err = "内容不是文本，没有保存。"
		return
	}
	data := []byte(text)
	savedVersion := ed.buf.Version()
	ed.busy = true
	ed.err = ""
	isLocal, path, name := ed.local, ed.path, ed.name
	expectSize, expectUnix := ed.size, ed.modUnix
	t := ed.tab
	var files *sshconn.Files
	if !isLocal {
		files = t.files
	}
	go func() {
		var err error
		var stamp editorMod
		if isLocal {
			stamp, err = writeLocalFile(path, data, expectSize, expectUnix, force)
		} else {
			err = files.WriteFile(context.Background(), path, data, expectSize, expectUnix, force)
			if err == nil {
				stamp = edMod(files, path)
				err = stamp.err
			}
		}
		a.post(func() {
			if a.editor.path != path || a.editor.local != isLocal || a.editor.buf == nil {
				return
			}
			a.editor.busy = false
			if errors.Is(err, sshconn.ErrRemoteChanged) || errors.Is(err, errLocalFileChanged) {
				a.editor.askOverwrite = true
				return
			}
			if err != nil {
				a.editor.err = editorReason(err)
				a.toast(ui.Toast{Title: "没有保存 " + name, Description: a.editor.err, Type: "error"})
				return
			}
			a.editor.size = stamp.size
			a.editor.modUnix = stamp.unix
			if a.editor.buf.Version() == savedVersion {
				a.editor.version = savedVersion
				a.editor.dirty = false
			}
			a.toast(ui.Toast{Title: locales.EditorSavedNamed(name)})
			if isLocal {
				if a.localDir != "" {
					a.openLocal(a.localDir)
				}
				return
			}
			if slices.Contains(a.tabs, t) {
				a.openDir(t, t.dir)
			}
		})
	}()
}

type editorMod struct {
	size int64
	unix int64
	err  error
}

func (m editorMod) Unix() int64 { return m.unix }

func edMod(files *sshconn.Files, path string) editorMod {
	size, mod, _, err := files.Stat(path)
	if err != nil {
		return editorMod{err: err}
	}
	return editorMod{size: size, unix: mod.Unix()}
}

func editorStatusSummary(text string) string {
	lines := strings.Count(text, "\n") + 1
	eol := "LF"
	crlf := strings.Count(text, "\r\n")
	if crlf > 0 {
		if strings.Count(text, "\n") > crlf {
			eol = locales.EditorEolMixed()
		} else {
			eol = "CRLF"
		}
	}
	return locales.EditorStatusSummary(lines, formatSize(int64(len(text))), eol)
}

func (a *app) editorDialog(c *ui.Context, colors design.Colors) {
	ed := &a.editor
	if ed.open && !ed.local && ed.tab != nil && !slices.Contains(a.tabs, ed.tab) {
		a.editor = editorState{}
		return
	}
	if ed.askDiscard || ed.askOverwrite {
		a.editorConfirm(c)
		return
	}
	if !ed.open || a.setup.open || ed.buf == nil {
		return
	}
	if ed.buf.Version() != ed.version {
		ed.dirty = true
	}
	t := c.Theme()
	windowW, windowH := c.Size()
	panelW := float32(680)
	if windowW > 0 {
		panelW = min(max(windowW-120, 520), 840)
		if windowW < 560 {
			panelW = max(windowW-32, 320)
		}
	}
	editorH := float32(360)
	if windowH > 0 {
		editorH = min(max(windowH-240, 260), 620)
		if windowH < 460 {
			editorH = max(windowH-180, 160)
		}
	}
	title := "编辑 " + ed.name
	saveText := "保存"
	if ed.busy {
		saveText = "正在保存…"
	}
	panel := sheet(c, colors, &ed.open, &ed.fresh, panelW, 22, func() {
		ui.Column(c).Gap(10).OnShortcut(chromeShortcut(), ui.KeyS, func() {
			if !ed.busy {
				a.saveEditor(false)
			}
		}).Children(func() {
			ui.Column(c).Gap(3).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, title).FontSize(design.TextHeading).FontWeight(600).SingleLine().MinWidth(0).Grow(1)
					if ed.dirty {
						chip(c, "未保存", t.Accent, t.Accent.Alpha(0.14))
					}
				})
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, ed.path).FontSize(design.TextSmall).TextColor(t.TextMuted).
						SingleLine().TruncateStart().Ellipsis("…").MinWidth(0).Grow(1).Tooltip(ed.path)
					ui.Text(c, shortcutText("S")+" 保存").FontSize(design.TextSmall).TextColor(colors.TextFaint).NoWrap().Shrink(0)
				})
			})
			if ed.err != "" {
				ui.Text(c, ed.err).FontSize(design.TextSmall).TextColor(colors.Danger).Selectable()
			}
			c.Key("editorText")
			ui.TextAreaBuffer(c, ed.buf).Label("编辑内容").Height(editorH).
				Font(design.FontTerminal).FontSize(design.TextBody).
				ReadOnly(ed.busy).AutoFocus()
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c.Key("editorStatus"), editorStatusSummary(ed.buf.String())).
					FontSize(design.TextSmall).TextColor(colors.TextFaint).FontFeatures("tnum").
					SingleLine().MinWidth(0).Grow(1)
				ui.Button(c.Key("editorClose"), "关闭").Disabled(ed.busy).OnClick(func() { a.requestCloseEditor() })
				ui.PrimaryButton(c.Key("editorSave"), saveText).Disabled(ed.busy).OnClick(func() { a.saveEditor(false) })
			})
		})
	})
	if panel.Valid() && panel.Dismissed() {
		if ed.dirty {
			ed.open = true
			ed.askDiscard = true
		} else {
			a.editor = editorState{}
		}
	}
}

func (a *app) editorConfirm(c *ui.Context) {
	ed := &a.editor
	if ed.askDiscard {
		open := true
		choice := ui.AlertDialog(c, &open, "放弃修改？", ed.name+" 有未保存的修改。", "放弃修改", "取消")
		if choice == -1 && open {
			return
		}
		ed.askDiscard = false
		if choice == 0 {
			// 确认来自关标签时，放弃修改后把那个标签也关掉。
			t := ed.closeAfterDiscard
			a.editor = editorState{}
			if t != nil && slices.Contains(a.tabs, t) {
				a.closeTab(t)
			}
		} else {
			ed.closeAfterDiscard = nil
		}
		return
	}
	if !ed.askOverwrite {
		return
	}
	title := "服务器上的文件已变化"
	body := "保存会覆盖服务器上的新内容。"
	if ed.local {
		title = locales.EditorLocalChanged()
		body = locales.EditorLocalChangedBody()
	}
	open := true
	choice := ui.AlertDialog(c, &open, title, body, "仍然保存", locales.Cancel())
	if choice == -1 && open {
		return
	}
	ed.askOverwrite = false
	if choice == 0 && (ed.local || slices.Contains(a.tabs, ed.tab)) {
		a.saveEditor(true)
	}
}
