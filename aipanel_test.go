package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"serval/core/ai"
	"serval/core/hosts"
	"serval/core/sshconn"
	"serval/core/vault"
	"serval/locales"

	"github.com/egoist/mygo/ui"
)

// mockAIServer 创建模拟 OpenAI API 的 HTTP 服务
func mockAIServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-sk" {
			http.Error(w, `{"error":{"message":"Incorrect API key"}}`, http.StatusUnauthorized)
			return
		}

		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": "gpt-4o"}},
			})
		case "/chat/completions":
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			res := map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]string{
							"content": `{"explanation":"使用 find 命令排查大文件","command":"find / -type f -size +100M"}`,
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(res)
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestAISettingsAndProbe 测试在设置中配置 AI 助手并测试连通性
func TestAISettingsAndProbe(t *testing.T) {
	ts := mockAIServer(t)
	defer ts.Close()

	h := newHarness(t)
	h.tt.Frame()

	// 打开设置
	h.click("设置")
	h.tt.Frame()
	if !h.a.settingsPanel.open {
		t.Fatal("设置面板未打开")
	}

	// 切换到 AI Tab
	h.click("AI")
	h.tt.Frame()

	// 填写配置
	h.a.settingsPanel.aiBaseURL = ts.URL
	h.a.settingsPanel.aiModel = "gpt-4o"
	h.a.settingsPanel.aiKey = "test-sk"
	h.tt.Frame()

	// 测试连接
	h.click("测试连接")
	h.pump("AI 测试连接", func() bool {
		return strings.Contains(h.a.settingsPanel.aiTestMsg, "测试成功")
	})

	// 关闭设置自动保存配置
	h.click("完成")
	h.tt.Frame()

	if h.a.settings.AIBaseURL != ts.URL || h.a.settings.AIModel != "gpt-4o" {
		t.Fatalf("设置未正确保存: baseURL=%s, model=%s", h.a.settings.AIBaseURL, h.a.settings.AIModel)
	}
	if h.a.getAIKey() != "test-sk" {
		t.Fatalf("API Key 未正确保存到加密保险库")
	}
}

// TestAIPanelGenerateAndInsertCommand 测试 AI 助手生成命令、复制及安全填入终端
func TestAIPanelGenerateAndInsertCommand(t *testing.T) {
	ts := mockAIServer(t)
	defer ts.Close()

	h := newHarness(t)
	h.tt.Frame()

	// 预先写入 AI 配置
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "gpt-4o"
	h.a.setAIKey("test-sk")

	// 开启一个本地终端作为活动标签
	h.click("新建")
	h.tt.Frame()
	h.click("本地终端")
	h.tt.Frame()
	if len(h.a.tabs) == 0 {
		t.Fatal("没有打开终端标签")
	}

	// 打开 AI Agent 面板（统一顶部标签栏入口）
	h.click("AI Agent（" + shortcutText("I") + "）")
	h.tt.Frame()
	if !h.a.ai.open {
		t.Fatal("AI 助手面板未打开")
	}

	// 输入查询并点击发送
	h.tt.Type("查找大于100M的文件")
	h.tt.Frame()
	if h.a.ai.query == "" {
		t.Fatalf("AutoFocus 未聚焦到 AI 输入框，query 为空")
	}
	h.click("发送")

	// 等待异步完成
	h.pump("AI 生成命令", func() bool {
		return h.a.ai.command != ""
	})

	if h.a.ai.command != "find / -type f -size +100M" {
		t.Fatalf("AI 返回命令不符合预期: %q, err=%s", h.a.ai.command, h.a.ai.err)
	}
	if !strings.Contains(h.a.ai.explanation, "find") {
		t.Fatalf("AI 返回解释不符合预期: %q", h.a.ai.explanation)
	}

	// 测试复制命令
	h.click("复制命令")
	h.tt.Frame()
	if h.tt.Clipboard() != "find / -type f -size +100M" {
		t.Fatalf("剪贴板未包含命令: got %q", h.tt.Clipboard())
	}

	// 测试安全填入终端（停靠式侧栏填入后保持打开，且不自动回车）
	h.click("填入终端（需确认）")
	h.tt.Frame()
	if !h.a.ai.open {
		t.Fatal("停靠式 AI 侧栏填入终端后应保持打开")
	}

	// 验证终端存在
	active := h.a.current()
	if active == nil || active.focus == nil || active.focus.term == nil {
		t.Fatal("没有活动的终端")
	}

	// 验证高危命令拦截
	h.a.openAI()
	h.a.ai.turns = []aiTurn{{query: "danger", explanation: "危险测试", commands: []string{"rm -rf /var/log/*"}}}
	h.tt.Frame()
	h.click("填入终端（需确认）")
	h.tt.Frame()

	// 应该触发确认弹窗
	if !h.a.ai.confirmDanger {
		t.Fatal("高危命令未触发危险拦截确认弹窗")
	}
	if !h.tt.HasText("执行高危命令确认") {
		t.Fatal("界面未显示高危命令确认弹窗")
	}

	// 点击确认填入
	h.click("确认填入")
	h.tt.Frame()
	if h.a.ai.confirmDanger {
		t.Fatal("确认填入后 confirmDanger 状态应复位")
	}
}

// TestAIKeyEncryptedAndMigrated 验证 AI Key 加密写进保险库，并自动迁移旧明文配置。
func TestAIKeyEncryptedAndMigrated(t *testing.T) {
	h := newHarness(t)
	h.a.setAIKey("sk-super-secret-999")
	if h.a.getAIKey() != "sk-super-secret-999" {
		t.Fatalf("getAIKey mismatch: %q", h.a.getAIKey())
	}
	if h.vaultFileContains("sk-super-secret-999") {
		t.Fatal("AI Key 不应以明文形式出现在 vault.json 中")
	}
	if h.a.local.LegacyAIKey != "" {
		t.Fatal("deviceSettings 不应保留明文 AI Key")
	}

	// 模拟从旧版 device.json 迁移明文 Key
	h.a.setAIKey("")
	legacy := h.a.local
	legacy.LegacyAIKey = "sk-legacy-plain-123"
	if err := h.a.saveLocal(legacy); err != nil {
		t.Fatal(err)
	}
	h.a.loadLocal()
	if h.a.getAIKey() != "sk-legacy-plain-123" {
		t.Fatalf("旧版 AI Key 迁移失败: got %q", h.a.getAIKey())
	}
	if h.a.local.LegacyAIKey != "" {
		t.Fatal("迁移后旧明文字段应当被清空")
	}
}

// TestAIMultilineCommandCannotInsert 验证多行脚本不提供“填入终端”且底层拒绝填入。
func TestAIMultilineCommandCannotInsert(t *testing.T) {
	h := newHarness(t)
	h.click("新建")
	h.tt.Frame()
	h.click("本地终端")
	h.tt.Frame()

	h.a.openAI()
	h.a.ai.turns = []aiTurn{{
		query:       "multi",
		explanation: "多行脚本",
		commands:    []string{"echo one\necho two"},
	}}
	h.tt.Frame()
	if h.tt.HasText("填入终端（需确认）") {
		t.Fatal("多行命令不应显示“填入终端（需确认）”按钮")
	}
	if !h.tt.HasText("多行脚本请复制后核对执行") {
		t.Fatal("多行命令应提示用户复制后核对执行")
	}
	h.a.insertCommandToTerminal("echo one\necho two")
	if !h.a.ai.open {
		t.Fatal("多行命令应被 insertCommandToTerminal 拒绝，面板保持打开")
	}
}

// TestAIDangerousCommandPatterns 测试高危命令匹配逻辑
func TestAIDangerousCommandPatterns(t *testing.T) {
	dangers := []string{
		"rm -rf /",
		"rm -r /home",
		"dd if=/dev/zero of=/dev/sda",
		"mkfs.ext4 /dev/sda1",
		":(){ :|:& };:",
		"echo dangerous\nreboot",
		"reboot",
		"shutdown -h now",
		"chmod -R 777 /",
	}

	for _, cmd := range dangers {
		if !ai.IsDangerousCommand(cmd) {
			t.Errorf("应该识别为危险命令: %q", cmd)
		}
	}

	safe := []string{
		"ls -la",
		"cat /etc/os-release",
		"grep -r 'test' .",
		"docker ps",
		"git status",
	}

	for _, cmd := range safe {
		if ai.IsDangerousCommand(cmd) {
			t.Errorf("不应误判为危险命令: %q", cmd)
		}
	}
}

// TestAISettingsAutoSaveOnCloseAndTabSwitch 验证在设置里修改 AI 项后直接切换标签页或点“完成”可自动保存，且未修改默认项时不误写保险库。
func TestAISettingsAutoSaveOnCloseAndTabSwitch(t *testing.T) {
	ts := mockAIServer(t)
	defer ts.Close()

	h := newHarness(t)
	h.tt.Frame()

	// 打开设置并直接点“完成”：未修改默认值时不应写入保险库
	h.click("设置")
	h.tt.Frame()
	h.click(locales.Done())
	h.tt.Frame()
	if h.a.settings.AIBaseURL != "" || h.a.settings.AIModel != "" {
		t.Fatalf("未修改 AI 默认设置时不应写入保险库: baseURL=%q, model=%q", h.a.settings.AIBaseURL, h.a.settings.AIModel)
	}

	// 在 AI 页填写配置后直接切换到“通用”页，应自动提交保存
	h.click("设置")
	h.tt.Frame()
	h.click("AI")
	h.tt.Frame()
	h.a.settingsPanel.aiBaseURL = ts.URL
	h.a.settingsPanel.aiModel = "gpt-4o"
	h.a.settingsPanel.aiKey = "test-sk"
	h.tt.Frame()

	h.click(locales.SetGeneral())
	h.tt.Frame()
	if h.a.settings.AIBaseURL != ts.URL || h.a.settings.AIModel != "gpt-4o" {
		t.Fatalf("切换设置页签后 AI 配置未自动保存: baseURL=%q, model=%q", h.a.settings.AIBaseURL, h.a.settings.AIModel)
	}
	if h.a.getAIKey() != "test-sk" {
		t.Fatal("切换设置页签后 AI Key 未自动保存到保险库")
	}

	// 再次在 AI 页修改后直接点“完成”（不点测试连接、不按回车）也应自动保存并关闭设置
	h.click("AI")
	h.tt.Frame()
	h.a.settingsPanel.aiModel = "gpt-4.1-mini"
	h.a.settingsPanel.aiKey = "test-sk-2"
	h.tt.Frame()
	h.click(locales.Done())
	h.tt.Frame()

	if h.a.settingsPanel.open {
		t.Fatal("点击完成后设置面板应当关闭")
	}
	if h.a.settings.AIModel != "gpt-4.1-mini" {
		t.Fatalf("点击完成后 AI 模型未保存: %q", h.a.settings.AIModel)
	}
	if h.a.getAIKey() != "test-sk-2" {
		t.Fatal("点击完成后 AI Key 未保存到保险库")
	}
}

// TestAISettingsCloseRequireProtection 验证未设置主密码时填写 AI Key 点“完成”会先弹出设置主密码，取消保留输入，完成后加密保存并关闭设置。
func TestAISettingsCloseRequireProtection(t *testing.T) {
	h := newBareHarness(t)
	h.tt.Frame()

	h.click("设置")
	h.tt.Frame()
	h.click("AI")
	h.tt.Frame()
	h.a.settingsPanel.aiKey = "sk-needs-protection"
	h.tt.Frame()

	h.click(locales.Done())
	h.tt.Frame()
	if !h.a.setup.open {
		t.Fatal("未加密保险库保存 AI Key 时应当先弹出设置主密码")
	}
	if !h.a.settingsPanel.open {
		t.Fatal("设置主密码让位期间应当保留设置面板状态")
	}
	if h.a.vault.State() != vault.Unprotected {
		t.Fatalf("设置主密码前保险库不应已加密: state=%v", h.a.vault.State())
	}

	// 取消设置主密码：回到设置面板且输入的 Key 还在，保险库里无秘密
	h.click(locales.Cancel())
	h.tt.Frame()
	if h.a.setup.open || !h.a.settingsPanel.open {
		t.Fatalf("取消设置主密码后应恢复设置面板: setup=%v, settings=%v", h.a.setup.open, h.a.settingsPanel.open)
	}
	if h.a.settingsPanel.aiKey != "sk-needs-protection" {
		t.Fatal("取消设置主密码后用户输入不应丢失")
	}
	if h.a.getAIKey() != "" {
		t.Fatal("未设主密码前不应保存 AI Key")
	}

	// 再次点“完成”并完成主密码设置
	h.click(locales.Done())
	h.tt.Frame()
	h.a.setup.password = masterPassword
	h.a.setup.confirm = masterPassword
	h.tt.Frame()
	h.click(locales.SheetSetAndContinue())
	h.pump("完成主密码设置并保存 AI Key", func() bool {
		return !h.a.setup.open && h.a.vault.State() == vault.Unlocked && !h.a.settingsPanel.open
	})

	if h.a.getAIKey() != "sk-needs-protection" {
		t.Fatal("设置主密码完成后 AI Key 未保存到保险库")
	}
	if h.vaultFileContains("sk-needs-protection") {
		t.Fatal("vault.json 中不应出现明文 AI Key")
	}
}

// TestAIKeyMigratedAfterUnlock 验证锁定启动并在输入主密码解锁后，旧 device.json 里的明文 AI Key 会被自动迁移并清除。
func TestAIKeyMigratedAfterUnlock(t *testing.T) {
	h := newHarness(t)
	h.tt.Frame()

	// 写入旧版明文 AI Key，并删除记住的主密码，模拟锁定状态下冷启动
	legacy := h.a.local
	legacy.LegacyAIKey = "sk-locked-legacy-123"
	if err := h.a.saveLocal(legacy); err != nil {
		t.Fatal(err)
	}
	_ = h.device.Delete(rememberedKeyName(h.a.vault.ID()))
	h.reopen()

	if h.a.vault.State() != vault.Locked {
		t.Fatalf("reopen 后保险库应处于锁定状态: %v", h.a.vault.State())
	}
	if h.a.local.LegacyAIKey == "" {
		t.Fatal("锁定状态下不应提前清空待迁移的 LegacyAIKey")
	}

	h.a.unlock.password = masterPassword
	h.a.submitUnlock()
	h.pump("解锁保险库并迁移旧 AI Key", func() bool {
		return !h.a.unlock.busy && h.a.vault.State() == vault.Unlocked
	})

	if h.a.getAIKey() != "sk-locked-legacy-123" {
		t.Fatal("解锁后未自动迁移旧版明文 AI Key")
	}
	if h.a.local.LegacyAIKey != "" {
		t.Fatal("迁移后旧明文字段应当被清空")
	}
	if h.vaultFileContains("sk-locked-legacy-123") {
		t.Fatal("迁移进保险库的 AI Key 不应以明文存于 vault.json")
	}
}

// TestLockVaultClearsAIAndTransientState 验证锁定保险库会取消进行中的 AI 请求并清空敏感会话与临时界面状态。
func TestLockVaultClearsAIAndTransientState(t *testing.T) {
	h := newHarness(t)
	h.tt.Frame()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.a.ai = aiPanel{
		open:          true,
		loading:       true,
		cancel:        cancel,
		query:         "inspect secret",
		contextError:  "sensitive terminal stderr",
		explanation:   "some explanation",
		command:       "rm -rf /tmp/test",
		turns:         []aiTurn{{query: "q", explanation: "e", commands: []string{"ls"}}},
		chatHistory:   []ai.Message{{Role: "user", Content: "q"}},
		confirmDanger: true,
		pendingCmd:    "rm -rf /tmp/test",
	}
	h.a.lastCopiedText = "copied secret output"
	h.a.customGroups = []string{"TempGroup"}
	h.a.groupSheet = groupDialogState{open: true, name: "Draft"}
	h.a.connectingPhase = map[string]string{"h1": "正在连接…"}

	h.a.lockVault()

	if ctx.Err() != context.Canceled {
		t.Fatal("锁定保险库时应当取消正在进行的 AI 请求")
	}
	if h.a.ai.open || h.a.ai.loading || h.a.ai.contextError != "" || len(h.a.ai.turns) != 0 || len(h.a.ai.chatHistory) != 0 || h.a.ai.pendingCmd != "" {
		t.Fatalf("锁定保险库后 AI 面板状态未清空: %+v", h.a.ai)
	}
	if h.a.lastCopiedText != "" {
		t.Fatal("锁定保险库后 lastCopiedText 未清空")
	}
	if len(h.a.customGroups) != 0 || h.a.groupSheet.open || len(h.a.connectingPhase) != 0 {
		t.Fatalf("锁定保险库后临时界面状态未清空: customGroups=%+v groupSheet=%+v phase=%+v", h.a.customGroups, h.a.groupSheet, h.a.connectingPhase)
	}
}

// TestAIPanelEnterSubmitsExplainWhenContextErrorOnly 验证仅有报错上下文且输入为空时，回车与主按钮同样触发报错分析。
func TestAIPanelEnterSubmitsExplainWhenContextErrorOnly(t *testing.T) {
	ts := mockAIServer(t)
	defer ts.Close()

	h := newHarness(t)
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "gpt-4o"
	h.a.setAIKey("test-sk")
	h.tt.Frame()

	// 模拟打开 AI 报错面板（例如首次报错或手动填入 contextError 后尚未生成首轮结果）
	h.a.ai = aiPanel{
		open:         true,
		fresh:        true,
		contextError: "permission denied (publickey)",
	}
	h.tt.Frame()
	if !h.tt.HasText("分析报错") {
		t.Fatal("仅有报错上下文且无输入时主按钮应显示“分析报错”")
	}

	// 触发输入框 OnSubmit 同款路径
	h.a.submitAIInput()
	h.pump("AI 回车分析报错", func() bool {
		return len(h.a.ai.turns) == 1
	})
	if h.a.ai.turns[0].query != "分析终端报错" {
		t.Fatalf("首轮空输入回车应触发报错分析，实际 query=%q", h.a.ai.turns[0].query)
	}
}

// TestAIStaleCanceledRequestAndEscCloseCancels 验证：
// 1. 旧请求被新请求取消后，其延迟到达的回调不会清空新请求的 loading / cancel 状态；
// 2. 通过 Esc 或点击遮罩将 aiDialog 关闭（open=false）时，即使在设置让位判断之前也会取消进行中的请求。
func TestAIStaleCanceledRequestAndEscCloseCancels(t *testing.T) {
	var seq atomic.Int32
	req1Entered := make(chan struct{})
	req1Done := make(chan struct{})
	req2Entered := make(chan struct{})
	req2Release := make(chan struct{})
	req3Entered := make(chan struct{})
	req3Canceled := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := seq.Add(1)
		switch n {
		case 1:
			close(req1Entered)
			<-r.Context().Done()
			close(req1Done)
			http.Error(w, "canceled", http.StatusRequestTimeout)
		case 2:
			close(req2Entered)
			select {
			case <-req2Release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]string{"content": `{"explanation":"ok","commands":["pwd"]}`}},
				},
			})
		case 3:
			close(req3Entered)
			<-r.Context().Done()
			close(req3Canceled)
			http.Error(w, "canceled", http.StatusRequestTimeout)
		}
	}))
	defer ts.Close()
	defer ts.CloseClientConnections()

	h := newHarness(t)
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "gpt-4o"
	h.a.setAIKey("test-sk")
	h.tt.Frame()

	h.a.ai = aiPanel{open: true, fresh: true, query: "first"}
	h.tt.Frame()
	h.a.runAIGenerate()
	select {
	case <-req1Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第一个 AI 请求未发出")
	}

	// 在第一个请求完成前直接发起第二个请求，取消第一个请求
	h.a.ai.query = "second"
	h.a.runAIGenerate()
	select {
	case <-req2Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第二个 AI 请求未发出")
	}
	select {
	case <-req1Done:
	case <-time.After(5 * time.Second):
		t.Fatal("第一个 AI 请求未被取消")
	}
	// 泵几帧让第一个请求延迟返回的 a.post 回调执行
	for range 5 {
		h.tt.Frame()
	}
	if !h.a.ai.loading || h.a.ai.cancel == nil {
		t.Fatalf("旧请求取消回调不应覆盖新请求的 loading/cancel 状态: loading=%v cancelNil=%v", h.a.ai.loading, h.a.ai.cancel == nil)
	}

	close(req2Release)
	h.pump("第二个 AI 请求完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 2 && !h.a.ai.turns[1].running
	})

	// 发起第三个请求后点击“停止”按钮，应立即取消进行中的请求
	h.a.ai.query = "third"
	h.a.runAIGenerate()
	select {
	case <-req3Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第三个 AI 请求未发出")
	}
	h.tt.Frame()
	h.click(locales.AiStop())
	h.tt.Frame()

	select {
	case <-req3Canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("点击停止后进行中的 AI 请求未被立即取消")
	}
	if h.a.ai.loading || h.a.ai.cancel != nil {
		t.Fatalf("点击停止后应停止加载状态: loading=%v cancelNil=%v", h.a.ai.loading, h.a.ai.cancel == nil)
	}
}

