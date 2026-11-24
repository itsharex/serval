package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"serval/core/sshconn"
)

func TestParsePasswdEntry(t *testing.T) {
	passwd := []byte(`
# comment
root:x:0:0:root:/root:/bin/bash
tdr:x:1001:1002::/data/home/tdr:/bin/bash
nologin:x:1002:1002:::/sbin/nologin
malformed
`)
	if info, ok := parsePasswdEntry(passwd, "root"); !ok || info.Home != "/root" || !info.HasIDs || info.UID != 0 || info.GID != 0 {
		t.Fatalf("root entry = %+v, ok=%v", info, ok)
	}
	if info, ok := parsePasswdEntry(passwd, "tdr"); !ok || info.Home != "/data/home/tdr" || !info.HasIDs || info.UID != 1001 || info.GID != 1002 {
		t.Fatalf("tdr entry = %+v, ok=%v", info, ok)
	}
	if got := parsePasswdHome(passwd, "nologin"); got != "" {
		t.Fatalf("nologin home = %q, want empty", got)
	}
	if got := parsePasswdHome(passwd, "missing"); got != "" {
		t.Fatalf("missing home = %q, want empty", got)
	}
	if got := parsePasswdHome(passwd, "bad;user"); got != "" {
		t.Fatalf("invalid user home = %q, want empty", got)
	}
}

