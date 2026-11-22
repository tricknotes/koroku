-- Memory reservations of production apps (ARCHITECTURE 6.2).
-- memory_reservation: app-wide value ('' = half of each service's limit,
-- '0' = none); service_memory_reservations: JSON object of per-service
-- values. deployments.memory_reservations: JSON object, service -> bytes,
-- of what the release's override reserves; NULL when not recorded.
ALTER TABLE apps ADD COLUMN memory_reservation TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN service_memory_reservations TEXT NOT NULL DEFAULT '{}';
ALTER TABLE deployments ADD COLUMN memory_reservations TEXT;