// TestAIAgentMultiStepSSHInspection 端到端测试：连接真实 devsshd，AI Agent 流式思考、多步调用 host_info 与 ssh_exec、渲染 Markdown 表格和 Token 统计。
func TestAIAgentMultiStepSSHInspection(t *testing.T) {
	var round atomic.Int32
	var sawSSHOutput atomic.Bool

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		n := round.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			chunks := []string{
				`{"choices":[{"delta":{"reasoning_content":"先检查主机信息并执行远端硬件探测"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc_1","type":"function","function":{"name":"host_info","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"tc_2","type":"function","function":{"name":"ssh_exec","arguments":"{\"command\":\"printf SERVAL_AGENT_PROBE_OK\",\"reason\":\"探测远端连通与环境\"}"}}]}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_cache_hit_tokens":20,"completion_tokens_details":{"reasoning_tokens":15}}}`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		case 2:
			msgs, _ := req["messages"].([]any)
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] == "tool" {
					if content, _ := mm["content"].(string); strings.Contains(content, "SERVAL_AGENT_PROBE_OK") {
						sawSSHOutput.Store(true)
					}
				}
			}
			chunks := []string{
				`{"choices":[{"delta":{"content":"### 这台主机配置概览\n\n| 项目 | 配置 |\n| --- | --- |\n| 探针 | SERVAL_AGENT_PROBE_OK |\n\n` + "```bash\\nfree -h\\n```" + `"}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":280,"completion_tokens":40,"total_tokens":320}}`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.Error(w, "unexpected round", 500)
		}
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "deepseek-chat"
	h.a.setAIKey("test-sk")

	// 连接测试 SSH 服务器
	h.connectDev()

	// 点击顶部标签栏“AI Agent”打开停靠侧栏并点击“这台服务配置如何”快捷提问
	h.click("AI Agent（" + shortcutText("I") + "）")
	h.tt.Frame()
	h.click("这台服务配置如何")

	h.pump("Agent 多轮 SSH 探测完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running
	})

	if !sawSSHOutput.Load() {
		t.Fatal("第二轮模型请求未收到 ssh_exec 的真实远端输出结果")
	}
	if !h.tt.HasText("这台主机配置概览") || !h.tt.HasText("SERVAL_AGENT_PROBE_OK") {
		t.Fatalf("未渲染 Markdown 表格报告内容: %q", h.tt.Texts())
	}
	if !h.tt.HasText("Token 470") {
		t.Fatalf("未显示累计 Token 消耗统计: %q", h.tt.Texts())
	}

	// 展开工具卡片应能看到真实输出
	h.click("$ printf SERVAL_AGENT_PROBE_OK")
	h.tt.Frame()
	if !h.tt.HasText("exit_code: 0") {
		t.Fatalf("展开工具卡片未显示真实输出: %q", h.tt.Texts())
	}

	// 验证 toggleAI 收起再打开保留会话内容
	h.a.toggleAI()
	h.tt.Frame()
	if h.a.ai.open {
		t.Fatal("toggleAI 应关闭侧栏")
	}
	h.a.toggleAI()
	h.tt.Frame()
	if !h.a.ai.open || len(h.a.ai.turns) != 1 {
		t.Fatal("重新打开侧栏应保留会话记录")
	}
}

