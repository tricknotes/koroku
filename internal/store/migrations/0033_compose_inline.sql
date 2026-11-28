-- A Compose file kept in koroku instead of the repository (ARCHITECTURE 6.3).
-- compose_source: 'repository' reads the repository's file (compose_path),
-- 'inline' deploys compose_content.
-- compose_content: the Compose file as written in the console; kept while
-- compose_source is 'repository'.
ALTER TABLE apps ADD COLUMN compose_source TEXT NOT NULL DEFAULT 'repository';
ALTER TABLE apps ADD COLUMN compose_content TEXT NOT NULL DEFAULT '';
