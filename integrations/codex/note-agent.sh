#!/bin/sh
# Stop (конец ответа): последняя реплика агента идёт в сводку сессии.
# Codex отдаёт её прямо в хук, разбирать стенограмму не нужно.
. "$(dirname "$0")/common.sh"
exec "$SEMAAGENT" -hook note -agent Codex
