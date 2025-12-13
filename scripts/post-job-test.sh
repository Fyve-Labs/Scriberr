#!/usr/bin/env bash

LOCAL_API_KEY=$(sqlite3 data/scriberr.db 'select key from api_keys limit 1')
BASE_URL=${BASE_URL:-http://localhost:8080}
API_KEY=${API_KEY:-$LOCAL_API_KEY}

MAX_JOBS=${1:-1}

for (( i=1; i<=$MAX_JOBS; i++ )); do
    random_uid=$(uuidgen)
    echo "POSTING JOB $i"
    xh http://localhost:8080/api/v1/transcription/aws-transcribe "X-API-Key:${API_KEY}" \
      TranscriptionJobName=viet-dev-${random_uid,,} \
      Media[MediaFileUri]=s3://prod-audio-raw-209479271613-us-east-1/734f3a78-4e04-4a4e-bf61-55a2151c44b9.mp3 \
      LanguageCode=en-US \
      OutputBucketName=dev-transcribe-output-209479271613-us-east-1 \
      Tags[0][Key]=media-id Tags[0][Value]=${random_uid,,}
done


