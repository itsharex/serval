package main

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"serval/core/ai"
	"serval/design"
	"serval/locales"

	"github.com/egoist/mygo/ui"
)

type aiToolStatus uint8

const (
	toolRunning aiToolStatus = iota
	toolWaitingApproval
	toolDone
	toolError
	toolDenied
)

type aiToolRecord struct {
	ID       string
	Name     string
	Summary  string
	Args     string
	Output   string
	Status   aiToolStatus
	Expanded bool
}

type aiStepRecord struct {
	Step            int
	Thinking        string
	ThinkingSeconds int
	ThinkExpanded   bool
	Content         string
	Tools           []*aiToolRecord
}

type aiTurn struct {
	query       string
	contextTags []string
	steps       []*aiStepRecord
	usage       ai.Usage
	running     bool
	err         string

	// 兼容字段：从最终回答同步提取的主解释与可执行命令块
	explanation string
	commands    []string
}

type aiApproval struct {
	seq        uint64
	tool       *aiToolRecord
	command    string
	reason     string
	target     string
	dangerous  bool
	production bool
	answer     chan bool
}

// aiConversation 表示某个终端标签页内的一条独立历史/当前 AI 对话会话。
type aiConversation struct {
	ID              string
	Title           string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Turns           []aiTurn
	ChatHistory     []ai.Message
	Explanation     string
	Command         string
	Commands        []string
	Loading         bool
	Cancel          context.CancelFunc
	ReqSeq          uint64
	Err             string
	ContextError    string
	Attached        []string
	PendingApproval *aiApproval
	ScrollY         float32
}

// aiPanel 是右侧停靠的 AI Agent 侧栏状态（每个标签页各自拥有独立的一份）。
type aiPanel struct {
	open         bool
	fresh        bool
	query        string
	loading      bool
	cancel       context.CancelFunc
	reqSeq       uint64
	mode         aiPermissionMode
	explanation  string
	command      string
	commands     []string
	turns        []aiTurn
	chatHistory  []ai.Message
	err          string
	contextError string
	targetOS     string

	// 会话管理：每个终端可保存多条历史对话并自由新开或切换
	convID      string
	convTitle   string
	convCreated time.Time
	convs       []*aiConversation
	showHistory bool
	convSeq     int

	// @ 上下文引用与 / 快捷命令浮层状态
	attachedContexts []string // "terminal", "host", "clipboard"
	showMentions     bool
	showSlash        bool

	// 等待人工审批的工具执行请求
	pendingApproval *aiApproval

	// 滚动状态
	scroll     ui.ScrollState
	autoScroll bool

	// confirmDanger 用于代码块点击“填入终端”触发高危命令时的二次确认框
	confirmDanger bool
	pendingCmd    string
}

func (s *aiPanel) convByID(id string) *aiConversation {
	if s == nil || id == "" {
		return nil
	}
	for _, c := range s.convs {
		if c != nil && c.ID == id {
			return c
		}
	}
	return nil
}

type aiSlashCommand struct {
	Cmd     string
	Title   string
	Desc    string
	Prompt  string
	Attach  []string
	IsClear bool
	IsNew   bool
}

var aiSlashCommands = []aiSlashCommand{
	{
		Cmd:    "/inspect",
		Title:  "服务器配置体检",
		Desc:   "全面检查系统、CPU、内存、磁盘、网络与监听端口",
		Prompt: "这台服务器配置和当前运行状态如何？请主动检查系统内核、CPU、内存、磁盘、网络接口、关键进程与监听端口并汇总成表格。",
		Attach: []string{"host"},
	},
	{
		Cmd:    "/diagnose",
		Title:  "终端报错诊断",
		Desc:   "结合终端最近输出分析报错根因与排查修复方案",
		Prompt: "请读取当前终端最近的输出和报错信息，分析根因并给出排查与修复建议。",
		Attach: []string{"terminal", "host"},
	},
	{
		Cmd:    "/ports",
		Title:  "端口与进程排查",
		Desc:   "查看监听端口、对应服务进程与网络连接状态",
		Prompt: "请检查当前服务器正在监听的端口和主要进程占用情况，列出端口、协议与对应服务。",
		Attach: []string{"host"},
	},
	{
		Cmd:    "/logs",
		Title:  "排查近期异常日志",
		Desc:   "检查系统日志与常见服务异常记录",
		Prompt: "请检查这台主机最近的系统与服务错误日志，定位是否有明显异常或告警。",
		Attach: []string{"host"},
	},
	{
		Cmd:   "/new",
		Title: "新开会话",
		Desc:  "保存当前对话到历史并开启新会话",
		IsNew: true,
	},
	{
		Cmd:     "/clear",
		Title:   "清空当前对话",
		Desc:    "重置当前 AI Agent 会话历史与上下文",
		IsClear: true,
	},
}

type aiMentionOption struct {
	ID    string
	Label string
	Desc  string
}

var aiMentionOptions = []aiMentionOption{
	{ID: "terminal", Label: "终端输出", Desc: "附加当前活动终端最近 80 行内容"},
	{ID: "host", Label: "主机信息", Desc: "附加当前主机名、系统、登录用户与工作目录"},
	{ID: "clipboard", Label: "最近复制", Desc: "附加最近一次复制的终端文本"},
}

func (a *app) activeTargetOS() string {
	t := a.current()
	if t == nil {
		return ""
	}
	if t.local {
		return localOSName()
	}
	return t.host.Distro
}

func (a *app) currentAITabID() int {
	if t := a.current(); t != nil && t.id > 0 {
		return t.id
	}
	return 0
}

// syncAITab 在切换标签页后把当前 a.ai 状态保存回旧标签，并切出新标签独立的 AI 会话与抽屉开关状态。
func (a *app) syncAITab() {
	curID := a.currentAITabID()
	if curID == a.aiActiveTabID {
		return
	}
	if a.aiSessions == nil {
		a.aiSessions = map[int]*aiPanel{}
	}
	saveActiveConversation(&a.ai)
	prev := a.ai
	a.aiSessions[a.aiActiveTabID] = &prev

	mode := a.ai.mode
	if next, ok := a.aiSessions[curID]; ok && next != nil {
		a.ai = *next
	} else {
		a.ai = aiPanel{
			mode:     mode,
			targetOS: a.activeTargetOS(),
		}
	}
	a.aiActiveTabID = curID
}

// aiSessionForTab 返回指定标签页对应的 AI 会话状态指针，后台协程更新仅作用于发起该轮对话的标签页。
func (a *app) aiSessionForTab(tabID int) *aiPanel {
	a.syncAITab()
	if tabID == a.aiActiveTabID {
		return &a.ai
	}
	if a.aiSessions == nil {
		a.aiSessions = map[int]*aiPanel{}
	}
	if s, ok := a.aiSessions[tabID]; ok && s != nil {
		return s
	}
	s := &aiPanel{}
	a.aiSessions[tabID] = s
	return s
}

func stopAIConversation(c *aiConversation) {
	if c == nil {
		return
	}
	c.ReqSeq++
	if c.Cancel != nil {
		c.Cancel()
		c.Cancel = nil
	}
	if c.PendingApproval != nil {
		select {
		case c.PendingApproval.answer <- false:
		default:
		}
		c.PendingApproval = nil
	}
	c.Loading = false
	for i := range c.Turns {
		c.Turns[i].running = false
	}
}

func stopAISession(s *aiPanel) {
	if s == nil {
		return
	}
	s.reqSeq++
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.pendingApproval != nil {
		select {
		case s.pendingApproval.answer <- false:
		default:
		}
		s.pendingApproval = nil
	}
	s.loading = false
	for i := range s.turns {
		s.turns[i].running = false
	}
	for _, c := range s.convs {
		stopAIConversation(c)
	}
}

func (a *app) closeAISessionForTab(tabID int) {
	if tabID <= 0 {
		return
	}
	if tabID == a.aiActiveTabID {
		stopAISession(&a.ai)
		mode := a.ai.mode
		a.ai = aiPanel{mode: mode}
		a.aiActiveTabID = 0
	}
	if s, ok := a.aiSessions[tabID]; ok {
		stopAISession(s)
		delete(a.aiSessions, tabID)
	}
}

