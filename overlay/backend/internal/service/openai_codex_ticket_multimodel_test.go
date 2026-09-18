package service

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 一个账号可同时为 gpt-6-astra 与 gpt-5.6-sol 各持一张票：各自采集、各自复验、各自注入；
// 仅模型集合变化时保留未变模型的票据与 revision；已勾选但缺票的模型只拦截该模型。
func TestCodexAccountTicketMultiModel(t *testing.T) {
	var mu sync.Mutex
	probed := map[string]int{}
	u := &codexTicketFuncUpstream{proxy: func(req *http.Request, _ string) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		model := gjson.GetBytes(body, "model").String()
		mu.Lock()
		probed[model]++
		mu.Unlock()
		return codexModelResponse(model), nil
	}}
	s, repo := ticketJobService(t, u)
	ctx := context.Background()
	astra, sol := openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel
	jobFor := func(model string) *codexAccountTicketJob {
		s.openaiCodexAccountMu.Lock()
		defer s.openaiCodexAccountMu.Unlock()
		return s.openaiCodexAccountJobs[codexTicketJobKey(41, model)]
	}
	liveAccount := func() *Account {
		live, err := repo.GetByID(ctx, 41)
		require.NoError(t, err)
		return live
	}

	_, err := s.ConfigureCodexAccountTicket(ctx, 41, CodexAccountTicketUpdate{Enabled: true, Models: []string{"gpt-5.5"}})
	require.Error(t, err, "models outside the allow-list are rejected")
	_, err = s.ConfigureCodexAccountTicket(ctx, 41, CodexAccountTicketUpdate{Enabled: true, Models: []string{" "}})
	require.Error(t, err, "at least one model is required")

	// Reset, then opt in with a single model.
	_, err = s.ConfigureCodexAccountTicket(ctx, 41, CodexAccountTicketUpdate{Enabled: false})
	require.NoError(t, err)
	status, err := s.ConfigureCodexAccountTicket(ctx, 41, CodexAccountTicketUpdate{Enabled: true, Models: []string{astra}})
	require.NoError(t, err)
	require.Equal(t, []string{astra}, status.Models)
	waitCodexTicketJob(t, jobFor(astra))
	revision := codexAccountTicketConfigOf(liveAccount()).Revision
	require.NotEmpty(t, revision)
	require.NotNil(t, s.lookupOpenAICodexTicket(liveAccount(), astra))

	// Adding a model keeps the revision and the existing ticket; only the new model is harvested.
	status, err = s.ConfigureCodexAccountTicket(ctx, 41, CodexAccountTicketUpdate{Enabled: true, Models: []string{sol, astra}})
	require.NoError(t, err)
	require.Equal(t, []string{astra, sol}, status.Models, "models are reported in canonical order")
	require.Len(t, status.ModelStatuses, 2)
	waitCodexTicketJob(t, jobFor(sol))
	live := liveAccount()
	require.Equal(t, revision, codexAccountTicketConfigOf(live).Revision)
	for _, model := range []string{astra, sol} {
		require.NotNil(t, s.lookupOpenAICodexTicket(live, model), model)
		headers := http.Header{}
		require.NoError(t, s.applyOpenAICodexTicket(ctx, live, model, headers))
		require.Len(t, headers.Get(openAICodexTurnStateHeader), 292, model)
		require.False(t, s.openAICodexTicketBlocksAccount(live, model), model)
	}
	mu.Lock()
	require.Equal(t, 2, probed[astra], "harvest + fixed-proxy replay, not re-harvested when sol was added")
	require.Equal(t, 2, probed[sol], "harvest + fixed-proxy replay")
	mu.Unlock()
	status, err = s.GetCodexAccountTicketStatus(ctx, 41)
	require.NoError(t, err)
	require.Equal(t, "ready", status.State)
	require.True(t, status.TicketUsable)
	require.Len(t, OpenAICodexTicketStatuses(live, s.cfg.Gateway.OpenAICodexTicket, time.Now()), 2)

	// Dropping a model removes only its ticket; the other model keeps its verified ticket and the revision.
	status, err = s.ConfigureCodexAccountTicket(ctx, 41, CodexAccountTicketUpdate{Enabled: true, Models: []string{astra}})
	require.NoError(t, err)
	require.Equal(t, []string{astra}, status.Models)
	require.Equal(t, "ready", status.State)
	live = liveAccount()
	require.Equal(t, revision, codexAccountTicketConfigOf(live).Revision)
	require.NotNil(t, s.lookupOpenAICodexTicket(live, astra))
	require.Nil(t, s.lookupOpenAICodexTicket(live, sol))
	require.Nil(t, live.Extra[openAICodexTicketExtraKey(sol)])
	headers := http.Header{}
	require.NoError(t, s.applyOpenAICodexTicket(ctx, live, sol, headers))
	require.Empty(t, headers.Get(openAICodexTurnStateHeader), "an unselected model is neither injected nor gated")
	require.False(t, s.openAICodexTicketBlocksAccount(live, sol))

	// A selected model without a ticket blocks only that model.
	status, err = s.ConfigureCodexAccountTicket(ctx, 41, CodexAccountTicketUpdate{Enabled: true, Models: []string{astra, sol}})
	require.NoError(t, err)
	waitCodexTicketJob(t, jobFor(sol))
	s.openaiCodexTickets.Delete(openAICodexTicketKey(41, astra))
	require.NoError(t, repo.UpdateExtra(ctx, 41, map[string]any{openAICodexTicketExtraKey(astra): nil}))
	live = liveAccount()
	require.True(t, s.openAICodexTicketBlocksAccount(live, astra))
	require.False(t, s.openAICodexTicketBlocksAccount(live, sol))
	require.ErrorIs(t, s.applyOpenAICodexTicket(ctx, live, astra, http.Header{}), ErrOpenAICodexTicketUnavailable)
	require.NoError(t, s.applyOpenAICodexTicket(ctx, live, sol, http.Header{}))
	status, err = s.GetCodexAccountTicketStatus(ctx, 41)
	require.NoError(t, err)
	require.False(t, status.TicketUsable)
	require.NotEqual(t, "ready", status.State)
}

