-- Retain the exact entry form while queued tasks survive Worker restarts.
-- Legacy rows continue to use their original shell command semantics.
ALTER TABLE tasks ADD COLUMN command_mode TEXT NOT NULL DEFAULT 'command'
    CHECK (command_mode IN ('command', 'script', 'argv'));
ALTER TABLE tasks ADD COLUMN command_argv TEXT;
