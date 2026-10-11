#!/bin/sh
# SessionStart: память по проекту уходит в контекст Codex. Возобновление,
# очистка и сжатие продолжают ту же сессию, а не открывают новую.
# Codex ждёт JSON — его печатает сам житель машины.
. "$(dirname "$0")/common.sh"
exec "$SEMAAGENT" -hook session-start -agent Codex
