-- Where the console reads the environment variables an app's repository
-- needs (ARCHITECTURE 9.4): 'compose' (${VAR} references in the Compose
-- file), 'dotenv' (a .env.example file) or 'off'. Existing apps get the
-- recommended 'compose'. env_example_path is the file read in dotenv mode;
-- empty means .env.example.
ALTER TABLE apps ADD COLUMN env_source TEXT NOT NULL DEFAULT 'compose'
    CHECK (env_source IN ('compose', 'dotenv', 'off'));
ALTER TABLE apps ADD COLUMN env_example_path TEXT NOT NULL DEFAULT '';
