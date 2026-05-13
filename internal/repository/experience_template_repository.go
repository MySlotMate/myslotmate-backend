package repository

import (
	"context"
	"database/sql"

	"myslotmate-backend/internal/models"
)

type ExperienceTemplateRepository interface {
	ListByMood(ctx context.Context, mood string) ([]*models.ExperienceTemplate, error)
	ListAll(ctx context.Context) ([]*models.ExperienceTemplate, error)
}

type postgresExperienceTemplateRepository struct {
	db *sql.DB
}

func NewExperienceTemplateRepository(db *sql.DB) ExperienceTemplateRepository {
	return &postgresExperienceTemplateRepository{db: db}
}

const experienceTemplateColumns = `id, mood, title, hook_line, created_at, updated_at`

func scanTemplateRows(rows *sql.Rows) ([]*models.ExperienceTemplate, error) {
	var out []*models.ExperienceTemplate
	for rows.Next() {
		t := &models.ExperienceTemplate{}
		if err := rows.Scan(&t.ID, &t.Mood, &t.Title, &t.HookLine, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *postgresExperienceTemplateRepository) ListByMood(ctx context.Context, mood string) ([]*models.ExperienceTemplate, error) {
	query := `SELECT ` + experienceTemplateColumns + ` FROM experience_templates WHERE mood = $1 ORDER BY title ASC`
	rows, err := r.db.QueryContext(ctx, query, mood)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTemplateRows(rows)
}

func (r *postgresExperienceTemplateRepository) ListAll(ctx context.Context) ([]*models.ExperienceTemplate, error) {
	query := `SELECT ` + experienceTemplateColumns + ` FROM experience_templates ORDER BY mood ASC, title ASC`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTemplateRows(rows)
}