// 全局「新账号默认开启」只认领：开启后创建、已绑固定代理、且从未配置过票据的账号；存量账号与手动关闭过的账号不动。
func TestCodexAccountTicketAdoptDefaults(t *testing.T) {
	s, repo := ticketJobService(t, &codexTicketFuncUpstream{proxy: func(req *http.Request, _ string) (*http.Response, error) {
		return codexTicketResponse(), nil
	}})
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	defaults := OpenAICodexTicketDefaults{Enabled: true, Since: since, Plan: "team", Models: []string{openAICodexTicketDefaultSolModel, openAICodexTicketDefaultModel}}
	proxyID := int64(7)
	fresh := &Account{ID: 61, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, ProxyID: &proxyID, Proxy: &Proxy{ID: 7, Protocol: "http", Host: "fixed.example.com", Port: 8080}, CreatedAt: since.Add(time.Minute), Credentials: map[string]any{"access_token": "tok"}}
	legacy := &Account{ID: 62, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, ProxyID: &proxyID, Proxy: fresh.Proxy, CreatedAt: since.Add(-time.Minute), Credentials: map[string]any{"access_token": "tok"}}
	noProxy := &Account{ID: 63, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, CreatedAt: since.Add(time.Minute), Credentials: map[string]any{"access_token": "tok"}}
	optedOut := &Account{ID: 64, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, ProxyID: &proxyID, Proxy: fresh.Proxy, CreatedAt: since.Add(time.Minute), Credentials: map[string]any{"access_token": "tok"}, Extra: map[string]any{codexAccountTicketConfigKey: codexAccountTicketConfig{Enabled: false, Revision: "manual"}}}
	repo.mu.Lock()
	repo.accounts = append(repo.accounts, *fresh, *legacy, *noProxy, *optedOut)
	repo.mu.Unlock()

	require.True(t, s.adoptCodexAccountTicketDefaults(ctx, fresh, defaults))
	require.False(t, s.adoptCodexAccountTicketDefaults(ctx, fresh, defaults), "already configured accounts are not adopted twice")
	require.False(t, s.adoptCodexAccountTicketDefaults(ctx, legacy, defaults), "accounts created before the default was enabled stay untouched")
	require.False(t, s.adoptCodexAccountTicketDefaults(ctx, noProxy, defaults), "accounts without a fixed proxy wait until one is bound")
	require.False(t, s.adoptCodexAccountTicketDefaults(ctx, optedOut, defaults), "manually disabled accounts are respected")
	require.False(t, s.adoptCodexAccountTicketDefaults(ctx, &Account{ID: 65, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, ProxyID: &proxyID, Proxy: fresh.Proxy, CreatedAt: since.Add(time.Minute)}, OpenAICodexTicketDefaults{Enabled: true, Plan: "team"}), "no effective-since means no adoption")

	live, err := repo.GetByID(ctx, 61)
	require.NoError(t, err)
	ac := codexAccountTicketConfigOf(live)
	require.True(t, ac.Enabled)
	require.Equal(t, codexTicketPlanTeam, ac.TicketPlan)
	require.Equal(t, []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}, ac.models())
	require.NotEmpty(t, ac.Revision)
	for _, id := range []int64{62, 63, 64} {
		other, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.False(t, codexAccountTicketConfigOf(other).Enabled, "account %d", id)
	}
	require.Equal(t, []string{openAICodexTicketDefaultModel}, SplitCodexTicketDefaultModels(""))
	require.Equal(t, []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}, SplitCodexTicketDefaultModels("gpt-5.6-sol, gpt-6-astra, bogus"))
	_, err = parseCodexTicketDefaultModelsStrict("gpt-6-astra,gpt-4o")
	require.Error(t, err)
	require.Equal(t, "", normalizeCodexTicketDefaultPlan("enterprise"))
	require.Equal(t, codexTicketPlanPro, normalizeCodexTicketDefaultPlan(""))
}
