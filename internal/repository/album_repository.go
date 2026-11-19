package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

const (
	MaxAlbumsPerUser  = 30
	MaxPhotosPerAlbum = 50
)

// ErrAlbumNotFound covers both "no such album" and "not yours": owners only
// ever see their own rows, and strangers learn nothing about ids.
var ErrAlbumNotFound = errors.New("album not found")

// AlbumRuleError is a broken album rule, e.g. a cap hit (HTTP 422).
type AlbumRuleError struct{ msg string }

func (e *AlbumRuleError) Error() string { return e.msg }

// AlbumUpdate holds the PATCH fields; nil means "leave as is".
type AlbumUpdate struct {
	Title        *string
	Description  *string
	Position     *int
	CoverPhotoID *uuid.UUID
}

// NewAlbumPhoto is one photo to attach.
type NewAlbumPhoto struct {
	URL     string
	Caption *string
	Width   *int
	Height  *int
}

type AlbumRepository interface {
	// ListByUser returns the user's albums by position, each with its photos.
	// withEmpty=false drops albums that have no photos (what visitors see).
	ListByUser(ctx context.Context, userID uuid.UUID, withEmpty bool) ([]*models.ProfileAlbum, error)
	GetOwned(ctx context.Context, albumID, userID uuid.UUID) (*models.ProfileAlbum, error)
	Create(ctx context.Context, userID uuid.UUID, title string, description *string) (uuid.UUID, error)
	Update(ctx context.Context, albumID, userID uuid.UUID, u AlbumUpdate) error
	// Delete removes the album (photos cascade) and returns the photo URLs it held.
	Delete(ctx context.Context, albumID, userID uuid.UUID) ([]string, error)
	AddPhotos(ctx context.Context, albumID, userID uuid.UUID, items []NewAlbumPhoto) error
	// DeletePhoto removes one photo and returns its URL.
	DeletePhoto(ctx context.Context, albumID, photoID, userID uuid.UUID) (string, error)
	// URLInUse reports whether any album photo still points at url.
	URLInUse(ctx context.Context, url string) (bool, error)
}

type postgresAlbumRepository struct {
	db *sql.DB
}

func NewAlbumRepository(db *sql.DB) AlbumRepository {
	return &postgresAlbumRepository{db: db}
}

const albumSelect = `SELECT a.id, a.user_id, a.title, a.description, a.position, a.cover_photo_id,
	a.created_at, a.updated_at,
	(SELECT count(*) FROM album_photos p WHERE p.album_id = a.id) AS photo_count
	FROM profile_albums a`

