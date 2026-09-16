-- +goose Up
-- +goose StatementBegin
UPDATE outcome_writebacks
SET grade = (
    SELECT backlog_items.grade
    FROM backlog_items
    WHERE backlog_items.id = outcome_writebacks.backlog_id
)
WHERE (grade IS NULL OR trim(grade) = '')
  AND EXISTS (
      SELECT 1
      FROM backlog_items
      WHERE backlog_items.id = outcome_writebacks.backlog_id
        AND backlog_items.grade IS NOT NULL
        AND trim(backlog_items.grade) <> ''
  );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