func TestResolveRemoteUserInfoViaSSH(t *testing.T) {
	h := newHarness(t)
	tb := h.connectDev()
	h.pump("打开 SFTP 通道", func() bool {
		h.a.ensureFiles(tb)
		return tb.files != nil
	})

	u, err := user.Current()
	if err != nil || !isValidPromptUser(u.Username) {
		t.Skip("当前系统用户名不适用于远端用户查询测试")
	}
	wantHome, err := os.UserHomeDir()
	if err != nil || wantHome == "" {
		t.Skip("无法获取当前用户主目录")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := resolveRemoteUserInfo(ctx, tb.client, tb.files, u.Username)
	if err != nil {
		t.Fatalf("resolveRemoteUserInfo(%q) failed: %v", u.Username, err)
	}
	if got.Home != wantHome {
		t.Fatalf("resolveRemoteUserInfo(%q).Home = %q, want %q", u.Username, got.Home, wantHome)
	}
	if os.Getuid() >= 0 && (!got.HasIDs || got.UID != os.Getuid() || got.GID != os.Getgid()) {
		t.Fatalf("resolveRemoteUserInfo(%q) ids = %+v, want uid=%d gid=%d", u.Username, got, os.Getuid(), os.Getgid())
	}

	if _, err := resolveRemoteUserInfo(ctx, tb.client, tb.files, "bad;user"); err == nil {
		t.Fatal("非法用户名应被拒绝")
	}
}

func TestPromptTrackerDoesNotDowngradeFullPathToBasename(t *testing.T) {
	pt := newPromptTracker()
	u, d, changed := pt.feed([]byte("\x1b]0;tdr@localhost:~/iot/etlinks/vue\x07[tdr@localhost vue]$ "))
	if !changed || u != "tdr" || d != "~/iot/etlinks/vue" {
		t.Fatalf("首次应保留完整路径: got (%q, %q, %v)", u, d, changed)
	}
	// 下一行只输出不带完整路径的 bash 提示符时，不应退化为 "vue"。
	u, d, changed = pt.feed([]byte("\r\n[tdr@localhost vue]$ "))
	if changed || u != "tdr" || d != "~/iot/etlinks/vue" {
		t.Fatalf("后续提示符不应把 ~/iot/etlinks/vue 覆盖为 basename: got (%q, %q, %v)", u, d, changed)
	}
}

func TestDropFilesOnTerminalUsesOSC0RelativeHomeDir(t *testing.T) {
	h := newHarness(t)
	h.a.transferHideDelay = 80 * time.Millisecond
	tb := h.connectDev()
	p := tb.focus

	// 取得 devsshd 的 SFTP 主目录，在下面创建 iot/etlinks/vue 子目录
	h.pump("确保 SFTP 通道就绪", func() bool {
		h.a.ensureFiles(tb)
		return tb.files != nil && tb.remoteHome != ""
	})
	targetDir := filepath.Join(tb.remoteHome, "iot", "etlinks", "vue")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 模拟 CentOS bash：不发 OSC 7，只发 OSC 0 + [user@host vue]$
	p.term.Send([]byte(fmt.Sprintf("printf '\\033]0;%s@localhost:~/iot/etlinks/vue\\007[%s@localhost vue]$ '\n", tb.host.User, tb.host.User)))
	h.pump("终端识别到 ~/iot/etlinks/vue", func() bool {
		_, dir := paneUserAndDir(tb, p)
		return dir == "~/iot/etlinks/vue"
	})

	local := t.TempDir()
	data := []byte("vue-dist-payload")
	file := filepath.Join(local, "dist.zip")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if !dropOnTerminal(t, h, file) {
		t.Fatal("拖到终端上的文件应被接住")
	}
	h.pump("文件上传到 ~/iot/etlinks/vue", func() bool {
		got, err := os.ReadFile(filepath.Join(targetDir, "dist.zip"))
		return err == nil && bytes.Equal(got, data)
	})
	if h.a.filesOpen || tb.filesOpen {
		t.Fatal("拖放上传不应自动展开文件抽屉")
	}
	if _, err := os.Stat(filepath.Join(tb.remoteHome, "dist.zip")); err == nil {
		t.Fatal("不应错误上传到登录用户的家目录根目录")
	}
}

func TestDropFilesOnTerminalUsesOSC0AbsoluteDir(t *testing.T) {
	h := newHarness(t)
	h.a.transferHideDelay = 80 * time.Millisecond
	tb := h.connectDev()
	p := tb.focus

	remote, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.term.Send([]byte(fmt.Sprintf("printf '\\033]0;%s@localhost:%s\\007'\n", tb.host.User, remote)))
	h.pump("终端识别到 OSC 0 绝对路径", func() bool {
		_, dir := paneUserAndDir(tb, p)
		return dir == remote
	})

	local := t.TempDir()
	data := []byte("abs-osc0-payload")
	file := filepath.Join(local, "bundle.tar")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if !dropOnTerminal(t, h, file) {
		t.Fatal("拖到终端上的文件应被接住")
	}
	h.pump("文件上传到 OSC 0 绝对路径", func() bool {
		got, err := os.ReadFile(filepath.Join(remote, "bundle.tar"))
		return err == nil && bytes.Equal(got, data)
	})
}

func TestDropFilesAfterSuUploadsToSwitchedUserHomeAndSubdir(t *testing.T) {
	h := newHarness(t)
	h.a.transferHideDelay = 80 * time.Millisecond
	tb := h.connectDev()
	p := tb.focus

	rootHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tdrHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tdrVue := filepath.Join(tdrHome, "iot", "etlinks", "vue")
	if err := os.MkdirAll(tdrVue, 0o755); err != nil {
		t.Fatal(err)
	}

	h.a.resolveUserInfo = func(_ context.Context, _ *sshconn.Client, _ *sshconn.Files, u string) (remoteUserInfo, error) {
		if u == "tdr" {
			return remoteUserInfo{Home: tdrHome, UID: os.Getuid(), GID: os.Getgid(), HasIDs: os.Getuid() >= 0}, nil
		}
		return remoteUserInfo{}, fmt.Errorf("unexpected user %q", u)
	}

	// 先模拟以 root 登录并上报过 OSC 7 (/root)
	setTerminalDir(t, h, tb, rootHome)
	// 再模拟 su - tdr 后，远端只更新了 OSC 0 为 tdr@localhost:~
	p.term.Send([]byte("printf '\\033]0;tdr@localhost:~\\007[tdr@localhost ~]$ '\n"))
	h.pump("切换用户为 tdr", func() bool {
		u, dir := paneUserAndDir(tb, p)
		return u == "tdr" && dir == "~"
	})

	local := t.TempDir()
	homeFile := filepath.Join(local, "env.txt")
	if err := os.WriteFile(homeFile, []byte("tdr-home"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !dropOnTerminal(t, h, homeFile) {
		t.Fatal("拖放上传应被接住")
	}
	h.pump("上传到 tdr 家目录", func() bool {
		got, err := os.ReadFile(filepath.Join(tdrHome, "env.txt"))
		return err == nil && string(got) == "tdr-home"
	})
	if _, err := os.Stat(filepath.Join(rootHome, "env.txt")); err == nil {
		t.Fatal("su - tdr 后不应上传到原 root 家目录")
	}

	// 再在 tdr 下切换到 ~/iot/etlinks/vue
	p.term.Send([]byte("printf '\\033]0;tdr@localhost:~/iot/etlinks/vue\\007[tdr@localhost vue]$ '\n"))
	h.pump("tdr 切换到子目录", func() bool {
		u, dir := paneUserAndDir(tb, p)
		return u == "tdr" && dir == "~/iot/etlinks/vue"
	})

	subFile := filepath.Join(local, "dist.zip")
	if err := os.WriteFile(subFile, []byte("tdr-vue"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !dropOnTerminal(t, h, subFile) {
		t.Fatal("拖放上传应被接住")
	}
	h.pump("上传到 tdr 子目录", func() bool {
		got, err := os.ReadFile(filepath.Join(tdrVue, "dist.zip"))
		return err == nil && string(got) == "tdr-vue"
	})

	// 切换后在绝对路径下覆盖一个 0755 文件并上传文件夹，确认属主上下文与原权限保留生效
	absDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	existingAbs := filepath.Join(absDir, "run.sh")
	if err := os.WriteFile(existingAbs, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	p.term.Send([]byte(fmt.Sprintf("printf '\\033]0;tdr@localhost:%s\\007[tdr@localhost abs]$ '\n", absDir)))
	h.pump("tdr 切换到绝对路径", func() bool {
		u, dir := paneUserAndDir(tb, p)
		return u == "tdr" && dir == absDir
	})
	newScript := filepath.Join(local, "run.sh")
	if err := os.WriteFile(newScript, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !dropOnTerminal(t, h, newScript) {
		t.Fatal("拖放覆盖文件应被接住")
	}
	h.pump("替换确认框出现", func() bool { return len(h.a.pendingUploads) == 1 })
	h.click("替换")
	h.tt.Frame()
	h.pump("覆盖上传完成并保留权限", func() bool {
		got, err := os.ReadFile(existingAbs)
		return err == nil && string(got) == "new"
	})
	info, err := os.Stat(existingAbs)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("覆盖上传应保留原文件权限 0755: info=%v err=%v", info, err)
	}

	// 文件夹上传同样携带目标用户属主
	localFolder := filepath.Join(local, "pkg")
	if err := os.MkdirAll(localFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localFolder, "index.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !dropOnTerminal(t, h, localFolder) {
		t.Fatal("拖放文件夹应被接住")
	}
	h.pump("文件夹上传到绝对路径", func() bool {
		got, err := os.ReadFile(filepath.Join(absDir, "pkg", "index.js"))
		return err == nil && string(got) == "console.log(1)"
	})
}

func TestDropFilesChownPermissionDeniedWarnsWithoutFailing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时 chown 0:0 不会失败")
	}
	h := newHarness(t)
	h.a.transferHideDelay = 80 * time.Millisecond
	tb := h.connectDev()
	p := tb.focus

	remote, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.a.resolveUserInfo = func(_ context.Context, _ *sshconn.Client, _ *sshconn.Files, u string) (remoteUserInfo, error) {
		if u == "root" {
			return remoteUserInfo{Home: remote, UID: 0, GID: 0, HasIDs: true}, nil
		}
		return remoteUserInfo{}, fmt.Errorf("unexpected user %q", u)
	}

	p.term.Send([]byte(fmt.Sprintf("printf '\\033]0;root@localhost:%s\\007[root@localhost dir]# '\n", remote)))
	h.pump("终端切换为 root", func() bool {
		u, dir := paneUserAndDir(tb, p)
		return u == "root" && dir == remote
	})

	local := t.TempDir()
	file := filepath.Join(local, "note.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !dropOnTerminal(t, h, file) {
		t.Fatal("拖放上传应被接住")
	}
	h.pump("文件写入且提示属主未改", func() bool {
		got, err := os.ReadFile(filepath.Join(remote, "note.txt"))
		return err == nil && string(got) == "hello" && h.tt.HasText("未能将属主改为 root")
	})
}

func TestFloatingTransferQueueStaysOnErrorAndHandsOffOnDrawerClose(t *testing.T) {
	h := newHarness(t)
	h.a.transferHideDelay = 60 * time.Millisecond
	tb := h.connectDev()
	h.pump("确保 SFTP 通道就绪", func() bool {
		h.a.ensureFiles(tb)
		return tb.files != nil
	})

	// 1. 失败的传输不应自动消失，方便用户查看与重试
	h.a.runTransfer(tb, transferDownload, filepath.Join(t.TempDir(), "missing.txt"), filepath.Join(t.TempDir(), "out.txt"), "missing.txt", false)
	h.pump("传输失败进入悬浮队列", func() bool {
		return len(tb.transfers) == 1 && tb.transfers[0].done.Load() && tb.transfers[0].err != ""
	})
	time.Sleep(100 * time.Millisecond)
	h.tt.Frame()
	if !tb.floatingQueue || !h.tt.HasText("传输队列") || !h.tt.HasText("重试") {
		t.Fatalf("失败记录应保留悬浮队列并显示重试: floating=%v texts=%q", tb.floatingQueue, h.tt.Texts())
	}

	// 窄窗口下悬浮队列也能正常渲染
	h.tt.SetSize(760, 420)
	h.tt.Frame()
	if !h.tt.HasText("传输队列") {
		t.Fatalf("窄窗口下悬浮传输队列应正常显示: %q", h.tt.Texts())
	}
	h.tt.SetSize(1200, 800)

	// 清除失败记录后悬浮队列应关闭
	h.click("清除已完成")
	h.tt.Frame()
	if tb.floatingQueue || len(tb.transfers) != 0 {
		t.Fatalf("清除已完成后悬浮队列应关闭: floating=%v len=%d", tb.floatingQueue, len(tb.transfers))
	}

	// 2. 在文件抽屉里完成一次上传后关闭抽屉，只有已完成记录时不应弹出悬浮队列
	h.a.setFilesOpen(tb, true)
	h.tt.Frame()
	remote := t.TempDir()
	local := filepath.Join(t.TempDir(), "ok.txt")
	if err := os.WriteFile(local, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.a.runTransfer(tb, transferUpload, filepath.Join(remote, "ok.txt"), local, "ok.txt", false)
	h.pump("抽屉内上传完成", func() bool {
		return len(tb.transfers) == 1 && tb.transfers[0].done.Load() && tb.transfers[0].err == ""
	})
	h.a.setFilesOpen(tb, false)
	h.tt.Frame()
	if tb.floatingQueue {
		t.Fatal("关闭文件抽屉时若无进行中传输，不应弹出悬浮队列")
	}
}
