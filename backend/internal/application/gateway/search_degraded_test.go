package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type searchDegradedFixture struct {
	service   *Service
	accounts  *relational.AccountRepository
	audits    *relational.AuditRepository
	responses *relational.ResponseRepository
	ids       []uint64
	key       clientkey.Key
	mu        sync.Mutex
	calls     []int
	late      bool
}

func (f *searchDegradedFixture) attempts() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.calls...)
}

func newSearchDegradedFixture(t *testing.T, allBad bool) *searchDegradedFixture {
	t.Helper()
	f := &searchDegradedFixture{}
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
		if r.URL.Path != "/ws/mgw/" {
			fhttp.NotFound(w, r)
			return
		}
		index := 0
		if strings.Contains(r.Header.Get("Cookie"), "search-sso-1") {
			index = 1
		}
		f.mu.Lock()
		f.calls = append(f.calls, index)
		late := f.late
		f.mu.Unlock()
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		var initial map[string]any
		if err := conn.ReadJSON(&initial); err != nil {
			t.Errorf("session: %v", err)
			return
		}
		id := initial["event"].(map[string]any)["event_id"]
		_ = conn.WriteJSON(map[string]any{"session_id": "session-1", "event": map[string]any{"type": "session.created", "client_event_id": id}})
		_ = conn.WriteJSON(map[string]any{"session_id": "session-1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "session-1"}}})
		for range 2 {
			var event map[string]any
			if err := conn.ReadJSON(&event); err != nil {
				t.Errorf("turn: %v", err)
				return
			}
		}
		query, answer := "capital of Portugal", "Lisbon"
		if index == 0 || allBad {
			query, answer = "current information and recent sources", "WORD_SALAD"
		}
		if late {
			_ = conn.WriteJSON(map[string]any{"event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": "prefix", "channel": "CHANNEL_ASSISTANT_RESPONSE"}}}})
		}
		_ = conn.WriteJSON(map[string]any{"event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"tool_usage_card": map[string]any{"tool_usage_card_id": "search-1", "web_search": map[string]any{"args": map[string]any{"query": query}}}}}})
		_ = conn.WriteJSON(map[string]any{"event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": answer, "channel": "CHANNEL_ASSISTANT_RESPONSE"}}}})
		_ = conn.WriteJSON(map[string]any{"event": map[string]any{"type": "response.done", "response": map[string]any{"id": "parent-1", "status": "completed"}}})
	}))
	t.Cleanup(server.Close)
	cipher := testCipher(t)
	f.accounts, f.audits = relational.NewAccountRepository(db), relational.NewAuditRepository(db)
	models, responses := relational.NewModelRepository(db), relational.NewResponseRepository(db)
	f.responses = responses
	if err := models.UpsertRoutes(ctx, web.Routes()); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		token, err := cipher.Encrypt(fmt.Sprintf("search-sso-%d", index))
		if err != nil {
			t.Fatal(err)
		}
		credential, _, err := f.accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: fmt.Sprintf("search-%d", index), SourceKey: fmt.Sprintf("search-%d", index),
			UserID: fmt.Sprintf("497f19f8-49d4-458a-bee4-43ec3dcaf8c%d", index), EncryptedAccessToken: token,
			Enabled: true, AuthStatus: account.AuthStatusActive, WebTier: account.WebTierBasic, Priority: 300 - index, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := models.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-chat-fast", "grok-imagine-video"}, now); err != nil {
			t.Fatal(err)
		}
		if err := f.accounts.SaveQuotaWindows(ctx, credential.ID, account.WebTierBasic, now, []account.QuotaWindow{{AccountID: credential.ID, Mode: "fast", Remaining: 20, Total: 20, SyncedAt: &now, Source: account.QuotaSourceUpstream}}); err != nil {
			t.Fatal(err)
		}
		f.ids = append(f.ids, credential.ID)
	}
	f.key, err = relational.NewClientKeyRepository(db).Create(ctx, clientkey.Key{Name: "search-test", Prefix: "search", SecretHash: strings.Repeat("c", 64), EncryptedSecret: "test-only", Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}
	adapter := web.NewAdapter(web.Config{BaseURL: server.URL}, infraegress.NewManager(relational.NewEgressRepository(db), cipher), cipher, responses, nil)
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accounts := accountapp.NewService(f.accounts, f.audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	selector := NewSelector(f.accounts, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	f.service = NewService(models, f.audits, accounts, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, responses, 3)
	return f
}

func TestWebSearchDegradedFailsOverBeforeAnyClientOutput(t *testing.T) {
	for _, operation := range []audit.Operation{audit.OperationResponses, audit.OperationChat, audit.OperationMessages} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", operation, stream), func(t *testing.T) {
				f := newSearchDegradedFixture(t, false)
				body := fmt.Sprintf(`{"model":"grok-chat-fast","input":"capital of Portugal","messages":[{"role":"user","content":"capital of Portugal"}],"max_tokens":100,"tools":[{"type":"web_search"}],"stream":%t}`, stream)
				// Messages uses Anthropic tool declarations; exercise the same
				// search event emitted by upstream even without a declared tool.
				if operation == audit.OperationMessages {
					body = fmt.Sprintf(`{"model":"grok-chat-fast","messages":[{"role":"user","content":"capital of Portugal"}],"max_tokens":100,"stream":%t}`, stream)
				}
				input := Input{RequestID: "search-failover", ClientKey: f.key, PublicModel: "Web/grok-chat-fast", Body: []byte(body), Streaming: stream, Operation: operation}
				var result *Result
				var err error
				switch operation {
				case audit.OperationChat:
					result, err = f.service.CreateChatCompletion(context.Background(), input)
				case audit.OperationMessages:
					result, err = f.service.CreateMessage(context.Background(), input)
				default:
					result, err = f.service.CreateResponse(context.Background(), input)
				}
				if err != nil {
					t.Fatal(err)
				}
				data, readErr := io.ReadAll(result.Body)
				result.Finalize(Usage{}, "", "")
				_ = result.Body.Close()
				if readErr != nil || !strings.Contains(string(data), "Lisbon") || strings.Contains(string(data), "WORD_SALAD") || strings.Contains(string(data), "current information") {
					t.Fatalf("client body=%s readErr=%v", data, readErr)
				}
				if calls := f.attempts(); !slices.Equal(calls, []int{0, 1}) {
					t.Fatalf("calls=%v", calls)
				}
				candidates, err := f.accounts.ListRoutingCandidates(context.Background(), account.ProviderWeb, 0, "grok-chat-fast", "")
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range candidates {
					if c.Credential.ID == f.ids[0] && (c.ModelQuotaBlock == nil || c.ModelQuotaBlock.Reason != audit.DegradeClassSearch || time.Until(c.ModelQuotaBlock.CooldownUntil) < 59*time.Minute) {
						t.Fatalf("missing model quarantine: %+v", c.ModelQuotaBlock)
					}
					if c.Credential.AuthStatus != account.AuthStatusActive || c.Credential.CooldownUntil != nil {
						t.Fatal("search failure invalidated whole account")
					}
				}
				summary, err := f.audits.SummarizeDegrade(context.Background(), repository.DegradeSummaryQuery{Start: time.Now().UTC().Add(-time.Minute), End: time.Now().UTC().Add(time.Second), AccountClass: audit.DegradeClassSearch})
				if err != nil || summary.Totals.Search != 1 || len(summary.Accounts) != 1 || summary.Accounts[0].ID != f.ids[0] || summary.Accounts[0].ModelCooldownUntil == nil {
					t.Fatalf("recovered bad account absent from report: %+v err=%v", summary, err)
				}
			})
		}
	}
}