func (a *app) stopAllAISessions() {
	stopAISession(&a.ai)
	for id, s := range a.aiSessions {
		stopAISession(s)
		delete(a.aiSessions, id)
	}
	a.aiSessions = nil
	a.aiActiveTabID = 0
	a.ai = aiPanel{}
}

func convTitleFromTurns(turns []aiTurn) string {
	for _, t := range turns {
		q := strings.TrimSpace(strings.ReplaceAll(t.query, "\n", " "))
		if q == "" {
			continue
		}
		r := []rune(q)
		if len(r) > 36 {
			return string(r[:36]) + "…"
		}
		return q
	}
	return "新会话"
}

func saveActiveConversation(s *aiPanel) {
	if s == nil {
		return
	}
	if len(s.turns) == 0 && !s.loading && s.convID == "" {
		return
	}
	now := time.Now()
	if s.convID == "" {
		s.convSeq++
		s.convID = fmt.Sprintf("conv-%d", s.convSeq)
		s.convCreated = now
	}
	if s.convTitle == "" && len(s.turns) > 0 {
		s.convTitle = convTitleFromTurns(s.turns)
	}
	title := s.convTitle
	if title == "" {
		title = "新会话"
	}
	created := s.convCreated
	if created.IsZero() {
		created = now
	}
	c := s.convByID(s.convID)
	if c == nil {
		c = &aiConversation{ID: s.convID, CreatedAt: created}
		s.convs = append([]*aiConversation{c}, s.convs...)
	}
	c.Title = title
	c.UpdatedAt = now
	c.Turns = s.turns
	c.ChatHistory = s.chatHistory
	c.Explanation = s.explanation
	c.Command = s.command
	c.Commands = s.commands
	c.Loading = s.loading
	c.Cancel = s.cancel
	c.ReqSeq = s.reqSeq
	c.Err = s.err
	c.ContextError = s.contextError
	c.Attached = s.attachedContexts
	c.PendingApproval = s.pendingApproval
	c.ScrollY = s.scroll.Y
}

func loadConversationInto(s *aiPanel, c *aiConversation) {
	if s == nil || c == nil {
		return
	}
	s.convID = c.ID
	s.convTitle = c.Title
	s.convCreated = c.CreatedAt
	s.turns = c.Turns
	s.chatHistory = c.ChatHistory
	s.explanation = c.Explanation
	s.command = c.Command
	s.commands = c.Commands
	s.loading = c.Loading
	s.cancel = c.Cancel
	s.reqSeq = c.ReqSeq
	s.err = c.Err
	s.contextError = c.ContextError
	s.attachedContexts = c.Attached
	s.pendingApproval = c.PendingApproval
	s.scroll.Y = c.ScrollY
	s.showHistory = false
	s.showMentions = false
	s.showSlash = false
	s.confirmDanger = false
	s.pendingCmd = ""
}

func resetActiveConversationDraft(s *aiPanel, targetOS string) {
	if s == nil {
		return
	}
	open, mode, convs, seq := s.open, s.mode, s.convs, s.convSeq
	*s = aiPanel{
		open:     open,
		mode:     mode,
		targetOS: targetOS,
		convs:    convs,
		convSeq:  seq,
	}
}

// newAIConversation 保存当前会话到本终端历史记录并开启一个新的空白会话。
func (a *app) newAIConversation() {
	a.syncAITab()
	saveActiveConversation(&a.ai)
	resetActiveConversationDraft(&a.ai, a.activeTargetOS())
}

// selectAIConversation 在当前终端内切换到指定历史会话。
func (a *app) selectAIConversation(id string) {
	a.syncAITab()
	if id == a.ai.convID {
		a.ai.showHistory = false
		return
	}
	saveActiveConversation(&a.ai)
	c := a.ai.convByID(id)
	if c == nil {
		return
	}
	loadConversationInto(&a.ai, c)
}

// deleteAIConversation 删除当前终端内的一条历史会话（若为当前会话则切换回新空白页）。
func (a *app) deleteAIConversation(id string) {
	a.syncAITab()
	idx := slices.IndexFunc(a.ai.convs, func(c *aiConversation) bool { return c != nil && c.ID == id })
	if idx >= 0 {
		stopAIConversation(a.ai.convs[idx])
		a.ai.convs = slices.Delete(a.ai.convs, idx, idx+1)
	}
	if a.ai.convID == id {
		showHist := a.ai.showHistory
		stopAISession(&a.ai)
		resetActiveConversationDraft(&a.ai, a.activeTargetOS())
		a.ai.showHistory = showHist
	}
}

// openAI 打开 AI Agent 侧栏并定位到当前活动环境（保留该标签页自己的对话记录与权限模式）。
func (a *app) openAI() {
	a.syncTabDrawers()
	a.ai.open = true
	a.ai.fresh = true
	a.lastDrawer = "ai"
	if cur := a.current(); cur != nil {
		cur.lastDrawer = "ai"
	}
	a.ai.targetOS = a.activeTargetOS()
}

// toggleAI 切换 AI Agent 侧栏展开与收起，不丢失已有对话。
func (a *app) toggleAI() {
	a.syncTabDrawers()
	if a.ai.open {
		a.ai.open = false
		return
	}
	a.openAI()
}

// clearAIChat 清空当前正在查看的对话并停止该对话进行中的请求（保留其他历史会话）。
func (a *app) clearAIChat() {
	a.syncAITab()
	if a.ai.convID != "" {
		a.deleteAIConversation(a.ai.convID)
		a.ai.showHistory = false
		return
	}
	a.stopAIRequest()
	resetActiveConversationDraft(&a.ai, a.activeTargetOS())
}

// openAIExplain 打开 AI Agent 并立即分析传入的报错文本。
func (a *app) openAIExplain(errorText string) {
	a.openAI()
	a.ai.contextError = strings.TrimSpace(errorText)
	if a.ai.contextError != "" {
		a.attachAIContext("terminal")
		a.runAIExplain()
	}
}

// openAIExplainFromActivePane 从最近复制内容或当前活动终端屏幕提取报错文本并启动分析。
func (a *app) openAIExplainFromActivePane() {
	text := strings.TrimSpace(a.lastCopiedText)
	if text == "" {
		text = a.captureActiveTerminalTail(60)
	}
	a.openAIExplain(text)
}

func (a *app) captureActiveTerminalTail(maxLines int) string {
	t := a.current()
	if t == nil || t.focus == nil || t.focus.term == nil {
		return ""
	}
	return captureTermTail(t.focus.term.Text(), maxLines)
}

func (a *app) attachAIContext(id string) {
	for _, existing := range a.ai.attachedContexts {
		if existing == id {
			return
		}
	}
	a.ai.attachedContexts = append(a.ai.attachedContexts, id)
}

func (a *app) removeAIContext(id string) {
	var out []string
	for _, existing := range a.ai.attachedContexts {
		if existing != id {
			out = append(out, existing)
		}
	}
	a.ai.attachedContexts = out
}

func mentionLabel(id string) string {
	for _, opt := range aiMentionOptions {
		if opt.ID == id {
			return "@" + opt.Label
		}
	}
	return "@" + id
}

func (a *app) buildAttachedContextBlock(snap aiSessionSnapshot, attached []string, extraErr string) (tags []string, block string) {
	var parts []string
	seen := map[string]bool{}
	for _, id := range attached {
		if seen[id] {
			continue
		}
		seen[id] = true
		switch id {
		case "terminal":
			tags = append(tags, "@终端输出")
			tail := strings.TrimSpace(extraErr)
			if tail == "" {
				if t := a.targetTabForSnapshot(snap); t != nil && t.focus != nil && t.focus.term != nil {
					tail = captureTermTail(t.focus.term.Text(), 80)
				}
			}
			if tail != "" {
				parts = append(parts, "[附加终端输出]\n"+tail)
			}
		case "host":
			tags = append(tags, "@主机信息")
			if snap.hasTab {
				parts = append(parts, fmt.Sprintf("[附加主机信息]\n名称=%s 地址=%s:%d 用户=%s 目录=%s 系统=%s 生产环境=%v",
					snap.hostName, snap.hostAddr, snap.hostPort, snap.liveUser, snap.liveDir, snap.distro, snap.production))
			}
		case "clipboard":
			tags = append(tags, "@最近复制")
			if clip := strings.TrimSpace(a.lastCopiedText); clip != "" {
				parts = append(parts, "[附加最近复制内容]\n"+clip)
			}
		}
	}
	if extraErr != "" && !seen["terminal"] {
		parts = append(parts, "[终端报错输出]\n"+extraErr)
	}
	return tags, strings.Join(parts, "\n\n")
}

