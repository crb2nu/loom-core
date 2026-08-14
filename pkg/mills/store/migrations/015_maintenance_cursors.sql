-- +goose Up
-- +goose StatementBegin

-- Durable cursors let bounded maintenance jobs rotate through their complete
-- candidate set across process restarts. cursor_json is intentionally generic:
-- each named job owns the shape and validation of its opaque position value.
CREATE TABLE maintenance_cursors (
    name        TEXT PRIMARY KEY CHECK (trim(name) <> ''),
    cursor_json TEXT NOT NULL CHECK (json_valid(cursor_json) AND json_type(cursor_json) = 'object'),
    updated_at  TEXT NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS maintenance_cursors;
-- +goose StatementEnd