// TestAIAgentPermissionModes 测试三档权限模式：只读观察（允许 SFTP/终端读取但拦截 ssh_exec）、确认执行（弹窗卡片审批）、自动执行（普通放行、高危拦截）。
func TestAIAgentPermissionModes(t *testing.T) {
	var phase atomic.Int32
	var toolOutputs []string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)

		// 从末尾收集最新 tool 消息文本
		msgs, _ := req["messages"].([]any)
		var lastToolText string
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if mm["role"] == "tool" {
				lastToolText, _ = mm["content"].(string)
			}
		}

		p := phase.Add(1)
		w.Header().Set("Content-Type", "application/json")
		var msg map[string]any
		switch p {
		case 1:
			// 只读观察模式下尝试执行 ssh_exec -> 应被阻止，且 sftp_read 应成功
			msg = map[string]any{
				"tool_calls": []map[string]any{
					{"id": "c_obs_read", "type": "function", "function": map[string]string{"name": "sftp_read", "arguments": `{"path":"go.mod"}`}},
					{"id": "c_obs_exec", "type": "function", "function": map[string]string{"name": "ssh_exec", "arguments": `{"command":"echo should_block"}`}},
				},
			}
		case 2:
			toolOutputs = append(toolOutputs, lastToolText)
			msg = map[string]any{"content": "只读探测结束"}
		case 3:
			// 确认执行模式：发起 ssh_exec，等待用户点击“允许执行”
			msg = map[string]any{
				"tool_calls": []map[string]any{
					{"id": "c_confirm_exec", "type": "function", "function": map[string]string{"name": "ssh_exec", "arguments": `{"command":"echo CONFIRM_PASS","reason":"验证确认模式"}`}},
				},
			}
		case 4:
			toolOutputs = append(toolOutputs, lastToolText)
			msg = map[string]any{"content": "确认模式执行结束"}
		case 5:
			// 自动执行模式：普通命令自动放行，高危 rm -rf 仍应拦截等待拒绝
			msg = map[string]any{
				"tool_calls": []map[string]any{
					{"id": "c_auto_danger", "type": "function", "function": map[string]string{"name": "ssh_exec", "arguments": `{"command":"rm -rf /tmp/serval-danger-test","reason":"高危拦截测试"}`}},
				},
			}
		case 6:
			toolOutputs = append(toolOutputs, lastToolText)
			msg = map[string]any{"content": "高危命令已拦截"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg}},
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "gpt-4o"
	h.a.setAIKey("test-sk")
	h.connectDev()

	// 1. 只读观察模式
	h.a.openAI()
	h.a.ai.mode = aiModeObserver
	h.a.ai.query = "observer check"
	h.a.runAIGenerate()
	h.pump("只读观察完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running
	})
	if len(toolOutputs) == 0 || !strings.Contains(toolOutputs[0], "只读观察") {
		t.Fatalf("只读观察模式应阻止 ssh_exec，实际=%v", toolOutputs)
	}
	if len(h.a.ai.turns[0].steps) == 0 || len(h.a.ai.turns[0].steps[0].Tools) != 2 || !strings.Contains(h.a.ai.turns[0].steps[0].Tools[0].Output, "module serval") {
		t.Fatalf("只读观察模式下 sftp_read 应成功读取文件内容")
	}

	// 2. 确认执行模式 -> 出现审批卡片 -> 点击允许执行
	h.a.ai.mode = aiModeConfirm
	h.a.ai.query = "confirm check"
	h.a.runAIGenerate()
	h.pump("出现命令执行确认卡片", func() bool {
		return h.a.ai.pendingApproval != nil && h.tt.HasText("允许执行")
	})
	h.click("允许执行")
	h.pump("确认执行完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 2 && !h.a.ai.turns[1].running
	})
	if !strings.Contains(toolOutputs[len(toolOutputs)-1], "CONFIRM_PASS") {
		t.Fatalf("确认执行放行后应收到真实 stdout，实际=%q", toolOutputs[len(toolOutputs)-1])
	}

	// 3. 自动执行模式下的高危命令仍拦截 -> 点击拒绝
	h.a.ai.mode = aiModeAuto
	h.a.ai.query = "danger check"
	h.a.runAIGenerate()
	h.pump("自动模式下高危命令触发拦截卡片", func() bool {
		return h.a.ai.pendingApproval != nil && h.tt.HasText("高危命令拦截确认")
	})
	h.click("拒绝")
	h.pump("高危命令拒绝后完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 3 && !h.a.ai.turns[2].running
	})
	if !strings.Contains(toolOutputs[len(toolOutputs)-1], "拒绝") {
		t.Fatalf("拒绝后应告知模型用户已拒绝，实际=%q", toolOutputs[len(toolOutputs)-1])
	}
}