// runAIGenerate 发起一轮多步流式 Agent 执行。
func (a *app) runAIGenerate() {
	a.syncAITab()
	query := strings.TrimSpace(a.ai.query)
	if query == "" {
		return
	}
	a.startAgentRun(query, query, a.ai.attachedContexts, "")
	a.ai.query = ""
	a.ai.attachedContexts = nil
	a.ai.showMentions = false
	a.ai.showSlash = false
}

// runAIExplain 异步调用 AI 分析终端报错。
func (a *app) runAIExplain() {
	a.syncAITab()
	errText := strings.TrimSpace(a.ai.contextError)
	if errText == "" {
		return
	}
	a.startAgentRun("分析终端报错", "请分析以下终端报错输出的原因，如需要可继续检查主机状态并给出修复建议。", []string{"terminal"}, errText)
	a.ai.contextError = ""
}

func (a *app) startAgentRun(displayQuery, promptText string, attached []string, extraErr string) {
	a.syncAITab()
	targetTabID := a.currentAITabID()
	a.ai.showHistory = false

	if a.ai.cancel != nil {
		a.ai.cancel()
		a.ai.cancel = nil
	}
	if a.ai.pendingApproval != nil {
		select {
		case a.ai.pendingApproval.answer <- false:
		default:
		}
		a.ai.pendingApproval = nil
	}

	client := a.aiClient()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)

	a.ai.reqSeq++
	seq := a.ai.reqSeq
	a.ai.loading = true
	a.ai.cancel = cancel
	a.ai.err = ""
	a.ai.autoScroll = true

	snap := a.captureAISessionSnapshot()
	mode := a.ai.mode
	tags, ctxBlock := a.buildAttachedContextBlock(snap, attached, extraErr)

	userContent := promptText
	if ctxBlock != "" {
		userContent = ctxBlock + "\n\n用户请求：" + promptText
	}

	turnIdx := len(a.ai.turns)
	a.ai.turns = append(a.ai.turns, aiTurn{
		query:       displayQuery,
		contextTags: tags,
		running:     true,
	})
	saveActiveConversation(&a.ai)
	targetConvID := a.ai.convID

	sysMsg := ai.Message{Role: "system", Content: agentSystemPrompt(snap, mode)}
	history := make([]ai.Message, 0, len(a.ai.chatHistory)+2)
	history = append(history, sysMsg)
	history = append(history, a.ai.chatHistory...)
	history = append(history, ai.Message{Role: "user", Content: userContent})
	tools := agentToolDefinitions()

	updateConv := func(fn func(c *aiConversation)) {
		sess := a.aiSessionForTab(targetTabID)
		c := sess.convByID(targetConvID)
		if c == nil || seq != c.ReqSeq {
			return
		}
		fn(c)
		c.UpdatedAt = time.Now()
		if sess.convID == targetConvID {
			sess.turns = c.Turns
			sess.chatHistory = c.ChatHistory
			sess.loading = c.Loading
			sess.cancel = c.Cancel
			sess.err = c.Err
			sess.explanation = c.Explanation
			sess.command = c.Command
			sess.commands = c.Commands
			sess.pendingApproval = c.PendingApproval
			sess.autoScroll = true
		}
	}

	go func() {
		var mu sync.Mutex
		lastProgressPost := time.Time{}
		toolByID := map[string]*aiToolRecord{}
		stepsByNum := map[int]*aiStepRecord{}

		getStep := func(stepNum int) *aiStepRecord {
			if s, ok := stepsByNum[stepNum]; ok {
				return s
			}
			s := &aiStepRecord{Step: stepNum}
			stepsByNum[stepNum] = s
			return s
		}

		res, err := ai.RunAgent(ctx, ai.AgentRunConfig{
			Client:   client,
			Messages: history,
			Tools:    tools,
			MaxSteps: 12,
			ExecuteTool: func(tctx context.Context, call ai.ToolCall) (string, bool) {
				mu.Lock()
				rec := toolByID[call.ID]
				mu.Unlock()
				return a.executeAgentTool(tctx, targetConvID, seq, snap, mode, rec, call)
			},
			OnEvent: func(ev ai.AgentEvent) {
				switch ev.Kind {
				case ai.EventStepStart:
					stepRec := getStep(ev.Step)
					a.post(func() {
						updateConv(func(c *aiConversation) {
							if turnIdx < len(c.Turns) {
								c.Turns[turnIdx].steps = append(c.Turns[turnIdx].steps, stepRec)
							}
						})
					})
				case ai.EventStepProgress:
					now := time.Now()
					mu.Lock()
					shouldPost := now.Sub(lastProgressPost) >= 45*time.Millisecond
					if shouldPost {
						lastProgressPost = now
					}
					mu.Unlock()
					if !shouldPost {
						return
					}
					th, thSec, ct, usage := ev.Thinking, ev.ThinkingSeconds, ev.Content, ev.TotalUsage
					a.post(func() {
						updateConv(func(c *aiConversation) {
							if turnIdx >= len(c.Turns) {
								return
							}
							s := getStep(ev.Step)
							s.Thinking = th
							s.ThinkingSeconds = thSec
							s.Content = ct
							c.Turns[turnIdx].usage = usage
						})
					})
				case ai.EventToolStart:
					summary, args := summarizeToolCall(ev.ToolCall)
					rec := &aiToolRecord{
						ID:      ev.ToolCall.ID,
						Name:    ev.ToolCall.Function.Name,
						Summary: summary,
						Args:    args,
						Status:  toolRunning,
					}
					mu.Lock()
					toolByID[ev.ToolCall.ID] = rec
					mu.Unlock()
					a.post(func() {
						updateConv(func(c *aiConversation) {
							if turnIdx >= len(c.Turns) {
								return
							}
							s := getStep(ev.Step)
							s.Tools = append(s.Tools, rec)
						})
					})
				case ai.EventToolDone:
					mu.Lock()
					rec := toolByID[ev.ToolCall.ID]
					mu.Unlock()
					out, isErr := ev.ToolOutput, ev.ToolError
					a.post(func() {
						updateConv(func(c *aiConversation) {
							if rec == nil {
								return
							}
							rec.Output = out
							if rec.Status != toolDenied {
								if isErr {
									rec.Status = toolError
								} else {
									rec.Status = toolDone
								}
							}
						})
					})
				case ai.EventStepDone:
					th, thSec, ct, usage := ev.Thinking, ev.ThinkingSeconds, ev.Content, ev.TotalUsage
					a.post(func() {
						updateConv(func(c *aiConversation) {
							if turnIdx >= len(c.Turns) {
								return
							}
							s := getStep(ev.Step)
							s.Thinking = th
							s.ThinkingSeconds = thSec
							s.Content = ct
							c.Turns[turnIdx].usage = usage
						})
					})
				}
			},
		})

		a.post(func() {
			canceled := ctx.Err() == context.Canceled
			cancel()
			if canceled {
				return
			}
			updateConv(func(c *aiConversation) {
				c.Loading = false
				c.Cancel = nil
				c.PendingApproval = nil
				if turnIdx < len(c.Turns) {
					c.Turns[turnIdx].running = false
				}
				if err != nil {
					msg := friendlyAIError(err)
					c.Err = msg
					if turnIdx < len(c.Turns) {
						c.Turns[turnIdx].err = msg
					}
					return
				}
				finishConversationTurn(c, turnIdx, userContent, res)
			})
		})
	}()
}

