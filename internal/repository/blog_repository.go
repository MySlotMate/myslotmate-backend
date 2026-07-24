package repository

import (
	"context"
	"database/sql"
	"fmt"
	"myslotmate-backend/internal/models"
	"time"

	"github.com/google/uuid"
)

type BlogRepository interface {
	Create(ctx context.Context, blog *models.Blog) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.Blog, error)
	GetBySlug(ctx context.Context, slug string) (*models.Blog, error)
	SlugExists(ctx context.Context, slug string) (bool, error)
	// SlugExistsExcluding reports whether a slug is taken by a blog other than
	// excludeID — used when updating a blog's own slug.
	SlugExistsExcluding(ctx context.Context, slug string, excludeID uuid.UUID) (bool, error)
	ListPublished(ctx context.Context, limit, offset int) ([]*models.Blog, error)
	ListPublishedByCategory(ctx context.Context, category string, limit, offset int) ([]*models.Blog, error)
	ListByAuthorID(ctx context.Context, authorID uuid.UUID, limit, offset int) ([]*models.Blog, error)
	// ListAll returns every blog including unpublished drafts. Admin-only.
	ListAll(ctx context.Context, limit, offset int) ([]*models.Blog, error)
	Update(ctx context.Context, blog *models.Blog) error
	Delete(ctx context.Context, id uuid.UUID) error
	Publish(ctx context.Context, id uuid.UUID) error
	Unpublish(ctx context.Context, id uuid.UUID) error
}

type postgresBlogRepository struct {
	db *sql.DB
}

func NewBlogRepository(db *sql.DB) BlogRepository {
	return &postgresBlogRepository{db: db}
}

var blogColumns = `id, slug, title, description, category, content, cover_image_url,
	author_id, author_name, read_time_minutes, published_at, created_at, updated_at`

func scanBlog(row interface {
	Scan(dest ...interface{}) error
}) (*models.Blog, error) {
	blog := &models.Blog{}
	err := row.Scan(
		&blog.ID, &blog.Slug, &blog.Title, &blog.Description, &blog.Category, &blog.Content, &blog.CoverImageURL,
		&blog.AuthorID, &blog.AuthorName, &blog.ReadTimeMinutes, &blog.PublishedAt, &blog.CreatedAt, &blog.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return blog, nil
}

func (r *postgresBlogRepository) Create(ctx context.Context, blog *models.Blog) error {
	blog.ID = uuid.New()
	blog.CreatedAt = time.Now()
	blog.UpdatedAt = time.Now()

	err := r.db.QueryRowContext(
		ctx,
		`INSERT INTO blogs (id, slug, title, description, category, content, cover_image_url, author_id, author_name, read_time_minutes, published_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 RETURNING `+blogColumns,
		blog.ID, blog.Slug, blog.Title, blog.Description, blog.Category, blog.Content, blog.CoverImageURL,
		blog.AuthorID, blog.AuthorName, blog.ReadTimeMinutes, blog.PublishedAt, blog.CreatedAt, blog.UpdatedAt,
	).Scan(
		&blog.ID, &blog.Slug, &blog.Title, &blog.Description, &blog.Category, &blog.Content, &blog.CoverImageURL,
		&blog.AuthorID, &blog.AuthorName, &blog.ReadTimeMinutes, &blog.PublishedAt, &blog.CreatedAt, &blog.UpdatedAt,
	)

	return err
}

func (r *postgresBlogRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.Blog, error) {
	row := r.db.QueryRowContext(
		ctx,
		fmt.Sprintf(`SELECT %s FROM blogs WHERE id = $1`, blogColumns),
		id,
	)
	return scanBlog(row)
}

func (r *postgresBlogRepository) GetBySlug(ctx context.Context, slug string) (*models.Blog, error) {
	row := r.db.QueryRowContext(
		ctx,
		fmt.Sprintf(`SELECT %s FROM blogs WHERE slug = $1`, blogColumns),
		slug,
	)
	return scanBlog(row)
}

func (r *postgresBlogRepository) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blogs WHERE slug = $1)`, slug).Scan(&exists)
	return exists, err
}

func (r *postgresBlogRepository) SlugExistsExcluding(ctx context.Context, slug string, excludeID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM blogs WHERE slug = $1 AND id <> $2)`,
		slug, excludeID,
	).Scan(&exists)
	return exists, err
}

