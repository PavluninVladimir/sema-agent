#!/bin/sh
# Stop (конец каждого ответа): последний обмен репликами дописывается в
# сводку открытой сессии. Кристалл делается при закрытии сессии.
. "$(dirname "$0")/common.sh"
exec "$SEMAAGENT" -hook note -agent "Claude Code"