func finishConversationTurn(c *aiConversation, turnIdx int, userContent string, res *ai.AgentRunResult) {
	if c == nil || res == nil {
		return
	}
	parsed := ai.ParseMarkdown(res.FinalText)
	var cmds []string
	var explain string
	// 如果模型（或测试桩）返回经典 JSON {"explanation":..., "command":...}，转换为可渲染 Markdown
	if parsedResult := ai.ParseCommandText(res.FinalText); parsedResult != nil && (parsedResult.Command != "" || parsedResult.Explanation != "") {
		if strings.HasPrefix(strings.TrimSpace(res.FinalText), "{") {
			var md strings.Builder
			if parsedResult.Explanation != "" {
				md.WriteString(parsedResult.Explanation)
			}
			for _, cmd := range parsedResult.Commands {
				if md.Len() > 0 {
					md.WriteString("\n\n")
				}
				md.WriteString("```bash\n" + cmd + "\n```")
			}
			if turnIdx < len(c.Turns) {
				steps := c.Turns[turnIdx].steps
				if len(steps) > 0 {
					steps[len(steps)-1].Content = md.String()
				}
			}
			explain = parsedResult.Explanation
			cmds = parsedResult.Commands
		}
	}
	if len(cmds) == 0 {
		for _, blk := range parsed {
			if blk.Kind == ai.BlockCode && strings.TrimSpace(blk.Code) != "" {
				cmds = append(cmds, strings.TrimSpace(blk.Code))
			} else if blk.Kind == ai.BlockParagraph && explain == "" {
				explain = blk.Text
			}
		}
		if explain == "" {
			explain = res.FinalText
		}
	}

	c.Explanation = explain
	c.Commands = cmds
	if len(cmds) > 0 {
		c.Command = cmds[0]
	} else {
		c.Command = ""
	}
	if turnIdx < len(c.Turns) {
		c.Turns[turnIdx].explanation = explain
		c.Turns[turnIdx].commands = cmds
		c.Turns[turnIdx].usage = res.Usage
		if res.ReachedMax {
			c.Turns[turnIdx].err = "已达到单轮最大工具调用步数（12 步），可继续追问让我深入排查。"
		}
	}

	// 将本轮消息写入上下文历史（保留多轮 tool/assistant 记录，剥离临时 system 提示）
	if len(res.Messages) > 1 {
		c.ChatHistory = append([]ai.Message(nil), res.Messages[1:]...)
	} else {
		c.ChatHistory = append(c.ChatHistory,
			ai.Message{Role: "user", Content: userContent},
			ai.Message{Role: "assistant", Content: res.FinalText},
		)
	}
}

func (a *app) stopAIRequest() {
	a.syncAITab()
	if c := a.ai.convByID(a.ai.convID); c != nil {
		stopAIConversation(c)
	}
	a.ai.reqSeq++
	if a.ai.cancel != nil {
		a.ai.cancel()
		a.ai.cancel = nil
	}
	if a.ai.pendingApproval != nil {
		select {
		case a.ai.pendingApproval.answer <- false:
		default:
		}
		a.ai.pendingApproval = nil
	}
	a.ai.loading = false
	for i := range a.ai.turns {
		a.ai.turns[i].running = false
	}
}

// insertCommandToTerminal 将单行命令安全填入当前激活窗格的终端，绝不自动执行（不发送回车换行）。
func (a *app) insertCommandToTerminal(cmd string) {
	cmd = strings.TrimSpace(strings.TrimRight(cmd, "\r\n"))
	if cmd == "" {
		return
	}

	// 多行脚本禁止直接填入终端（在未开启 bracketed paste 的 shell 里换行会直接触发执行）
	if strings.ContainsAny(cmd, "\r\n") {
		a.toast(ui.Toast{Title: "多行脚本不支持直接填入，请使用「复制命令」后核对执行"})
		return
	}

	// 高危单行命令二次拦截
	if ai.IsDangerousCommand(cmd) && !a.ai.confirmDanger {
		a.ai.confirmDanger = true
		a.ai.pendingCmd = cmd
		return
	}

	a.ai.confirmDanger = false
	a.ai.pendingCmd = ""

	t := a.current()
	if t == nil || t.focus == nil || t.focus.term == nil {
		a.toast(ui.Toast{Title: "当前没有活动的终端窗格"})
		return
	}

	// 兜底剥离所有换行符，保证绝不自动敲击回车
	safeCmd := strings.NewReplacer("\r", "", "\n", "").Replace(cmd)
	t.focus.term.Paste(safeCmd)
	a.toast(ui.Toast{Title: "已将命令填入终端，请核对后回车执行"})
}

func (a *app) submitAIInput() {
	a.syncAITab()
	if a.ai.loading {
		return
	}
	q := strings.TrimSpace(a.ai.query)
	if strings.HasPrefix(q, "/") {
		for _, sc := range aiSlashCommands {
			if strings.EqualFold(q, sc.Cmd) {
				a.runSlashCommand(sc)
				return
			}
		}
	}
	if len(a.ai.turns) == 0 && a.ai.contextError != "" && q == "" {
		a.runAIExplain()
		return
	}
	a.runAIGenerate()
}

func (a *app) runSlashCommand(sc aiSlashCommand) {
	a.ai.showSlash = false
	a.ai.query = ""
	if sc.IsNew {
		a.newAIConversation()
		return
	}
	if sc.IsClear {
		a.clearAIChat()
		return
	}
	for _, att := range sc.Attach {
		a.attachAIContext(att)
	}
	a.startAgentRun(sc.Cmd+" · "+sc.Title, sc.Prompt, a.ai.attachedContexts, "")
	a.ai.attachedContexts = nil
}

// aiDrawer 在右侧渲染可拖拽调节宽度的 AI Agent 停靠面板。
func (a *app) aiDrawer(c *ui.Context, colors design.Colors, width, maxW float32) {
	a.syncAITab()
	p := &a.ai
	if !p.open {
		return
	}
	if a.aiWidth <= 0 {
		a.aiWidth = 460
	}
	if width <= 0 {
		width = a.aiWidth
	}
	if maxW < 340 {
		maxW = 340
	}

	// 左侧拖拽手柄（不做宽度动画，避免终端连续收到 SIGWINCH）
	grip := ui.Box(c).Key("aiDrawerResize").Width(8).Shrink(0).Margin(design.Gap/2, -4).
		Cursor(ui.CursorResizeEW).Focusable().Label("调整 AI 面板宽度")
	dx, _, held := grip.Dragged()
	switch {
	case grip.DoubleClicked():
		a.aiWidth = 460
	case held && dx != 0:
		a.aiWidth = min(max(width-dx, 340), maxW)
	}
	if held || grip.Hovered() {
		accent := c.Theme().Accent
		grip.Draw(func(p *ui.Painter, r ui.Rect) {
			p.Fill(ui.Rect{X: r.X + r.W/2 - 0.5, Y: r.Y + 8, W: 1, H: max(0, r.H-16)}, accent, 0)
		})
	}

	drawer := raised(ui.Column(c).Key("aiDrawer").Width(width).Shrink(0).Margin(design.Gap/2).Clip(), colors, design.RadiusCard)
	drawer.Transition(design.SlideIn(18)).Children(func() {
		a.aiDrawerHeader(c, colors)
		if p.showHistory {
			a.aiHistoryView(c, colors)
		} else {
			a.aiMessageStream(c, colors)
			if p.pendingApproval != nil {
				a.aiApprovalCard(c, colors, p.pendingApproval)
			}
			a.aiComposer(c, colors)
		}
	})

	// 代码块填入高危命令的弹窗二次确认（默认按钮放取消）
	if p.confirmDanger {
		open := true
		choice := ui.AlertDialog(c, &open, "执行高危命令确认", fmt.Sprintf("该命令包含高危操作：\n\n%s\n\n是否仍将其填入终端？（填入后仍需在终端人工敲击回车执行）", p.pendingCmd), "确认填入", locales.Cancel())
		if choice == -1 && open {
			return
		}
		if choice == 0 {
			a.insertCommandToTerminal(p.pendingCmd)
		} else {
			p.confirmDanger = false
			p.pendingCmd = ""
		}
	}
}

