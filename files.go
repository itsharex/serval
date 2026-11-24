package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"serval/core/sshconn"
	"serval/locales"

	"github.com/egoist/mygo/ui"
)

type transferKind int

const (
	transferDownload transferKind = iota
	transferUpload
)

// fileTransfer 记录单次文件上传或下载的状态。
// 进度字段在传输的 goroutine 里写、在界面上读，全部用原子操作；其余字段只在主线程读写。
type fileTransfer struct {
	id          int
	kind        transferKind
	remotePath  string
	localPath   string
	fileName    string
	total       atomic.Int64
	transferred atomic.Int64
	speed       atomic.Int64 // bytes / sec
	done        atomic.Bool
	// folder 表示这一行是整个文件夹的上传或下载，filesDone/filesTotal 是文件数进度。
	folder     bool
	filesDone  atomic.Int64
	filesTotal atomic.Int64
	err        string
	guardLocal bool
	owner      *sshconn.FileOwner
	targetUser string
	cancel     context.CancelFunc
}

// pendingUpload 是远端已有同名文件、等用户确认替换的上传。
type pendingUpload struct {
	tab                             *tab
	localPath, remotePath, fileName string
	owner                           *sshconn.FileOwner
	targetUser                      string
}

// pendingDownload 是本地已有同名普通文件、拖放下载前等用户确认替换的下载。
type pendingDownload struct {
	tab                             *tab
	remotePath, localPath, fileName string
}

func (a *app) startDownload(t *tab, remotePath, fileName string) {
	if t.files == nil {
		return
	}
	localPath, err := a.chooseSavePath(fileName)
	if err != nil {
		a.toast(ui.Toast{Title: locales.FileOpenSaveDialogFailed(), Description: err.Error(), Type: "error"})
		return
	}
	if localPath == "" {
		return // 用户取消
	}
	a.runTransfer(t, transferDownload, remotePath, localPath, fileName, false)
}

// startUpload 选一个本地文件上传到当前目录。远端已有同名文件时先请用户确认替换。
// 检查是否存在要走网络，放在后台。
func (a *app) startUpload(t *tab) {
	if t.files == nil || t.dir == "" {
		return
	}
	localPath, err := a.chooseUploadFile()
	if err != nil {
		a.toast(ui.Toast{Title: locales.FileOpenPickerFailed(), Description: err.Error(), Type: "error"})
		return
	}
	if localPath == "" {
		return
	}
	a.uploadTo(t, localPath, path.Join(t.dir, filepath.Base(localPath)))
}

// startUploadFolder 选一个本地文件夹，上传到抽屉当前目录。
// 远端已有同名文件夹时的合并和确认，与拖到终端上的文件夹相同。
func (a *app) startUploadFolder(t *tab) {
	if t.files == nil || t.dir == "" || t.local {
		return
	}
	if a.chooseUploadFolder == nil {
		a.toast(ui.Toast{Title: "无法打开文件夹选择框", Description: "这个版本没有接上文件夹选择。", Type: "error"})
		return
	}
	local, err := a.chooseUploadFolder()
	if err != nil {
		a.toast(ui.Toast{Title: "无法打开文件夹选择框", Description: err.Error(), Type: "error"})
		return
	}
	if local == "" {
		return
	}
	info, err := os.Stat(local)
	if err != nil || !info.IsDir() {
		a.toast(ui.Toast{Title: "请选择一个文件夹", Type: "error"})
		return
	}
	a.planFolderUpload(t, local, t.dir)
}

// startDownloadFolder 把远程文件夹下载到用户选的目录里，保存为同名文件夹。
func (a *app) startDownloadFolder(t *tab, remotePath, name string) {
	if t.files == nil || t.local {
		return
	}
	if err := validateFileName(name); err != nil {
		a.toast(ui.Toast{Title: "无法下载这个文件夹", Description: err.Error(), Type: "error"})
		return
	}
	if a.chooseDownloadFolder == nil {
		a.toast(ui.Toast{Title: "无法打开文件夹选择框", Description: "这个版本没有接上文件夹选择。", Type: "error"})
		return
	}
	parent, err := a.chooseDownloadFolder()
	if err != nil {
		a.toast(ui.Toast{Title: "无法打开文件夹选择框", Description: err.Error(), Type: "error"})
		return
	}
	if parent == "" {
		return
	}
	a.runFolderDownload(t, remotePath, filepath.Join(parent, name), name)
}

