package service

import (
	"context"
	"errors"
	"time"

	"myslotmate-backend/internal/lib/realtime"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

type InboxService interface {
	SendMessage(ctx context.Context, req SendMessageRequest) (*models.InboxMessage, error)
	BroadcastMessage(ctx context.Context, hostID uuid.UUID, req BroadcastRequest) (*models.InboxMessage, error)
	GetEventMessages(ctx context.Context, eventID uuid.UUID) ([]*models.InboxMessage, error)
	GetHostMessages(ctx context.Context, hostID uuid.UUID) ([]*models.InboxMessage, error)
	MarkRead(ctx context.Context, messageID uuid.UUID) error
}

type SendMessageRequest struct {
	EventID       uuid.UUID                `json:"event_id"`
	SenderType    models.MessageSenderType `json:"sender_type"`
	SenderID      *uuid.UUID               `json:"sender_id,omitempty"`
	Message       string                   `json:"message"`
	AttachmentURL *string                  `json:"attachment_url,omitempty"`
}

type BroadcastRequest struct {
	EventID uuid.UUID
	Message string
}

type inboxService struct {
	inboxRepo     repository.InboxRepository
	eventRepo     repository.EventRepository
	socketService *realtime.SocketService
}

func NewInboxService(ir repository.InboxRepository, er repository.EventRepository, ss *realtime.SocketService) InboxService {
	return &inboxService{
		inboxRepo:     ir,
		eventRepo:     er,
		socketService: ss,
	}
}

func (s *inboxService) SendMessage(ctx context.Context, req SendMessageRequest) (*models.InboxMessage, error) {
	// Validate event exists
	evt, err := s.eventRepo.GetByID(ctx, req.EventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}

	msg := &models.InboxMessage{
		ID:            uuid.New(),
		EventID:       req.EventID,
		HostID:        evt.HostID,
		SenderType:    req.SenderType,
		SenderID:      req.SenderID,
		Message:       req.Message,
		AttachmentURL: req.AttachmentURL,
		IsRead:        false,
		CreatedAt:     time.Now(),
	}

	if err := s.inboxRepo.Create(ctx, msg); err != nil {
		return nil, err
	}

	// Real-time broadcast (only if socket service is available)
	if s.socketService != nil {
		roomName := "event_" + req.EventID.String()
		s.socketService.BroadcastToRoom(roomName, "inbox_update", map[string]interface{}{
			"id":          msg.ID,
			"sender_type": msg.SenderType,
			"sender_id":   msg.SenderID,
			"message":     msg.Message,
			"created_at":  msg.CreatedAt,
		})
	}

	return msg, nil
}

func (s *inboxService) BroadcastMessage(ctx context.Context, hostID uuid.UUID, req BroadcastRequest) (*models.InboxMessage, error) {
	// Verify Event Ownership
	evt, err := s.eventRepo.GetByID(ctx, req.EventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if !HostCanManageEvent(ctx, s.eventRepo, evt, hostID) {
		return nil, errors.New("unauthorized: you do not own this event")
	}

	msg := &models.InboxMessage{
		ID:         uuid.New(),
		EventID:    req.EventID,
		HostID:     hostID,
		SenderType: models.MessageSenderHost,
		SenderID:   &hostID,
		Message:    req.Message,
		IsRead:     false,
		CreatedAt:  time.Now(),
	}

	if err := s.inboxRepo.Create(ctx, msg); err != nil {
		return nil, err
	}

	// Real-time broadcast (only if socket service is available)
	if s.socketService != nil {
		roomName := "event_" + req.EventID.String()
		s.socketService.BroadcastToRoom(roomName, "inbox_update", map[string]interface{}{
			"id":          msg.ID,
			"sender_type": msg.SenderType,
			"sender_id":   msg.SenderID,
			"message":     req.Message,
			"event_id":    req.EventID,
			"created_at":  msg.CreatedAt,
		})
	}

	return msg, nil
}

func (s *inboxService) GetEventMessages(ctx context.Context, eventID uuid.UUID) ([]*models.InboxMessage, error) {
	return s.inboxRepo.ListByEventID(ctx, eventID)
}

func (s *inboxService) GetHostMessages(ctx context.Context, hostID uuid.UUID) ([]*models.InboxMessage, error) {
	return s.inboxRepo.ListByHostID(ctx, hostID)
}

func (s *inboxService) MarkRead(ctx context.Context, messageID uuid.UUID) error {
	return s.inboxRepo.MarkRead(ctx, messageID)
}
