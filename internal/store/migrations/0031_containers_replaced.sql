-- Whether the deployment's run touched the running containers, that is,
-- reached the start step (ARCHITECTURE 9.5): 1 or 0, written when the run
-- ends. NULL when it never ran to the end (canceled while queued, or
-- koroku stopped during it). A failed deployment with 1 started the new
-- release and rolled back, so the memory history marks it.
ALTER TABLE deployments ADD COLUMN containers_replaced INTEGER;