// runFolderDownload 在后台递归下载文件夹。取消时保留已经下完的文件。
func (a *app) runFolderDownload(t *tab, remoteRoot, localRoot, name string) {
	if t.files == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.nextID++
	tr := &fileTransfer{id: a.nextID, kind: transferDownload, remotePath: remoteRoot, localPath: localRoot,
		fileName: name, folder: true, cancel: cancel}
	t.transfers = append(t.transfers, tr)
	a.noteTransferStarted(t)
	files := t.files
	go func() {
		lastBytes, lastTime := int64(0), time.Now()
		var lastWake atomic.Int64
		skipped, err := files.DownloadTree(ctx, remoteRoot, localRoot, func(doneFiles, totalFiles, doneBytes, totalBytes int64) {
			tr.filesDone.Store(doneFiles)
			tr.filesTotal.Store(totalFiles)
			tr.transferred.Store(doneBytes)
			tr.total.Store(totalBytes)
			if dur := time.Since(lastTime); dur >= 500*time.Millisecond {
				tr.speed.Store((doneBytes - lastBytes) * int64(time.Second) / int64(dur))
				lastBytes, lastTime = doneBytes, time.Now()
			}
			a.wakeUI(&lastWake, 100*time.Millisecond)
		})
		a.post(func() {
			defer cancel()
			tr.done.Store(true)
			done := tr.filesDone.Load()
			switch {
			case ctx.Err() != nil:
				tr.err = "已取消"
				a.toast(ui.Toast{Title: "已取消下载文件夹 " + name, Description: fmt.Sprintf("已下完的 %d 个文件留在本地。", done)})
			case errors.Is(err, sshconn.ErrLocalExists):
				tr.err = "本地已经有同名的文件或文件夹"
				a.toast(ui.Toast{Title: "没有下载文件夹 " + name, Description: "本地已经有同名的文件或文件夹，请换一个位置，或先把原来的移走。", Type: "error"})
			case err != nil:
				tr.err = transferReason(err)
				a.toast(ui.Toast{Title: "下载文件夹 " + name + " 失败", Description: tr.err, Type: "error"})
			default:
				desc := fmt.Sprintf("%d 个文件", done)
				if skipped > 0 {
					desc += fmt.Sprintf("，跳过了 %d 个符号链接或特殊文件", skipped)
				}
				a.toast(ui.Toast{Title: "已下载文件夹 " + name, Description: desc + "。"})
				if a.localDir != "" {
					a.openLocal(a.localDir)
				}
			}
			a.maybeScheduleHideFloatingQueue(t)
		})
	}()
}

// uploadTo 把本地文件上传到 remotePath。远端已有同名文件时先请用户确认替换；
// 检查是否存在要走网络，放在后台。
func (a *app) uploadTo(t *tab, localPath, remotePath string) {
	a.uploadToOwned(t, localPath, remotePath, nil, "")
}

func (a *app) uploadToOwned(t *tab, localPath, remotePath string, owner *sshconn.FileOwner, targetUser string) {
	fileName := filepath.Base(localPath)
	files := t.files
	loginUser := strings.TrimSpace(t.host.User)
	go func() {
		exists, err := files.Exists(remotePath)
		a.post(func() {
			if !slices.Contains(a.tabs, t) {
				return
			}
			if err != nil {
				reason := transferReason(err)
				if errors.Is(err, os.ErrPermission) && targetUser != "" && loginUser != "" && targetUser != loginUser {
					reason = fmt.Sprintf("没有权限（SFTP 以登录用户 %s 运行，无法写入 %s 的目录；可用 rz 上传或以 %s 登录）", loginUser, targetUser, targetUser)
				}
				a.toast(ui.Toast{Title: locales.FileUploadFailedNamed(fileName), Description: reason, Type: "error"})
				a.maybeScheduleHideFloatingQueue(t)
				return
			}
			if exists {
				a.pendingUploads = append(a.pendingUploads, &pendingUpload{
					tab: t, localPath: localPath, remotePath: remotePath, fileName: fileName,
					owner: owner, targetUser: targetUser,
				})
				return
			}
			a.runTransferOwned(t, transferUpload, remotePath, localPath, fileName, false, owner, targetUser)
		})
	}()
}

// uploadConfirmDialog 确认替换服务器上的同名文件。默认按钮是“取消”。
func (a *app) uploadConfirmDialog(c *ui.Context) {
	if len(a.pendingUploads) == 0 {
		return
	}
	u := a.pendingUploads[0]
	open := true
	body := locales.FileReplaceExists(path.Dir(u.remotePath))
	var choice int
	if len(a.pendingUploads) > 1 {
		body = fmt.Sprintf("%s\n\n还有 %d 个同名文件待确认。", body, len(a.pendingUploads)-1)
		choice = ui.AlertDialog(c, &open, locales.FileReplaceNamed(u.fileName),
			body, locales.FileReplace(), locales.FolderReplaceAll(), locales.FolderSkipAll(), locales.Cancel())
	} else {
		choice = ui.AlertDialog(c, &open, locales.FileReplaceNamed(u.fileName),
			body, locales.FileReplace(), locales.Cancel())
	}
	if choice == -1 && open {
		return
	}
	if len(a.pendingUploads) > 1 {
		switch choice {
		case 1:
			pending := a.pendingUploads
			a.pendingUploads = nil
			for _, item := range pending {
				if slices.Contains(a.tabs, item.tab) {
					a.runTransferOwned(item.tab, transferUpload, item.remotePath, item.localPath, item.fileName, false, item.owner, item.targetUser)
				}
			}
			return
		case 2:
			pending := a.pendingUploads
			a.pendingUploads = nil
			for _, item := range pending {
				a.maybeScheduleHideFloatingQueue(item.tab)
			}
			return
		}
	}
	a.pendingUploads = a.pendingUploads[1:]
	if choice == 0 && slices.Contains(a.tabs, u.tab) {
		a.runTransferOwned(u.tab, transferUpload, u.remotePath, u.localPath, u.fileName, false, u.owner, u.targetUser)
	} else {
		a.maybeScheduleHideFloatingQueue(u.tab)
	}
}

