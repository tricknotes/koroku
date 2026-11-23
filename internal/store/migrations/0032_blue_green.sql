-- Blue/green deploys of the public service (ARCHITECTURE 7.2).
-- blue_green: 1 when the app replaces its public service side by side.
-- blue_green_drain_seconds: how long the old colour keeps running after
-- the switch (0 to 300).
-- alt_host_port: the second host port (green), allocated the first time
-- blue/green is turned on and kept afterwards; NULL before that.
-- web_color: the colour that receives the requests (blue on host_port,
-- green on alt_host_port); NULL while the public service runs as itself
-- on host_port.
ALTER TABLE apps ADD COLUMN blue_green INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN blue_green_drain_seconds INTEGER NOT NULL DEFAULT 10;
ALTER TABLE apps ADD COLUMN alt_host_port INTEGER;
ALTER TABLE apps ADD COLUMN web_color TEXT;
CREATE UNIQUE INDEX apps_alt_host_port ON apps (alt_host_port);

-- host_port and alt_host_port are unique together across all apps.
CREATE TRIGGER apps_ports_unique_insert BEFORE INSERT ON apps
WHEN NEW.host_port IN (SELECT alt_host_port FROM apps WHERE alt_host_port IS NOT NULL)
  OR NEW.alt_host_port IN (SELECT host_port FROM apps)
  OR NEW.alt_host_port = NEW.host_port
BEGIN
  SELECT RAISE(ABORT, 'UNIQUE constraint failed: apps host ports');
END;

CREATE TRIGGER apps_ports_unique_update BEFORE UPDATE OF host_port, alt_host_port ON apps
WHEN NEW.host_port IN (SELECT alt_host_port FROM apps WHERE alt_host_port IS NOT NULL AND id != NEW.id)
  OR NEW.alt_host_port IN (SELECT host_port FROM apps WHERE id != NEW.id)
  OR NEW.alt_host_port = NEW.host_port
BEGIN
  SELECT RAISE(ABORT, 'UNIQUE constraint failed: apps host ports');
END;

-- web_color: the colour the deployment started (NULL when it ran the
-- public service as itself); switched_at: when the requests were switched
-- to it (Unix milliseconds).
ALTER TABLE deployments ADD COLUMN web_color TEXT;
ALTER TABLE deployments ADD COLUMN switched_at INTEGER;
