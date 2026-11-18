package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"serval/core/hosts"
	"serval/core/syncer"
	"serval/core/vault"

	"golang.org/x/net/webdav"
)

type testS3Mock struct {
	mu      sync.Mutex
	objects map[string][]byte
	etags   map[string]string
}

func (m *testS3Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	key := r.URL.Path
	m.mu.Lock()
	defer m.mu.Unlock()

	switch r.Method {
	case http.MethodHead:
		data, exists := m.objects[key]
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"`+m.etags[key]+`"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		data, exists := m.objects[key]
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"`+m.etags[key]+`"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)

	case http.MethodPut:
		ifMatch := r.Header.Get("If-Match")
		ifNoneMatch := r.Header.Get("If-None-Match")

		_, exists := m.objects[key]
		if ifNoneMatch == "*" && exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if ifMatch != "" {
			currentETag := `"` + m.etags[key] + `"`
			if ifMatch != currentETag && ifMatch != m.etags[key] {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		h := md5.Sum(body)
		etag := hex.EncodeToString(h[:])
		m.objects[key] = body
		m.etags[key] = etag

		w.Header().Set("ETag", `"`+etag+`"`)
		w.WriteHeader(http.StatusOK)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestSyncNowWithoutConfigExplains(t *testing.T) {
	h := newHarness(t)
	h.click("搜索主机和操作")
	h.a.pal.query = "立即同步"
	h.tt.Frame()
	h.click("立即同步")
	h.tt.Frame()
	if !h.tt.HasText("还没有开启同步") {
		t.Fatalf("没配置时应说明还没开启: %q", h.tt.Texts())
	}
}

func TestSyncUploadsAfterEditAndPasswordStaysOutOfFile(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	srv := httptest.NewServer(&webdav.Handler{
		FileSystem: webdav.Dir(dir),
		LockSystem: webdav.NewMemLS(),
	})
	t.Cleanup(func() {
		h.a.stopSyncLoop()
		srv.Close()
	})
	const pass = "dav-password-not-in-file"
	if err := h.a.configureSync(srv.URL+"/vault.json", "alice", pass, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(h.a.devicePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), pass) {
		t.Fatal("同步密码不应出现在 device.json")
	}
	if !strings.Contains(string(raw), `"syncURL"`) || !strings.Contains(string(raw), `"syncUser": "alice"`) {
		t.Fatalf("地址和账号应在本机设置里: %s", raw)
	}
	if h.vaultFileContains(pass) || h.vaultFileContains("alice") {
		t.Fatal("同步账号和密码不应写进保险库文件")
	}

	h.a.syncDelay = 20 * time.Millisecond
	if err := h.a.apply(hostChange(hosts.Host{
		ID: "synced", Name: "synced-box", Address: "10.1.1.1", Port: 22, User: "u", Group: defaultGroup,
	})); err != nil {
		t.Fatal(err)
	}
	h.pump("上传新主机", func() bool { return remoteHasHost(t, dir, h.a.vault.Key(), "synced") })
}

func TestSyncSettingsSaveProbeAndStatus(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	const pass = "dav-password-not-in-file"
	dav := &webdav.Handler{FileSystem: webdav.Dir(dir), LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "alice" || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="serval"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		dav.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		h.a.stopSyncLoop()
		srv.Close()
	})

	h.click("设置")
	h.tt.Frame()
	h.click("同步")
	h.tt.Frame()
	if !h.tt.HasText("WebDAV 服务器地址") || !h.tt.HasText("WebDAV 密码") {
		t.Fatalf("同步页没有打开: %q", h.tt.Texts())
	}
	h.a.settingsPanel.webdavServer = srv.URL
	h.a.settingsPanel.webdavUser = "alice"
	h.a.settingsPanel.webdavPassword = "wrong-password"
	h.a.settingsPanel.syncSubDir = ""
	h.tt.Frame()
	h.click("自动同步")
	h.tt.Frame()
	if !h.a.settingsPanel.syncOn {
		t.Fatal("勾选后应启用同步")
	}
	if box, ok := h.tt.Find("WebDAV 服务器地址"); ok {
		h.tt.Scroll(box.X+50, box.Y+50, 0, 200)
		h.tt.Frame()
	}
	h.click("测试连接")
	h.pump("错误密码的测试结果", func() bool { return h.tt.HasText("同步账号或密码不正确。") })

	h.a.settingsPanel.webdavPassword = pass
	h.tt.Frame()
	h.click("测试连接")
	h.pump("正确密码的测试结果", func() bool { return h.tt.HasText("可以连接。") })

	h.click("保存配置")
	h.tt.Frame()
	if !h.tt.HasText("与已有数据合并？") {
		t.Fatalf("已有数据时应先说明合并，而不是覆盖: %q", h.tt.Texts())
	}
	h.click("取消")
	h.tt.Frame()
	if h.a.local.SyncOn {
		t.Fatal("取消后不应开启同步")
	}
	h.click("保存配置")
	h.tt.Frame()
	h.click("合并并同步")
	h.tt.Frame()
	if h.a.settingsPanel.webdavPassword != "" {
		t.Fatal("保存后应清掉表单里的密码")
	}
	if !h.a.local.SyncOn || !strings.HasSuffix(h.a.local.SyncURL, "/vault.json") || h.a.local.SyncUser != "alice" {
		t.Fatalf("本机设置 = %+v", h.a.local)
	}
	raw, err := os.ReadFile(h.a.devicePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), pass) || !strings.Contains(string(raw), `"syncUser": "alice"`) {
		t.Fatalf("device.json 不应含密码，且应记下账号: %s", raw)
	}
	got, err := h.device.Get(syncSecretName)
	if err != nil || string(got) != pass {
		t.Fatalf("安全存储里的密码 = %q, err=%v", got, err)
	}
	if h.vaultFileContains(pass) {
		t.Fatal("同步密码不应写进保险库")
	}
	h.pump("侧栏同步状态", func() bool { return h.tt.HasText("同步正常") })
	if h.a.lastSync.IsZero() {
		t.Fatal("同步成功后应记下时间")
	}

	h.click("完成")
	h.tt.Frame()
	h.click("同步状态")
	h.tt.Frame()
	if !h.a.settingsPanel.open || h.a.settingsPanel.tab != settingsTabSync {
		t.Fatalf("点状态图标应打开同步页: open=%v tab=%d", h.a.settingsPanel.open, h.a.settingsPanel.tab)
	}
	if h.a.settingsPanel.webdavPassword != "" {
		t.Fatal("再次打开时不应把已存密码填回输入框")
	}
	if box, ok := h.tt.Find("WebDAV 服务器地址"); ok {
		h.tt.Scroll(box.X+50, box.Y+50, 0, 160)
		h.tt.Frame()
	}
	h.click("保存配置")
	h.tt.Frame()
	got, err = h.device.Get(syncSecretName)
	if err != nil || string(got) != pass {
		t.Fatalf("空密码应保留已存密码，得到 %q err=%v", got, err)
	}
	h.pump("再次同步结束", func() bool { return !h.a.syncing && h.tt.HasText("同步正常") })

	h.click("自动同步")
	h.tt.Frame()
	h.click("保存配置")
	h.tt.Frame()
	if h.a.local.SyncOn || h.a.syncLoopStop != nil {
		t.Fatal("取消启用后应停用同步")
	}
	if h.tt.HasText("同步正常") || h.tt.HasText("同步失败") {
		t.Fatalf("停用后侧栏不应再显示同步状态: %q", h.tt.Texts())
	}
}

func TestSyncRestoreEmptyDeviceThenUnlock(t *testing.T) {
	h := newBareHarness(t)
	empty, err := vault.Open(filepath.Join(h.dir, "empty.json"), "empty-device")
	if err != nil {
		t.Fatal(err)
	}
	h.a.vault = empty
	h.a.reload()

	const pass = "dav-password-not-in-file"
	remote, err := vault.Open(filepath.Join(t.TempDir(), "remote.json"), "remote-device")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Protect(masterPassword); err != nil {
		t.Fatal(err)
	}
	if err := remote.Put("host", "remote-box", []byte(`{"name":"remote-box","address":"10.9.9.9","port":22,"user":"u","group":"我的主机"}`)); err != nil {
		t.Fatal(err)
	}
	body, err := remote.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	davDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(davDir, "vault.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	dav := &webdav.Handler{FileSystem: webdav.Dir(davDir), LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "alice" || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="serval"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		dav.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		h.a.stopSyncLoop()
		srv.Close()
	})

	h.tt.Frame()
	h.click("从同步恢复")
	h.tt.Frame()
	if !h.tt.HasText("恢复保险库") {
		t.Fatalf("恢复面板没有打开: %q", h.tt.Texts())
	}
	h.a.restore.url = srv.URL
	h.a.restore.user = "alice"
	h.a.restore.password = pass
	h.tt.Frame()
	h.click("开始恢复")
	h.pump("下载并锁定", func() bool {
		return h.a.vault.State() == vault.Locked && h.tt.HasText("Serval 已锁定")
	})
	if h.a.vault.ID() != remote.ID() {
		t.Fatalf("应换上远端保险库 id=%s want %s", h.a.vault.ID(), remote.ID())
	}
	raw, err := os.ReadFile(h.a.devicePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), pass) {
		t.Fatal("同步密码不应出现在 device.json")
	}
	got, err := h.device.Get(syncSecretName)
	if err != nil || string(got) != pass {
		t.Fatalf("安全存储里的密码不正确, err=%v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(h.dir, "empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), pass) {
		t.Fatal("同步密码不应写进保险库")
	}

	h.a.unlock.password = masterPassword
	h.tt.Frame()
	h.click("解锁")
	h.pump("解锁后看到远端主机", func() bool {
		return h.a.vault.State() == vault.Unlocked && h.tt.HasText("remote-box")
	})
}