// downloadConfirmDialog 确认替换本机上的同名普通文件。默认按钮是“取消”。
func (a *app) downloadConfirmDialog(c *ui.Context) {
	if len(a.pendingDownloads) == 0 {
		return
	}
	d := a.pendingDownloads[0]
	open := true
	body := locales.FileReplaceLocalExists(filepath.Dir(d.localPath))
	var choice int
	if len(a.pendingDownloads) > 1 {
		body = fmt.Sprintf("%s\n\n还有 %d 个同名文件待确认。", body, len(a.pendingDownloads)-1)
		choice = ui.AlertDialog(c, &open, locales.FileReplaceNamed(d.fileName),
			body, locales.FileReplace(), locales.FolderReplaceAll(), locales.FolderSkipAll(), locales.Cancel())
	} else {
		choice = ui.AlertDialog(c, &open, locales.FileReplaceNamed(d.fileName),
			body, locales.FileReplace(), locales.Cancel())
	}
	if choice == -1 && open {
		return
	}
	if len(a.pendingDownloads) > 1 {
		switch choice {
		case 1:
			pending := a.pendingDownloads
			a.pendingDownloads = nil
			for _, item := range pending {
				if slices.Contains(a.tabs, item.tab) {
					a.runTransfer(item.tab, transferDownload, item.remotePath, item.localPath, item.fileName, false)
				}
			}
			return
		case 2:
			a.pendingDownloads = nil
			return
		}
	}
	a.pendingDownloads = a.pendingDownloads[1:]
	if choice == 0 && slices.Contains(a.tabs, d.tab) {
		a.runTransfer(d.tab, transferDownload, d.remotePath, d.localPath, d.fileName, false)
	}
}

// runTransfer 在后台执行一次传输，进度通过原子字段给界面读取，结束后回到主线程提示结果。
// guardLocal 为真时，下载落盘前再检查一次本地同名文件，存在就不覆盖（拖放下载承诺不覆盖）。
func (a *app) runTransfer(t *tab, kind transferKind, remotePath, localPath, fileName string, guardLocal bool) {
	a.runTransferOwned(t, kind, remotePath, localPath, fileName, guardLocal, nil, "")
}

func (a *app) runTransferOwned(t *tab, kind transferKind, remotePath, localPath, fileName string, guardLocal bool, owner *sshconn.FileOwner, targetUser string) {
	if t.files == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.nextID++
	tr := &fileTransfer{
		id: a.nextID, kind: kind, remotePath: remotePath, localPath: localPath, fileName: fileName,
		guardLocal: guardLocal, owner: owner, targetUser: targetUser, cancel: cancel,
	}
	t.transfers = append(t.transfers, tr)
	a.noteTransferStarted(t)
	files := t.files
	loginUser := strings.TrimSpace(t.host.User)
	go func() {
		lastBytes, lastTime := int64(0), time.Now()
		var lastWake atomic.Int64
		progress := func(transferred, total int64) {
			tr.total.Store(total)
			tr.transferred.Store(transferred)
			if dur := time.Since(lastTime); dur >= 500*time.Millisecond {
				tr.speed.Store((transferred - lastBytes) * int64(time.Second) / int64(dur))
				lastBytes, lastTime = transferred, time.Now()
			}
			a.wakeUI(&lastWake, 100*time.Millisecond)
		}
		var chownErr, err error
		switch {
		case kind == transferDownload && guardLocal:
			err = files.DownloadIfAbsent(ctx, remotePath, localPath, progress)
		case kind == transferDownload:
			err = files.Download(ctx, remotePath, localPath, progress)
		default:
			chownErr, err = files.UploadOwned(ctx, localPath, remotePath, owner, progress)
		}
		a.post(func() {
			defer cancel()
			tr.done.Store(true)
			upload := kind == transferUpload
			switch {
			case ctx.Err() != nil:
				tr.err = locales.FileCancelled()
				title := locales.FileDownloadCancelledNamed(fileName)
				if upload {
					title = locales.FileUploadCancelledNamed(fileName)
				}
				a.toast(ui.Toast{Title: title})
			case errors.Is(err, sshconn.ErrLocalExists):
				tr.err = "本地已经有同名的文件或文件夹"
				a.toast(ui.Toast{Title: "没有下载 " + fileName, Description: "下载期间本地出现了同名的文件或文件夹，没有覆盖它，请先把原来的移走。", Type: "error"})
			case err != nil:
				reason := transferReason(err)
				if upload && errors.Is(err, os.ErrPermission) && targetUser != "" && loginUser != "" && targetUser != loginUser {
					reason = fmt.Sprintf("没有权限（SFTP 以登录用户 %s 运行，无法写入 %s 的目录；可用 rz 上传或以 %s 登录）", loginUser, targetUser, targetUser)
				}
				tr.err = reason
				title := locales.FileDownloadFailedNamed(fileName)
				if upload {
					title = locales.FileUploadFailedNamed(fileName)
				}
				a.toast(ui.Toast{Title: title, Description: tr.err, Type: "error"})
			default:
				title := locales.FileDownloadedNamed(fileName)
				if upload {
					title = locales.FileUploadedNamed(fileName)
				}
				var desc string
				if upload && chownErr != nil && targetUser != "" && loginUser != "" && targetUser != loginUser {
					desc = fmt.Sprintf("已写入目标目录，但 SFTP 以登录用户 %s 运行，未能将属主改为 %s。", loginUser, targetUser)
				}
				a.toast(ui.Toast{Title: title, Description: desc})
				if upload && slices.Contains(a.tabs, t) {
					a.openDir(t, t.dir)
				}
				if kind == transferDownload && a.localDir != "" {
					a.openLocal(a.localDir)
				}
			}
			a.maybeScheduleHideFloatingQueue(t)
		})
	}()
}

