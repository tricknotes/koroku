-- What each service's memory limit and reservation depend on besides the
-- app settings (ARCHITECTURE 6.2): JSON object, service -> {own_limit,
-- one_shot}, of the deployment's release. It lets the reservations the
-- current settings would give be told without the Compose file. NULL when
-- not recorded.
ALTER TABLE deployments ADD COLUMN memory_shapes TEXT;