// TestAIAgentComposerSlashMentionsAndProfiles 测试设置页多服务商独立保存、自动拉取模型、底部厂商模型切换、@ 引用上下文与 / 快捷命令。
func TestAIAgentComposerSlashMentionsAndProfiles(t *testing.T) {
	var seenModel string
	var seenUserPrompt string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer sk-ds-1" && auth != "Bearer sk-oai-1" && auth != "Bearer test-sk" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			if auth == "Bearer sk-oai-1" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": []map[string]string{
						{"id": "gpt-6.1-terra"},
						{"id": "gpt-5.4"},
						{"id": "gpt-5.4-mini"},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{
					{"id": "deepseek-chat"},
					{"id": "deepseek-reasoner"},
					{"id": "text-embedding-3-small"},
				},
			})
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		seenModel, _ = req["model"].(string)
		msgs, _ := req["messages"].([]any)
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if mm["role"] == "user" {
				seenUserPrompt, _ = mm["content"].(string)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.click("设置")
	h.tt.Frame()
	h.click("AI")
	h.tt.Frame()
	if !h.tt.HasText("尚未添加 AI 提供商") {
		t.Fatalf("未添加提供商时应展示引导提示，界面=%q", h.tt.Texts())
	}

	// 1. 添加 Ollama 后删除并关闭设置再打开：验证删除后绝不会复活
	h.click("+ 添加提供商")
	h.tt.Frame()
	h.click("Ollama")
	h.tt.Frame()
	if len(h.a.settings.AIProfiles) != 1 || h.a.settings.AIProfiles[0].ID != "ollama" {
		t.Fatalf("添加 Ollama 失败: %+v", h.a.settings.AIProfiles)
	}
	h.click("删除 Ollama")
	h.tt.Frame()
	h.click(locales.Done())
	h.tt.Frame()
	if len(h.a.settings.AIProfiles) != 0 {
		t.Fatalf("删除 Ollama 后关闭设置不应复活: %+v", h.a.settings.AIProfiles)
	}

	// 2. 重新打开设置，添加 DeepSeek 并从接口获取模型（不填任何硬编码旧模型）
	h.click("设置")
	h.tt.Frame()
	h.click("AI")
	h.tt.Frame()
	h.click("+ 添加提供商")
	h.tt.Frame()
	h.click("DeepSeek")
	h.tt.Frame()
	if h.a.settingsPanel.aiModel != "" {
		t.Fatalf("新建提供商不应预填硬编码默认模型，实际=%q", h.a.settingsPanel.aiModel)
	}
	h.a.settingsPanel.aiBaseURL = ts.URL
	h.a.settingsPanel.aiKey = "sk-ds-1"
	h.tt.Frame()
	h.click("获取模型")
	h.pump("获取 DeepSeek 模型列表", func() bool {
		return !h.a.settingsPanel.aiFetchBusy && len(h.a.settingsPanel.aiModels) == 2
	})
	if h.a.settingsPanel.aiModels[0] != "deepseek-chat" || h.a.settingsPanel.aiModels[1] != "deepseek-reasoner" {
		t.Fatalf("获取并过滤模型列表失败: %+v", h.a.settingsPanel.aiModels)
	}

	// 3. 添加 OpenAI：DeepSeek 保持活跃，且 OpenAI 不应残留 DeepSeek 的 Key
	h.click("+ 添加提供商")
	h.tt.Frame()
	h.click("OpenAI")
	h.tt.Frame()
	if h.a.settingsPanel.aiKey != "" {
		t.Fatalf("新增 OpenAI 时不应残留上一个服务商的 Key: %q", h.a.settingsPanel.aiKey)
	}
	if h.a.settings.ActiveAIProfile != "deepseek" {
		t.Fatalf("添加第二个服务商时原活跃服务商应保持活跃，实际=%q", h.a.settings.ActiveAIProfile)
	}
	h.a.settingsPanel.aiBaseURL = ts.URL
	h.a.settingsPanel.aiKey = "sk-oai-1"
	h.tt.Frame()
	h.click("获取模型")
	h.pump("获取 OpenAI 模型列表", func() bool {
		return !h.a.settingsPanel.aiFetchBusy && len(h.a.settingsPanel.aiModels) == 3
	})
	if h.a.settingsPanel.aiModel != "gpt-5.4" {
		t.Fatalf("应优先选用主流模型 gpt-5.4 而非 gpt-6.1-terra，实际=%q", h.a.settingsPanel.aiModel)
	}
	if h.tt.HasText("默认模型") {
		t.Fatal("已从接口自动获取模型后不应再展示混淆的「默认模型」下拉项")
	}

	// 验证支持多个提供商同时处于活跃状态，并测试关闭/重新启用 OpenAI 开关
	if len(h.a.configuredAIProfiles()) != 2 {
		t.Fatalf("DeepSeek 与 OpenAI 均配置后应同时活跃: %+v", h.a.configuredAIProfiles())
	}
	h.click("启用 OpenAI")
	h.tt.Frame()
	if act := h.a.configuredAIProfiles(); len(act) != 1 || act[0].ID != "deepseek" {
		t.Fatalf("关闭 OpenAI 开关后应仅剩 DeepSeek 活跃: %+v", act)
	}

	h.click(locales.Done())
	h.tt.Frame()
	if len(h.a.settings.AIProfiles) != 2 {
		t.Fatalf("期望保存 2 个提供商卡片，实际=%+v", h.a.settings.AIProfiles)
	}

	// 打开本地终端与 AI 侧栏：此时 OpenAI 已关闭，底部菜单不应出现 OpenAI
	h.click("新建")
	h.tt.Frame()
	h.click("本地终端")
	h.tt.Frame()
	h.a.openAI()
	h.tt.Frame()

	h.click("DeepSeek · deepseek-chat")
	h.tt.Frame()
	if h.tt.HasText("OpenAI") {
		t.Fatalf("未活跃的 OpenAI 不应出现在 AI 面板模型菜单中，界面=%q", h.tt.Texts())
	}
	h.tt.CloseMenu()
	h.tt.Frame()

	// 回到设置重新打开 OpenAI 开关，确认多活跃提供商同时可见
	h.click("设置")
	h.tt.Frame()
	h.click("AI")
	h.tt.Frame()
	h.click("启用 OpenAI")
	h.tt.Frame()
	if act := h.a.configuredAIProfiles(); len(act) != 2 {
		t.Fatalf("重新开启 OpenAI 后应两个提供商同时活跃: %+v", act)
	}
	h.click(locales.Done())
	h.tt.Frame()

	if !h.tt.HasText("DeepSeek · deepseek-chat") {
		t.Fatalf("底部模型切换栏应完整显示当前激活配置，界面=%q", h.tt.Texts())
	}
	btnBox, ok := h.tt.Find("DeepSeek · deepseek-chat")
	if !ok || btnBox.W < 130 {
		t.Fatalf("底部模型切换文字应完整展开不被截断: ok=%v box=%+v", ok, btnBox)
	}

	// 4. 在底部栏打开固定结构的厂商/模型菜单，通过子菜单切换同一厂商的不同模型
	h.click("DeepSeek · deepseek-chat")
	h.tt.Frame()
	if !h.tt.HasText("DeepSeek") || !h.tt.HasText("OpenAI") {
		t.Fatalf("多厂商时顶层菜单应保持固定一致的提供商列表，界面=%q", h.tt.Texts())
	}
	dsMenuBox, ok := h.tt.Find("DeepSeek")
	if !ok {
		t.Fatalf("未找到 DeepSeek 菜单项")
	}
	h.tt.Move(dsMenuBox.X+dsMenuBox.W/2, dsMenuBox.Y+dsMenuBox.H/2)
	h.tt.Frame()
	h.click("deepseek-reasoner")
	h.tt.Frame()
	if h.a.activeAIConfig().Model != "deepseek-reasoner" {
		t.Fatalf("切换模型失败: %+v", h.a.activeAIConfig())
	}
	reasonerBox, ok := h.tt.Find("DeepSeek · deepseek-reasoner")
	if !ok || reasonerBox.W <= 150 {
		t.Fatalf("切换后底部模型文字应完整显示 deepseek-reasoner: ok=%v box=%+v", ok, reasonerBox)
	}

	// 5. 在底部栏通过子菜单切换到另一提供商及模型，再切回 DeepSeek
	h.click("DeepSeek · deepseek-reasoner")
	h.tt.Frame()
	oaiMenuBox, ok := h.tt.Find("OpenAI")
	if !ok {
		t.Fatalf("未找到 OpenAI 菜单项")
	}
	h.tt.Move(oaiMenuBox.X+oaiMenuBox.W/2, oaiMenuBox.Y+oaiMenuBox.H/2)
	h.tt.Frame()
	if _, ok := h.tt.Find("gpt-5.4"); !ok {
		t.Fatalf("悬停 OpenAI 后未展开子菜单中的 gpt-5.4")
	}
	h.click("gpt-5.4")
	h.tt.Frame()
	if h.a.activeAIConfig().ID != "openai" || h.a.activeAIConfig().Model != "gpt-5.4" {
		t.Fatalf("底部栏切换提供商与模型失败: %+v", h.a.activeAIConfig())
	}
	h.click("OpenAI · gpt-5.4")
	h.tt.Frame()
	dsMenuBox, _ = h.tt.Find("DeepSeek")
	h.tt.Move(dsMenuBox.X+dsMenuBox.W/2, dsMenuBox.Y+dsMenuBox.H/2)
	h.tt.Frame()
	h.click("deepseek-reasoner")
	h.tt.Frame()

	// 测试 @ 引用上下文
	h.click("@")
	h.tt.Frame()
	h.click("@主机信息")
	h.tt.Frame()
	if len(h.a.ai.attachedContexts) != 1 || h.a.ai.attachedContexts[0] != "host" {
		t.Fatalf("点击 @主机信息 后应附加 host 上下文: %+v", h.a.ai.attachedContexts)
	}

	// 测试 / 快捷命令执行
	h.click("/")
	h.tt.Frame()
	h.click("/inspect")
	h.pump("斜杠命令执行完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running
	})
	if seenModel != "deepseek-reasoner" || !strings.Contains(seenUserPrompt, "[附加主机信息]") {
		t.Fatalf("期望使用切换后的 deepseek-reasoner 且附带主机信息，实际 model=%q prompt=%q", seenModel, seenUserPrompt)
	}

	// /clear 清空对话
	h.a.ai.query = "/clear"
	h.a.submitAIInput()
	h.tt.Frame()
	if len(h.a.ai.turns) != 0 {
		t.Fatalf("/clear 后应清空对话轮次: %d", len(h.a.ai.turns))
	}
}

