-- Migration: Create experience_templates table
-- Added on: 2026-05-13
-- Description: Reusable mood-keyed title + hook_line suggestions surfaced in the
-- event creation form. Hosts pick a mood, then choose a template to prefill
-- title and hook line, with full freedom to edit afterwards.

CREATE TABLE IF NOT EXISTS experience_templates (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    mood       TEXT NOT NULL,
    title      TEXT NOT NULL,
    hook_line  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_experience_templates_mood ON experience_templates (mood);

-- Seed initial suggestions across all canonical moods
INSERT INTO experience_templates (mood, title, hook_line) VALUES
    ('adventurous', 'Sunrise Mountain Trek', 'Chase the first light at the summit'),
    ('adventurous', 'River Rafting Expedition', 'Ride the rapids with seasoned guides'),
    ('adventurous', 'Cliffside Rock Climbing', 'Scale heights, conquer fears'),
    ('adventurous', 'Backcountry Camping', 'Off-grid nights under starry skies'),

    ('relaxing', 'Sunset Yoga by the Beach', 'Unwind as the sun melts into the sea'),
    ('relaxing', 'Hot Spring Soak Retreat', 'Let warm waters wash the week away'),
    ('relaxing', 'Forest Bathing Walk', 'Slow steps, deep breaths, quiet woods'),

    ('creative', 'Pottery Wheel Workshop', 'Shape clay, shape stories'),
    ('creative', 'Watercolor in the Park', 'Paint the city in soft hues'),
    ('creative', 'Hands-on Photography Walk', 'Frame the streets through your lens'),

    ('social', 'Trivia & Tapas Night', 'Sharp wits, shared plates'),
    ('social', 'Rooftop Mixer with Live DJ', 'Skyline views, new connections'),
    ('social', 'Board Game Café Meetup', 'Bring your competitive side'),

    ('educational', 'Local History Walking Tour', 'Hidden stories on every corner'),
    ('educational', 'Astronomy Night Under the Stars', 'Trace constellations with experts'),
    ('educational', 'Sustainable Living Workshop', 'Small habits, big impact'),

    ('wellness', 'Morning Meditation Circle', 'Start the day grounded'),
    ('wellness', 'Sound Bath Healing Session', 'Vibrations that restore'),
    ('wellness', 'Breathwork & Cold Plunge', 'Reset body and mind'),

    ('culinary', 'Farm-to-Table Cooking Class', 'Pick it, cook it, savor it'),
    ('culinary', 'Street Food Crawl', 'Six stops, one unforgettable evening'),
    ('culinary', 'Wine & Cheese Pairing Night', 'A glass for every wedge'),

    ('cultural', 'Heritage Quarter Photo Walk', 'Old walls, new perspectives'),
    ('cultural', 'Traditional Dance Workshop', 'Move to rhythms passed down generations'),
    ('cultural', 'Local Artisan Studio Visit', 'Meet the makers behind the craft')
ON CONFLICT DO NOTHING;
