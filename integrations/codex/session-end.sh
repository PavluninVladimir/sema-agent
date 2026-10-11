#!/bin/sh
# SessionEnd: закрыть сессию — сводка уже накоплена заметками, кристалл и
# факты из неё сделает роль «Память».
. "$(dirname "$0")/common.sh"
exec "$SEMAAGENT" -hook session-end -agent Codex