// TestDedupAIProfiles 验证历史版本重复保存的同服务商配置会自动去重合并，保留激活项和密钥。
func TestDedupAIProfiles(t *testing.T) {
	raw := []aiProfile{
		{ID: "ai-1", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini", APIKey: "sk-oai"},
		{ID: "ai-2", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat", APIKey: "sk-ds-old"},
		{ID: "ai-3", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat", APIKey: ""},
		{ID: "ai-4", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-reasoner", APIKey: "sk-ds-new", Models: []string{"deepseek-chat", "deepseek-reasoner"}},
	}
	deduped, active := dedupAIProfiles(raw, "ai-4")
	if active != "deepseek" {
		t.Fatalf("active=%q, want deepseek", active)
	}
	if len(deduped) != 2 {
		t.Fatalf("去重后应剩 2 个服务商，实际=%+v", deduped)
	}
	if deduped[0].ID != "openai" || deduped[0].APIKey != "sk-oai" {
		t.Fatalf("OpenAI 记录异常: %+v", deduped[0])
	}
	if deduped[1].ID != "deepseek" || deduped[1].Model != "deepseek-reasoner" || deduped[1].APIKey != "sk-ds-new" || len(deduped[1].Models) != 2 {
		t.Fatalf("DeepSeek 合并记录异常: %+v", deduped[1])
	}
}

func TestGoogleAndMiniMaxProviderPresets(t *testing.T) {
	if got := providerIDFor("Google", ""); got != "google" {
		t.Fatalf("providerIDFor(Google) = %q, want google", got)
	}
	if got := providerIDFor("", "https://generativelanguage.googleapis.com/v1beta/openai"); got != "google" {
		t.Fatalf("providerIDFor(google url) = %q, want google", got)
	}
	if got := providerIDFor("MiniMax", ""); got != "minimax" {
		t.Fatalf("providerIDFor(MiniMax) = %q, want minimax", got)
	}
	for _, u := range []string{"https://api.minimaxi.com/v1", "https://api.minimax.io/v1"} {
		if got := providerIDFor("", u); got != "minimax" {
			t.Fatalf("providerIDFor(%q) = %q, want minimax", u, got)
		}
	}

	h := newHarness(t)
	h.a.openSettings()
	h.pump("打开设置", func() bool { return h.a.settingsPanel.open })
	h.click("AI")
	h.tt.Frame()

	for _, want := range []struct {
		name string
		id   string
		url  string
	}{
		{"Google", "google", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"MiniMax", "minimax", "https://api.minimaxi.com/v1"},
	} {
		h.click("+ 添加提供商")
		h.tt.Frame()
		h.click(want.name)
		h.tt.Frame()
		if h.a.settingsPanel.editingProfileID != want.id || h.a.settingsPanel.aiBaseURL != want.url {
			t.Fatalf("选择 %s 预设失败: id=%q url=%q", want.name, h.a.settingsPanel.editingProfileID, h.a.settingsPanel.aiBaseURL)
		}
	}
}

func TestAISessionIsolatedPerTab(t *testing.T) {
	req1Started := make(chan struct{})
	req1Release := make(chan struct{})
	var reqCount atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := reqCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			close(req1Started)
			select {
			case <-req1Release:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]string{"content": `{"explanation":"tab1 result","command":"uptime"}`}},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": `{"explanation":"tab2 result","command":"whoami"}`}},
			},
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "gpt-4o"
	h.a.setAIKey("test-sk")

	tab1 := h.connectDev()
	h.a.openLocalTerminal()
	if len(h.a.tabs) != 2 {
		t.Fatalf("期望打开 2 个标签页，实际=%d", len(h.a.tabs))
	}
	tab2 := h.a.tabs[1]

	// 切回标签 1 打开 AI 与 SFTP 抽屉，并发起异步 AI 请求
	h.a.active = 0
	h.a.openAI()
	h.a.toggleFiles()
	h.tt.Frame()
	if !h.a.ai.open || !h.a.filesOpen {
		t.Fatalf("标签 1 应同时展开 AI 与 SFTP: ai=%v files=%v", h.a.ai.open, h.a.filesOpen)
	}
	h.a.ai.query = "question from tab1"
	h.a.runAIGenerate()
	select {
	case <-req1Started:
	case <-time.After(5 * time.Second):
		t.Fatal("标签 1 AI 请求未启动")
	}

	// 新建或切换到标签 2：抽屉开关与会话均独立，默认关闭且不受标签 1 影响
	h.a.active = 1
	h.tt.Frame()
	if h.a.ai.open || h.a.filesOpen || len(h.a.ai.turns) != 0 || h.a.ai.loading {
		t.Fatalf("切换到标签 2 后抽屉开关和会话都应独立: aiOpen=%v filesOpen=%v loading=%v turns=%+v", h.a.ai.open, h.a.filesOpen, h.a.ai.loading, h.a.ai.turns)
	}

	// 标签 1 后台请求在切走期间完成，结果必须只落入标签 1 的会话而不污染标签 2
	close(req1Release)
	h.pump("标签 1 后台会话完成", func() bool {
		s := h.a.aiSessionForTab(tab1.id)
		return !s.loading && len(s.turns) == 1 && !s.turns[0].running
	})
	if h.a.ai.open || len(h.a.ai.turns) != 0 || h.a.ai.command != "" {
		t.Fatalf("标签 1 后台完成不应污染标签 2 当前界面: open=%v turns=%+v cmd=%q", h.a.ai.open, h.a.ai.turns, h.a.ai.command)
	}

	// 在标签 2 自己打开 AI 并发起一轮对话
	h.a.openAI()
	h.a.ai.query = "question from tab2"
	h.a.runAIGenerate()
	h.pump("标签 2 会话完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running
	})
	if h.a.ai.turns[0].query != "question from tab2" || h.a.ai.command != "whoami" {
		t.Fatalf("标签 2 对话内容异常: query=%q cmd=%q", h.a.ai.turns[0].query, h.a.ai.command)
	}

	// 关闭标签 2 的 AI 抽屉，切回标签 1 后标签 1 的 AI、SFTP 与对话依然保持打开
	h.a.toggleAI()
	h.tt.Frame()
	if h.a.ai.open {
		t.Fatal("关闭标签 2 的 AI 抽屉后应为关闭状态")
	}

	h.a.active = 0
	h.tt.Frame()
	if !h.a.ai.open || !h.a.filesOpen || len(h.a.ai.turns) != 1 || h.a.ai.turns[0].query != "question from tab1" || h.a.ai.command != "uptime" {
		t.Fatalf("切回标签 1 未恢复标签 1 的抽屉和独立会话: aiOpen=%v filesOpen=%v turns=%+v cmd=%q", h.a.ai.open, h.a.filesOpen, h.a.ai.turns, h.a.ai.command)
	}

	// 验证再新开一个 SSH 标签页时，不会继承标签 1 已打开的 SFTP / AI 抽屉
	h.a.connect(hosts.Host{ID: "dev2", Name: "dev2", Address: "127.0.0.1", Port: h.server.Addr().Port, User: "tester", Password: testPassword}, nil)
	h.pump("连接第三个 SSH 标签页", func() bool { return len(h.a.tabs) == 3 })
	tab3 := h.a.tabs[2]
	h.tt.Frame()
	if h.a.current() != tab3 || h.a.ai.open || h.a.filesOpen {
		t.Fatalf("新开的 SSH 标签页不应继承旧标签的 AI/SFTP 展开状态: aiOpen=%v filesOpen=%v", h.a.ai.open, h.a.filesOpen)
	}
	h.a.closeTab(tab3)
	h.a.active = 0
	h.tt.Frame()

	h.a.closeTab(tab1)
	h.tt.Frame()
	if h.a.current() != tab2 {
		t.Fatalf("关闭标签 1 后应切到标签 2")
	}
	if h.a.ai.open || len(h.a.ai.turns) != 1 || h.a.ai.turns[0].query != "question from tab2" || h.a.ai.command != "whoami" {
		t.Fatalf("关闭标签 1 不应破坏标签 2 的 AI 状态: aiOpen=%v turns=%+v cmd=%q", h.a.ai.open, h.a.ai.turns, h.a.ai.command)
	}
}

func TestAIPerTabConversationHistoryAndNewChat(t *testing.T) {
	ts := mockAIServer(t)
	defer ts.Close()

	h := newHarness(t)
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "gpt-4o"
	h.a.setAIKey("test-sk")

	h.connectDev()
	h.a.openLocalTerminal()
	h.a.active = 0
	h.a.openAI()
	h.tt.Frame()

	// 第一条会话
	h.a.ai.query = "第一条历史提问"
	h.a.runAIGenerate()
	h.pump("第一条会话完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running
	})

	// 点击“新开会话”按钮后当前进入空白会话，且第一条已存入本终端历史
	h.click("新开会话")
	h.tt.Frame()
	if len(h.a.ai.turns) != 0 || len(h.a.ai.convs) != 1 {
		t.Fatalf("新开会话后当前应空白且保留 1 条历史: turns=%d convs=%d", len(h.a.ai.turns), len(h.a.ai.convs))
	}

	// 第二条会话
	h.a.ai.query = "第二条历史提问"
	h.a.runAIGenerate()
	h.pump("第二条会话完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running
	})

	// 打开历史会话列表应看到 2 条属于当前终端的历史
	h.click("历史会话")
	h.tt.Frame()
	if !h.tt.HasText("本终端历史会话（2）") || !h.tt.HasText("第一条历史提问") || !h.tt.HasText("第二条历史提问") {
		t.Fatalf("历史会话列表未正确显示两条会话: texts=%q", h.tt.Texts())
	}

	// 切换到标签 2：其历史会话应为空，完全隔离
	h.a.active = 1
	h.a.openAI()
	h.tt.Frame()
	h.click("历史会话")
	h.tt.Frame()
	if !h.tt.HasText("本终端历史会话（0）") {
		t.Fatalf("标签 2 不应看到标签 1 的历史会话: texts=%q", h.tt.Texts())
	}

	// 切回标签 1，点击第一条历史会话恢复查看，再删除第二条会话
	h.a.active = 0
	h.tt.Frame()
	h.click("打开会话 第一条历史提问")
	h.tt.Frame()
	if h.a.ai.showHistory || len(h.a.ai.turns) != 1 || h.a.ai.turns[0].query != "第一条历史提问" {
		t.Fatalf("未能切回第一条历史会话: showHistory=%v turns=%+v", h.a.ai.showHistory, h.a.ai.turns)
	}

	h.click("历史会话")
	h.tt.Frame()
	secondID := ""
	for _, c := range h.a.ai.convs {
		if c.Title == "第二条历史提问" {
			secondID = c.ID
		}
	}
	if secondID == "" {
		t.Fatal("找不到第二条历史会话 ID")
	}
	h.click("删除会话 " + secondID)
	h.tt.Frame()
	if len(h.a.ai.convs) != 1 || !h.tt.HasText("本终端历史会话（1）") {
		t.Fatalf("删除历史会话后应剩 1 条: convs=%d texts=%q", len(h.a.ai.convs), h.tt.Texts())
	}
}