func (a *app) aiDrawerHeader(c *ui.Context, colors design.Colors) {
	t := c.Theme()
	ui.Row(c).Padding(10, 12).Gap(8).AlignItems(ui.Center).BorderColor(colors.Hairline).BorderWidth(0, 0, 1, 0).Shrink(0).Children(func() {
		ui.Icon(c, design.Icon("sparkles")).FontSize(15).TextColor(t.Accent)
		ui.Text(c, "AI Agent").FontSize(design.TextBody).FontWeight(600)

		targetText := "未连接终端"
		isProd := false
		if cur := a.current(); cur != nil {
			targetText = tabLabel(cur)
			if cur.host.Production {
				isProd = true
			}
		}
		chipBg, chipFg := colors.Chip, t.TextMuted
		if isProd {
			chipBg, chipFg = colors.DangerSoft, colors.Danger
			targetText = "生产 · " + targetText
		}
		ui.Box(c).Padding(2, 8).Radius(design.RadiusChip).Background(chipBg).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, targetText).FontSize(design.TextSmall).TextColor(chipFg).SingleLine().Ellipsis("…")
		})

		histBtn := iconButton(c, colors, "history", "历史会话", func() {
			saveActiveConversation(&a.ai)
			a.ai.showHistory = !a.ai.showHistory
		}).Key("aiHistoryBtn")
		if a.ai.showHistory {
			histBtn.Background(colors.Hover)
		}
		if len(a.ai.turns) > 0 || a.ai.showHistory {
			iconButton(c, colors, "plus", "新开会话", a.newAIConversation).Key("aiNewChatBtn")
		}
		iconButton(c, colors, "x", "关闭 AI 面板", func() {
			a.ai.open = false
		}).Key("aiCloseBtn")
	})
}

func (a *app) aiHistoryView(c *ui.Context, colors design.Colors) {
	p := &a.ai
	t := c.Theme()
	ui.Column(c).Grow(1).MinHeight(0).Padding(12).Gap(10).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(8).Shrink(0).Children(func() {
			ui.Text(c, fmt.Sprintf("本终端历史会话（%d）", len(p.convs))).
				FontSize(design.TextBody).FontWeight(600).Grow(1).MinWidth(0)
			ui.Button(c, "新建会话").Key("aiHistoryNewBtn").Height(26).Padding(0, 10).OnClick(a.newAIConversation)
			ui.Button(c, "返回对话").Key("aiHistoryBackBtn").Height(26).Padding(0, 10).OnClick(func() {
				p.showHistory = false
			})
		})
		ui.Scroll(c).Key("aiHistoryScroll").Grow(1).MinHeight(0).Children(func() {
			if len(p.convs) == 0 {
				ui.Box(c).Padding(14).Radius(design.RadiusRow).Background(colors.Hover).Children(func() {
					ui.Text(c, "当前终端暂无历史会话，发送消息后会自动记录在这里。").
						FontSize(design.TextSmall).TextColor(t.TextMuted)
				})
				return
			}
			ui.Column(c).Gap(6).Children(func() {
				for _, conv := range p.convs {
					if conv == nil {
						continue
					}
					item := conv
					isCur := item.ID == p.convID
					row := ui.Row(c).Gap(6).Padding(8, 10).Radius(design.RadiusRow).AlignItems(ui.Center)
					if isCur {
						row.Background(t.Accent.Alpha(0.12)).Border(1, t.Accent.Alpha(0.35))
					} else {
						row.Background(colors.Hover).Border(1, colors.Hairline)
					}
					row.Children(func() {
						openBtn := ui.ButtonBase(c).Key("aiConv:" + item.ID).Label("打开会话 " + item.Title).
							Grow(1).MinWidth(0).OnClick(func() {
							a.selectAIConversation(item.ID)
						})
						openBtn.Children(func() {
							ui.Column(c).Gap(2).Children(func() {
								ui.Text(c, item.Title).FontSize(design.TextBody).FontWeight(600).
									TextColor(t.Text).SingleLine().Ellipsis("…")
								meta := fmt.Sprintf("%d 轮 · %s", len(item.Turns), item.UpdatedAt.Format("15:04"))
								if isCur {
									meta += " · 当前"
								}
								if item.Loading {
									meta += " · 进行中"
								}
								ui.Text(c, meta).FontSize(design.TextSmall).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
							})
						})
						iconButton(c, colors, "trash-2", "删除会话 "+item.ID, func() {
							a.deleteAIConversation(item.ID)
						}).Key("aiDelConv:" + item.ID).Tooltip("删除该会话")
					})
				}
			})
		})
	})
}

func (a *app) aiMessageStream(c *ui.Context, colors design.Colors) {
	p := &a.ai
	t := c.Theme()
	if p.autoScroll {
		p.scroll.Y = math.MaxFloat32
		p.autoScroll = false
	}

	ui.Scroll(c).Key("aiScroll").Grow(1).MinHeight(0).TrackScroll(&p.scroll).Children(func() {
		ui.Column(c).Padding(12).Gap(14).Children(func() {
			if len(p.turns) == 0 && p.contextError == "" {
				a.aiWelcomeView(c, colors)
			}

			if p.contextError != "" && len(p.turns) == 0 {
				ui.Column(c).Padding(10).Gap(6).Radius(design.RadiusRow).Background(colors.Hover).Border(1, colors.Hairline).Children(func() {
					ui.Text(c, "已捕获终端输出上下文").FontSize(design.TextSmall).FontWeight(600).TextColor(t.Accent)
					ui.Text(c, p.contextError).FontSize(design.TextSmall).Font(design.FontTerminal).TextColor(t.TextMuted).Selectable()
				})
			}

			for i := range p.turns {
				a.renderAITurn(c, colors, i, &p.turns[i])
			}

			if p.err != "" && (len(p.turns) == 0 || p.turns[len(p.turns)-1].err == "") {
				ui.Text(c, p.err).Key("aiError").TextColor(colors.Danger).FontSize(design.TextSmall).Selectable()
			}
		})
	})
}

func (a *app) aiWelcomeView(c *ui.Context, colors design.Colors) {
	t := c.Theme()
	ui.Column(c).Gap(10).Padding(8, 2).Children(func() {
		ui.Text(c, "直接用自然语言提问，Agent 会自动探测主机、读取终端并总结结论：").
			FontSize(design.TextSmall).TextColor(t.TextMuted)
		starters := []struct {
			key    string
			label  string
			prompt string
			attach []string
		}{
			{"starterInspect", "这台服务配置如何", "这台服务器配置和当前运行状态如何？请主动检查系统内核、CPU、内存、磁盘、网络接口、关键进程与监听端口并汇总成表格。", []string{"host"}},
			{"starterDiagnose", "分析当前终端报错", "请读取当前终端最近的输出和报错信息，分析根因并给出排查与修复建议。", []string{"terminal", "host"}},
			{"starterPorts", "查看监听端口与进程", "请检查当前服务器正在监听的端口和主要进程占用情况，列出端口、协议与对应服务。", []string{"host"}},
			{"starterDisk", "排查磁盘与内存占用", "请排查这台主机的磁盘分区使用率、大目录及内存占用最高的进程。", []string{"host"}},
		}
		for _, st := range starters {
			item := st
			b := ui.ButtonBase(c).Key(item.key).Padding(8, 10).Radius(design.RadiusRow).
				Background(colors.Hover).Border(1, colors.Hairline).Transition(design.Hover)
			if b.Hovered() {
				b.Background(colors.Selected)
			}
			b.Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, design.Icon("sparkles")).FontSize(13).TextColor(t.Accent)
					ui.Text(c, item.label).FontSize(design.TextBody)
				})
			}).OnClick(func() {
				for _, att := range item.attach {
					a.attachAIContext(att)
				}
				a.startAgentRun(item.label, item.prompt, a.ai.attachedContexts, "")
				a.ai.attachedContexts = nil
			})
		}
	})
}