func TestSyncJoinMergesBothSides(t *testing.T) {
	h := newHarness(t)
	raw, err := h.a.vault.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	remotePath := filepath.Join(dir, "vault.json")
	if err := os.WriteFile(remotePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	other, err := vault.Open(remotePath, "other-device")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Unlock(masterPassword); err != nil {
		t.Fatal(err)
	}
	if err := other.Put("host", "remote-box", []byte(`{"name":"remote-box","address":"10.2.2.2","port":22,"user":"u","group":"我的主机"}`)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&webdav.Handler{FileSystem: webdav.Dir(dir), LockSystem: webdav.NewMemLS()})
	t.Cleanup(func() {
		h.a.stopSyncLoop()
		srv.Close()
	})

	if err := h.a.apply(hostChange(hosts.Host{
		ID: "local-box", Name: "local-box", Address: "10.3.3.3", Port: 22, User: "u", Group: defaultGroup,
	})); err != nil {
		t.Fatal(err)
	}
	h.tt.Frame()
	h.click("设置")
	h.tt.Frame()
	h.click("同步")
	h.tt.Frame()
	h.a.settingsPanel.webdavServer = srv.URL
	h.a.settingsPanel.webdavUser = "alice"
	h.a.settingsPanel.webdavPassword = "merge-secret"
	h.a.settingsPanel.syncSubDir = ""
	h.tt.Frame()
	h.click("自动同步")
	h.tt.Frame()
	if box, ok := h.tt.Find("WebDAV 服务器地址"); ok {
		h.tt.Scroll(box.X+50, box.Y+50, 0, 200)
		h.tt.Frame()
	}
	h.click("保存配置")
	h.tt.Frame()
	h.click("合并并同步")
	h.tt.Frame()
	h.pump("两边合并", func() bool {
		return h.tt.HasText("local-box") && h.tt.HasText("remote-box") && remoteHasHost(t, dir, h.a.vault.Key(), "local-box") && remoteHasHost(t, dir, h.a.vault.Key(), "remote-box")
	})
}

func remoteHasHost(t *testing.T, dir string, key []byte, id string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "vault.json"))
	if err != nil || len(raw) == 0 {
		return false
	}
	path := filepath.Join(dir, "readback.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(path, "reader")
	if err != nil {
		return false
	}
	if err := v.UnlockWithKey(key); err != nil {
		return false
	}
	_, ok, err := v.Get(id)
	return err == nil && ok
}

