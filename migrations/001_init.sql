-- Mail icerigi saklanmaz: yalnizca hangi mailin islendigi (kimligin ozeti) ve verilen karar.
CREATE TABLE IF NOT EXISTS oauth_token (
    id          int PRIMARY KEY CHECK (id = 1),
    refresh_enc bytea NOT NULL,          -- AES-GCM ile sifreli yenileme anahtari
    scope       text NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS state (
    key   text PRIMARY KEY,
    value text NOT NULL
);

CREATE TABLE IF NOT EXISTS processed (
    id_hash      bytea PRIMARY KEY,      -- sha256(Graph mesaj kimligi)
    received_at  timestamptz NOT NULL,
    level        text NOT NULL,          -- none | important | urgent
    notified     boolean NOT NULL DEFAULT false,
    marked_read  boolean NOT NULL DEFAULT false,
    processed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS processed_processed_at_idx ON processed (processed_at);
