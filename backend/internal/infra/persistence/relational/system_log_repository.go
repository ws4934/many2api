package relational

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/systemlog"
	"github.com/chenyme/grok2api/backend/internal/pkg/diagnostic"
	"gorm.io/gorm"
)

type systemLogModel struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement"`
	CreatedAt   time.Time `gorm:"not null"`
	Level       string    `gorm:"size:8;not null"`
	Message     string    `gorm:"type:text;not null"`
	InstanceID  string    `gorm:"size:128;not null"`
	Provider    string    `gorm:"size:64;not null"`
	AccountName string    `gorm:"size:255"`
	AccountID   uint64    `gorm:"not null"`
	RequestID   string    `gorm:"size:64;not null"`
	FieldsJSON  string    `gorm:"type:text;not null"`
}

func (systemLogModel) TableName() string { return "system_logs" }

type SystemLogRepository struct{ db *Database }

func NewSystemLogRepository(db *Database) *SystemLogRepository { return &SystemLogRepository{db: db} }

func (r *SystemLogRepository) AppendSystemLogs(ctx context.Context, entries []systemlog.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	rows := make([]systemLogModel, 0, len(entries))
	var accountIDs []uint64
	for _, entry := range entries {
		if entry.Level != "WARN" && entry.Level != "ERROR" {
			continue
		}
		if entry.AccountID != 0 {
			accountIDs = append(accountIDs, entry.AccountID)
		}
		fieldsValue := entry.Fields
		if fieldsValue == nil {
			fieldsValue = map[string]any{}
		}
		fields, err := json.Marshal(fieldsValue)
		if err != nil {
			return err
		}
		rows = append(rows, systemLogModel{CreatedAt: entry.CreatedAt, Level: entry.Level, Message: entry.Message, InstanceID: entry.InstanceID, Provider: entry.Provider, AccountID: entry.AccountID, RequestID: entry.RequestID, FieldsJSON: string(fields)})
	}
	if len(rows) == 0 {
		return nil
	}
	// Snapshot the admin display name in the writer batch, not on the request path.
	// A label lookup failure must not discard the underlying warning or error.
	names, _ := NewAccountRepository(r.db).DisplayNames(ctx, accountIDs)
	for i := range rows {
		rows[i].AccountName = diagnostic.Text(names[rows[i].AccountID], 255)
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.CreateInBatches(rows, 50).Error; err != nil {
			return err
		}
		return pruneSystemLogs(tx, time.Now().UTC().AddDate(0, 0, -systemlog.RetentionDays), systemlog.MaxEntries)
	})
}

func (r *SystemLogRepository) ListSystemLogs(ctx context.Context, input systemlog.Query) ([]systemlog.Entry, bool, error) {
	limit := min(max(input.Limit, 1), 200)
	query := r.db.db.WithContext(ctx).Model(&systemLogModel{}).Where("level IN ?", []string{"WARN", "ERROR"})
	if input.BeforeID > 0 {
		query = query.Where("id < ?", input.BeforeID)
	}
	if input.Level != "" {
		query = query.Where("level = ?", input.Level)
	}
	if input.Provider != "" {
		query = query.Where("provider = ?", input.Provider)
	}
	if input.AccountID > 0 {
		query = query.Where("account_id = ?", input.AccountID)
	}
	if input.RequestID != "" {
		query = query.Where("request_id = ?", input.RequestID)
	}
	if !input.Since.IsZero() {
		query = query.Where("created_at >= ?", input.Since)
	}
	if input.Search != "" {
		pattern := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(input.Search)) + "%"
		query = query.Where(`(LOWER(message) LIKE ? ESCAPE '!' OR LOWER(fields_json) LIKE ? ESCAPE '!' OR LOWER(account_name) LIKE ? ESCAPE '!'
 OR (account_id > 0 AND COALESCE(account_name,'') = '' AND EXISTS (
 SELECT 1 FROM provider_accounts identity WHERE identity.id = system_logs.account_id AND LOWER(`+accountDisplayNameSQL+`) LIKE ? ESCAPE '!')))`, pattern, pattern, pattern, pattern)
	}
	var rows []systemLogModel
	if err := query.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	// Older records have only an ID. Resolve missing snapshots once per page.
	var missing []uint64
	for _, row := range rows {
		if row.AccountID != 0 && row.AccountName == "" {
			missing = append(missing, row.AccountID)
		}
	}
	names, _ := NewAccountRepository(r.db).DisplayNames(ctx, missing)
	values := make([]systemlog.Entry, 0, len(rows))
	for _, row := range rows {
		fields := map[string]any{}
		if err := json.Unmarshal([]byte(row.FieldsJSON), &fields); err != nil {
			return nil, false, err
		}
		if row.AccountName == "" {
			row.AccountName = diagnostic.Text(names[row.AccountID], 255)
		}
		values = append(values, systemlog.Entry{ID: row.ID, CreatedAt: row.CreatedAt, Level: row.Level, Message: row.Message, InstanceID: row.InstanceID, Provider: row.Provider, AccountID: row.AccountID, AccountName: row.AccountName, RequestID: row.RequestID, Fields: fields})
	}
	return values, hasMore, nil
}

// Keep the newest rows globally across instances. No inference/audit tables are touched.
func (r *SystemLogRepository) PruneSystemLogs(ctx context.Context, cutoff time.Time, keep int) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return pruneSystemLogs(tx, cutoff, keep) })
}

func pruneSystemLogs(tx *gorm.DB, cutoff time.Time, keep int) error {
	if err := tx.Where("created_at < ?", cutoff).Delete(&systemLogModel{}).Error; err != nil {
		return err
	}
	var boundary systemLogModel
	err := tx.Select("id").Order("id DESC").Offset(max(keep, 1)).Limit(1).Find(&boundary).Error
	if err != nil || boundary.ID == 0 {
		return err
	}
	return tx.Where("id <= ?", boundary.ID).Delete(&systemLogModel{}).Error
}