// transferReason 把文件操作的错误说成用户能看懂的原因。
func transferReason(err error) string {
	switch {
	case errors.Is(err, os.ErrExist), errors.Is(err, sshconn.ErrRemoteExists):
		return locales.FileAlreadyExists()
	case errors.Is(err, os.ErrPermission):
		return locales.FileNoPermission()
	case errors.Is(err, os.ErrNotExist):
		return locales.FileNotExist()
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return locales.FileConnectionLost()
	}
	return err.Error()
}

func (a *app) cancelTransfer(tr *fileTransfer) {
	if tr.cancel != nil {
		tr.cancel()
	}
}

func (a *app) cancelAllTransfers(t *tab) {
	if t == nil {
		return
	}
	for _, tr := range t.transfers {
		if !tr.done.Load() && tr.cancel != nil {
			tr.cancel()
		}
	}
}

func (a *app) dismissTransfer(t *tab, id int) {
	if t == nil {
		return
	}
	t.transfers = slices.DeleteFunc(t.transfers, func(tr *fileTransfer) bool {
		return tr.id == id && tr.done.Load()
	})
	a.maybeScheduleHideFloatingQueue(t)
}

func (a *app) clearFinishedTransfers(t *tab) {
	if t == nil {
		return
	}
	t.transfers = slices.DeleteFunc(t.transfers, func(tr *fileTransfer) bool {
		return tr.done.Load()
	})
	a.maybeScheduleHideFloatingQueue(t)
}

func (a *app) retryTransfer(t *tab, item *fileTransfer) {
	if t == nil || item == nil || !item.done.Load() || item.err == "" || t.files == nil {
		return
	}
	a.dismissTransfer(t, item.id)
	if item.folder {
		if item.kind == transferDownload {
			a.runFolderDownload(t, item.remotePath, item.localPath, item.fileName)
		} else {
			a.planFolderUploadOwned(t, item.localPath, path.Dir(item.remotePath), item.owner, item.targetUser)
		}
		return
	}
	if item.kind == transferDownload {
		if item.guardLocal {
			if info, err := os.Lstat(item.localPath); err == nil {
				if info.IsDir() {
					a.toast(ui.Toast{Title: "没有下载 " + item.fileName, Description: "本地已经有同名的文件夹，请先把原来的移走。", Type: "error"})
					return
				}
				a.pendingDownloads = append(a.pendingDownloads, &pendingDownload{
					tab: t, remotePath: item.remotePath, localPath: item.localPath, fileName: item.fileName,
				})
				return
			}
		}
		a.runTransfer(t, transferDownload, item.remotePath, item.localPath, item.fileName, item.guardLocal)
		return
	}
	a.runTransferOwned(t, transferUpload, item.remotePath, item.localPath, item.fileName, false, item.owner, item.targetUser)
}

type fileOpKind int

const (
	fileOpNone fileOpKind = iota
	fileOpNewFolder
	fileOpNewFile
	fileOpRename
	fileOpPermissions
)

type fileOpState struct {
	open  bool
	fresh bool
	kind  fileOpKind
	local bool
	tab   *tab
	path  string
	name  string
	input string
	err   string
	// busy 表示操作正在后台执行，期间按钮禁用。
	busy bool
}

type fileDeleteState struct {
	local bool
	tab   *tab
	path  string
	name  string
	isDir bool
}

func (a *app) openNewFolder(t *tab) {
	if t.files == nil || t.dir == "" {
		return
	}
	a.fileOp = fileOpState{
		open:  true,
		fresh: true,
		kind:  fileOpNewFolder,
		tab:   t,
		path:  t.dir,
		input: locales.NewFolder(),
	}
}

func (a *app) openLocalNewFolder() {
	if a.localDir == "" {
		return
	}
	a.fileOp = fileOpState{
		open:  true,
		fresh: true,
		kind:  fileOpNewFolder,
		local: true,
		path:  a.localDir,
		input: locales.NewFolder(),
	}
}

func (a *app) openNewFile(t *tab) {
	if t.files == nil || t.dir == "" {
		return
	}
	a.fileOp = fileOpState{
		open:  true,
		fresh: true,
		kind:  fileOpNewFile,
		tab:   t,
		path:  t.dir,
		input: "untitled.txt",
	}
}

func (a *app) openLocalNewFile() {
	if a.localDir == "" {
		return
	}
	a.fileOp = fileOpState{
		open:  true,
		fresh: true,
		kind:  fileOpNewFile,
		local: true,
		path:  a.localDir,
		input: "untitled.txt",
	}
}