func TestS3SyncIntegration(t *testing.T) {
	mock := &testS3Mock{
		objects: make(map[string][]byte),
		etags:   make(map[string]string),
	}
	ts := httptest.NewServer(mock)
	defer ts.Close()

	s3Target := syncer.S3Target{
		Endpoint: ts.URL,
		Region:   "auto",
		Bucket:   "serval-test-bucket",
		Key:      "vault.json",
	}
	s3URL := s3Target.FormatURL()

	h := newHarness(t)
	t.Cleanup(func() {
		h.a.stopSyncLoop()
	})

	// 1. 设置主密码以解锁
	h.a.openSettings()
	h.tt.Frame()
	h.a.settingsPanel.tab = settingsTabSync
	h.a.settingsPanel.syncKind = "S3 兼容存储"
	h.a.settingsPanel.s3Endpoint = ts.URL
	h.a.settingsPanel.s3Region = "auto"
	h.a.settingsPanel.s3Bucket = "serval-test-bucket"
	h.a.settingsPanel.s3AccessID = "AKIAIOSFODNN7EXAMPLE"
	h.a.settingsPanel.s3Secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	h.a.settingsPanel.syncSubDir = ""
	h.a.settingsPanel.syncURL = s3URL
	h.a.settingsPanel.syncUser = "AKIAIOSFODNN7EXAMPLE"
	h.a.settingsPanel.syncPassword = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	h.a.settingsPanel.syncOn = true
	h.tt.Frame()

	h.a.testSyncFromPanel()
	h.pump("S3 probe 完成", func() bool {
		return !h.a.settingsPanel.probeBusy && h.a.settingsPanel.probeOK
	})

	// 2. 保存 S3 同步设置并触发合并
	if box, ok := h.tt.Find("存储桶 (Bucket)"); ok {
		h.tt.Scroll(box.X+50, box.Y+50, 0, 300)
		h.tt.Frame()
	}
	h.click("保存配置")
	h.tt.Frame()
	if h.tt.HasText("与已有数据合并？") {
		h.click("合并并同步")
		h.tt.Frame()
	}

	if h.a.local.SyncURL != s3URL {
		t.Fatalf("local.SyncURL 不匹配: %s != %s", h.a.local.SyncURL, s3URL)
	}
	if h.a.local.SyncUser != "AKIAIOSFODNN7EXAMPLE" {
		t.Fatalf("local.SyncUser 不匹配: %s", h.a.local.SyncUser)
	}

	// 3. 手动同步上传初始保险库
	h.a.syncNow(true)
	h.pump("S3 手动同步完成", func() bool {
		return !h.a.syncing && !h.a.lastSync.IsZero()
	})
	if h.a.syncErr != "" {
		t.Fatalf("S3 首次同步失败: %s", h.a.syncErr)
	}

	// 4. 新建另一台空设备测试从 S3 恢复
	h2 := newBareHarness(t)
	t.Cleanup(func() {
		h2.a.stopSyncLoop()
	})
	empty, err := vault.Open(filepath.Join(h2.dir, "empty.json"), "empty-device")
	if err != nil {
		t.Fatal(err)
	}
	h2.a.vault = empty
	h2.a.reload()

	h2.tt.Frame()
	h2.click("从同步恢复")
	h2.tt.Frame()
	if !h2.tt.HasText("恢复保险库") {
		t.Fatalf("恢复面板没有打开: %q", h2.tt.Texts())
	}
	h2.a.restore.url = s3URL
	h2.a.restore.user = "AKIAIOSFODNN7EXAMPLE"
	h2.a.restore.password = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	h2.tt.Frame()
	h2.click("开始恢复")

	h2.pump("S3 恢复完成并锁定", func() bool {
		return h2.a.vault.State() == vault.Locked && h2.tt.HasText("Serval 已锁定")
	})
	if h2.a.vault.ID() != h.a.vault.ID() {
		t.Fatalf("应换上远端保险库 id=%s want %s", h2.a.vault.ID(), h.a.vault.ID())
	}
}

