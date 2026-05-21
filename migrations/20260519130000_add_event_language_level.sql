-- Migration: Add language(s) and level to events
-- Added on: 2026-05-19
-- Description: Experiences can now declare the languages they are conducted in
-- (multi-select: English, Hindi, Bengali, Assamese, or custom) and a difficulty
-- level (Beginner Friendly, Intermediate, Advanced). languages is a text array
-- so a host can pick several; level is a single nullable string.

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS languages TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS level VARCHAR(30);