func (a *app) openRenameFile(t *tab, fullPath, name string) {
	if t.files == nil {
		return
	}
	a.fileOp = fileOpState{
		open:  true,
		fresh: true,
		kind:  fileOpRename,
		tab:   t,
		path:  fullPath,
		name:  name,
		input: name,
	}
}

func (a *app) openLocalRenameFile(fullPath, name string) {
	if a.localDir == "" || fullPath == "" {
		return
	}
	a.fileOp = fileOpState{
		open:  true,
		fresh: true,
		kind:  fileOpRename,
		local: true,
		path:  fullPath,
		name:  name,
		input: name,
	}
}

func (a *app) openPermissions(t *tab, fullPath, name string, mode fs.FileMode) {
	if t.files == nil {
		return
	}
	a.fileOp = fileOpState{
		open:  true,
		fresh: true,
		kind:  fileOpPermissions,
		tab:   t,
		path:  fullPath,
		name:  name,
		input: fmt.Sprintf("%04o", mode.Perm()),
	}
}

// validateFileName 检查新建或重命名用的名称。名称里带 / 或者是 ..，
// 会把文件建到或移到别的目录去，这不是用户在这里想做的事。
func validateFileName(name string) error {
	switch {
	case name == "":
		return errors.New(locales.FileNameEmpty())
	case name == "." || name == "..":
		return errors.New(locales.FileNameDot())
	case strings.ContainsAny(name, "/\x00"):
		return errors.New(locales.FileNameSlash())
	}
	return nil
}

