package service

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const codexAccountTicketConfigKey = "codex_ticket_config"

// 每轮采集最多尝试次数与失败后的冷却时间。票据 1 小时有效、到期前 10 分钟开始续期，
// 原来的 8 次 + 5 分钟冷却在续期窗口里只能跑两轮，坏运气时票会过期出现调度空隙；
// 改成 30 次 + 1 分钟冷却后窗口内可跑约 75 次。401/403/429 仍会立即终止整轮。
const codexTicketMaxAttempts = 30
const codexTicketRetryCooldown = time.Minute

// 同一轮内两次尝试之间的间隔；测试里会调小，避免 30 次尝试真的等 30 秒。
var codexTicketAttemptSpacing = time.Second

const (
	codexTicketPlanPro  = "pro"
	codexTicketPlanTeam = "team"
)

// codexTicketAllowedModels 是账号可勾选的目标模型。每个模型独立持票：
// 各自采集、各自经固定代理复验、各自注入；任一已勾选模型缺票时，只暂停该模型
// 到本账号的调度，其余模型不受影响。
var codexTicketAllowedModels = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}

func codexTicketModelAllowed(model string) bool {
	for _, m := range codexTicketAllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

// normalizeCodexTicketModels 去空、去重并只保留允许的模型；为空时回退到 legacy 单模型，
// 再为空回退默认模型。输出顺序固定按 codexTicketAllowedModels，便于比较与展示。
func normalizeCodexTicketModels(models []string, legacy string) []string {
	want := map[string]struct{}{}
	for _, m := range models {
		if m = normalizeOpenAICodexTicketModel(m); codexTicketModelAllowed(m) {
			want[m] = struct{}{}
		}
	}
	if len(want) == 0 {
		if legacy = normalizeOpenAICodexTicketModel(legacy); codexTicketModelAllowed(legacy) {
			want[legacy] = struct{}{}
		}
	}
	if len(want) == 0 {
		want[openAICodexTicketDefaultModel] = struct{}{}
	}
	out := make([]string, 0, len(want))
	for _, m := range codexTicketAllowedModels {
		if _, ok := want[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

func sameCodexTicketModels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// This is a manual account setting, never inferred from subscription metadata.
func codexTicketTargetLength(plan string) int {
	switch plan {
	case "", codexTicketPlanPro:
		return 292
	case codexTicketPlanTeam:
		return 332
	default:
		return 0
	}
}

// This key is server managed and is never accepted through general account edits.
type codexAccountTicketConfig struct {
	TicketPlan string   `json:"ticket_plan"`
	Enabled    bool     `json:"enabled"`
	Model      string   `json:"model"`               // 首个目标模型；兼容旧记录与旧读取方
	Models     []string `json:"models,omitempty"`    // 全部目标模型，每个模型独立持票
	ProxyURL   string   `json:"proxy_url,omitempty"` // Legacy data only; harvesting always uses the global pool.
	Revision   string   `json:"revision"`
}

func (c codexAccountTicketConfig) models() []string {
	return normalizeCodexTicketModels(c.Models, c.Model)
}

func (c codexAccountTicketConfig) hasModel(model string) bool {
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		return false
	}
	for _, m := range c.models() {
		if m == model {
			return true
		}
	}
	return false
}

type CodexAccountTicketUpdate struct {
	TicketPlan string   `json:"ticket_plan"`
	Enabled    bool     `json:"enabled"`
	ProxyURL   string   `json:"proxy_url"`
	Model      string   `json:"model"`  // 单模型写法（兼容）
	Models     []string `json:"models"` // 多模型写法；非空时优先
	ClearProxy bool     `json:"clear_proxy"`
}

// CodexAccountTicketModelStatus 是单个目标模型的票据状态。
type CodexAccountTicketModelStatus struct {
	Model            string     `json:"model"`
	State            string     `json:"state"`
	TicketUsable     bool       `json:"ticket_usable"`
	Refreshing       bool       `json:"refreshing"`
	CapturedAt       *time.Time `json:"captured_at,omitempty"`
	RetryAfter       *time.Time `json:"retry_after,omitempty"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	LastError        string     `json:"last_error"`
	Attempts         int        `json:"attempts"`
}

// CodexAccountTicketStatus 顶层字段是全部目标模型的汇总（全部可用才算 ready），
// ModelStatuses 给出逐模型明细。
type CodexAccountTicketStatus struct {
	Watchdog             CodexTicketWatchdogStatus       `json:"watchdog"`
	TicketPlan           string                          `json:"ticket_plan"`
	TargetLength         int                             `json:"target_length"`
	Enabled              bool                            `json:"enabled"`
	GlobalEnabled        bool                            `json:"global_enabled"`
	Model                string                          `json:"model"`
	Models               []string                        `json:"models"`
	ModelStatuses        []CodexAccountTicketModelStatus `json:"model_statuses"`
	ProxyConfigured      bool                            `json:"proxy_configured"`
	ProxyDisplay         string                          `json:"proxy_display"`
	FixedProxyConfigured bool                            `json:"fixed_proxy_configured"`
	State                string                          `json:"state"`
	TicketUsable         bool                            `json:"ticket_usable"`
	Refreshing           bool                            `json:"refreshing"`
	CapturedAt           *time.Time                      `json:"captured_at,omitempty"`
	RetryAfter           *time.Time                      `json:"retry_after,omitempty"`
	RemainingSeconds     int64                           `json:"remaining_seconds"`
	ExpiresAt            *time.Time                      `json:"expires_at,omitempty"`
	LastError            string                          `json:"last_error"`
	Attempts             int                             `json:"attempts"`
}

type codexAccountTicketJob struct {
	model            string
	revision         string
	fixedFingerprint string
	harvestProxyURL  string // Immutable global pool snapshot for this job; never returned to clients.
	cancel           context.CancelFunc
	done             chan struct{}
	running          bool
	attempts         int
	lastError        string
	retryAfter       time.Time
}

// 采集任务按「账号 × 模型」登记，键与票据键同构。
func codexTicketJobKey(accountID int64, model string) string {
	return openAICodexTicketKey(accountID, model)
}

func codexTicketJobKeyPrefix(accountID int64) string {
	return fmt.Sprintf("%d\x00", accountID)
}

func defaultCodexAccountTicketConfig() codexAccountTicketConfig {
	return codexAccountTicketConfig{Model: openAICodexTicketDefaultModel, Models: []string{openAICodexTicketDefaultModel}, TicketPlan: codexTicketPlanPro}
}

func codexAccountTicketConfigOf(account *Account) codexAccountTicketConfig {
	if account == nil || account.Extra == nil {
		return defaultCodexAccountTicketConfig()
	}
	raw, err := json.Marshal(account.Extra[codexAccountTicketConfigKey])
	if err != nil {
		return defaultCodexAccountTicketConfig()
	}
	var out codexAccountTicketConfig
	if err = json.Unmarshal(raw, &out); err != nil {
		return defaultCodexAccountTicketConfig()
	}
	out.Models = normalizeCodexTicketModels(out.Models, out.Model)
	out.Model = out.Models[0]
	if out.TicketPlan == "" {
		out.TicketPlan = codexTicketPlanPro
	}
	if codexTicketTargetLength(out.TicketPlan) == 0 {
		out.Enabled = false
	}
	// An incomplete or imported legacy blob must never opt an account in.
	if out.Revision == "" {
		out.Enabled = false
	}
	return out
}

func codexAccountTicketEligible(account *Account) bool {
	return isOpenAICodexTicketAccount(account) && account.Status == StatusActive && account.Proxy != nil && account.ProxyID != nil
}

func codexTicketFixedProxyFingerprint(account *Account) string {
	if account == nil || account.Proxy == nil || account.ProxyID == nil {
		return ""
	}
	raw := fmt.Sprintf("%d\x00%d\x00%s\x00%v", account.ID, *account.ProxyID, account.Proxy.URL(), account.Credentials["chatgpt_account_id"])
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}

func (s *OpenAIGatewayService) codexTicketLiveAccount(ctx context.Context, account *Account) (*Account, error) {
	if account == nil {
		return nil, errors.New("account unavailable")
	}
	if s.accountRepo == nil {
		return account, nil
	}
	// Repository lookup prevents stale scheduler snapshots from re-enabling disabled tickets.
	live, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil || live == nil {
		return nil, errors.New("account unavailable")
	}
	return live, nil
}
func (s *OpenAIGatewayService) codexTicketAccountByID(ctx context.Context, id int64) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, apperrors.New(503, "CODEX_TICKET_UNAVAILABLE", "Ticket service is unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil || account == nil {
		return nil, apperrors.New(404, "ACCOUNT_NOT_FOUND", "Account not found")
	}
	if !isOpenAICodexTicketAccount(account) {
		return nil, apperrors.BadRequest("CODEX_TICKET_ACCOUNT", "STATE tickets require a non-shadow OpenAI OAuth account")
	}
	return account, nil
}

func (s *OpenAIGatewayService) GetCodexAccountTicketStatus(ctx context.Context, id int64) (*CodexAccountTicketStatus, error) {
	account, err := s.codexTicketAccountByID(ctx, id)
	if err != nil {
		return nil, err
	}
	ac := codexAccountTicketConfigOf(account)
	models := ac.models()
	pool := s.openAICodexTicketHarvestProxyURLContext(ctx)
	poolConfigured := pool != "" && ValidateOpenAICodexTicketHarvestProxyURL(pool) == nil
	status := &CodexAccountTicketStatus{TicketPlan: ac.TicketPlan, TargetLength: codexTicketTargetLength(ac.TicketPlan), Enabled: ac.Enabled, GlobalEnabled: s.openAICodexTicketEnabledContext(ctx), Model: models[0], Models: models, ProxyConfigured: poolConfigured, FixedProxyConfigured: account.Proxy != nil && account.ProxyID != nil, State: "waiting"}
	status.Watchdog = codexTicketWatchdogStatusOf(account, ac.Enabled && status.GlobalEnabled)
	if parsed, err := url.Parse(strings.ReplaceAll(pool, "{sid}", "%7Bsid%7D")); err == nil {
		status.ProxyDisplay = parsed.Host
	}
	if !ac.Enabled {
		status.State = "disabled"
		return status, nil
	}
	if !status.GlobalEnabled {
		status.State = "global_disabled"
		return status, nil
	}
	now := time.Now()
	status.ModelStatuses = make([]CodexAccountTicketModelStatus, 0, len(models))
	for _, model := range models {
		ms := CodexAccountTicketModelStatus{Model: model, State: "waiting"}
		if ticket := s.lookupOpenAICodexTicket(account, model); ticket.validFor(account, ac, now) {
			ms.State = "ready"
			ms.TicketUsable = true
			captured := ticket.CapturedAt
			ms.CapturedAt = &captured
			ms.RemainingSeconds = int64(ticket.ExpiresAt.Sub(now) / time.Second)
			expiry := ticket.ExpiresAt
			ms.ExpiresAt = &expiry
		}
		status.ModelStatuses = append(status.ModelStatuses, ms)
	}
	fingerprint := codexTicketFixedProxyFingerprint(account)
	s.openaiCodexAccountMu.Lock()
	for i := range status.ModelStatuses {
		ms := &status.ModelStatuses[i]
		job := s.openaiCodexAccountJobs[codexTicketJobKey(id, ms.Model)]
		if job == nil || job.revision != ac.Revision || job.harvestProxyURL != pool || job.fixedFingerprint != fingerprint {
			continue
		}
		ms.Attempts = job.attempts
		ms.LastError = job.lastError
		if job.running {
			ms.State = "harvesting"
			ms.Refreshing = ms.TicketUsable
		} else if job.lastError != "" && ms.State != "ready" {
			ms.State = "error"
		}
		if !job.running && now.Before(job.retryAfter) {
			retryAfter := job.retryAfter
			ms.RetryAfter = &retryAfter
		}
	}
	s.openaiCodexAccountMu.Unlock()
	aggregateCodexAccountTicketStatus(status)
	if !poolConfigured && !status.TicketUsable {
		status.State = "error"
		status.LastError = "Configure the global dynamic proxy pool in gateway settings"
	}
	if !codexAccountTicketEligible(account) {
		status.State = "error"
		status.TicketUsable = false
		status.Refreshing = false
		status.CapturedAt = nil
		status.ExpiresAt = nil
		status.RemainingSeconds = 0
		status.LastError = "Account must be active and have a fixed business proxy"
		for i := range status.ModelStatuses {
			ms := &status.ModelStatuses[i]
			ms.State = "error"
			ms.TicketUsable = false
			ms.Refreshing = false
			ms.CapturedAt = nil
			ms.ExpiresAt = nil
			ms.RemainingSeconds = 0
			ms.LastError = status.LastError
		}
	}
	return status, nil
}

// aggregateCodexAccountTicketStatus 把逐模型状态折叠到顶层：全部可用才 ready，任一在采集
// 即 harvesting，任一失败且未全部可用即 error；剩余时间取最早到期的模型。
func aggregateCodexAccountTicketStatus(status *CodexAccountTicketStatus) {
	if status == nil || len(status.ModelStatuses) == 0 {
		return
	}
	allUsable, anyHarvesting, anyError, anyRefreshing := true, false, false, false
	var errs []string
	maxAttempts := 0
	for i := range status.ModelStatuses {
		ms := &status.ModelStatuses[i]
		if !ms.TicketUsable {
			allUsable = false
		}
		if ms.State == "harvesting" {
			anyHarvesting = true
		}
		if ms.State == "error" {
			anyError = true
		}
		if ms.Refreshing {
			anyRefreshing = true
		}
		if ms.LastError != "" {
			if len(status.ModelStatuses) == 1 {
				errs = append(errs, ms.LastError)
			} else {
				errs = append(errs, ms.Model+": "+ms.LastError)
			}
		}
		if ms.Attempts > maxAttempts {
			maxAttempts = ms.Attempts
		}
		if ms.TicketUsable {
			if status.ExpiresAt == nil || (ms.ExpiresAt != nil && ms.ExpiresAt.Before(*status.ExpiresAt)) {
				status.ExpiresAt = ms.ExpiresAt
				status.RemainingSeconds = ms.RemainingSeconds
			}
			if status.CapturedAt == nil || (ms.CapturedAt != nil && ms.CapturedAt.After(*status.CapturedAt)) {
				status.CapturedAt = ms.CapturedAt
			}
		}
		if ms.RetryAfter != nil && (status.RetryAfter == nil || ms.RetryAfter.Before(*status.RetryAfter)) {
			status.RetryAfter = ms.RetryAfter
		}
	}
	status.TicketUsable = allUsable
	status.Refreshing = anyRefreshing
	status.Attempts = maxAttempts
	status.LastError = strings.Join(errs, "; ")
	switch {
	case anyHarvesting:
		status.State = "harvesting"
	case anyError && !allUsable:
		status.State = "error"
	case allUsable:
		status.State = "ready"
	default:
		status.State = "waiting"
	}
}

func (s *OpenAIGatewayService) ConfigureCodexAccountTicket(ctx context.Context, id int64, input CodexAccountTicketUpdate) (*CodexAccountTicketStatus, error) {
	s.openaiCodexAccountMu.Lock()
	account, err := s.codexTicketAccountByID(ctx, id)
	if err != nil {
		s.openaiCodexAccountMu.Unlock()
		return nil, err
	}
	old := codexAccountTicketConfigOf(account)
	next := old
	next.Enabled = input.Enabled
	if input.TicketPlan != "" {
		next.TicketPlan = strings.ToLower(strings.TrimSpace(input.TicketPlan))
	}
	if next.TicketPlan != codexTicketPlanPro && next.TicketPlan != codexTicketPlanTeam {
		s.openaiCodexAccountMu.Unlock()
		return nil, apperrors.BadRequest("CODEX_TICKET_PLAN", "Ticket plan must be pro (292) or team (332)")
	}
	requested := input.Models
	if len(requested) == 0 && strings.TrimSpace(input.Model) != "" {
		requested = []string{input.Model}
	}
	if len(requested) > 0 {
		valid := 0
		for _, m := range requested {
			m = normalizeOpenAICodexTicketModel(m)
			if m == "" {
				continue
			}
			if !codexTicketModelAllowed(m) {
				s.openaiCodexAccountMu.Unlock()
				return nil, apperrors.BadRequest("CODEX_TICKET_MODEL", "Ticket models must be gpt-6-astra and/or gpt-5.6-sol")
			}
			valid++
		}
		if valid == 0 {
			s.openaiCodexAccountMu.Unlock()
			return nil, apperrors.BadRequest("CODEX_TICKET_MODEL", "Select at least one ticket model")
		}
		next.Models = normalizeCodexTicketModels(requested, "")
	}
	next.Models = normalizeCodexTicketModels(next.Models, next.Model)
	next.Model = next.Models[0]
	if input.ClearProxy || strings.TrimSpace(input.ProxyURL) != "" {
		s.openaiCodexAccountMu.Unlock()
		return nil, apperrors.BadRequest("CODEX_TICKET_GLOBAL_PROXY", "Configure the dynamic proxy pool in gateway settings, not per account")
	}
	pool := s.openAICodexTicketHarvestProxyURLContext(ctx)
	if next.Enabled && (pool == "" || ValidateOpenAICodexTicketHarvestProxyURL(pool) != nil || account.Proxy == nil || account.ProxyID == nil) {
		s.openaiCodexAccountMu.Unlock()
		return nil, apperrors.BadRequest("CODEX_TICKET_PROXY_REQUIRED", "Configure the global dynamic proxy pool and this account's fixed business proxy first")
	}
	// Retire stored account overrides without invalidating an otherwise valid ticket.
	next.ProxyURL = ""
	oldModels, newModels := old.models(), next.models()
	// 套餐或开关变化会换 revision，作废全部票据；仅模型集合变化时保留未变模型的票据与 revision。
	revisionChanged := next.TicketPlan != old.TicketPlan || next.Enabled != old.Enabled || old.Revision == ""
	modelsChanged := !sameCodexTicketModels(oldModels, newModels)
	var startModels []string
	switch {
	case revisionChanged:
		next.Revision = uuid.NewString()
		updates := map[string]any{codexAccountTicketConfigKey: next, codexTicketWatchdogExtraKey: nil}
		for key := range account.Extra {
			if strings.HasPrefix(key, openAICodexTicketExtraKeyPrefix) {
				updates[key] = nil
			}
		}
		if err := s.accountRepo.UpdateExtra(ctx, id, updates); err != nil {
			s.openaiCodexAccountMu.Unlock()
			return nil, apperrors.New(500, "CODEX_TICKET_SAVE_FAILED", "Could not save account ticket settings")
		}
		s.cancelCodexTicketAccountJobsLocked(id)
		s.openaiCodexTickets.Range(func(key, value any) bool {
			if ticket, ok := value.(*openAICodexTicket); ok && ticket.AccountID == id {
				s.openaiCodexTickets.Delete(key)
			}
			return true
		})
		startModels = newModels
	case modelsChanged:
		updates := map[string]any{codexAccountTicketConfigKey: next}
		var removed []string
		for _, m := range oldModels {
			if !next.hasModel(m) {
				removed = append(removed, m)
				updates[openAICodexTicketExtraKey(m)] = nil
			}
		}
		if err := s.accountRepo.UpdateExtra(ctx, id, updates); err != nil {
			s.openaiCodexAccountMu.Unlock()
			return nil, apperrors.New(500, "CODEX_TICKET_SAVE_FAILED", "Could not save account ticket settings")
		}
		for _, m := range removed {
			s.cancelCodexTicketModelJobLocked(id, m)
			s.openaiCodexTickets.Delete(openAICodexTicketKey(id, m))
		}
		for _, m := range newModels {
			if !old.hasModel(m) {
				startModels = append(startModels, m)
			}
		}
	case old.ProxyURL != "":
		if err := s.accountRepo.UpdateExtra(ctx, id, map[string]any{codexAccountTicketConfigKey: next}); err != nil {
			s.openaiCodexAccountMu.Unlock()
			return nil, apperrors.New(500, "CODEX_TICKET_SAVE_FAILED", "Could not save account ticket settings")
		}
	}
	s.openaiCodexAccountMu.Unlock()
	if revisionChanged {
		s.InvalidateAgentIdentityWSConnections(id)
	}
	if next.Enabled && len(startModels) > 0 && s.openAICodexTicketEnabledContext(ctx) {
		for _, m := range startModels {
			s.startCodexAccountTicketJob(context.Background(), id, m, true)
		}
	}
	return s.GetCodexAccountTicketStatus(ctx, id)
}

func (s *OpenAIGatewayService) HarvestCodexAccountTicket(ctx context.Context, id int64) (*CodexAccountTicketStatus, error) {
	account, err := s.codexTicketAccountByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !s.openAICodexTicketEnabledContext(ctx) {
		return nil, apperrors.BadRequest("CODEX_TICKET_GLOBAL_DISABLED", "Enable the gateway STATE master switch first")
	}
	ac := codexAccountTicketConfigOf(account)
	if !ac.Enabled {
		return nil, apperrors.BadRequest("CODEX_TICKET_DISABLED", "Enable STATE tickets for this account first")
	}
	if !codexAccountTicketEligible(account) {
		return nil, apperrors.BadRequest("CODEX_TICKET_ACCOUNT_INACTIVE", "Account must be active and have a fixed business proxy")
	}
	pool := s.openAICodexTicketHarvestProxyURLContext(ctx)
	if pool == "" || ValidateOpenAICodexTicketHarvestProxyURL(pool) != nil {
		return nil, apperrors.BadRequest("CODEX_TICKET_GLOBAL_PROXY", "Configure the global dynamic proxy pool in gateway settings")
	}
	for _, m := range ac.models() {
		s.startCodexAccountTicketJob(context.Background(), id, m, true)
	}
	return s.GetCodexAccountTicketStatus(ctx, id)
}

func (s *OpenAIGatewayService) cancelCodexTicketJobsLocked() {
	for _, job := range s.openaiCodexAccountJobs {
		if job.cancel != nil {
			job.cancel()
		}
	}
}
func (s *OpenAIGatewayService) cancelCodexTicketJobs() {
	s.openaiCodexAccountMu.Lock()
	defer s.openaiCodexAccountMu.Unlock()
	s.cancelCodexTicketJobsLocked()
}

// cancelCodexTicketAccountJobsLocked 取消并移除某账号全部模型的采集任务。
func (s *OpenAIGatewayService) cancelCodexTicketAccountJobsLocked(id int64) {
	prefix := codexTicketJobKeyPrefix(id)
	for key, job := range s.openaiCodexAccountJobs {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if job.cancel != nil {
			job.cancel()
		}
		delete(s.openaiCodexAccountJobs, key)
	}
}

func (s *OpenAIGatewayService) cancelCodexTicketModelJobLocked(id int64, model string) {
	key := codexTicketJobKey(id, model)
	if job := s.openaiCodexAccountJobs[key]; job != nil {
		if job.cancel != nil {
			job.cancel()
		}
		delete(s.openaiCodexAccountJobs, key)
	}
}

func (s *OpenAIGatewayService) startCodexAccountTicketJob(ctx context.Context, id int64, model string, manual bool) *codexAccountTicketJob {
	model = normalizeOpenAICodexTicketModel(model)
	if s == nil || model == "" || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return nil
	}
	s.openaiCodexAccountMu.Lock()
	defer s.openaiCodexAccountMu.Unlock()
	if s.openaiCodexAccountStopping {
		return nil
	}
	account, err := s.codexTicketAccountByID(ctx, id)
	if err != nil || !codexAccountTicketEligible(account) {
		return nil
	}
	ac := codexAccountTicketConfigOf(account)
	pool := s.openAICodexTicketHarvestProxyURLContext(ctx)
	if !ac.Enabled || !ac.hasModel(model) || pool == "" || ValidateOpenAICodexTicketHarvestProxyURL(pool) != nil {
		return nil
	}
	if s.openaiCodexAccountJobs == nil {
		s.openaiCodexAccountJobs = make(map[string]*codexAccountTicketJob)
	}
	key := codexTicketJobKey(id, model)
	if job := s.openaiCodexAccountJobs[key]; job != nil {
		if job.running && job.revision == ac.Revision && job.fixedFingerprint == codexTicketFixedProxyFingerprint(account) && job.harvestProxyURL == pool {
			return job
		}
		if job.running && job.cancel != nil {
			job.cancel()
		}
		if !manual && job.revision == ac.Revision && job.harvestProxyURL == pool && time.Now().Before(job.retryAfter) {
			return nil
		}
	}
	// Own a detached job context; the request that clicked Save may finish immediately.
	s.openaiCodexTicketLifecycleMu.Lock()
	parentCtx := s.openaiCodexTicketContext
	s.openaiCodexTicketLifecycleMu.Unlock()
	if parentCtx == nil {
		parentCtx = context.WithoutCancel(ctx)
	}
	jobCtx, cancel := context.WithCancel(parentCtx)
	job := &codexAccountTicketJob{model: model, revision: ac.Revision, fixedFingerprint: codexTicketFixedProxyFingerprint(account), harvestProxyURL: pool, cancel: cancel, done: make(chan struct{}), running: true}
	s.openaiCodexAccountJobs[key] = job
	s.openaiCodexAccountWG.Add(1)
	go func() {
		defer cancel()
		defer s.openaiCodexAccountWG.Done()
		defer close(job.done)
		s.runCodexAccountTicketJob(jobCtx, id, model, job)
	}()
	return job
}

func (s *OpenAIGatewayService) runCodexAccountTicketJob(ctx context.Context, id int64, model string, job *codexAccountTicketJob) {
	lastError := "Unable to obtain a verified STATE ticket"
	defer func() {
		s.openaiCodexAccountMu.Lock()
		defer s.openaiCodexAccountMu.Unlock()
		job.running = false
		job.retryAfter = time.Time{}
		if ctx.Err() == nil && lastError != "" {
			job.retryAfter = time.Now().Add(codexTicketRetryCooldown)
		}
		if ctx.Err() != nil {
			job.lastError = ""
		} else {
			job.lastError = lastError
		}
	}()
	key := codexTicketJobKey(id, model)
	timeout := time.Duration(s.openAICodexTicketConfig().HarvestAttemptTimeoutSeconds) * time.Second
	for attempt := 1; attempt <= codexTicketMaxAttempts; attempt++ {
		if ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
			return
		}
		account, err := s.codexTicketAccountByID(ctx, id)
		if err != nil {
			return
		}
		ac := codexAccountTicketConfigOf(account)
		if !codexAccountTicketEligible(account) || !ac.Enabled || !ac.hasModel(model) || ac.Revision != job.revision || codexTicketFixedProxyFingerprint(account) != job.fixedFingerprint || s.openAICodexTicketHarvestProxyURLContext(ctx) != job.harvestProxyURL {
			return
		}
		// Token helpers are permitted to update metadata, but not shared account maps.
		account.Extra = maps.Clone(account.Extra)
		account.Credentials = maps.Clone(account.Credentials)
		s.openaiCodexAccountMu.Lock()
		job.attempts = attempt
		s.openaiCodexAccountMu.Unlock()
		token, _, err := s.GetAccessToken(ctx, account)
		if err != nil || token == "" {
			lastError = "Account authentication failed"
			return
		}
		harvestProxy := freshCodexTicketProxyURL(job.harvestProxyURL)
		state, status, err := s.fireCodexAccountTicketProbe(ctx, account, token, model, harvestProxy, "", timeout)
		if reason := codexTicketProbeRejection(status); reason != "" {
			lastError = reason
			return
		}
		if err != nil || status != 200 || !validCodexTicketState(state) {
			lastError = "Harvest did not return a completed target-model response and valid STATE"
		} else if len(state) != codexTicketTargetLength(ac.TicketPlan) {
			lastError = fmt.Sprintf("STATE length does not match selected %s plan (expected %d, received %d)", ac.TicketPlan, codexTicketTargetLength(ac.TicketPlan), len(state))
		} else {
			if ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) || s.openAICodexTicketHarvestProxyURLContext(ctx) != job.harvestProxyURL {
				return
			}
			replayState, status, err := s.fireCodexAccountTicketProbe(ctx, account, token, model, account.Proxy.URL(), state, timeout)
			if reason := codexTicketProbeRejection(status); reason != "" {
				lastError = reason
				return
			}
			if err == nil && status == 200 && !(len(replayState) == 312 && validCodexTicketState(replayState)) {
				// Serialize against account opt-out/source changes; reread persistent values immediately before publication.
				s.openaiCodexAccountMu.Lock()
				live, readErr := s.codexTicketAccountByID(ctx, id)
				if readErr == nil && ctx.Err() == nil && s.openaiCodexAccountJobs[key] == job && s.openAICodexTicketEnabledContext(ctx) && s.openAICodexTicketHarvestProxyURLContext(ctx) == job.harvestProxyURL && codexAccountTicketEligible(live) && codexAccountTicketConfigOf(live).Enabled && codexAccountTicketConfigOf(live).hasModel(model) && codexAccountTicketConfigOf(live).Revision == job.revision && codexTicketFixedProxyFingerprint(live) == job.fixedFingerprint {
					now := time.Now()
					ticket := &openAICodexTicket{AccountID: id, Model: model, State: state, Length: len(state), CapturedAt: now, ExpiresAt: now.Add(time.Hour), Attempts: attempt, Verified: true, ConfigRevision: job.revision, FixedProxyFingerprint: job.fixedFingerprint}
					s.storeOpenAICodexTicket(ctx, live, ticket)
					if got := s.lookupOpenAICodexTicket(live, model); got != nil && got.CapturedAt.Equal(now) {
						lastError = ""
					} else {
						lastError = "Could not save verified STATE"
					}
				}
				s.openaiCodexAccountMu.Unlock()
				return
			}
			lastError = "STATE did not preserve the target model on this account's fixed proxy"
		}
		if attempt < codexTicketMaxAttempts {
			timer := time.NewTimer(codexTicketAttemptSpacing)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

// Account authentication, access and rate-limit rejections end the entire round.
// Rotating harvest exits cannot resolve these reliably; retain any still-valid
// ticket and use the existing failure cooldown instead of spending more probes.
func codexTicketProbeRejection(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "Upstream rejected authentication (HTTP 401); acquisition paused for cooldown"
	case http.StatusForbidden:
		return "Upstream denied access (HTTP 403); acquisition paused for cooldown"
	case http.StatusTooManyRequests:
		return "Upstream rate limit (HTTP 429); acquisition paused for cooldown"
	default:
		return ""
	}
}

var codexTicketSIDPattern = regexp.MustCompile(`(?i)-sid-[^-]+(-t-[0-9]+)`)

func freshCodexTicketProxyURL(raw string) string {
	parsed, err := url.Parse(strings.ReplaceAll(raw, "{sid}", "%7Bsid%7D"))
	if err != nil || parsed.User == nil {
		return raw
	}
	username := parsed.User.Username()
	sid := strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	if strings.Contains(username, "{sid}") {
		username = strings.ReplaceAll(username, "{sid}", sid)
	} else if strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".1024proxy.io") || strings.EqualFold(parsed.Hostname(), "1024proxy.io") {
		username = codexTicketSIDPattern.ReplaceAllString(username, "-sid-"+sid+"${1}")
	}
	if password, ok := parsed.User.Password(); ok {
		parsed.User = url.UserPassword(username, password)
	} else {
		parsed.User = url.User(username)
	}
	return parsed.String()
}

func validateCodexTicketCompletedModel(body io.Reader, model string) error {
	if body == nil {
		return errors.New("missing completion")
	}
	scanner := bufio.NewScanner(io.LimitReader(body, 2<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	eventType := ""
	var data strings.Builder
	validate := func() (bool, error) {
		raw := strings.TrimSpace(data.String())
		if raw == "" {
			return false, nil
		}
		if !gjson.Valid(raw) {
			return false, errors.New("invalid completion")
		}
		typ := gjson.Get(raw, "type").String()
		if typ == "" {
			typ = eventType
		}
		if typ == "response.failed" || typ == "response.incomplete" || typ == "error" {
			return false, errors.New("incomplete response")
		}
		if typ != "response.completed" {
			return false, nil
		}
		if gjson.Get(raw, "response.model").String() != model {
			return false, errors.New("returned model differs from requested model")
		}
		status := gjson.Get(raw, "response.status").String()
		if status != "" && status != "completed" {
			return false, errors.New("incomplete response")
		}
		return true, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			okay, err := validate()
			if err != nil || okay {
				return err
			}
			data.Reset()
			eventType = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if okay, err := validate(); err != nil || okay {
		return err
	}
	return errors.New("response did not complete")
}

// normalizeCodexTicketDefaultPlan 规范化全局默认套餐：空 → pro；非法 → ""。
func normalizeCodexTicketDefaultPlan(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", codexTicketPlanPro:
		return codexTicketPlanPro
	case codexTicketPlanTeam:
		return codexTicketPlanTeam
	default:
		return ""
	}
}

func splitCodexTicketModelList(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' })
}

// SplitCodexTicketDefaultModels 宽松解析逗号分隔的默认模型：忽略非法项，空则回退默认模型。
func SplitCodexTicketDefaultModels(raw string) []string {
	return normalizeCodexTicketModels(splitCodexTicketModelList(raw), "")
}

// parseCodexTicketDefaultModelsStrict 严格解析：出现非法模型名即报错；空则回退默认模型。
func parseCodexTicketDefaultModelsStrict(raw string) ([]string, error) {
	for _, m := range splitCodexTicketModelList(raw) {
		if m = normalizeOpenAICodexTicketModel(m); m != "" && !codexTicketModelAllowed(m) {
			return nil, fmt.Errorf("default ticket models must be gpt-6-astra and/or gpt-5.6-sol, got %q", m)
		}
	}
	return SplitCodexTicketDefaultModels(raw), nil
}

func (s *OpenAIGatewayService) openAICodexTicketDefaultsContext(ctx context.Context) OpenAICodexTicketDefaults {
	if s == nil || s.settingService == nil {
		return OpenAICodexTicketDefaults{}
	}
	return s.settingService.GetOpenAICodexTicketDefaults(ctx)
}

// adoptCodexAccountTicketDefaults 把全局默认项写成账号级配置并返回是否认领成功。
// 只认领：开启默认之后创建、当前 active、已绑定固定代理、且从未有过账号级票据配置的
// 非影子 OpenAI OAuth 账号。手动关闭过的账号带有 enabled=false 的配置，不会被再次认领。
func (s *OpenAIGatewayService) adoptCodexAccountTicketDefaults(ctx context.Context, account *Account, defaults OpenAICodexTicketDefaults) bool {
	if s == nil || s.accountRepo == nil || account == nil || !defaults.Enabled || defaults.Since.IsZero() {
		return false
	}
	if account.Extra != nil && account.Extra[codexAccountTicketConfigKey] != nil {
		return false
	}
	if !isOpenAICodexTicketAccount(account) || account.Status != StatusActive || account.ProxyID == nil || account.Proxy == nil {
		return false
	}
	if account.CreatedAt.IsZero() || account.CreatedAt.Before(defaults.Since) {
		return false
	}
	plan := normalizeCodexTicketDefaultPlan(defaults.Plan)
	if plan == "" {
		return false
	}
	models := normalizeCodexTicketModels(defaults.Models, "")
	cfg := codexAccountTicketConfig{TicketPlan: plan, Enabled: true, Model: models[0], Models: models, Revision: uuid.NewString()}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(writeCtx, account.ID, map[string]any{codexAccountTicketConfigKey: cfg}); err != nil {
		logger.L().Warn("openai_codex_ticket adopt defaults failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return false
	}
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	account.Extra[codexAccountTicketConfigKey] = cfg
	logger.L().Info("openai_codex_ticket account adopted defaults", zap.Int64("account_id", account.ID), zap.String("plan", plan), zap.Strings("models", models))
	return true
}