func (r *postgresBlogRepository) ListPublished(ctx context.Context, limit, offset int) ([]*models.Blog, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := r.db.QueryContext(
		ctx,
		fmt.Sprintf(`SELECT %s FROM blogs WHERE published_at IS NOT NULL ORDER BY published_at DESC LIMIT $1 OFFSET $2`, blogColumns),
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blogs []*models.Blog
	for rows.Next() {
		blog, err := scanBlog(rows)
		if err != nil {
			return nil, err
		}
		blogs = append(blogs, blog)
	}

	return blogs, rows.Err()
}

func (r *postgresBlogRepository) ListPublishedByCategory(ctx context.Context, category string, limit, offset int) ([]*models.Blog, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := r.db.QueryContext(
		ctx,
		fmt.Sprintf(`SELECT %s FROM blogs WHERE published_at IS NOT NULL AND category = $1 ORDER BY published_at DESC LIMIT $2 OFFSET $3`, blogColumns),
		category, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blogs []*models.Blog
	for rows.Next() {
		blog, err := scanBlog(rows)
		if err != nil {
			return nil, err
		}
		blogs = append(blogs, blog)
	}

	return blogs, rows.Err()
}

func (r *postgresBlogRepository) ListByAuthorID(ctx context.Context, authorID uuid.UUID, limit, offset int) ([]*models.Blog, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := r.db.QueryContext(
		ctx,
		fmt.Sprintf(`SELECT %s FROM blogs WHERE author_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, blogColumns),
		authorID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blogs []*models.Blog
	for rows.Next() {
		blog, err := scanBlog(rows)
		if err != nil {
			return nil, err
		}
		blogs = append(blogs, blog)
	}

	return blogs, rows.Err()
}

// ListAll returns every blog (drafts + published), newest first. Admin-only.
func (r *postgresBlogRepository) ListAll(ctx context.Context, limit, offset int) ([]*models.Blog, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := r.db.QueryContext(
		ctx,
		fmt.Sprintf(`SELECT %s FROM blogs ORDER BY created_at DESC LIMIT $1 OFFSET $2`, blogColumns),
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blogs []*models.Blog
	for rows.Next() {
		blog, err := scanBlog(rows)
		if err != nil {
			return nil, err
		}
		blogs = append(blogs, blog)
	}

	return blogs, rows.Err()
}

func (r *postgresBlogRepository) Update(ctx context.Context, blog *models.Blog) error {
	blog.UpdatedAt = time.Now()

	err := r.db.QueryRowContext(
		ctx,
		`UPDATE blogs SET title = $1, description = $2, category = $3, content = $4, cover_image_url = $5, read_time_minutes = $6, updated_at = $7, slug = $8
		 WHERE id = $9
		 RETURNING `+blogColumns,
		blog.Title, blog.Description, blog.Category, blog.Content, blog.CoverImageURL, blog.ReadTimeMinutes, blog.UpdatedAt, blog.Slug, blog.ID,
	).Scan(
		&blog.ID, &blog.Slug, &blog.Title, &blog.Description, &blog.Category, &blog.Content, &blog.CoverImageURL,
		&blog.AuthorID, &blog.AuthorName, &blog.ReadTimeMinutes, &blog.PublishedAt, &blog.CreatedAt, &blog.UpdatedAt,
	)

	return err
}

func (r *postgresBlogRepository) Delete(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM blogs WHERE id = $1`, id)
	return err
}

func (r *postgresBlogRepository) Publish(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.ExecContext(
		ctx,
		`UPDATE blogs SET published_at = NOW(), updated_at = NOW() WHERE id = $1 AND published_at IS NULL`,
		id,
	)
	return err
}

func (r *postgresBlogRepository) Unpublish(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.ExecContext(
		ctx,
		`UPDATE blogs SET published_at = NULL, updated_at = NOW() WHERE id = $1`,
		id,
	)
	return err
}