func TestSyncS3SettingsUIAndManualTrigger(t *testing.T) {
	h := newHarness(t)
	t.Cleanup(func() {
		h.a.stopSyncLoop()
	})
	h.tt.Frame()
	h.click("设置")
	h.tt.Frame()
	h.click("同步")
	h.tt.Frame()

	// 切换为 S3 兼容存储
	h.a.settingsPanel.syncKind = "S3 兼容存储"
	h.tt.Frame()
	if !h.tt.HasText("存储桶 (Bucket)") || !h.tt.HasText("Access Key ID") {
		t.Fatalf("S3 表单项没有正常展示: %q", h.tt.Texts())
	}

	h.a.settingsPanel.syncVendor = "AWS S3"
	h.a.settingsPanel.s3Region = "us-east-1"
	h.a.settingsPanel.s3Bucket = "my-backup"
	h.a.settingsPanel.s3AccessID = "AKIA123456"
	h.a.settingsPanel.s3Secret = "SECRET987654"
	h.a.settingsPanel.syncSubDir = "serval"
	h.tt.Frame()
	if box, ok := h.tt.Find("存储桶 (Bucket)"); ok {
		h.tt.Scroll(box.X+50, box.Y+50, 0, 300)
		h.tt.Frame()
	}
	h.click("自动同步")
	h.tt.Frame()
	h.click("保存配置")
	h.tt.Frame()
	if !h.tt.HasText("与已有数据合并？") {
		t.Fatalf("应弹出合并确认: %q", h.tt.Texts())
	}
	h.click("合并并同步")
	h.tt.Frame()

	if !h.a.local.SyncOn {
		t.Fatal("保存后 SyncOn 应为 true")
	}
	if h.a.local.SyncKind != "S3 兼容存储" {
		t.Fatalf("保存后 SyncKind=%q", h.a.local.SyncKind)
	}
	if !strings.HasPrefix(h.a.local.SyncURL, "s3://") {
		t.Fatalf("保存后 SyncURL 应为 s3:// 开头: %q", h.a.local.SyncURL)
	}
	if h.a.local.SyncUser != "AKIA123456" {
		t.Fatalf("保存后 SyncUser=%q", h.a.local.SyncUser)
	}
	if !h.tt.HasText("立即同步") {
		t.Fatalf("已配置同步时应展示立即同步按钮: %q", h.tt.Texts())
	}
	if h.tt.HasText("上传到云端") || h.tt.HasText("从云端下载") {
		t.Fatalf("不应再展示旧的上传/下载双按钮: %q", h.tt.Texts())
	}

	// 测试清除配置
	h.click("清除配置")
	h.tt.Frame()
	if h.a.local.SyncOn || h.a.local.SyncURL != "" {
		t.Fatal("清除后配置应为空")
	}
}

