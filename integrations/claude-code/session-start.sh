#!/bin/sh
# SessionStart: память по проекту в контекст. Возобновление и сжатие
# контекста продолжают ту же сессию, а не открывают новую.
. "$(dirname "$0")/common.sh"
exec "$SEMAAGENT" -hook session-start -agent "Claude Code"
