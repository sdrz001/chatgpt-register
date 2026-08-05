// Package producer 编排 ChatGPT + Codex 账号的批量"生产"。
//
// 规则：维持"母号 + 指定数量的裂变"——每个邮箱先注册主号(母号，用邮箱本身地址)，
// 母号成功后才用别名(email-001@…)注册裂变子号，每个邮箱最多 1 + FissionCount 个账号。
//
// 目标数量 target 表示本次要成功产出的账号数。注册失败不计入成功，会自动补一个新任务
// 继续注册（"注册失败→注册数量-1→待生产+1"），直到达标或邮箱容量耗尽。
// 母号注册失败时该邮箱不会往下开裂变，下次仍优先重试母号。
//
// 验证码由 mailfetch 从邮箱自动读取，无需人工输入。
package producer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/codexoauth"
	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/integrationcfg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/smsactivate"

	"gorm.io/gorm"
)

const (
	defaultMaxConcurrency = 3
	defaultFissionCount   = 5
	codePollTimeout       = 3 * time.Minute
	codePollInterval      = 5 * time.Second
	maxLogLines           = 300
)

// openAI 验证码：6 位数字。
var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

// Config 从系统设置装载的运行参数。
type Config struct {
	MaxConcurrency   int
	FissionCount     int
	Headless         bool
	BrowserBackend   string
	PythonExecutable string
	Proxies          []string // 代理池，按账户轮转；空=直连
}

type Scope struct {
	CategoryID *uint
	MailboxIDs []uint
}