func TestSyncDraftAutoSavesOnTabSwitchAndClose(t *testing.T) {
	h := newHarness(t)
	t.Cleanup(func() {
		h.a.stopSyncLoop()
	})
	h.click("设置")
	h.tt.Frame()
	h.click("同步")
	h.tt.Frame()

	// 在 WebDAV 草稿填写配置后，不点“保存配置”，直接切换分页到“安全”：草稿应自动持久化。
	h.a.settingsPanel.webdavServer = "http://127.0.0.1:18080/dav"
	h.a.settingsPanel.webdavUser = "alice"
	h.a.settingsPanel.webdavPassword = "webdav-secret-1"
	h.a.settingsPanel.showSyncPassword = true
	h.tt.Frame()

	h.click("安全")
	h.tt.Frame()
	if h.a.local.SyncURL != "http://127.0.0.1:18080/dav/serval/vault.json" {
		t.Fatalf("切换分页应自动保存 WebDAV 地址，实际 %q", h.a.local.SyncURL)
	}
	if h.a.local.SyncUser != "alice" || h.a.syncPassword() != "webdav-secret-1" {
		t.Fatalf("切换分页应自动保存账号与密码: user=%q pass=%q", h.a.local.SyncUser, h.a.syncPassword())
	}
	if h.a.settingsPanel.showSyncPassword {
		t.Fatal("自动保存后应重置同步密码明文显示开关")
	}

	// 切回同步页修改账号并勾选“自动同步”（非空保险库尚未确认合并），直接点“完成”关闭设置：
	// 修改的账号应保存，但不会跳过合并确认直接开启 SyncOn。
	h.click("同步")
	h.tt.Frame()
	h.a.settingsPanel.webdavUser = "bob"
	h.a.settingsPanel.syncOn = true
	h.click("完成")
	h.tt.Frame()
	if h.a.local.SyncUser != "bob" {
		t.Fatalf("关闭设置时应自动保存修改后的账号，实际 %q", h.a.local.SyncUser)
	}
	if h.a.local.SyncOn {
		t.Fatal("非空保险库未经合并确认不应在关闭设置时直接开启自动同步")
	}
}

