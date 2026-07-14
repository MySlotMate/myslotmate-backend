package repository

import (
	"context"
	"database/sql"

	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type AttendeeProfileRepository interface {
	GetByUserID(ctx context.Context, userID uuid.UUID) (*models.AttendeeProfile, error)
	ListByUserIDs(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]*models.AttendeeProfile, error)
	Upsert(ctx context.Context, p *models.AttendeeProfile) error
}

type postgresAttendeeProfileRepository struct {
	db *sql.DB
}

func NewAttendeeProfileRepository(db *sql.DB) AttendeeProfileRepository {
	return &postgresAttendeeProfileRepository{db: db}
}

const attendeeProfileColumns = `user_id, name, age, gender, qualification, occupation,
	marital_status, contact_number, whatsapp_number, registration_type, govt_id_url, travel,
	created_at, updated_at`

func scanAttendeeProfile(row interface {
	Scan(dest ...interface{}) error
}) (*models.AttendeeProfile, error) {
	p := &models.AttendeeProfile{}
	err := row.Scan(
		&p.UserID, &p.Name, &p.Age, &p.Gender, &p.Qualification, &p.Occupation,
		&p.MaritalStatus, &p.ContactNumber, &p.WhatsappNumber, &p.RegistrationType, &p.GovtIDURL, &p.Travel,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

func (r *postgresAttendeeProfileRepository) GetByUserID(ctx context.Context, userID uuid.UUID) (*models.AttendeeProfile, error) {
	query := `SELECT ` + attendeeProfileColumns + ` FROM attendee_profiles WHERE user_id = $1`
	return scanAttendeeProfile(r.db.QueryRowContext(ctx, query, userID))
}

func (r *postgresAttendeeProfileRepository) ListByUserIDs(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]*models.AttendeeProfile, error) {
	out := make(map[uuid.UUID]*models.AttendeeProfile)
	if len(userIDs) == 0 {
		return out, nil
	}
	query := `SELECT ` + attendeeProfileColumns + ` FROM attendee_profiles WHERE user_id = ANY($1)`
	rows, err := r.db.QueryContext(ctx, query, pq.Array(userIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		p, err := scanAttendeeProfile(rows)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out[p.UserID] = p
		}
	}
	return out, rows.Err()
}

// Upsert writes the profile, overwriting existing values for that user. Only the
// supplied (non-nil) fields are updated; a nil field leaves the stored value
// intact so an event that collects a subset of fields never wipes earlier answers.
func (r *postgresAttendeeProfileRepository) Upsert(ctx context.Context, p *models.AttendeeProfile) error {
	query := `
		INSERT INTO attendee_profiles (
			user_id, name, age, gender, qualification, occupation,
			marital_status, contact_number, whatsapp_number, registration_type, govt_id_url, travel,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11, $12,
			now(), now()
		)
		ON CONFLICT (user_id) DO UPDATE SET
			name              = COALESCE(EXCLUDED.name, attendee_profiles.name),
			age               = COALESCE(EXCLUDED.age, attendee_profiles.age),
			gender            = COALESCE(EXCLUDED.gender, attendee_profiles.gender),
			qualification     = COALESCE(EXCLUDED.qualification, attendee_profiles.qualification),
			occupation        = COALESCE(EXCLUDED.occupation, attendee_profiles.occupation),
			marital_status    = COALESCE(EXCLUDED.marital_status, attendee_profiles.marital_status),
			contact_number    = COALESCE(EXCLUDED.contact_number, attendee_profiles.contact_number),
			whatsapp_number   = COALESCE(EXCLUDED.whatsapp_number, attendee_profiles.whatsapp_number),
			registration_type = COALESCE(EXCLUDED.registration_type, attendee_profiles.registration_type),
			govt_id_url       = COALESCE(EXCLUDED.govt_id_url, attendee_profiles.govt_id_url),
			travel            = COALESCE(EXCLUDED.travel, attendee_profiles.travel),
			updated_at        = now()
	`
	_, err := r.db.ExecContext(ctx, query,
		p.UserID, p.Name, p.Age, p.Gender, p.Qualification, p.Occupation,
		p.MaritalStatus, p.ContactNumber, p.WhatsappNumber, p.RegistrationType, p.GovtIDURL, p.Travel,
	)
	return err
}