func (a *app) renderAITurn(c *ui.Context, colors design.Colors, turnIdx int, turn *aiTurn) {
	t := c.Theme()
	ui.Column(c).Gap(10).Children(func() {
		// 用户消息气泡
		ui.Row(c).Justify(ui.End).Children(func() {
			ui.Column(c).MaxWidth(340).Padding(8, 12).Gap(4).Radius(design.RadiusRow).
				Background(t.Accent.Alpha(0.13)).Border(1, t.Accent.Alpha(0.24)).Children(func() {
				if len(turn.contextTags) > 0 {
					ui.Row(c).Gap(4).Wrap().Children(func() {
						for _, tag := range turn.contextTags {
							ui.Box(c).Padding(1, 6).Radius(4).Background(colors.Card).Children(func() {
								ui.Text(c, tag).FontSize(design.TextSmall - 1).TextColor(t.Accent)
							})
						}
					})
				}
				ui.Text(c, turn.query).FontSize(design.TextBody).Selectable()
			})
		})

		// 多轮推理与工具步骤
		for sIdx, step := range turn.steps {
			if strings.TrimSpace(step.Thinking) != "" {
				label := "正在思考…"
				if step.ThinkingSeconds > 0 {
					label = fmt.Sprintf("思考了 %ds", step.ThinkingSeconds)
				} else if !turn.running {
					label = "思考过程"
				}
				ui.Column(c).Padding(6, 10).Radius(design.RadiusRow).Background(colors.Hover).Children(func() {
					c.Key(fmt.Sprintf("aiThink:%d:%d", turnIdx, sIdx))
					ui.Collapsible(c, label, &step.ThinkExpanded, func() {
						ui.Text(c, step.Thinking).FontSize(design.TextSmall).TextColor(t.TextMuted).Selectable()
					})
				})
			}

			for tIdx, tool := range step.Tools {
				a.renderToolCard(c, colors, turnIdx, sIdx, tIdx, tool)
			}

			if strings.TrimSpace(step.Content) != "" {
				a.renderMarkdown(c, colors, turnIdx, sIdx, step.Content)
			}
		}

		// 兼容旧结构直接填充 explanation / commands 的情况
		if len(turn.steps) == 0 && !turn.running && (turn.explanation != "" || len(turn.commands) > 0) {
			var md strings.Builder
			if turn.explanation != "" {
				md.WriteString(turn.explanation)
			}
			for _, cmd := range turn.commands {
				if md.Len() > 0 {
					md.WriteString("\n\n")
				}
				md.WriteString("```bash\n" + cmd + "\n```")
			}
			a.renderMarkdown(c, colors, turnIdx, 0, md.String())
		}

		// 若尚未收到任何 step（刚发出请求等待首包）
		if turn.running && len(turn.steps) == 0 {
			ui.Text(c, "Agent 正在连接模型并分析需求…").FontSize(design.TextSmall).TextColor(t.TextMuted)
		}

		if turn.err != "" {
			ui.Text(c, turn.err).FontSize(design.TextSmall).TextColor(colors.Danger).Selectable()
		}

		if !turn.running && turn.usage.TotalTokens > 0 {
			u := turn.usage
			usageLine := fmt.Sprintf("Token %s · 输入 %s · 输出 %s",
				formatIntComma(u.TotalTokens), formatIntComma(u.PromptTokens), formatIntComma(u.CompletionTokens))
			if u.CachedTokens > 0 {
				usageLine += " · 缓存 " + formatIntComma(u.CachedTokens)
			}
			if u.ReasoningTokens > 0 {
				usageLine += " · 推理 " + formatIntComma(u.ReasoningTokens)
			}
			ui.Text(c, usageLine).FontSize(design.TextSmall - 0.5).TextColor(colors.TextFaint).FontFeatures("tnum")
		}
	})
}

func (a *app) renderToolCard(c *ui.Context, colors design.Colors, turnIdx, sIdx, tIdx int, tool *aiToolRecord) {
	t := c.Theme()
	dotColor := t.Accent
	statusText := "执行中"
	switch tool.Status {
	case toolWaitingApproval:
		dotColor = t.Accent
		statusText = "待确认"
	case toolDone:
		dotColor = colors.Live
		statusText = "完成"
	case toolError:
		dotColor = colors.Danger
		statusText = "异常"
	case toolDenied:
		dotColor = colors.Danger
		statusText = "已拒绝"
	}

	ui.Column(c).Padding(7, 10).Gap(6).Radius(design.RadiusRow).Background(colors.Hover).Border(1, colors.Hairline).Children(func() {
		hdr := ui.ButtonBase(c).Key(fmt.Sprintf("aiTool:%d:%d:%d", turnIdx, sIdx, tIdx)).
			OnClick(func() {
				tool.Expanded = !tool.Expanded
			})
		hdr.Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(7, 7).Radius(4).Background(dotColor).Shrink(0)
				ui.Text(c, tool.Summary).FontSize(design.TextSmall).Font(design.FontTerminal).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
				ui.Text(c, statusText).FontSize(design.TextSmall - 1).TextColor(colors.TextFaint).Shrink(0)
				chev := "chevron-right"
				if tool.Expanded {
					chev = "chevron-down"
				}
				ui.Icon(c, design.Icon(chev)).FontSize(12).TextColor(colors.TextFaint)
			})
		})
		if tool.Expanded {
			if strings.TrimSpace(tool.Args) != "" {
				ui.Text(c, tool.Args).FontSize(design.TextSmall - 0.5).Font(design.FontTerminal).TextColor(t.TextMuted).Selectable()
			}
			if strings.TrimSpace(tool.Output) != "" {
				out := tool.Output
				if len(out) > 2400 {
					out = out[:2400] + "\n...[展开预览已截断]"
				}
				ui.Box(c).Padding(6, 8).Radius(6).Background(colors.Card).Border(1, colors.Hairline).Children(func() {
					ui.Text(c, out).FontSize(design.TextSmall - 0.5).Font(design.FontTerminal).TextColor(t.TextMuted).Selectable()
				})
			}
		}
	})
}

type markdownRenderOptions struct {
	bodyFontSize    float32
	smallFontSize   float32
	headingFontSize float32
	blockGap        float32
	firstColWidth   float32
	renderCode      func(blk ai.MarkdownBlock)
}

func (a *app) renderMarkdown(c *ui.Context, colors design.Colors, turnIdx, sIdx int, content string) {
	codeIdx := 0
	renderMarkdownBlocks(c, colors, content, markdownRenderOptions{
		bodyFontSize:    design.TextBody,
		smallFontSize:   design.TextSmall,
		headingFontSize: design.TextHeading,
		blockGap:        8,
		firstColWidth:   84,
		renderCode: func(blk ai.MarkdownBlock) {
			cmdVal := blk.Code
			isMultiline := strings.ContainsAny(cmdVal, "\r\n")
			curCodeIdx := codeIdx
			codeIdx++
			t := c.Theme()
			ui.Column(c).Gap(6).Children(func() {
				ui.Box(c).Padding(8, 10).Radius(6).Background(colors.Card).Border(1, colors.CardBorder).Children(func() {
					ui.Text(c, cmdVal).FontSize(design.TextSmall).Font(design.FontTerminal).Selectable()
				})
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					copyKey := fmt.Sprintf("aiCopyBtn:%d:%d:%d", turnIdx, sIdx, curCodeIdx)
					if turnIdx == len(a.ai.turns)-1 && curCodeIdx == 0 {
						copyKey = "aiCopyBtn"
					}
					ui.Button(c, "复制命令").Key(copyKey).Height(26).Padding(0, 10).OnClick(func() {
						c.WriteClipboard(cmdVal)
						a.toast(ui.Toast{Title: "命令已复制到剪贴板"})
					})
					if isMultiline {
						ui.Text(c, "多行脚本请复制后核对执行").FontSize(design.TextSmall).TextColor(t.TextMuted)
					} else {
						insertKey := fmt.Sprintf("aiInsertBtn:%d:%d:%d", turnIdx, sIdx, curCodeIdx)
						if turnIdx == len(a.ai.turns)-1 && curCodeIdx == 0 {
							insertKey = "aiInsertBtn"
						}
						ui.PrimaryButton(c, "填入终端（需确认）").Key(insertKey).Height(26).Padding(0, 10).OnClick(func() {
							a.insertCommandToTerminal(cmdVal)
						})
					}
				})
			})
		},
	})
}

