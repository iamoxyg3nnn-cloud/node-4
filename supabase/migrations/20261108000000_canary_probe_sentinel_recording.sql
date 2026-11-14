-- Migration 20261108000000_canary_probe_sentinel_recording.sql
-- Inserts the sentinel recording row with all-zeros UUID used by the health check
-- canary (server.VerifyUploadLinkWritePath) to satisfy upload_links(recording_id) FK constraint.

INSERT INTO public.recordings (id, username, filename, timestamp, instance_id, room_title)
VALUES (
  '00000000-0000-0000-0000-000000000000',
  '__write_path_probe__',
  '__write_path_probe__.mp4',
  now(),
  'sentinel',
  'Canary Probe Recording'
)
ON CONFLICT (id) DO NOTHING;