func TestAIMultilineComposerAndMentionToken(t *testing.T) {
	if _, _, ok := aiMentionToken("user@example.com"); ok {
		t.Fatal("邮箱中间的 @ 不应触发上下文引用浮层")
	}
	if start, needle, ok := aiMentionToken("第一行\n请帮我看下 @sf"); !ok || start != len("第一行\n请帮我看下 ") || needle != "sf" {
		t.Fatalf("多行文本中的 @ 匹配异常: start=%d needle=%q ok=%v", start, needle, ok)
	}

	ts := mockAIServer(t)
	defer ts.Close()

	h := newHarness(t)
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "gpt-4o"
	h.a.setAIKey("test-sk")
	h.connectDev()

	h.a.openAI()
	h.tt.Frame()

	// Shift+Enter 插入换行而不提交，plain Enter 提交
	h.tt.Type("第一行")
	h.tt.Key(ui.Shift, ui.KeyEnter)
	h.tt.Type("第二行")
	h.tt.Frame()
	if !strings.Contains(h.a.ai.query, "\n") || len(h.a.ai.turns) != 0 {
		t.Fatalf("Shift+Enter 应插入换行且不触发发送: query=%q turns=%d", h.a.ai.query, len(h.a.ai.turns))
	}
	h.tt.Key(0, ui.KeyEnter)
	h.pump("多行输入 Enter 发送完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running
	})
	if h.a.ai.turns[0].query != "第一行\n第二行" {
		t.Fatalf("发送内容应保留换行: %q", h.a.ai.turns[0].query)
	}
}

