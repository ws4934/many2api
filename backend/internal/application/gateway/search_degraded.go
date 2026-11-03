package gateway

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

const degradedSearchCooldown = time.Hour

func (s *Service) applyDegradedSearchPenalty(ctx context.Context, requestID string, credential account.Credential, model string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationHealthBudget)
	defer cancel()
	now := time.Now().UTC()
	// Reuse the durable model block also used for model access denials. An
	// unhealthy chat model must not disable this account's images or videos.
	err := s.selector.accounts.UpsertModelQuotaBlock(writeCtx, account.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: model, Reason: audit.DegradeClassSearch,
		CooldownUntil: now.Add(degradedSearchCooldown), UpdatedAt: now,
	})
	if err != nil {
		s.logger.Error("web_search_quarantine_failed", "request_id", requestID, "account_id", credential.ID, "model", model, "error", err)
		return
	}
	s.selector.MarkQuotaStateChanged(credential.Provider)
	s.logger.Warn("web_search_placeholder", "request_id", requestID, "account_id", credential.ID, "model", model, "cooldown", degradedSearchCooldown)
}
