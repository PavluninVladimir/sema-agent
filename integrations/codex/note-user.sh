#!/bin/sh
# UserPromptSubmit: реплика человека дописывается в сводку открытой сессии.
. "$(dirname "$0")/common.sh"
exec "$SEMAAGENT" -hook note -agent Codex
