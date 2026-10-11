# Общее для хуков Codex: где лежит житель машины. Адреса Сёмы и токена
# здесь больше нет — хук ходит к жителю машины, а тот держит опознанный
# провод и сам знает, куда звонить.
SEMAAGENT="${SEMA_AGENT_BIN:-$(command -v semaagent 2>/dev/null)}"
[ -n "$SEMAAGENT" ] || SEMAAGENT="$HOME/go/bin/semaagent"
[ -x "$SEMAAGENT" ] || exit 0
