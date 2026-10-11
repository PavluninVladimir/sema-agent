#!/bin/sh
# Установка жителя машины на маке по записанному порядку. Тот же порядок,
# что на Linux (deploy/README.md), только служба — launchd.
#
#   ./deploy/install-mac.sh [адрес-Сёмы]     # по умолчанию sema.local:8766
#
# Метка службы launchd — SEMA_AGENT_LABEL (по умолчанию org.example.semaagent).
# Служба жителя, поставленная раньше под другой меткой (имя кончается на
# .semaagent), снимается: два жителя на одной машине делили бы одну дверь
# и одно удостоверение.
#
# Ставит бинарник в ~/.local/bin, разворачивает шаблон plist с настоящими
# путями и перезагружает службу. Удостоверение в ~/.sema-agent не трогает:
# обновление на месте — замена бинарника, заводить агента заново не надо.
#
# Имена переменных латиницей нарочно: /bin/sh на маке кириллических имён не
# принимает, а `sh -n` этого не показывает — он разбирает «ИМЯ=значение» как
# команду и молчит.
set -eu

ADDR="${1:-sema.local:8766}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LABEL="${SEMA_AGENT_LABEL:-org.example.semaagent}"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

echo "== сборка"
make -C "$ROOT" build

echo "== бинарник в $HOME/.local/bin"
mkdir -p "$HOME/.local/bin" "$HOME/.sema-agent"
# права каталога состояния — только владельцу: в нём удостоверение агента
chmod 700 "$HOME/.sema-agent"
# сначала рядом, потом переименованием: живой процесс держит свой файл, и
# перезапись на месте дала бы «text file busy»
cp "$ROOT/semaagent" "$HOME/.local/bin/semaagent.новый"
mv -f "$HOME/.local/bin/semaagent.новый" "$HOME/.local/bin/semaagent"

echo "== служба launchd"
mkdir -p "$HOME/Library/LaunchAgents"
# шаблон живёт в репозитории, подстановка — здесь: launchd не понимает ни
# «~», ни переменных окружения в путях
sed -e "s|@LABEL@|$LABEL|g" -e "s|@ДОМ@|$HOME|g" -e "s|@АДРЕС_СЁМЫ@|$ADDR|g" \
	"$ROOT/deploy/semaagent.plist" > "$PLIST"
plutil -lint "$PLIST"

# bootout перед bootstrap, а не kickstart: kickstart перезапускает процесс
# по уже загруженному описанию и изменений в plist не видит.
#
# И ждать, пока служба действительно ушла: bootout возвращается раньше, чем
# launchd её убрал, и следующий bootstrap падает с «Input/output error» —
# после чего службы нет ни старой, ни новой. Наступали 27 сентября 2026.
# прежняя служба жителя под другой меткой — снять, иначе их станет две
for old in $(launchctl list | awk '{print $3}' | grep '\.semaagent$' | grep -vx "$LABEL" || true); do
	echo "снимаю прежнюю службу $old"
	launchctl bootout "gui/$(id -u)/$old" 2>/dev/null || true
	rm -f "$HOME/Library/LaunchAgents/$old.plist"
done

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
i=0
while launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -gt 50 ]; then
		echo "служба $LABEL не уходит после bootout — разбираться руками" >&2
		exit 1
	fi
	sleep 0.2
done
launchctl bootstrap "gui/$(id -u)" "$PLIST"

echo "== проверка"
sleep 3
launchctl print "gui/$(id -u)/$LABEL" | grep -E '^[[:space:]]+(state|pid) ' || true
echo "журнал: tail -f $HOME/.sema-agent/agent.log"
echo "нет удостоверения? получите одноразовый код на экране «Агенты» и:"
echo "  $HOME/.local/bin/semaagent -enroll <код> -role житель -sema http://sema.local:8765"
