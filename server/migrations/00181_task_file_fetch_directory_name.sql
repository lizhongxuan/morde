-- +goose Up
ALTER TABLE task_file_fetches
  ADD COLUMN source_directory_name text NOT NULL DEFAULT '',
  ADD CONSTRAINT task_file_fetches_directory_name_check
    CHECK (source_directory_name = '' OR (
      octet_length(source_directory_name) BETWEEN 1 AND 100
      AND source_directory_name NOT IN ('.', '..')
      AND position('/' in source_directory_name) = 0
      AND position(chr(92) in source_directory_name) = 0
      AND source_directory_name !~ '[[:cntrl:]]'
    ));

-- +goose Down
ALTER TABLE task_file_fetches
  DROP CONSTRAINT task_file_fetches_directory_name_check,
  DROP COLUMN source_directory_name;