// Progress 生产进度快照，供 /api/produce/status 展示。
type Progress struct {
	Running    bool      `json:"running"`
	Target     int       `json:"target"`
	Pending    int       `json:"pending"`     // 待生产
	RunningNum int       `json:"running_num"` // 在跑
	Registered int       `json:"registered"`  // 已注册(成功)
	Failed     int       `json:"failed"`      // 注册失败(累计)
	Message    string    `json:"message"`
	Error      string    `json:"error"`
	Logs       []string  `json:"logs"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type smsLeaseToken struct {
	marker byte
}

type mailClient interface {
	ListMessages(context.Context, mailfetch.Account, int) ([]mailfetch.Message, error)
	GetMessage(context.Context, mailfetch.Account, string) (mailfetch.Message, error)
}

// Producer 单例，管理一次生产任务的生命周期与进度。
type Producer struct {
	db   *gorm.DB
	mail mailClient

	mu     sync.Mutex
	prog   Progress
	cancel context.CancelFunc

	claimMu  sync.Mutex      // 串行化任务领取
	inflight map[string]uint // email -> mailboxID，正在处理中的任务
	// failed 记录当前仍处于失败态的邮箱（重试成功后会移除）。
	// 只有最终没能注册成功的邮箱才计入失败数——中途重试失败不算。
	failed   map[string]struct{}
	pxMu     sync.Mutex
	pxIdx    int
	smsMu    sync.Mutex
	smsLease map[uint]*smsLeaseToken

	authorizeCodex func(context.Context, codexoauth.Input) (codexoauth.Tokens, error)
	acquirePhone   func(context.Context, uint) (*codexoauth.PhoneSession, error)
	newSMSClient   func(smsactivate.Config) (*smsactivate.Client, error)
}

func New(db *gorm.DB, mail *mailfetch.Client) *Producer {
	producer := &Producer{
		db: db, mail: mail, inflight: map[string]uint{}, failed: map[string]struct{}{}, smsLease: map[uint]*smsLeaseToken{},
		authorizeCodex: codexoauth.Authorize,
		newSMSClient:   func(config smsactivate.Config) (*smsactivate.Client, error) { return smsactivate.New(config) },
	}
	producer.acquirePhone = producer.acquireSMSPhone
	return producer
}

// Start 启动一次注册任务（异步）。已在运行则返回错误。
func (p *Producer) Start(target int, selected Scope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prog.Running {
		return fmt.Errorf("注册任务已在运行中")
	}
	if target < 1 {
		return fmt.Errorf("注册数量必须 ≥ 1")
	}
	scope := Scope{MailboxIDs: append([]uint(nil), selected.MailboxIDs...)}
	if selected.CategoryID != nil {
		categoryID := *selected.CategoryID
		scope.CategoryID = &categoryID
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.inflight = map[string]uint{}
	p.failed = map[string]struct{}{}
	p.pxMu.Lock()
	p.pxIdx = 0
	p.pxMu.Unlock()
	p.prog = Progress{Running: true, Target: target, Pending: target, Message: "初始化…", UpdatedAt: time.Now()}
	go p.run(ctx, target, scope)
	return nil
}

// Stop 请求停止（在跑的账号会跑完，不再开新的）。
func (p *Producer) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
}

// Snapshot 返回进度副本。
func (p *Producer) Snapshot() Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := p.prog
	cp.Logs = append([]string(nil), p.prog.Logs...)
	return cp
}

func (p *Producer) run(ctx context.Context, target int, scope Scope) {
	defer func() {
		p.mu.Lock()
		p.prog.Running = false
		p.recalcLocked()
		p.prog.UpdatedAt = time.Now()
		p.mu.Unlock()
	}()

	cfg := p.loadConfig()
	backendName := "Go Rod"
	if cfg.BrowserBackend == codexreg.BackendCloakBrowser {
		backendName = "Python CloakBrowser"
	}
	p.logf("开始注册，目标 %d 个账号（每邮箱母号+%d 裂变，并发 %d，浏览器 %s）", target, cfg.FissionCount, cfg.MaxConcurrency, backendName)

	sem := make(chan struct{}, cfg.MaxConcurrency)
	var wg sync.WaitGroup

	for {
		if ctx.Err() != nil {
			p.logf("已手动停止")
			break
		}
		// 目标只统计"本次生产新产出 + 在跑"，不受库里历史已注册数影响，
		// 否则库里已有 N 个已注册时，再要求生产 ≤N 个会被误判为已完成而不跑。
		done := p.producedThisRun()
		running := p.inflightCount()
		if done+running >= target {
			if running == 0 {
				break
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}

		mb, email, isMother, ok := p.nextJob(cfg, scope)
		if !ok {
			// 暂无可开的新任务：若还有在跑的，等它们（可能失败后要补），否则容量耗尽
			if p.inflightCount() == 0 {
				p.logf("没有更多可用邮箱容量，本次已产出 %d 个", p.producedThisRun())
				break
			}
			time.Sleep(800 * time.Millisecond)
			continue
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(mb models.Mailbox, email string, isMother bool) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				p.releaseInflight(email)
				p.updateProgress()
			}()
			// 兜底：rod 的 MustXxx 会 panic，若不 recover 会连累整个进程崩溃（宕机）。
			// 把 panic 归一成一次注册失败，服务继续存活。
			defer func() {
				if r := recover(); r != nil {
					p.markFailed(email)
					msg := fmt.Sprintf("注册异常(panic): %v", r)
					// log 传空，保留 appendLog 实时写入的账号日志
					p.setRegistrationFailed(email, msg, "")
					p.logf("✗ %s %s\n%s", mask(email), msg, debug.Stack())
					p.updateProgress()
				}
			}()
			p.updateProgress()

			if err := p.produceOne(ctx, cfg, mb, email, isMother); err != nil {
				if errors.Is(err, codexreg.ErrAccountTaken) {
					// 账号停用不再重试，也不计为“失败”（属于跳过换号）
					p.logf("⚠ %s 停用（%v），不再重试，换下一个地址", mask(email), err)
				} else {
					// 记为失败态；若后续重试成功会被 markSuccess 清除
					p.markFailed(email)
					p.logf("✗ %s 注册失败：%v", mask(email), err)
				}
			} else {
				p.markSuccess(email)
				p.incRegistered()
				p.logf("✓ %s 注册成功", mask(email))
			}
			p.updateProgress()
		}(mb, email, isMother)
	}

	wg.Wait()
	produced := p.producedThisRun()
	if ctx.Err() != nil {
		p.setMessage(fmt.Sprintf("已停止，本次成功 %d 个", produced))
	} else {
		p.setMessage(fmt.Sprintf("已完成，本次成功 %d 个", produced))
	}
}

// nextJob 领取下一个要注册的账号：先在选定邮箱中补齐母号，再开裂变子号。
// 每个邮箱同一时刻只允许一个在跑任务（避免验证码串号，也保证母号先行）。
func (p *Producer) nextJob(cfg Config, scope Scope) (models.Mailbox, string, bool, bool) {
	p.claimMu.Lock()
	defer p.claimMu.Unlock()

	query := p.db.Where("status = ?", "verified")
	if scope.CategoryID != nil {
		query = query.Where("category_id = ?", *scope.CategoryID)
	}
	if len(scope.MailboxIDs) > 0 {
		query = query.Where("id IN ?", scope.MailboxIDs)
	}
	var mailboxes []models.Mailbox
	if err := query.Order("id asc").Find(&mailboxes).Error; err != nil {
		return models.Mailbox{}, "", false, false
	}

	// Pass 1：母号（邮箱本身地址）未注册成功且该邮箱空闲 → 注册母号
	for _, mb := range mailboxes {
		if p.mailboxBusy(mb.ID) {
			continue
		}
		if !p.isRegistered(mb.Email) {
			p.markInflight(mb.Email, mb.ID)
			return mb, mb.Email, true, true
		}
	}

	// Pass 2：母号已成功、该邮箱空闲、裂变未满 → 注册一个新的别名子号
	for _, mb := range mailboxes {
		if p.mailboxBusy(mb.ID) {
			continue
		}
		if !p.isRegistered(mb.Email) {
			continue
		}
		if p.fissionCount(mb) >= cfg.FissionCount {
			continue
		}
		alias := p.nextFissionEmail(mb.Email)
		if alias == "" {
			continue
		}
		p.markInflight(alias, mb.ID)
		return mb, alias, false, true
	}
	return models.Mailbox{}, "", false, false
}

// produceOne 完整生产一个账号：注册 ChatGPT → 获取 accessToken → 入库。
func mailAccount(mailbox models.Mailbox) mailfetch.Account {
	return mailfetch.Account{
		Email: mailbox.Email, Provider: mailbox.Provider, ClientID: mailbox.ClientID,
		RefreshToken: mailbox.RefreshToken, CodeURL: mailbox.CodeURL,
	}
}

func (p *Producer) produceOne(ctx context.Context, cfg Config, mb models.Mailbox, email string, isMother bool) error {
	password := codexreg.GenPassword(16)
	accountProxy := p.nextProxy(cfg)
	note := ""
	if !isMother {
		note = "裂变(" + mb.Email + ")"
	}
	p.upsert(models.Registration{
		Email: email, MailboxID: mb.ID, Password: password, Proxy: accountProxy,
		Status: "registering", IsMother: isMother, Note: note,
	})

	var logMu sync.Mutex
	var logBuf strings.Builder
	var existing models.Registration
	if err := p.db.Select("log").Where("email = ?", email).First(&existing).Error; err == nil && strings.TrimSpace(existing.Log) != "" {
		logBuf.WriteString(existing.Log)
		if !strings.HasSuffix(existing.Log, "\n") {
			logBuf.WriteString("\n")
		}
		logBuf.WriteString(time.Now().Format("2006-01-02 15:04:05"))
		logBuf.WriteString(" --- 新一轮注册尝试 ---\n")
	}
	appendLog := func(line string) {
		logMu.Lock()
		logBuf.WriteString(time.Now().Format("2006-01-02 15:04:05"))
		logBuf.WriteString(" ")
		logBuf.WriteString(line)
		logBuf.WriteString("\n")
		snapshot := logBuf.String()
		logMu.Unlock()
		// 实时写库，注册中的账号也能在弹窗里看到执行日志
		p.db.Model(&models.Registration{}).Where("email = ?", email).Update("log", snapshot)
	}

	since := time.Now().Add(-30 * time.Second)
	ignoredMessageIDs := map[string]struct{}{}
	account := mailAccount(mb)
	if existingMessages, snapshotErr := p.mail.ListMessages(ctx, account, 30); snapshotErr != nil {
		appendLog("⚠ 读取注册前邮件快照失败，将仅按邮件时间过滤旧验证码: " + snapshotErr.Error())
		since = time.Now()
	} else {
		for _, message := range existingMessages {
			ignoredMessageIDs[message.ID] = struct{}{}
		}
	}
	appendLog("浏览器后端: " + cfg.BrowserBackend)
	in := codexreg.Input{
		Email:            email,
		Password:         password,
		Proxy:            accountProxy,
		Headless:         cfg.Headless,
		Backend:          cfg.BrowserBackend,
		PythonExecutable: cfg.PythonExecutable,
		Log: func(f string, a ...any) {
			msg := fmt.Sprintf(f, a...)
			appendLog(msg)
			p.logf("%s", "  "+mask(email)+" "+msg)
		},
		FetchCode: func(ctx context.Context) (string, error) {
			return p.fetchCodeAfter(ctx, mb, since, ignoredMessageIDs)
		},
		SaveShot: func(png []byte) {
			p.db.Model(&models.Registration{}).Where("email = ?", email).Update("shot", png)
		},
	}
	res, err := codexreg.Register(ctx, in)
	if err != nil {
		if errors.Is(err, codexreg.ErrAccountTaken) {
			appendLog("⚠ 停用（账号不存在或已被删除/停用），不再重试，换下一个地址继续")
			p.setRegistrationStatus(email, "already_registered", "停用："+err.Error(), logBuf.String())
			return err
		}
		appendLog("✗ 失败: " + err.Error())
		p.setRegistrationFailed(email, err.Error(), logBuf.String())
		return err
	}

	appendLog("✓ 注册成功")
	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	now := time.Now()
	_, expiresAt, _ := codexreg.AccessTokenDetails(res.AccessToken)
	p.upsert(models.Registration{
		Email: email, MailboxID: mb.ID, Password: password, Proxy: accountProxy,
		Status: "registered", IsMother: isMother, Note: note,
		AuthData: string(authBytes), AccountID: res.AccountID,
		UserID: res.UserID, PlanType: res.PlanType, ATStatus: "valid",
		ATCheckedAt: &now, ATExpiresAt: expiresAt, Log: logBuf.String(),
	})
	values, configErr := integrationcfg.Load(p.db)
	if configErr != nil {
		appendLog("⚠ 读取后置集成配置失败: " + configErr.Error())
		return nil
	}
	if values.CodexAutoAuthorize() {
		var registration models.Registration
		if err := p.db.Select("id").Where("email = ?", email).First(&registration).Error; err != nil {
			appendLog("⚠ Codex OAuth 自动授权跳过: " + err.Error())
			return nil
		}
		if _, err := p.AuthorizeCodex(ctx, registration.ID); err != nil {
			p.logf("⚠ %s ChatGPT 已注册，Codex OAuth 自动授权失败：%v", mask(email), err)
		}
	}
	return nil
}

// fetchCode 轮询邮箱，从 OpenAI/ChatGPT 验证邮件里提取 6 位验证码。
func (p *Producer) fetchCode(ctx context.Context, mb models.Mailbox, since time.Time) (string, error) {
	return p.fetchCodeAfter(ctx, mb, since, nil)
}

func (p *Producer) fetchCodeAfter(ctx context.Context, mb models.Mailbox, since time.Time, ignoredIDs map[string]struct{}) (string, error) {
	if ignoredIDs == nil {
		ignoredIDs = map[string]struct{}{}
	}
	acc := mailAccount(mb)
	deadline := time.Now().Add(codePollTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := p.mail.ListMessages(ctx, acc, 15)
		if err == nil {
			for _, m := range msgs {
				if _, ignored := ignoredIDs[m.ID]; ignored || m.ReceivedAt.Before(since) || !looksLikeOpenAI(m) {
					continue
				}
				if code := codeRe.FindStringSubmatch(m.Subject); code != nil {
					ignoredIDs[m.ID] = struct{}{}
					return code[1], nil
				}
				full, gerr := p.mail.GetMessage(ctx, acc, m.ID)
				if gerr != nil {
					continue
				}
				if code := codeRe.FindStringSubmatch(full.Subject + " " + full.Text); code != nil {
					ignoredIDs[m.ID] = struct{}{}
					return code[1], nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(codePollInterval):
		}
	}
	return "", fmt.Errorf("超时未收到验证码邮件")
}

func looksLikeOpenAI(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "openai") || strings.Contains(s, "chatgpt") || strings.Contains(s, "code")
}

// ---- inflight / 计数 ----

func (p *Producer) markInflight(email string, mbID uint) {
	p.mu.Lock()
	p.inflight[email] = mbID
	p.mu.Unlock()
}

func (p *Producer) releaseInflight(email string) {
	p.mu.Lock()
	delete(p.inflight, email)
	p.mu.Unlock()
}

func (p *Producer) inflightCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inflight)
}

// mailboxBusy 该邮箱是否已有在跑任务。调用方需持有 claimMu。
func (p *Producer) mailboxBusy(mbID uint) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range p.inflight {
		if id == mbID {
			return true
		}
	}
	return false
}

// counts 返回(已注册成功数, 在跑数)。
func (p *Producer) counts() (int, int) {
	registered := p.registeredCount()
	return registered, p.inflightCount()
}

// producedThisRun 返回本次生产任务已成功产出的账号数（不含库里历史已注册）。
func (p *Producer) producedThisRun() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prog.Registered
}

func (p *Producer) registeredCount() int {
	var n int64
	p.db.Model(&models.Registration{}).Where("status = ?", "registered").Count(&n)
	return int(n)
}

// isRegistered 该地址是否已终结（注册成功或已被他人注册），不再对其发起新注册。
func (p *Producer) isRegistered(email string) bool {
	var n int64
	p.db.Model(&models.Registration{}).Where("email = ? AND status IN ?", email, []string{"registered", "already_registered"}).Count(&n)
	return n > 0
}

// fissionCount 该邮箱已注册成功或在跑的裂变子号数量（不含母号）。
func (p *Producer) fissionCount(mb models.Mailbox) int {
	var n int64
	q := p.db.Model(&models.Registration{}).
		Where("mailbox_id = ? AND status IN ? AND email <> ?", mb.ID, []string{"registered", "already_registered"}, mb.Email)
	q.Count(&n)
	count := int(n)
	// 加上该邮箱在跑的裂变
	p.mu.Lock()
	for email, id := range p.inflight {
		if id == mb.ID && email != mb.Email {
			count++
		}
	}
	p.mu.Unlock()
	return count
}

func (p *Producer) nextFissionEmail(base string) string {
	for range 999 {
		email := emailalias.Address(base, emailalias.RandomSuffix(8))
		if email == base {
			return ""
		}
		p.mu.Lock()
		_, busy := p.inflight[email]
		p.mu.Unlock()
		if busy {
			continue
		}
		if !p.registrationExists(email) {
			return email
		}
	}
	return ""
}

func (p *Producer) registrationExists(email string) bool {
	var n int64
	p.db.Model(&models.Registration{}).Where("email = ?", email).Count(&n)
	return n > 0
}

// ---- DB / 设置 ----

func (p *Producer) loadConfig() Config {
	browserBackend := p.getSetting("browser_backend")
	if browserBackend == "" {
		browserBackend = codexreg.BackendRod
	}
	cfg := Config{
		MaxConcurrency:   atoiDefault(p.getSetting("max_concurrency"), defaultMaxConcurrency),
		FissionCount:     atoiDefault(p.getSetting("fission_count"), defaultFissionCount),
		Headless:         p.getSetting("headless") != "0", // 默认无头，仅当设置为 "0" 时才有头
		BrowserBackend:   browserBackend,
		PythonExecutable: p.getSetting("python_executable"),
	}
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	if cfg.FissionCount < 0 {
		cfg.FissionCount = defaultFissionCount
	}
	if p.getSetting("proxy_enabled") == "1" {
		cfg.Proxies = proxyList(p.getSetting("proxy_list"))
	}
	return cfg
}

// nextProxy 从代理池按轮转取一个；池为空返回空串（直连）。
func (p *Producer) nextProxy(cfg Config) string {
	if len(cfg.Proxies) == 0 {
		return ""
	}
	p.pxMu.Lock()
	proxy := cfg.Proxies[p.pxIdx%len(cfg.Proxies)]
	p.pxIdx++
	p.pxMu.Unlock()
	return proxy
}

func (p *Producer) upsert(reg models.Registration) {
	var existing models.Registration
	if err := p.db.Where("email = ?", reg.Email).First(&existing).Error; err == nil {
		updates := map[string]any{
			"password": reg.Password, "status": reg.Status,
			"is_mother": reg.IsMother, "note": reg.Note, "mailbox_id": reg.MailboxID,
			"proxy": reg.Proxy,
		}
		if reg.AuthData != "" {
			updates["auth_data"] = reg.AuthData
			updates["account_id"] = reg.AccountID
			updates["user_id"] = reg.UserID
			updates["plan_type"] = reg.PlanType
			updates["at_status"] = reg.ATStatus
			updates["at_error"] = reg.ATError
			updates["at_checked_at"] = reg.ATCheckedAt
			updates["at_expires_at"] = reg.ATExpiresAt
		}
		if reg.Log != "" {
			updates["log"] = reg.Log
		}
		p.db.Model(&existing).Updates(updates)
		return
	}
	p.db.Create(&reg)
}

func (p *Producer) setRegistrationFailed(email, note, log string) {
	p.setRegistrationStatus(email, "register_failed", note, log)
}

func (p *Producer) setRegistrationStatus(email, status, note, log string) {
	upd := map[string]any{"status": status, "note": truncateStr(note, 500)}
	if log != "" { // 为空时保留已实时写入的账号日志，不覆盖
		upd["log"] = log
	}
	p.db.Model(&models.Registration{}).Where("email = ?", email).Updates(upd)
}

func (p *Producer) getSetting(key string) string {
	var s models.Setting
	if err := p.db.Where("key = ?", key).First(&s).Error; err != nil {
		return ""
	}
	return s.Value
}

// ---- 进度 ----

func (p *Producer) logf(format string, a ...any) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + fmt.Sprintf(format, a...)
	p.mu.Lock()
	p.prog.Logs = append(p.prog.Logs, line)
	if len(p.prog.Logs) > maxLogLines {
		p.prog.Logs = p.prog.Logs[len(p.prog.Logs)-maxLogLines:]
	}
	p.prog.UpdatedAt = time.Now()
	p.mu.Unlock()
}

func (p *Producer) incRegistered() {
	p.mu.Lock()
	p.prog.Registered++
	p.recalcLocked()
	p.mu.Unlock()
}

// markFailed 把邮箱标记为失败态，失败数=仍处于失败态的邮箱数。
func (p *Producer) markFailed(email string) {
	p.mu.Lock()
	p.failed[email] = struct{}{}
	p.prog.Failed = len(p.failed)
	p.mu.Unlock()
}

// markSuccess 邮箱最终注册成功，从失败态移除（重试成功不再计入失败）。
func (p *Producer) markSuccess(email string) {
	p.mu.Lock()
	delete(p.failed, email)
	p.prog.Failed = len(p.failed)
	p.mu.Unlock()
}

func (p *Producer) updateProgress() {
	p.mu.Lock()
	p.recalcLocked()
	p.mu.Unlock()
}

// recalcLocked 重新计算 待生产/在跑，调用方需持锁。
func (p *Producer) recalcLocked() {
	p.prog.RunningNum = len(p.inflight)
	pending := p.prog.Target - p.prog.Registered - p.prog.RunningNum
	if pending < 0 {
		pending = 0
	}
	p.prog.Pending = pending
	p.prog.UpdatedAt = time.Now()
}

func (p *Producer) setMessage(msg string) {
	p.mu.Lock()
	p.prog.Message = msg
	p.prog.UpdatedAt = time.Now()
	p.mu.Unlock()
}