func TestSyncUserMessageNoDoublePrefix(t *testing.T) {
	err1 := fmt.Errorf("%w: 没有读完响应", syncer.ErrUnavailable)
	msg1 := syncUserMessage(err1)
	if strings.Contains(msg1, "连不上同步服务: 连不上同步服务:") {
		t.Fatalf("出现重复前缀: %s", msg1)
	}
	if msg1 != "连不上同步服务: 没有读完响应" {
		t.Fatalf("期望 '连不上同步服务: 没有读完响应'，得到 %s", msg1)
	}

	err2 := fmt.Errorf("%w: 测试连接时服务器返回了 502", syncer.ErrUnavailable)
	msg2 := syncUserMessage(err2)
	if strings.Contains(msg2, "连不上同步服务: 连不上同步服务:") {
		t.Fatalf("出现重复前缀: %s", msg2)
	}
	if msg2 != "连不上同步服务: 测试连接时服务器返回了 502" {
		t.Fatalf("期望 '连不上同步服务: 测试连接时服务器返回了 502'，得到 %s", msg2)
	}
}

func TestS3VendorGuidanceAndGuidedRestore(t *testing.T) {
	h := newHarness(t)
	h.click("设置")
	h.tt.Frame()
	h.click("同步")
	h.tt.Frame()

	h.a.settingsPanel.syncKind = "S3 兼容存储"
	h.tt.Frame()
	if !h.tt.HasText("S3 私有桶 + IAM 子用户读写密钥；Endpoint 可留空。") {
		t.Fatalf("AWS S3 应展示清晰的新手摘要引导: %q", h.tt.Texts())
	}
	h.click("查看配置步骤")
	h.tt.Frame()
	if !h.tt.HasText("1. 在 S3 控制台新建私有 Bucket，记下桶名和所在 Region（如 ap-southeast-1）。") {
		t.Fatalf("点击查看配置步骤应展开详细 1-2-3 步引导: %q", h.tt.Texts())
	}

	h.a.settingsPanel.syncVendor = "Cloudflare R2"
	h.tt.Frame()
	if h.a.settingsPanel.s3Region != "auto" || !h.tt.HasText("打开 R2 控制台") {
		t.Fatalf("切到 Cloudflare R2 应自动切换默认 Region 为 auto 并显示 R2 引导与链接: region=%q texts=%q",
			h.a.settingsPanel.s3Region, h.tt.Texts())
	}

	h.a.settingsPanel.syncVendor = "阿里云 OSS"
	h.tt.Frame()
	if h.a.settingsPanel.s3Region != "oss-cn-hangzhou" {
		t.Fatalf("切到阿里云 OSS 应切换默认 Region 为 oss-cn-hangzhou，实际 %q", h.a.settingsPanel.s3Region)
	}
	h.a.settingsPanel.s3Endpoint = "https://oss-cn-beijing.aliyuncs.com"
	h.tt.Frame()
	if h.a.settingsPanel.s3Region != "oss-cn-beijing" {
		t.Fatalf("填写 OSS 北京 Endpoint 应自动识别 Region=oss-cn-beijing，实际 %q", h.a.settingsPanel.s3Region)
	}
	h.click("打开 OSS 控制台")
	h.tt.Frame()
	if len(h.openedURLs) != 1 || h.openedURLs[0] != "https://oss.console.aliyun.com/" {
		t.Fatalf("点击 OSS 控制台快捷链接打开地址不正确: %v", h.openedURLs)
	}

	h.a.settingsPanel.syncVendor = "腾讯云 COS"
	h.tt.Frame()
	h.a.settingsPanel.s3Endpoint = "https://cos.ap-shanghai.myqcloud.com"
	h.tt.Frame()
	if h.a.settingsPanel.s3Region != "ap-shanghai" || !h.tt.HasText("打开 CAM 访问管理") {
		t.Fatalf("切到腾讯云 COS 应自动识别 ap-shanghai 并提供 CAM 访问管理直达链接: region=%q texts=%q",
			h.a.settingsPanel.s3Region, h.tt.Texts())
	}
	h.click("打开 CAM 访问管理")
	h.tt.Frame()
	if len(h.openedURLs) != 2 || h.openedURLs[1] != "https://console.cloud.tencent.com/cam" {
		t.Fatalf("点击腾讯云 CAM 快捷链接打开地址不正确: %v", h.openedURLs)
	}

	// 验证在“从同步恢复”中直接使用向导式 S3 表单（无需手写 s3://...?endpoint=）即可恢复并回退兼容默认 serval 目录
	remote, err := vault.Open(filepath.Join(t.TempDir(), "remote.json"), "mac-dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Protect(masterPassword); err != nil {
		t.Fatal(err)
	}
	remoteBody, err := remote.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	objects := map[string][]byte{
		"/team-vault/serval/vault.json": remoteBody,
	}
	etags := map[string]string{
		"/team-vault/serval/vault.json": `"etag-1"`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			data, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
				return
			}
			w.Header().Set("ETag", etags[r.URL.Path])
			w.WriteHeader(http.StatusOK)
			w.Write(data)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = body
			etags[r.URL.Path] = `"etag-2"`
			w.Header().Set("ETag", `"etag-2"`)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	h2 := newBareHarness(t)
	t.Cleanup(func() {
		h2.a.stopSyncLoop()
	})
	empty, err := vault.Open(filepath.Join(h2.dir, "empty.json"), "win-dev")
	if err != nil {
		t.Fatal(err)
	}
	h2.a.vault = empty
	h2.a.reload()
	h2.tt.Frame()

	h2.click("从同步恢复")
	h2.tt.Frame()
	h2.a.restore.syncKind = "S3 兼容存储"
	h2.a.restore.syncVendor = "MinIO"
	h2.a.restore.s3Endpoint = srv.URL
	h2.a.restore.s3Bucket = "team-vault"
	h2.a.restore.user = "minio-ak"
	h2.a.restore.password = "minio-sk"
	h2.a.restore.syncSubDir = "" // 未填子目录时应自动回退命中 serval/vault.json
	h2.tt.Frame()

	h2.click("开始恢复")
	h2.pump("向导式 S3 恢复成功并锁定", func() bool {
		return h2.a.vault.State() == vault.Locked && h2.tt.HasText("Serval 已锁定")
	})
	if h2.a.vault.ID() != remote.ID() {
		t.Fatalf("应采纳远端 VaultID=%s，实际=%s", remote.ID(), h2.a.vault.ID())
	}
	if h2.a.local.SyncKind != "S3 兼容存储" || h2.a.local.SyncVendor != "MinIO" || h2.a.local.SyncSubDir != "serval" {
		t.Fatalf("恢复后应记住同步类型、服务商与回退命中的子目录: kind=%q vendor=%q subDir=%q",
			h2.a.local.SyncKind, h2.a.local.SyncVendor, h2.a.local.SyncSubDir)
	}
}
