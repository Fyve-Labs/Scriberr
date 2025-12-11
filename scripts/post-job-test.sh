#!/usr/bin/env bash

API_KEY=$(sqlite3 data/scriberr.db 'select key from api_keys limit 1')

xh http://localhost:8080/api/v1/transcription/aws-transcribe "X-API-Key:${API_KEY}" \
  TranscriptionJobName=tessa-dev-734f3a78-4e04-4a4e-bf61-55a2151c44b9 \
  Media[MediaFileUri]=s3://prod-audio-raw-209479271613-us-east-1/734f3a78-4e04-4a4e-bf61-55a2151c44b9.mp3 \
  LanguageCode=en-US \
  OutputBucketName=dev-transcribe-output-209479271613-us-east-1 \
  Tags[0][Key]=env Tags[0][Value]=dev