func renderMarkdownBlocks(c *ui.Context, colors design.Colors, content string, opts markdownRenderOptions) {
	t := c.Theme()
	blocks := ai.ParseMarkdown(content)
	if len(blocks) == 0 {
		ui.Text(c, replaceHTMLBreaks(content)).FontSize(opts.bodyFontSize).Selectable()
		return
	}
	ui.Column(c).Gap(opts.blockGap).Children(func() {
		for _, blk := range blocks {
			switch blk.Kind {
			case ai.BlockHeading:
				fs := opts.bodyFontSize + 1
				if blk.Level == 1 {
					fs = opts.headingFontSize
				}
				ui.Text(c, cleanInlineMarkdown(blk.Text)).FontSize(fs).FontWeight(600).Selectable()
			case ai.BlockParagraph:
				ui.Text(c, cleanInlineMarkdown(blk.Text)).FontSize(opts.bodyFontSize).Selectable()
			case ai.BlockQuote:
				ui.Row(c).Gap(8).Children(func() {
					ui.Box(c).Width(3).Background(t.Accent.Alpha(0.5)).Radius(2)
					ui.Text(c, cleanInlineMarkdown(blk.Text)).FontSize(opts.smallFontSize).TextColor(t.TextMuted).Grow(1).MinWidth(0).Selectable()
				})
			case ai.BlockList:
				ui.Column(c).Gap(4).Padding(0, 0, 0, 4).Children(func() {
					for idx, item := range blk.Items {
						prefix := "• "
						if blk.Ordered {
							prefix = fmt.Sprintf("%d. ", idx+1)
						}
						ui.Text(c, prefix+cleanInlineMarkdown(item)).FontSize(opts.bodyFontSize).Selectable()
					}
				})
			case ai.BlockDivider:
				ui.Box(c).Height(1).Background(colors.Hairline)
			case ai.BlockTable:
				renderMarkdownTable(c, colors, blk.Headers, blk.Rows, opts.firstColWidth)
			case ai.BlockCode:
				if opts.renderCode != nil {
					opts.renderCode(blk)
				} else {
					ui.Box(c).Padding(8, 10).Radius(6).Background(colors.Card).Border(1, colors.CardBorder).Children(func() {
						ui.Text(c, blk.Code).FontSize(opts.smallFontSize).Font(design.FontTerminal).Selectable()
					})
				}
			}
		}
	})
}

func renderMarkdownTable(c *ui.Context, colors design.Colors, headers []string, rows [][]string, firstColWidth float32) {
	if len(headers) == 0 {
		return
	}
	if firstColWidth <= 0 {
		firstColWidth = 84
	}
	colWeight := func(colIdx int) float32 {
		if len(headers) == 2 && colIdx == 0 {
			return 0
		}
		return 1
	}
	cellContainer := func(colIdx int, header bool, txt string) {
		cell := ui.Box(c).Padding(6, 8).MinWidth(0)
		if colWeight(colIdx) == 0 {
			cell.Width(firstColWidth).Shrink(0)
		} else {
			cell.Grow(1)
		}
		if colIdx > 0 {
			cell.BorderColor(colors.Hairline).BorderWidth(0, 0, 0, 1)
		}
		cell.Children(func() {
			el := ui.Text(c, cleanInlineMarkdown(txt)).FontSize(design.TextSmall).Selectable()
			if header {
				el.FontWeight(600)
			}
		})
	}

	ui.Column(c).Radius(6).Background(colors.Card).Border(1, colors.CardBorder).Clip().Children(func() {
		ui.Row(c).Background(colors.Hover).BorderColor(colors.Hairline).BorderWidth(0, 0, 1, 0).AlignItems(ui.Stretch).Children(func() {
			for i, h := range headers {
				cellContainer(i, true, h)
			}
		})
		for rIdx, row := range rows {
			rEl := ui.Row(c).AlignItems(ui.Stretch)
			if rIdx < len(rows)-1 {
				rEl.BorderColor(colors.Hairline).BorderWidth(0, 0, 1, 0)
			}
			rEl.Children(func() {
				for i := range headers {
					val := ""
					if i < len(row) {
						val = row[i]
					}
					cellContainer(i, false, val)
				}
			})
		}
	})
}

func cleanInlineMarkdown(s string) string {
	s = replaceHTMLBreaks(s)
	spans := ai.ParseInlineMarkdown(s)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	for _, sp := range spans {
		b.WriteString(sp.Text)
	}
	return b.String()
}

// replaceHTMLBreaks 将 GFM 表格与段落中的 <br> 换行标记转为真实换行，并折叠连续空行标签。
func replaceHTMLBreaks(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	for _, tag := range []string{"<br />", "<br/>", "<br>", "<BR />", "<BR/>", "<BR>"} {
		s = strings.ReplaceAll(s, tag, "\n")
	}
	for strings.Contains(s, "\n\n") {
		s = strings.ReplaceAll(s, "\n\n", "\n")
	}
	return strings.TrimSpace(s)
}

func (a *app) aiApprovalCard(c *ui.Context, colors design.Colors, ap *aiApproval) {
	t := c.Theme()
	bg := colors.Hover
	border := t.Accent.Alpha(0.45)
	title := "请求执行命令确认"
	titleColor := t.Accent
	if ap.dangerous || ap.production {
		bg = colors.DangerSoft
		border = colors.Danger.Alpha(0.55)
		titleColor = colors.Danger
		if ap.dangerous {
			title = "高危命令拦截确认"
		} else {
			title = "生产环境命令确认"
		}
	}
	ui.Column(c).Margin(0, 10, 6, 10).Padding(10).Gap(8).Radius(design.RadiusRow).Background(bg).Border(1, border).Shrink(0).Children(func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, title).FontSize(design.TextSmall).FontWeight(600).TextColor(titleColor).Grow(1)
			if ap.target != "" {
				ui.Text(c, ap.target).FontSize(design.TextSmall - 0.5).TextColor(t.TextMuted)
			}
		})
		if ap.reason != "" {
			ui.Text(c, ap.reason).FontSize(design.TextSmall).TextColor(t.TextMuted)
		}
		ui.Box(c).Padding(6, 8).Radius(6).Background(colors.Card).Border(1, colors.CardBorder).Children(func() {
			ui.Text(c, ap.command).FontSize(design.TextSmall).Font(design.FontTerminal).Selectable()
		})
		ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
			ui.Button(c, "拒绝").Key("aiDenyBtn").Height(26).Padding(0, 12).OnClick(func() {
				a.resolveAgentApproval(ap, false)
			})
			ui.PrimaryButton(c, "允许执行").Key("aiApproveBtn").Height(26).Padding(0, 12).OnClick(func() {
				a.resolveAgentApproval(ap, true)
			})
		})
	})
}

func (a *app) resolveAgentApproval(ap *aiApproval, ok bool) {
	if a.ai.pendingApproval != ap {
		return
	}
	a.ai.pendingApproval = nil
	if c := a.ai.convByID(a.ai.convID); c != nil && c.PendingApproval == ap {
		c.PendingApproval = nil
	}
	select {
	case ap.answer <- ok:
	default:
	}
}