func (r *postgresAlbumRepository) queryAlbums(ctx context.Context, where string, args ...interface{}) ([]*models.ProfileAlbum, error) {
	rows, err := r.db.QueryContext(ctx, albumSelect+" "+where+" ORDER BY a.position, a.created_at", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	albums := []*models.ProfileAlbum{}
	byID := map[uuid.UUID]*models.ProfileAlbum{}
	ids := []string{}
	for rows.Next() {
		a := &models.ProfileAlbum{Photos: []*models.AlbumPhoto{}}
		var cover uuid.NullUUID
		if err := rows.Scan(&a.ID, &a.UserID, &a.Title, &a.Description, &a.Position, &cover,
			&a.CreatedAt, &a.UpdatedAt, &a.PhotoCount); err != nil {
			return nil, err
		}
		if cover.Valid {
			a.CoverPhotoID = &cover.UUID
		}
		albums = append(albums, a)
		byID[a.ID] = a
		ids = append(ids, a.ID.String())
	}
	if err := rows.Err(); err != nil || len(albums) == 0 {
		return albums, err
	}

	prows, err := r.db.QueryContext(ctx, `SELECT id, album_id, url, caption, width, height, position, created_at
		FROM album_photos WHERE album_id = ANY($1::uuid[]) ORDER BY position, created_at`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	for prows.Next() {
		p := &models.AlbumPhoto{}
		var w, h sql.NullInt64
		if err := prows.Scan(&p.ID, &p.AlbumID, &p.URL, &p.Caption, &w, &h, &p.Position, &p.CreatedAt); err != nil {
			return nil, err
		}
		if w.Valid {
			v := int(w.Int64)
			p.Width = &v
		}
		if h.Valid {
			v := int(h.Int64)
			p.Height = &v
		}
		byID[p.AlbumID].Photos = append(byID[p.AlbumID].Photos, p)
	}
	return albums, prows.Err()
}

func (r *postgresAlbumRepository) ListByUser(ctx context.Context, userID uuid.UUID, withEmpty bool) ([]*models.ProfileAlbum, error) {
	where := "WHERE a.user_id = $1"
	if !withEmpty {
		where += " AND EXISTS (SELECT 1 FROM album_photos p WHERE p.album_id = a.id)"
	}
	return r.queryAlbums(ctx, where, userID)
}

func (r *postgresAlbumRepository) GetOwned(ctx context.Context, albumID, userID uuid.UUID) (*models.ProfileAlbum, error) {
	albums, err := r.queryAlbums(ctx, "WHERE a.id = $1 AND a.user_id = $2", albumID, userID)
	if err != nil {
		return nil, err
	}
	if len(albums) == 0 {
		return nil, ErrAlbumNotFound
	}
	return albums[0], nil
}

func (r *postgresAlbumRepository) Create(ctx context.Context, userID uuid.UUID, title string, description *string) (uuid.UUID, error) {
	// ponytail: count-then-insert in one statement; two simultaneous creates at
	// 29 albums can both pass and land at 31. Lock the user row if that matters.
	var id uuid.UUID
	err := r.db.QueryRowContext(ctx, `INSERT INTO profile_albums (user_id, title, description, position)
		SELECT $1, $2, $3, COALESCE(MAX(position) + 1, 0) FROM profile_albums WHERE user_id = $1
		HAVING count(*) < $4
		RETURNING id`, userID, title, description, MaxAlbumsPerUser).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, &AlbumRuleError{fmt.Sprintf("You can have up to %d albums", MaxAlbumsPerUser)}
	}
	return id, err
}

func (r *postgresAlbumRepository) Update(ctx context.Context, albumID, userID uuid.UUID, u AlbumUpdate) error {
	sets := []string{"updated_at = now()"}
	args := []interface{}{albumID, userID}
	add := func(col string, v interface{}) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if u.Title != nil {
		add("title", *u.Title)
	}
	if u.Description != nil {
		add("description", *u.Description)
	}
	if u.Position != nil {
		add("position", *u.Position)
	}
	cond := ""
	if u.CoverPhotoID != nil {
		add("cover_photo_id", *u.CoverPhotoID)
		// The cover must be a photo of this same album.
		cond = fmt.Sprintf(" AND EXISTS (SELECT 1 FROM album_photos WHERE id = $%d AND album_id = $1)", len(args))
	}
	res, err := r.db.ExecContext(ctx, `UPDATE profile_albums SET `+strings.Join(sets, ", ")+
		` WHERE id = $1 AND user_id = $2`+cond, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if u.CoverPhotoID != nil {
			if _, gerr := r.GetOwned(ctx, albumID, userID); gerr == nil {
				return &AlbumRuleError{"Cover photo must be a photo in this album"}
			}
		}
		return ErrAlbumNotFound
	}
	return nil
}

func (r *postgresAlbumRepository) Delete(ctx context.Context, albumID, userID uuid.UUID) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `WITH gone AS (
			DELETE FROM profile_albums WHERE id = $1 AND user_id = $2 RETURNING id
		)
		SELECT p.url FROM album_photos p JOIN gone ON p.album_id = gone.id
		UNION ALL SELECT NULL FROM gone`, albumID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	urls := []string{}
	found := false
	for rows.Next() {
		var u sql.NullString
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		found = true
		if u.Valid {
			urls = append(urls, u.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrAlbumNotFound
	}
	return urls, nil
}

func (r *postgresAlbumRepository) AddPhotos(ctx context.Context, albumID, userID uuid.UUID, items []NewAlbumPhoto) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Lock the album so concurrent uploads can't both pass the cap.
	var locked uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT id FROM profile_albums WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		albumID, userID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAlbumNotFound
	}
	if err != nil {
		return err
	}

	var count, next int
	if err := tx.QueryRowContext(ctx, `SELECT count(*), COALESCE(MAX(position) + 1, 0) FROM album_photos WHERE album_id = $1`,
		albumID).Scan(&count, &next); err != nil {
		return err
	}
	if count+len(items) > MaxPhotosPerAlbum {
		return &AlbumRuleError{fmt.Sprintf("An album holds up to %d photos (%d left)", MaxPhotosPerAlbum, MaxPhotosPerAlbum-count)}
	}

	for i, it := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO album_photos (album_id, url, caption, width, height, position)
			VALUES ($1, $2, $3, $4, $5, $6)`, albumID, it.URL, it.Caption, it.Width, it.Height, next+i); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE profile_albums SET updated_at = now() WHERE id = $1`, albumID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *postgresAlbumRepository) DeletePhoto(ctx context.Context, albumID, photoID, userID uuid.UUID) (string, error) {
	var url string
	err := r.db.QueryRowContext(ctx, `DELETE FROM album_photos p USING profile_albums a
		WHERE p.id = $1 AND p.album_id = $2 AND a.id = p.album_id AND a.user_id = $3
		RETURNING p.url`, photoID, albumID, userID).Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAlbumNotFound
	}
	return url, err
}

func (r *postgresAlbumRepository) URLInUse(ctx context.Context, url string) (bool, error) {
	var used bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM album_photos WHERE url = $1)`, url).Scan(&used)
	return used, err
}
