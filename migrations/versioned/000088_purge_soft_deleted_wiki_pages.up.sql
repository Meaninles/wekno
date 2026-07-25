-- Wiki pages have no restore workflow. Historical GORM soft-delete rows still
-- retain generated source prose even though every user-facing query hides
-- them. Permanently remove only rows that were already logically deleted;
-- active/quarantined shared-source pages have deleted_at IS NULL and are not
-- affected.
DELETE FROM wiki_pages
WHERE deleted_at IS NOT NULL;