func TestWebSearchDegradedAllAccountsFailAndPinnedNeverRotates(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned=%t", pinned), func(t *testing.T) {
			f := newSearchDegradedFixture(t, true)
			ctx := context.Background()
			if pinned {
				ctx = WithCreativeAccount(ctx, f.ids[0])
			}
			result, err := f.service.CreateResponse(ctx, Input{RequestID: "search-rejected", ClientKey: f.key, PublicModel: "Web/grok-chat-fast", Streaming: true, Body: []byte(`{"model":"grok-chat-fast","input":"hello","stream":true}`)})
			failure, ok := err.(*UpstreamFailure)
			if result != nil || !ok || failure.HTTPStatus != http.StatusBadGateway || failure.Code != audit.DegradeClassSearch || strings.Contains(failure.PublicMessage, "Grok") {
				t.Fatalf("result=%v err=%v", result, err)
			}
			want := []int{0, 1}
			if pinned {
				want = []int{0}
			}
			if calls := f.attempts(); !slices.Equal(calls, want) {
				t.Fatalf("calls=%v want=%v", calls, want)
			}
		})
	}
}

func TestDegradedSearchReplayKeepsHostedSideEffectsOutOfFailover(t *testing.T) {
	for _, body := range []string{`{"tools":[{"type":"web_search"}]}`, `{"web_search_options":{}}`} {
		if searchQualityRequestHasReplayUnsafeTools([]byte(body)) {
			t.Fatalf("read-only search cannot retry: %s", body)
		}
		if !qualityRequestHasReplayUnsafeHostedTools([]byte(body)) {
			t.Fatal("changed existing Build quality-retry contract")
		}
	}
	for _, body := range []string{`{"tools":[{"type":"web_search"},{"type":"mcp"}]}`, `{"mcp_servers":[{"url":"https://example.com"}]}`, `{"tools":[{"type":"code_interpreter"}]}`, `{"tools":[{"type":"namespace","tools":[{"type":"unknown_hosted_tool"}]}]}`} {
		if !searchQualityRequestHasReplayUnsafeTools([]byte(body)) {
			t.Fatalf("unsafe hosted tool can retry: %s", body)
		}
	}
}