func TestAIInPlaceRetryCopyAnswerAndNonShellCodeBlock(t *testing.T) {
	var failFirst atomic.Bool
	failFirst.Store(true)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if failFirst.Swap(false) {
			http.Error(w, `{"error":{"message":"temporary upstream error"}}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "配置示例：\n```json\n{\"port\": 8080}\n```"}},
			},
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "gpt-4o"
	h.a.setAIKey("test-sk")
	h.connectDev()

	h.a.openAI()
	h.tt.Frame()
	h.a.ai.query = "看看配置"
	h.a.runAIGenerate()
	h.pump("首次失败完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && h.a.ai.turns[0].err != ""
	})

	// 点击重试应原地复用第 1 轮，而不是追加重复气泡
	h.click("重试")
	h.pump("原地重试成功", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running && h.a.ai.turns[0].err == ""
	})
	if len(h.a.ai.turns) != 1 {
		t.Fatalf("原地重试不应产生重复气泡: turns=%d", len(h.a.ai.turns))
	}
	// 非 shell 代码块应显示“复制代码”而不显示“填入终端（需确认）”
	if !h.tt.HasText("复制代码") || h.tt.HasText("填入终端（需确认）") {
		t.Fatalf("json 代码块不应出现填入终端按钮: texts=%q", h.tt.Texts())
	}
	if !h.tt.HasText("复制回答") {
		t.Fatalf("完成后应显示复制回答按钮: texts=%q", h.tt.Texts())
	}
}

func TestAIEmptyStateMigrationAndPersistence(t *testing.T) {
	h := newHarness(t)
	// 空状态打开 AI 并输入内容
	h.a.openAI()
	h.tt.Frame()
	h.a.ai.query = "待迁移的问题"
	h.a.ai.mode = aiModeConfirm
	_ = h.a.saveLocal(deviceSettings{AIWidth: 510, AIMode: uint8(aiModeConfirm)})

	// 打开首个本地终端标签页，空状态会话应自动迁移到该标签页
	h.a.openLocalTerminal()
	h.tt.Frame()
	if !h.a.ai.open || h.a.ai.query != "待迁移的问题" {
		t.Fatalf("空状态 AI 会话未迁移到新打开的标签页: open=%v query=%q", h.a.ai.open, h.a.ai.query)
	}

	// 验证 loadLocal 能恢复 AIWidth 和 AIMode
	h.a.aiWidth = 0
	h.a.ai.mode = aiModeAuto
	h.a.loadLocal()
	if h.a.aiWidth != 510 || h.a.ai.mode != aiModeConfirm {
		t.Fatalf("loadLocal 应恢复 aiWidth 和 ai.mode: width=%v mode=%v", h.a.aiWidth, h.a.ai.mode)
	}
}

func TestAIProductionApprovalAndHistoryCompression(t *testing.T) {
	// 验证历史工具输出压缩
	longOut := strings.Repeat("log-line-", 120)
	msgs := []ai.Message{
		{Role: "user", Content: "q1"},
		{Role: "tool", Content: longOut},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "q3"},
		{Role: "assistant", Content: "a3"},
		{Role: "tool", Content: longOut},
		{Role: "assistant", Content: "a4"},
	}
	comp := compressChatHistory(msgs)
	if !strings.Contains(comp[1].Content, "早期工具输出已摘要折叠") {
		t.Fatalf("早期 tool 消息应被摘要折叠: %q", comp[1].Content)
	}
	if comp[7].Content != longOut {
		t.Fatalf("最近 6 条内的 tool 消息应保持完整")
	}

	// 验证生产环境自动执行模式下：只读命令放行、变更命令拦截确认
	h := newHarness(t)
	h.connectDev()
	tab := h.a.current()
	prodHost := tab.host
	prodHost.Production = true
	_ = h.a.apply(hostChange(prodHost))
	tab.host.Production = true
	h.a.openAI()
	h.tt.Frame()
	snap := snapshotFromTab(tab)
	if !snap.production || ai.IsReadOnlyProbeCommand("touch /tmp/serval_prod_test") {
		t.Fatalf("前置条件异常: snap.production=%v IsReadOnly=%v", snap.production, ai.IsReadOnlyProbeCommand("touch /tmp/serval_prod_test"))
	}

	// 只读探测命令（uname -s）在生产环境自动模式下无需人工弹窗确认即可执行
	roDone := make(chan struct {
		out   string
		isErr bool
	}, 1)
	go func() {
		o, e := h.a.executeAgentTool(context.Background(), h.a.ai.convID, h.a.ai.reqSeq, snap, aiModeAuto, nil, ai.ToolCall{
			ID:       "t1",
			Function: ai.ToolCallFunction{Name: "ssh_exec", Arguments: `{"command":"uname -s"}`},
		})
		roDone <- struct {
			out   string
			isErr bool
		}{o, e}
	}()
	var roRes struct {
		out   string
		isErr bool
	}
	h.pump("等待只读命令执行完成", func() bool {
		select {
		case roRes = <-roDone:
			return true
		default:
			return false
		}
	})
	if roRes.isErr || !strings.Contains(roRes.out, "exit_code: 0") {
		t.Fatalf("生产环境只读探测命令应自动放行: isErr=%v out=%q", roRes.isErr, roRes.out)
	}
	if h.a.ai.pendingApproval != nil {
		t.Fatalf("只读探测命令不应触发确认卡片")
	}

	// 非只读变更命令在生产环境自动模式下必须触发确认卡片，拒绝后返回用户拒绝
	done := make(chan string, 1)
	go func() {
		o, _ := h.a.executeAgentTool(context.Background(), h.a.ai.convID, h.a.ai.reqSeq, snap, aiModeAuto, nil, ai.ToolCall{
			ID:       "t2",
			Function: ai.ToolCallFunction{Name: "ssh_exec", Arguments: `{"command":"touch /tmp/serval_prod_test","reason":"测试变更"}`},
		})
		done <- o
	}()
	earlyOut := ""
	h.pump("等待生产环境确认卡片出现", func() bool {
		select {
		case earlyOut = <-done:
			return true
		default:
			return h.a.ai.pendingApproval != nil
		}
	})
	if earlyOut != "" {
		t.Fatalf("生产环境变更命令未触发确认卡片而是直接返回: %q", earlyOut)
	}
	if !h.a.ai.pendingApproval.production || !h.tt.HasText("生产环境命令确认") {
		t.Fatalf("确认卡片应标记并渲染为生产环境确认，texts=%q", h.tt.Texts())
	}
	h.a.resolveAgentApproval(h.a.ai.pendingApproval, false)
	if got := <-done; !strings.Contains(got, "用户拒绝") {
		t.Fatalf("拒绝后应返回拒绝提示: %q", got)
	}
}

// TestAISlashAndMentionRowsLeftAligned 验证 / 快捷指令与 @ 上下文引用弹窗中的每一行均为左对齐，不随描述长度居中偏移。
func TestAISlashAndMentionRowsLeftAligned(t *testing.T) {
	h := newHarness(t)
	h.tt.Frame()

	h.a.openAI()
	h.a.ai.showSlash = true
	h.tt.Frame()

	inspectCmd, ok := h.tt.Find("/inspect")
	if !ok {
		t.Fatalf("未找到 /inspect 快捷指令: %q", h.tt.Texts())
	}
	newCmd, ok := h.tt.Find("/new")
	if !ok {
		t.Fatalf("未找到 /new 快捷指令: %q", h.tt.Texts())
	}
	if abs := inspectCmd.X - newCmd.X; abs < -1 || abs > 1 {
		t.Fatalf("快捷指令行未左对齐: /inspect.X=%.1f, /new.X=%.1f", inspectCmd.X, newCmd.X)
	}

	h.a.ai.showSlash = false
	h.a.ai.showMentions = true
	h.tt.Frame()

	if len(aiMentionOptions) >= 2 {
		m0, ok0 := h.tt.Find("@" + aiMentionOptions[0].Label)
		m1, ok1 := h.tt.Find("@" + aiMentionOptions[1].Label)
		if !ok0 || !ok1 {
			t.Fatalf("未找到 @ 引用选项: %q", h.tt.Texts())
		}
		if abs := m0.X - m1.X; abs < -1 || abs > 1 {
			t.Fatalf("@ 引用行未左对齐: %s.X=%.1f, %s.X=%.1f", aiMentionOptions[0].Label, m0.X, aiMentionOptions[1].Label, m1.X)
		}
	}
}

func TestFriendlyAIErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "超时"},
		{"ai_timeout", ai.ErrTimeout, "超时"},
		{"stream_interrupted", ai.ErrStreamInterrupted, "连接中途断开"},
		{"invalid_response", ai.ErrInvalidResponse, "无法解析的数据格式"},
		{"api_400_detail", &ai.APIError{StatusCode: 400, Message: "Function call is missing a thought_signature"}, "Function call is missing a thought_signature"},
		{"dns", &net.DNSError{Err: "no such host", Name: "api.example.com"}, "域名"},
		{"refused", syscall.ECONNREFUSED, "拒绝连接"},
	}
	for _, tc := range cases {
		got := friendlyAIError(tc.err)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: friendlyAIError(%v) = %q, want substring %q", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestAIAgentApprovalCanceledResetsToolStatus(t *testing.T) {
	h := newHarness(t)
	h.connectDev()
	h.a.openAI()
	h.tt.Frame()

	snap := snapshotFromTab(h.a.current())
	toolRec := &aiToolRecord{
		ID:      "t-cancel",
		Name:    "ssh_exec",
		Summary: "$ touch /tmp/x",
		Status:  toolRunning,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		ok, _ := h.a.requestAgentApproval(ctx, snap.tabID, h.a.ai.convID, h.a.ai.reqSeq, toolRec, "touch /tmp/x", "测试取消", "dev", false, true)
		done <- ok
	}()

	h.pump("等待审批卡片出现", func() bool {
		return h.a.ai.pendingApproval != nil && toolRec.Status == toolWaitingApproval
	})

	cancel()
	<-done
	h.pump("等待取消后状态重置", func() bool {
		return h.a.ai.pendingApproval == nil && toolRec.Status != toolWaitingApproval
	})
	if toolRec.Status == toolWaitingApproval {
		t.Fatalf("取消后 toolRec.Status 不应停留在待确认")
	}
}

func TestFormatExecResultUTF8AndNullSanitization(t *testing.T) {
	longChinese := strings.Repeat("中文日志测试输出", 1000) + "\x00\xff\xfe"
	out := formatExecResult(sshconn.ExecResult{
		ExitCode: 0,
		Stdout:   longChinese,
	})
	if strings.ContainsRune(out, '\x00') {
		t.Fatal("formatExecResult 不应保留 NUL 字节")
	}
	if !utf8.ValidString(out) {
		t.Fatal("formatExecResult 截断后必须保持合法 UTF-8")
	}
}

func TestAIConversationHistoryPersistsAcrossRestart(t *testing.T) {
	var sawPriorInFollowUp atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		msgs, _ := req["messages"].([]any)
		var lastUser string
		var sawDiskQuestion bool
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if mm["role"] == "user" {
				ct, _ := mm["content"].(string)
				lastUser = ct
				if strings.Contains(ct, "检查磁盘空间") {
					sawDiskQuestion = true
				}
			}
		}
		if strings.Contains(lastUser, "继续") && sawDiskQuestion {
			sawPriorInFollowUp.Store(true)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "排查结果正常。\n```bash\ndf -h\n```"}},
			},
		})
	}))
	defer ts.Close()

	h := newHarness(t)
	h.a.settings.AIBaseURL = ts.URL
	h.a.settings.AIModel = "gpt-4o"
	h.a.setAIKey("test-sk")
	h.connectDev()

	h.a.openAI()
	h.tt.Frame()

	// 1. 第一条会话 + 重命名
	h.a.ai.query = "检查磁盘空间"
	h.a.runAIGenerate()
	h.pump("第一条会话完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running
	})
	firstID := h.a.ai.convID
	if firstID == "" {
		t.Fatal("第一条会话应生成持久化 convID")
	}
	h.click("历史会话")
	h.tt.Frame()
	h.click("重命名会话 " + firstID)
	h.tt.Frame()
	h.a.ai.renameConvDraft = "磁盘排查记录"
	h.click("保存")
	h.tt.Frame()

	// 2. 新开会话并发送第二条会话
	h.click("新建会话")
	h.tt.Frame()
	h.a.ai.query = "查看监听端口"
	h.a.runAIGenerate()
	h.pump("第二条会话完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 1 && !h.a.ai.turns[0].running
	})

	// 3. 验证写入磁盘 vault.json 且密文不包含明文历史
	if h.vaultFileContains("检查磁盘空间") || h.vaultFileContains("磁盘排查记录") {
		t.Fatal("AI 对话历史不应以明文出现在 vault.json 中")
	}

	// 4. 模拟关闭程序并重启（reopen 会重新打开 vault.json 并清零内存状态）
	h.reopen()
	if err := h.a.vault.Unlock(masterPassword); err != nil {
		t.Fatal(err)
	}
	h.a.reload()
	h.tt.Frame()
	if len(h.a.tabs) == 0 {
		h.a.connect(h.host("dev"), nil)
	}
	h.pump("等待恢复 dev 会话", func() bool {
		return h.a.current() != nil && h.a.current().client != nil
	})

	h.a.openAI()
	h.tt.Frame()
	if !h.tt.HasText("最近会话") || !h.tt.HasText("磁盘排查记录") {
		t.Fatalf("重启后欢迎页应显示最近会话与重命名标题，texts=%q", h.tt.Texts())
	}

	h.click("历史会话")
	h.tt.Frame()
	if !h.tt.HasText("本终端历史会话（2）") || !h.tt.HasText("磁盘排查记录") || !h.tt.HasText("查看监听端口") {
		t.Fatalf("重启后历史列表应恢复 2 条会话，texts=%q", h.tt.Texts())
	}

	// 5. 打开重命名的历史会话并继续追问，验证上下文消息已恢复
	h.click("打开会话 磁盘排查记录")
	h.tt.Frame()
	if len(h.a.ai.turns) != 1 || h.a.ai.turns[0].query != "检查磁盘空间" || h.a.ai.command != "df -h" {
		t.Fatalf("未能完整恢复历史会话内容: turns=%+v cmd=%q", h.a.ai.turns, h.a.ai.command)
	}

	h.a.ai.query = "继续"
	h.a.runAIGenerate()
	h.pump("恢复历史后追问完成", func() bool {
		return !h.a.ai.loading && len(h.a.ai.turns) == 2 && !h.a.ai.turns[1].running
	})
	if !sawPriorInFollowUp.Load() {
		t.Fatal("重启恢复的历史会话在追问时未带上先前的对话上下文")
	}

	// 6. 新开本地终端，其历史与 dev 主机隔离
	h.a.openLocalTerminal()
	h.a.openAI()
	h.tt.Frame()
	h.click("历史会话")
	h.tt.Frame()
	if !h.tt.HasText("本终端历史会话（0）") {
		t.Fatalf("本地终端不应混入 dev 主机的历史会话: texts=%q", h.tt.Texts())
	}

	// 7. 切回 dev 删除第一条历史后再次重启，验证墓碑删除持久化
	h.a.active = 0
	h.tt.Frame()
	h.click("历史会话")
	h.tt.Frame()
	h.click("删除会话 " + firstID)
	h.tt.Frame()

	h.reopen()
	if err := h.a.vault.Unlock(masterPassword); err != nil {
		t.Fatal(err)
	}
	h.a.reload()
	h.tt.Frame()
	if len(h.a.tabs) == 0 {
		h.a.connect(h.host("dev"), nil)
	}
	h.pump("等待恢复 dev 会话", func() bool {
		return h.a.current() != nil && h.a.current().client != nil
	})
	h.a.openAI()
	h.tt.Frame()
	h.click("历史会话")
	h.tt.Frame()
	if !h.tt.HasText("本终端历史会话（1）") || h.tt.HasText("磁盘排查记录") || !h.tt.HasText("查看监听端口") {
		t.Fatalf("删除历史并重启后应仅剩 1 条会话，texts=%q", h.tt.Texts())
	}

	// 8. 锁定保险库应清空内存中的历史记录
	h.a.lockVault()
	if len(h.a.aiChats) != 0 || len(h.a.ai.convs) != 0 {
		t.Fatalf("lockVault 后应清空内存中的 AI 历史: aiChats=%d convs=%d", len(h.a.aiChats), len(h.a.ai.convs))
	}
}
