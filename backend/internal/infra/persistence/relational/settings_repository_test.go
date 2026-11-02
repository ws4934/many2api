package relational

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	repositorypkg "github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRuntimeSettingsRepositoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewRuntimeSettingsRepository(database)
	if _, _, _, found, err := repository.Get(ctx); err != nil || found {
		t.Fatalf("initial get found = %v, err = %v", found, err)
	}
	settings := settingsdomain.Config{
		Server:      settingsdomain.ServerConfig{MaxConcurrentRequests: 2048},
		ProviderWeb: settingsdomain.ProviderWebConfig{BaseURL: "https://grok.com", ChatTimeout: time.Minute},
		Media: settingsdomain.MediaConfig{
			MaxImageBytes: 16 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80,
			CleanupInterval: 10 * time.Minute,
		},
		Routing: settingsdomain.RoutingConfig{StickyTTL: time.Hour, MaxAttempts: 3, PreferFreeBuild: true},
	}
	updatedAt, revision, err := repository.Save(ctx, settings, 0)
	if err != nil {
		t.Fatal(err)
	}
	value, storedUpdatedAt, storedRevision, found, err := repository.Get(ctx)
	if err != nil || !found {
		t.Fatalf("saved get found = %v, err = %v", found, err)
	}
	if value.Server != settings.Server || value.Routing != settings.Routing || value.Media != settings.Media || value.ProviderWeb != settings.ProviderWeb || !storedUpdatedAt.Equal(updatedAt) || revision != 1 || storedRevision != revision {
		t.Fatalf("saved value = %#v", value)
	}
	var row runtimeSettingsModel
	if err := database.db.WithContext(ctx).Where("key = ?", runtimeSettingsKey).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"version"`, "Statsig", "encryptedStatsigManualValue"} {
		if strings.Contains(row.ValueJSON, forbidden) {
			t.Fatalf("runtime settings contain removed metadata %q: %s", forbidden, row.ValueJSON)
		}
	}
	// Persisted rows from before removal may still contain the obsolete secret.
	// It must not require decryption or change any of the active configuration.
	var oldPayload map[string]any
	if err := json.Unmarshal([]byte(row.ValueJSON), &oldPayload); err != nil {
		t.Fatal(err)
	}
	oldPayload["encryptedStatsigManualValue"] = "obsolete-ciphertext"
	oldWeb := oldPayload["config"].(map[string]any)["ProviderWeb"].(map[string]any)
	oldWeb["StatsigMode"] = "url"
	oldWeb["StatsigSignerURL"] = "https://removed.example.com/sign"
	oldJSON, err := json.Marshal(oldPayload)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Model(&runtimeSettingsModel{}).Where("key = ?", runtimeSettingsKey).Update("value_json", string(oldJSON)).Error; err != nil {
		t.Fatal(err)
	}
	loaded, _, loadedRevision, found, err := repository.Get(ctx)
	if err != nil || !found || loaded.ProviderWeb != settings.ProviderWeb || loaded.Routing != settings.Routing || loadedRevision != revision {
		t.Fatalf("loading existing settings changed active values: %#v, revision=%d, err=%v", loaded, loadedRevision, err)
	}
	if _, _, err := repository.Save(ctx, settings, 0); !errors.Is(err, repositorypkg.ErrConflict) {
		t.Fatalf("stale repository revision error = %v", err)
	}
}