func TestWebSearchModelQuarantineSurvivesRequestsWithoutBlockingVideo(t *testing.T) {
	f := newSearchDegradedFixture(t, false)
	for range 2 {
		result, err := f.service.CreateResponse(context.Background(), Input{RequestID: "quarantine", ClientKey: f.key, PublicModel: "Web/grok-chat-fast", Body: []byte(`{"model":"grok-chat-fast","input":"hello"}`)})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, result.Body)
		result.Finalize(Usage{}, "", "")
		_ = result.Body.Close()
	}
	if calls := f.attempts(); !slices.Equal(calls, []int{0, 1, 1}) {
		t.Fatalf("cooled model was selected again: %v", calls)
	}
	video, err := f.accounts.ListRoutingCandidates(context.Background(), account.ProviderWeb, 0, "grok-imagine-video", "")
	if err != nil || len(video) != 2 {
		t.Fatalf("video candidates=%d err=%v", len(video), err)
	}
	for _, candidate := range video {
		if candidate.ModelQuotaBlock != nil || !candidate.Credential.Enabled {
			t.Fatal("chat quarantine affected video")
		}
	}
}

func TestWebSearchStoredContinuationDoesNotSwitchOwner(t *testing.T) {
	f := newSearchDegradedFixture(t, false)
	ctx := context.Background()
	route, err := f.service.models.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-chat-fast")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := f.responses.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: "resp-owner", AccountID: f.ids[0], ClientKeyID: f.key.ID, ModelRouteID: route.ID, Provider: account.ProviderWeb, ExpiresAt: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := f.responses.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: "resp-owner", AccountID: f.ids[0], ConversationID: "session-1", UpstreamParentResponseID: "parent-1", ResponseJSON: "{}", Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	result, err := f.service.CreateResponse(ctx, Input{RequestID: "owner-failed", ClientKey: f.key, PublicModel: "Web/grok-chat-fast", PreviousResponseID: "resp-owner", Body: []byte(`{"model":"grok-chat-fast","input":"follow-up","previous_response_id":"resp-owner"}`)})
	failure, ok := err.(*UpstreamFailure)
	if result != nil || !ok || failure.Code != audit.DegradeClassSearch {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if calls := f.attempts(); !slices.Equal(calls, []int{0}) {
		t.Fatalf("continuation switched owner: %v", calls)
	}
}

func TestWebSearchLateSignatureStopsWithoutReplayAndCoolsModel(t *testing.T) {
	f := newSearchDegradedFixture(t, true)
	f.mu.Lock()
	f.late = true
	f.mu.Unlock()
	result, err := f.service.CreateResponse(context.Background(), Input{RequestID: "late-search", ClientKey: f.key, PublicModel: "Web/grok-chat-fast", Streaming: true, Body: []byte(`{"model":"grok-chat-fast","input":"hello","stream":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", audit.DegradeClassSearch)
	_ = result.Body.Close()
	if !errors.Is(err, provider.ErrWebSearchPlaceholder) || !strings.Contains(string(data), "prefix") || strings.Contains(string(data), "WORD_SALAD") {
		t.Fatalf("output=%s err=%v", data, err)
	}
	if calls := f.attempts(); !slices.Equal(calls, []int{0}) {
		t.Fatalf("partial stream replayed: %v", calls)
	}
	candidates, err := f.accounts.ListRoutingCandidates(context.Background(), account.ProviderWeb, 0, "grok-chat-fast", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.Credential.ID == f.ids[0] && candidate.ModelQuotaBlock == nil {
			t.Fatal("late failure did not cool the model")
		}
	}
}
