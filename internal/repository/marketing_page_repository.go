package repository

import (
	"context"
	"database/sql"
	"errors"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
)

type MarketingPageRepository interface {
	Create(ctx context.Context, p *models.MarketingPage) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.MarketingPage, error)
	// GetBySlug returns nil, nil when no page has the slug.
	GetBySlug(ctx context.Context, slug string) (*models.MarketingPage, error)
	SlugExistsExcluding(ctx context.Context, slug string, excludeID uuid.UUID) (bool, error)
	ListAll(ctx context.Context) ([]*models.MarketingPage, error)
	Update(ctx context.Context, p *models.MarketingPage) error
	Delete(ctx context.Context, id uuid.UUID) error
	SetPublished(ctx context.Context, id uuid.UUID, published bool) error
}

type postgresMarketingPageRepository struct {
	db *sql.DB
}

func NewMarketingPageRepository(db *sql.DB) MarketingPageRepository {
	return &postgresMarketingPageRepository{db: db}
}

const marketingPageSelect = `SELECT p.id, p.slug, p.event_id, p.title, p.hook_line, p.cover_image_url,
	p.gallery_urls, p.content, p.cta_text, p.seo_description, p.published_at, p.created_at, p.updated_at,
	e.slug, e.title
	FROM marketing_pages p JOIN events e ON e.id = p.event_id`

func scanMarketingPage(row interface {
	Scan(dest ...interface{}) error
}) (*models.MarketingPage, error) {
	p := &models.MarketingPage{}
	err := row.Scan(&p.ID, &p.Slug, &p.EventID, &p.Title, &p.HookLine, &p.CoverImageURL,
		&p.GalleryURLs, &p.Content, &p.CTAText, &p.SEODescription, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt,
		&p.EventSlug, &p.EventTitle)
	if err != nil {
		return nil, err
	}
	if p.GalleryURLs == nil {
		p.GalleryURLs = []string{}
	}
	return p, nil
}

func (r *postgresMarketingPageRepository) Create(ctx context.Context, p *models.MarketingPage) error {
	if p.GalleryURLs == nil {
		p.GalleryURLs = []string{}
	}
	return r.db.QueryRowContext(ctx,
		`INSERT INTO marketing_pages (slug, event_id, title, hook_line, cover_image_url, gallery_urls, content, cta_text, seo_description)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, created_at, updated_at`,
		p.Slug, p.EventID, p.Title, p.HookLine, p.CoverImageURL, p.GalleryURLs, p.Content, p.CTAText, p.SEODescription,
	).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
}

func (r *postgresMarketingPageRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.MarketingPage, error) {
	p, err := scanMarketingPage(r.db.QueryRowContext(ctx, marketingPageSelect+` WHERE p.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (r *postgresMarketingPageRepository) GetBySlug(ctx context.Context, slug string) (*models.MarketingPage, error) {
	p, err := scanMarketingPage(r.db.QueryRowContext(ctx, marketingPageSelect+` WHERE p.slug = $1`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (r *postgresMarketingPageRepository) SlugExistsExcluding(ctx context.Context, slug string, excludeID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM marketing_pages WHERE slug = $1 AND id <> $2)`, slug, excludeID,
	).Scan(&exists)
	return exists, err
}

func (r *postgresMarketingPageRepository) ListAll(ctx context.Context) ([]*models.MarketingPage, error) {
	rows, err := r.db.QueryContext(ctx, marketingPageSelect+` ORDER BY p.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pages := []*models.MarketingPage{}
	for rows.Next() {
		p, err := scanMarketingPage(rows)
		if err != nil {
			return nil, err
		}
		pages = append(pages, p)
	}
	return pages, rows.Err()
}

func (r *postgresMarketingPageRepository) Update(ctx context.Context, p *models.MarketingPage) error {
	if p.GalleryURLs == nil {
		p.GalleryURLs = []string{}
	}
	return r.db.QueryRowContext(ctx,
		`UPDATE marketing_pages SET slug = $1, title = $2, hook_line = $3, cover_image_url = $4, gallery_urls = $5,
		 content = $6, cta_text = $7, seo_description = $8, updated_at = NOW()
		 WHERE id = $9 RETURNING updated_at`,
		p.Slug, p.Title, p.HookLine, p.CoverImageURL, p.GalleryURLs, p.Content, p.CTAText, p.SEODescription, p.ID,
	).Scan(&p.UpdatedAt)
}

func (r *postgresMarketingPageRepository) Delete(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM marketing_pages WHERE id = $1`, id)
	return err
}

func (r *postgresMarketingPageRepository) SetPublished(ctx context.Context, id uuid.UUID, published bool) error {
	q := `UPDATE marketing_pages SET published_at = NULL, updated_at = NOW() WHERE id = $1`
	if published {
		q = `UPDATE marketing_pages SET published_at = COALESCE(published_at, NOW()), updated_at = NOW() WHERE id = $1`
	}
	_, err := r.db.ExecContext(ctx, q, id)
	return err
}