// submitFileOp 执行新建文件夹、新建文件、重命名、修改权限。网络操作或本地磁盘操作放在后台，
// 期间面板显示处理中，结果回到主线程。
func (a *app) submitFileOp() {
	op := &a.fileOp
	if !op.open || op.busy {
		return
	}
	if !op.local && (op.tab == nil || op.tab.files == nil) {
		return
	}
	t := op.tab
	isLocal := op.local
	input := strings.TrimSpace(op.input)
	var run func() error
	var done string
	switch op.kind {
	case fileOpNewFolder, fileOpNewFile, fileOpRename:
		if err := validateFileName(input); err != nil {
			op.err = err.Error()
			return
		}
		if isLocal {
			switch op.kind {
			case fileOpNewFolder:
				dir := filepath.Join(op.path, input)
				run, done = func() error { return os.Mkdir(dir, 0o755) }, locales.FileFolderCreatedNamed(input)
			case fileOpNewFile:
				target := filepath.Join(op.path, input)
				run, done = func() error {
					f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
					if err != nil {
						return err
					}
					return f.Close()
				}, locales.FileCreatedNamed(input)
			default:
				from, to := op.path, filepath.Join(filepath.Dir(op.path), input)
				run, done = func() error {
					if from == to {
						return nil
					}
					if _, err := os.Lstat(to); err == nil {
						return errors.New("目标名称已存在")
					}
					return os.Rename(from, to)
				}, locales.FileRenamedNamed(input)
			}
		} else {
			files := t.files
			switch op.kind {
			case fileOpNewFolder:
				dir := path.Join(op.path, input)
				run, done = func() error { return files.Mkdir(dir) }, locales.FileFolderCreatedNamed(input)
			case fileOpNewFile:
				target := path.Join(op.path, input)
				run, done = func() error { return files.CreateEmpty(target) }, locales.FileCreatedNamed(input)
			default:
				from, to := op.path, path.Join(path.Dir(op.path), input)
				run, done = func() error { return files.Rename(from, to) }, locales.FileRenamedNamed(input)
			}
		}
	case fileOpPermissions:
		if isLocal || t == nil || t.files == nil {
			return
		}
		files := t.files
		target := op.path
		perm, err := strconv.ParseUint(input, 8, 32)
		if err != nil || perm > 0o7777 {
			op.err = locales.FileModeInvalid()
			return
		}
		run, done = func() error { return files.Chmod(target, fs.FileMode(perm)) }, locales.FileModeChanged(fmt.Sprintf("%04o", perm))
	default:
		return
	}
	op.busy, op.err = true, ""
	go func() {
		err := run()
		a.post(func() {
			op := &a.fileOp
			op.busy = false
			if err != nil {
				op.err = transferReason(err)
				return
			}
			a.fileOp = fileOpState{}
			a.toast(ui.Toast{Title: done})
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

func (a *app) askDeleteFile(t *tab, fullPath, name string, isDir bool) {
	a.fileDelete = &fileDeleteState{tab: t, path: fullPath, name: name, isDir: isDir}
}

func (a *app) askDeleteLocalFile(fullPath, name string, isDir bool) {
	a.fileDelete = &fileDeleteState{local: true, path: fullPath, name: name, isDir: isDir}
}

func removeLocalPath(target string, isDir bool) error {
	cleaned := filepath.Clean(target)
	base := filepath.Base(cleaned)
	if cleaned == "" || cleaned == "." || cleaned == ".." || base == "." || base == ".." || cleaned == filepath.Dir(cleaned) {
		return errors.New("无法删除根目录或相对保留路径")
	}
	if !isDir {
		return os.Remove(cleaned)
	}
	info, err := os.Lstat(cleaned)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return os.Remove(cleaned)
	}
	return os.RemoveAll(cleaned)
}

// confirmDeleteFile 在后台安全删除文件或递归删除目录（支持本地与远程，不跟随符号链接）。
func (a *app) confirmDeleteFile() {
	del := a.fileDelete
	a.fileDelete = nil
	if del == nil || (!del.local && (del.tab == nil || del.tab.files == nil)) {
		return
	}
	t := del.tab
	isLocal := del.local
	go func() {
		var err error
		if isLocal {
			err = removeLocalPath(del.path, del.isDir)
		} else {
			files := t.files
			if del.isDir {
				err = files.RemoveAll(context.Background(), del.path)
			} else {
				err = files.Remove(del.path)
			}
		}
		a.post(func() {
			if err != nil {
				a.toast(ui.Toast{Title: locales.FileNotDeletedNamed(del.name), Description: transferReason(err), Type: "error"})
				return
			}
			a.toast(ui.Toast{Title: locales.FileDeletedNamed(del.name)})
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

// classifyDrop 把拖进来的路径分成普通文件和文件夹。读不到的路径当场说明，不中断其余文件。
func (a *app) classifyDrop(paths []string) (files, folders []string) {
	for _, local := range paths {
		info, err := os.Stat(local)
		if err != nil {
			a.toast(ui.Toast{Title: locales.FileCannotUploadNamed(filepath.Base(local)), Description: err.Error(), Type: "error"})
			continue
		}
		if info.IsDir() {
			folders = append(folders, local)
			continue
		}
		files = append(files, local)
	}
	return files, folders
}

// uploadPaths 把已经分好类的本地路径上传到远程 dir。
func (a *app) uploadPaths(t *tab, dir string, files, folders []string) {
	a.uploadPathsOwned(t, dir, files, folders, nil, "")
}

func (a *app) uploadPathsOwned(t *tab, dir string, files, folders []string, owner *sshconn.FileOwner, targetUser string) {
	if !slices.Contains(a.tabs, t) || t.files == nil || dir == "" {
		return
	}
	for _, local := range files {
		a.uploadToOwned(t, local, path.Join(dir, filepath.Base(local)), owner, targetUser)
	}
	for _, folder := range folders {
		a.planFolderUploadOwned(t, folder, dir, owner, targetUser)
	}
}

// uploadInto 把拖进来的文件和文件夹上传到远程 dir（文件抽屉的当前目录）。
func (a *app) uploadInto(t *tab, dir string, paths []string) {
	files, folders := a.classifyDrop(paths)
	if len(files) == 0 && len(folders) == 0 {
		return
	}
	a.uploadPaths(t, dir, files, folders)
}

type remoteUserInfo struct {
	Home   string
	UID    int
	GID    int
	HasIDs bool
}

// parsePasswdEntry 从 /etc/passwd 单行或多行内容里解析指定用户的主目录与 UID/GID。
func parsePasswdEntry(data []byte, user string) (remoteUserInfo, bool) {
	if !isValidPromptUser(user) {
		return remoteUserInfo{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) >= 6 && fields[0] == user && strings.HasPrefix(fields[5], "/") {
			info := remoteUserInfo{Home: path.Clean(fields[5])}
			if uid, uErr := strconv.Atoi(fields[2]); uErr == nil && uid >= 0 {
				if gid, gErr := strconv.Atoi(fields[3]); gErr == nil && gid >= 0 {
					info.UID = uid
					info.GID = gid
					info.HasIDs = true
				}
			}
			return info, true
		}
	}
	return remoteUserInfo{}, false
}

// parsePasswdHome 从 /etc/passwd 内容里解析指定用户的主目录。
func parsePasswdHome(data []byte, user string) string {
	info, ok := parsePasswdEntry(data, user)
	if !ok {
		return ""
	}
	return info.Home
}

// resolveRemoteUserInfo 查询远程指定用户的主目录与 UID/GID（支持 su / sudo -i 切换用户后的 ~ 展开与属主对齐）。
func resolveRemoteUserInfo(ctx context.Context, client *sshconn.Client, files *sshconn.Files, user string) (remoteUserInfo, error) {
	if !isValidPromptUser(user) {
		return remoteUserInfo{}, fmt.Errorf("无效的远程用户名: %q", user)
	}
	if client != nil {
		// isValidPromptUser 仅允许 [a-zA-Z0-9._-] 且不以 - 开头，拼入 shell 命令是安全的。
		cmd := fmt.Sprintf("line=$(getent passwd %s 2>/dev/null | head -n 1); if [ -z \"$line\" ]; then line=$(awk -F: -v u=%s '$1==u{print; exit}' /etc/passwd 2>/dev/null); fi; if [ -n \"$line\" ]; then printf 'P:%%s\\n' \"$line\"; else eval \"h=~%s\"; u=$(id -u %s 2>/dev/null); g=$(id -g %s 2>/dev/null); printf 'I:%%s:%%s:%%s\\n' \"$u\" \"$g\" \"$h\"; fi", user, user, user, user, user)
		res, err := client.ExecContext(ctx, cmd, 4<<10)
		if err == nil && res.ExitCode == 0 {
			for _, rawLine := range strings.Split(res.Stdout, "\n") {
				line := strings.TrimSpace(rawLine)
				switch {
				case strings.HasPrefix(line, "P:"):
					if info, ok := parsePasswdEntry([]byte(strings.TrimPrefix(line, "P:")), user); ok {
						return info, nil
					}
				case strings.HasPrefix(line, "I:"):
					parts := strings.SplitN(strings.TrimPrefix(line, "I:"), ":", 3)
					if len(parts) == 3 && strings.HasPrefix(parts[2], "/") {
						info := remoteUserInfo{Home: path.Clean(parts[2])}
						if uid, uErr := strconv.Atoi(parts[0]); uErr == nil && uid >= 0 {
							if gid, gErr := strconv.Atoi(parts[1]); gErr == nil && gid >= 0 {
								info.UID = uid
								info.GID = gid
								info.HasIDs = true
							}
						}
						return info, nil
					}
				}
			}
		}
	}
	if files != nil {
		if data, _, err := files.ReadLimited(ctx, "/etc/passwd", 512<<10); err == nil {
			if info, ok := parsePasswdEntry(data, user); ok {
				return info, nil
			}
		}
		candidates := []string{"/home/" + user, "/Users/" + user}
		if user == "root" {
			candidates = []string{"/root", "/var/root"}
		}
		for _, cand := range candidates {
			if _, _, mode, err := files.Stat(cand); err == nil && mode.IsDir() {
				return remoteUserInfo{Home: cand}, nil
			}
		}
	}
	return remoteUserInfo{}, fmt.Errorf("无法确定用户 %s 的主目录", user)
}

func joinRemoteHome(home, sub string) string {
	home = path.Clean(home)
	if sub == "" {
		return home
	}
	return path.Join(home, sub)
}

// resolveUploadTarget 根据终端追踪到的当前用户与目录解析拖放上传的目标绝对路径及目标用户信息。
func resolveUploadTarget(
	ctx context.Context,
	client *sshconn.Client,
	remote *sshconn.Files,
	loginUser, liveUser, liveDir string,
	cachedInfo remoteUserInfo,
	lookupUser func(context.Context, *sshconn.Client, *sshconn.Files, string) (remoteUserInfo, error),
) (dir string, resolvedInfo remoteUserInfo, fallback bool, err error) {
	if lookupUser == nil {
		lookupUser = resolveRemoteUserInfo
	}
	switched := liveUser != "" && (loginUser == "" || liveUser != loginUser)
	ensureSwitchedInfo := func() (remoteUserInfo, error) {
		info := cachedInfo
		if strings.HasPrefix(info.Home, "/") && info.HasIDs {
			return info, nil
		}
		looked, lookErr := lookupUser(ctx, client, remote, liveUser)
		if lookErr != nil {
			if strings.HasPrefix(info.Home, "/") {
				return info, nil
			}
			return remoteUserInfo{}, lookErr
		}
		if info.Home == "" {
			info.Home = looked.Home
		}
		if !info.HasIDs && looked.HasIDs {
			info.UID, info.GID, info.HasIDs = looked.UID, looked.GID, true
		}
		return info, nil
	}

	if strings.HasPrefix(liveDir, "/") {
		if switched {
			info, _ := ensureSwitchedInfo()
			return path.Clean(liveDir), info, false, nil
		}
		return path.Clean(liveDir), remoteUserInfo{}, false, nil
	}
	if liveDir == "~" || strings.HasPrefix(liveDir, "~/") {
		sub := strings.TrimPrefix(strings.TrimPrefix(liveDir, "~"), "/")
		if !switched {
			home, homeErr := remote.Home()
			if homeErr != nil {
				return "", remoteUserInfo{}, false, homeErr
			}
			info := cachedInfo
			info.Home = path.Clean(home)
			return joinRemoteHome(home, sub), info, false, nil
		}
		info, infoErr := ensureSwitchedInfo()
		if infoErr != nil || !strings.HasPrefix(info.Home, "/") {
			return "", remoteUserInfo{}, false, fmt.Errorf("无法确定用户 %s 的主目录，请先切换到绝对路径后再上传", liveUser)
		}
		return joinRemoteHome(info.Home, sub), info, false, nil
	}
	if switched {
		if info, infoErr := ensureSwitchedInfo(); infoErr == nil && strings.HasPrefix(info.Home, "/") {
			return path.Clean(info.Home), info, true, nil
		}
	}
	home, homeErr := remote.Home()
	if homeErr != nil {
		return "", remoteUserInfo{}, false, homeErr
	}
	return path.Clean(home), remoteUserInfo{}, true, nil
}

// previewRemoteDir 在主线程上把终端提示符目录格式化为可预览的上传目标路径（不做网络 I/O）。
func (a *app) previewRemoteDir(t *tab, user, dir string) string {
	if strings.HasPrefix(dir, "/") {
		return path.Clean(dir)
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		sub := strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/")
		if user != "" && t.userInfos != nil && t.userInfos[user].Home != "" {
			return joinRemoteHome(t.userInfos[user].Home, sub)
		}
		loginUser := strings.TrimSpace(t.host.User)
		if (user == "" || user == loginUser) && t.remoteHome != "" {
			return joinRemoteHome(t.remoteHome, sub)
		}
		if user != "" && loginUser != "" && user != loginUser {
			if sub == "" {
				return "~" + user
			}
			return "~" + user + "/" + sub
		}
		return dir
	}
	return ""
}

func hasActiveTransfer(t *tab) bool {
	if t == nil {
		return false
	}
	for _, tr := range t.transfers {
		if !tr.done.Load() {
			return true
		}
	}
	return false
}

func (a *app) hasPendingForTab(t *tab) bool {
	if t == nil {
		return false
	}
	if t.scanningFolders > 0 {
		return true
	}
	for _, u := range a.pendingUploads {
		if u.tab == t {
			return true
		}
	}
	for _, d := range a.pendingDownloads {
		if d.tab == t {
			return true
		}
	}
	for _, f := range a.pendingFolders {
		if f.tab == t {
			return true
		}
	}
	return false
}

func (a *app) cancelFloatingQueueTimer(t *tab) {
	if t == nil {
		return
	}
	t.transferHideGen++
	if t.transferHideTimer != nil {
		t.transferHideTimer.Stop()
		t.transferHideTimer = nil
	}
}

func (a *app) noteTransferStarted(t *tab) {
	if t == nil {
		return
	}
	a.cancelFloatingQueueTimer(t)
	if !t.filesOpen {
		t.floatingQueue = true
	}
}

func (a *app) maybeScheduleHideFloatingQueue(t *tab) {
	if t == nil || !t.floatingQueue || t.filesOpen {
		return
	}
	if len(t.transfers) == 0 {
		t.floatingQueue = false
		a.cancelFloatingQueueTimer(t)
		return
	}
	if a.hasPendingForTab(t) {
		a.cancelFloatingQueueTimer(t)
		return
	}
	for _, tr := range t.transfers {
		if !tr.done.Load() || tr.err != "" {
			a.cancelFloatingQueueTimer(t)
			return
		}
	}
	a.cancelFloatingQueueTimer(t)
	gen := t.transferHideGen
	delay := a.floatingQueueHideDelay()
	t.transferHideTimer = time.AfterFunc(delay, func() {
		if a.post == nil {
			return
		}
		a.post(func() {
			if gen != t.transferHideGen || !slices.Contains(a.tabs, t) || !t.floatingQueue || t.filesOpen || a.hasPendingForTab(t) {
				return
			}
			for _, tr := range t.transfers {
				if !tr.done.Load() || tr.err != "" {
					return
				}
			}
			t.transferHideTimer = nil
			a.clearFinishedTransfers(t)
			t.floatingQueue = false
		})
	})
}

// uploadDropped 把拖到终端上的文件和文件夹用 SFTP 传到终端的当前目录（优先 OSC 7，其次 OSC 0 / 提示符追踪；
// 若通过 su 切换过用户，会按当前用户的主目录展开 ~ 并对齐文件属主）。传输期间不自动展开文件抽屉，只在终端右下角显示传输队列。
func (a *app) uploadDropped(t *tab, p *pane, paths []string) {
	if t.local || t.client == nil {
		return
	}
	files, folders := a.classifyDrop(paths)
	if len(files) == 0 && len(folders) == 0 {
		return
	}
	liveUser, liveDir := paneUserAndDir(t, p)
	loginUser := strings.TrimSpace(t.host.User)
	switched := liveUser != "" && (loginUser == "" || liveUser != loginUser)
	var cachedInfo remoteUserInfo
	if liveUser != "" && t.userInfos != nil {
		cachedInfo = t.userInfos[liveUser]
	}
	targetUser := ""
	if switched {
		targetUser = liveUser
	}
	client := t.client
	lookupUser := a.resolveUserInfo
	start := func() {
		if !slices.Contains(a.tabs, t) || t.files == nil {
			return
		}
		if strings.HasPrefix(liveDir, "/") && (!switched || cachedInfo.HasIDs) {
			var owner *sshconn.FileOwner
			if switched && cachedInfo.HasIDs {
				owner = &sshconn.FileOwner{UID: cachedInfo.UID, GID: cachedInfo.GID}
			}
			a.uploadPathsOwned(t, path.Clean(liveDir), files, folders, owner, targetUser)
			return
		}
		remote := t.files
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			dir, resolvedInfo, fallback, err := resolveUploadTarget(ctx, client, remote, loginUser, liveUser, liveDir, cachedInfo, lookupUser)
			a.post(func() {
				if !slices.Contains(a.tabs, t) || t.files == nil {
					return
				}
				if liveUser != "" && (resolvedInfo.Home != "" || resolvedInfo.HasIDs) {
					if t.userInfos == nil {
						t.userInfos = map[string]remoteUserInfo{}
					}
					t.userInfos[liveUser] = resolvedInfo
				}
				if err != nil {
					a.toast(ui.Toast{Title: locales.FileCannotUpload(), Description: transferReason(err), Type: "error"})
					a.maybeScheduleHideFloatingQueue(t)
					return
				}
				if fallback {
					a.toast(ui.Toast{Title: locales.FileUploadedToHome(), Description: locales.FileNoCwd(dir)})
				}
				var owner *sshconn.FileOwner
				if switched && resolvedInfo.HasIDs {
					owner = &sshconn.FileOwner{UID: resolvedInfo.UID, GID: resolvedInfo.GID}
				}
				a.uploadPathsOwned(t, dir, files, folders, owner, targetUser)
			})
		}()
	}
	if t.files != nil {
		start()
		return
	}
	t.filesError = ""
	t.filesWaiting = append(t.filesWaiting, start)
	a.ensureFiles(t)
}