func (a *app) aiComposer(c *ui.Context, colors design.Colors) {
	p := &a.ai
	t := c.Theme()

	showSlash := p.showSlash || strings.HasPrefix(p.query, "/")
	showMentions := p.showMentions || strings.HasSuffix(p.query, "@")

	ui.Column(c).Padding(6, 8, 8, 8).Gap(6).BorderColor(colors.Hairline).BorderWidth(1, 0, 0, 0).Shrink(0).Children(func() {
		if showSlash {
			ui.Column(c).Padding(6).Gap(2).Radius(design.RadiusRow).Background(colors.Hover).Border(1, colors.CardBorder).Children(func() {
				for _, sc := range aiSlashCommands {
					item := sc
					if strings.HasPrefix(p.query, "/") && len(p.query) > 1 && !strings.HasPrefix(item.Cmd, strings.TrimSpace(p.query)) {
						continue
					}
					row := ui.ButtonBase(c).Key("aiSlash:"+item.Cmd).Padding(5, 8).Radius(6).AlignItems(ui.Stretch).Transition(design.Hover)
					if row.Hovered() {
						row.Background(colors.Selected)
					}
					row.Children(func() {
						ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
							ui.Text(c, item.Cmd).FontSize(design.TextSmall).Font(design.FontTerminal).TextColor(t.Accent).Shrink(0)
							ui.Text(c, item.Title).FontSize(design.TextSmall).FontWeight(500).Shrink(0)
							ui.Text(c, item.Desc).FontSize(design.TextSmall - 1).TextColor(t.TextMuted).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
						})
					}).OnClick(func() {
						a.runSlashCommand(item)
					})
				}
			})
		}

		if showMentions {
			ui.Column(c).Padding(6).Gap(2).Radius(design.RadiusRow).Background(colors.Hover).Border(1, colors.CardBorder).Children(func() {
				for _, opt := range aiMentionOptions {
					item := opt
					row := ui.ButtonBase(c).Key("aiMention:"+item.ID).Padding(5, 8).Radius(6).AlignItems(ui.Stretch).Transition(design.Hover)
					if row.Hovered() {
						row.Background(colors.Selected)
					}
					row.Children(func() {
						ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
							ui.Text(c, "@"+item.Label).FontSize(design.TextSmall).TextColor(t.Accent).FontWeight(500).Shrink(0)
							ui.Text(c, item.Desc).FontSize(design.TextSmall - 1).TextColor(t.TextMuted).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
						})
					}).OnClick(func() {
						if strings.HasSuffix(p.query, "@") {
							p.query = strings.TrimSuffix(p.query, "@")
						}
						a.attachAIContext(item.ID)
						p.showMentions = false
					})
				}
			})
		}

		// 已附加的 @ 上下文标签
		if len(p.attachedContexts) > 0 {
			ui.Row(c).Gap(6).Wrap().Children(func() {
				for _, id := range p.attachedContexts {
					ctxID := id
					chip := ui.Row(c).Padding(2, 6).Gap(4).AlignItems(ui.Center).Radius(design.RadiusChip).
						Background(t.Accent.Alpha(0.12)).Border(1, t.Accent.Alpha(0.28))
					chip.Children(func() {
						ui.Text(c, mentionLabel(ctxID)).FontSize(design.TextSmall - 0.5).TextColor(t.Accent)
						ui.ButtonBase(c).Key("aiDropCtx:"+ctxID).Label("移除 "+mentionLabel(ctxID)).Size(14, 14).Center().Children(func() {
							ui.Icon(c, design.Icon("x")).FontSize(10).TextColor(t.Accent)
						}).OnClick(func() {
							a.removeAIContext(ctxID)
						})
					})
				}
			})
		}

		// 输入卡片：文本框 + 底部工具行
		ui.Column(c).Padding(7, 8).Gap(6).Radius(design.RadiusRow).Background(colors.Hover).Border(1, colors.CardBorder).Children(func() {
			c.Key("aiQueryInput")
			placeholder := "向 AI Agent 发送消息，@ 引用上下文，/ 快捷命令"
			if p.contextError != "" && len(p.turns) == 0 {
				placeholder = "按回车直接诊断报错，或补充说明你想排查的问题"
			}
			in := ui.TextInput(c, &p.query).Placeholder(placeholder).AutoFocus()
			if p.fresh {
				in.AutoFocus()
				p.fresh = false
			}
			in.OnSubmit(a.submitAIInput)

			ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
				ui.Button(c, "@").Key("aiMentionBtn").Label("引用上下文").Tooltip("@ 引用终端或主机上下文").
					Height(24).Padding(0, 7).OnClick(func() {
					p.showMentions = !p.showMentions
					p.showSlash = false
				})
				ui.Button(c, "/").Key("aiSlashBtn").Label("快捷指令").Tooltip("/ 快捷运维指令").
					Height(24).Padding(0, 7).OnClick(func() {
					p.showSlash = !p.showSlash
					p.showMentions = false
				})

				cfg := a.activeAIConfig()
				configured := a.configuredAIProfiles()
				modelLabel := "选择 AI 模型"
				if len(configured) == 0 && len(a.settings.AIProfiles) > 0 {
					modelLabel = "未启用 AI 模型"
				} else if cfg.Name != "" && cfg.Model != "" {
					modelLabel = cfg.Name + " · " + cfg.Model
				} else if cfg.Name != "" {
					modelLabel = cfg.Name + " · 未选模型"
				} else if cfg.Model != "" {
					modelLabel = cfg.Model
				}
				c.Key("aiModelSwitchBtn")
				ui.PopoverMenuButton(c, modelLabel, func(m *ui.Menu) {
					hasItems := false
					switch len(configured) {
					case 0:
						// 未启用任何提供商时仅显示配置入口
					case 1:
						prof := configured[0]
						models := a.modelsForProfile(prof.ID, prof.Models, prof.Model)
						isCur := cfg.ID == prof.ID
						if len(models) == 0 {
							hasItems = true
							if m.Item(prof.Name + "（前往获取模型）").ID("provider:" + prof.ID).Chosen() {
								a.openAISettings()
							}
						}
						for _, modelName := range models {
							mv := modelName
							hasItems = true
							if m.Item(prof.Name + " · " + mv).ID("model:" + prof.ID + ":" + mv).Radio(isCur && cfg.Model == mv).Chosen() {
								_ = a.switchAIProviderModel(prof.ID, mv)
							}
						}
					default:
						for _, prof := range configured {
							item := prof
							isCur := cfg.ID == item.ID
							models := a.modelsForProfile(item.ID, item.Models, item.Model)
							hasItems = true
							switch {
							case len(models) > 1:
								sub := m.Submenu(item.Name, func(sm *ui.Menu) {
									for _, modelName := range models {
										mv := modelName
										if sm.Item(mv).ID("model:" + item.ID + ":" + mv).Radio(isCur && cfg.Model == mv).Chosen() {
											_ = a.switchAIProviderModel(item.ID, mv)
										}
									}
								}).ID("provider:" + item.ID)
								if isCur {
									sub.Trailing("当前")
								}
							case len(models) == 1:
								mv := models[0]
								if m.Item(item.Name + " · " + mv).ID("provider:" + item.ID).Radio(isCur).Chosen() {
									_ = a.switchAIProviderModel(item.ID, mv)
								}
							default:
								if m.Item(item.Name).ID("provider:" + item.ID).Radio(isCur).Chosen() {
									_ = a.switchAIProfile(item.ID)
								}
							}
						}
					}
					if hasItems {
						m.Separator()
					}
					if m.Item("配置模型与密钥…").ID("openAISettings").Chosen() {
						a.openAISettings()
					}
				}).Height(24).Padding(0, 8).Shrink(1).MinWidth(0).Tooltip(modelLabel)

				ui.Box(c).Grow(1)

				c.Key("aiModeSwitchBtn")
				ui.PopoverMenuButton(c, p.mode.label(), func(m *ui.Menu) {
					for _, md := range []aiPermissionMode{aiModeObserver, aiModeConfirm, aiModeAuto} {
						modeVal := md
						if m.Item(modeVal.label() + " — " + modeVal.description()).Radio(p.mode == modeVal).Chosen() {
							p.mode = modeVal
						}
					}
				}).Height(24).Padding(0, 8).Shrink(0)

				if p.loading {
					ui.Button(c, locales.AiStop()).Key("aiStopBtn").Height(26).Padding(0, 10).OnClick(a.stopAIRequest)
				} else {
					sendLabel := "发送"
					if len(p.turns) == 0 && p.contextError != "" && strings.TrimSpace(p.query) == "" {
						sendLabel = "分析报错"
					}
					ui.PrimaryButton(c, sendLabel).Key("aiSubmitBtn").Height(26).Padding(0, 12).OnClick(a.submitAIInput)
				}
			})
		})
	})
}

func formatIntComma(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
	}
	for i := rem; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}
