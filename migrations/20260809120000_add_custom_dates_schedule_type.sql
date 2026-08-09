-- Add schedule_type and custom_dates columns to events table
ALTER TABLE events ADD COLUMN IF NOT EXISTS schedule_type VARCHAR(20) NOT NULL DEFAULT 'one_time';
ALTER TABLE events ADD COLUMN IF NOT EXISTS custom_dates text[] DEFAULT '{}';
