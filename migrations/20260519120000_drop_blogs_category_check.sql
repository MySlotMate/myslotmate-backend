-- Migration: Drop the blogs.category check constraint
-- Added on: 2026-05-19
-- Description: Removes the enum-style CHECK that restricted blog categories to
-- ('Hosting', 'Wellness', 'Adventure'). Editorial team needs to introduce new
-- categories (e.g. Community, Safety, Culinary, Host Stories) without a code
-- change each time, so categories are now free-form VARCHAR(50) with no
-- whitelist. Length and NOT NULL constraints remain.

ALTER TABLE blogs
    DROP CONSTRAINT IF EXISTS blogs_category_check;
