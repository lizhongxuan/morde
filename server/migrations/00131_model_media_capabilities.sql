-- +goose Up
ALTER TABLE model_connections
  ADD COLUMN capability_media_image_generation boolean NOT NULL DEFAULT false,
  ADD COLUMN capability_media_video_generation boolean NOT NULL DEFAULT false,
  ADD COLUMN capability_media_speech_generation boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT model_connections_media_capabilities_checked_check CHECK (
    checked_at IS NOT NULL OR (
      NOT capability_media_image_generation AND
      NOT capability_media_video_generation AND
      NOT capability_media_speech_generation
    )
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM model_connections
    WHERE capability_media_image_generation OR capability_media_video_generation OR capability_media_speech_generation
  ) THEN
    RAISE EXCEPTION 'cannot roll back checked model media capabilities';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE model_connections
  DROP CONSTRAINT model_connections_media_capabilities_checked_check,
  DROP COLUMN capability_media_image_generation,
  DROP COLUMN capability_media_video_generation,
  DROP COLUMN capability_media_speech_generation;
