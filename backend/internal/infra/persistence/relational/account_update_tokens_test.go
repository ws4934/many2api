package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/google/uuid"
)

// Update saves an account read before a token renewal; the renewed tokens,
// the recovered auth state and the inspected bot flag must survive it.
func TestAccountUpdateKeepsRenewedTokens(t *testing.T) {
	accountStateDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		repo := NewAccountRepository(database)
		stale, _, err := repo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: "renewed", SourceKey: "renewed-" + uuid.NewString(),
			EncryptedAccessToken: "access-0", EncryptedRefreshToken: "refresh-0", ExpiresAt: time.Now().UTC().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusReauthRequired,
		})
		if err != nil {
			t.Fatal(err)
		}
		renewed, err := repo.UpdateTokens(ctx, stale.ID, "access-1", "refresh-1", "", time.Now().UTC().Add(2*time.Hour), 2)
		if err != nil {
			t.Fatal(err)
		}
		stale.Name = "edited"
		got, err := repo.Update(ctx, stale)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "edited" || got.EncryptedAccessToken != "access-1" || got.EncryptedRefreshToken != "refresh-1" ||
			!got.ExpiresAt.Equal(renewed.ExpiresAt) || got.LastRefreshAt == nil || got.AuthStatus != account.AuthStatusActive || got.BuildBotFlagSource != 2 {
			t.Fatalf("after a stale update: name=%q access=%q refresh=%q expires=%v lastRefresh=%v auth=%s botFlag=%d", got.Name, got.EncryptedAccessToken, got.EncryptedRefreshToken, got.ExpiresAt, got.LastRefreshAt, got.AuthStatus, got.BuildBotFlagSource)
		}
	})
}

func TestMuseRotationPreservesHealthAndNewerCredentials(t *testing.T) {
	accountStateDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		r := NewAccountRepository(db)
		v, _, err := r.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderMuse, SourceKey: uuid.NewString(), EncryptedAccessToken: "primary", EncryptedRefreshToken: "original", Enabled: true, AuthStatus: account.AuthStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		until := time.Now().UTC().Add(time.Hour)
		if err = r.UpdateCredentialRefreshFailure(ctx, v.ID, repository.CredentialRefreshFailure{Count: 2, Code: "muse_session_unassigned", RetryAt: until}); err != nil {
			t.Fatal(err)
		}
		if err = r.UpdateMuseCookieJar(ctx, v.ID, "original", "rotated"); err != nil {
			t.Fatal(err)
		}
		got, err := r.Get(ctx, v.ID)
		if err != nil || got.EncryptedAccessToken != "rotated" || got.EncryptedRefreshToken != "rotated" || got.LastRefreshAt != nil || got.LastRefreshErrorCode != "muse_session_unassigned" || got.RefreshFailureCount != 2 || got.RefreshDueAt == nil {
			t.Fatal("partial rotation changed refresh health", err)
		}
		if _, err = r.CommitCredentialRenewal(ctx, v, "stale", "stale", "", until, 0); !errors.Is(err, repository.ErrConflict) {
			t.Fatal("stale exchange overwrote rotation", err)
		}
		if err = r.MarkReauthRequired(ctx, v.ID, "blocked"); err != nil {
			t.Fatal(err)
		}
		if err = r.UpdateMuseCookieJar(ctx, v.ID, "rotated", "rejected"); err != nil {
			t.Fatal(err)
		}
		got, err = r.Get(ctx, v.ID)
		if err != nil || got.EncryptedAccessToken != "rotated" || got.AuthStatus != account.AuthStatusReauthRequired {
			t.Fatal("partial rotation changed invalid account", err)
		}
		completed, err := r.CommitCredentialRenewal(ctx, got, "in-flight", "in-flight", "", until, 0)
		if err != nil || completed.AuthStatus != account.AuthStatusReauthRequired || completed.LastError != "blocked" {
			t.Fatal("in-flight renewal recovered invalid account", err)
		}
	})
}
