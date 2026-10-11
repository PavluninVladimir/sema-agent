package main

// Роль агента — житель машины или шлюз, взаимоисключающие
// (openspec/changes/agent-anywhere). Роль закрепляется Сёмой при выдаче
// одноразового кода приглашения и не меняется у уже заведённого агента:
// смена роли — новый экземпляр с новым удостоверением (design.md,
// решение 1). Здесь — само понятие и запрет совмещать способности; сам
// шлюз — предмет отдельного изменения (agent-gateway).

import (
	"log"
	"strings"
)

// Role — одна из двух ролей агента.
type Role string

const (
	// RoleResident — житель машины: карточки кода, память, хуки, значок у
	// часов. Читает репозитории человека — поэтому не должен смотреть в
	// интернет.
	RoleResident Role = "житель"
	// RoleGateway — шлюз: лицо агента в интернет. Держит домен, принимает
	// запросы браузеров и вносит их к Сёме по проводу (internal/gate).
	RoleGateway Role = "шлюз"
)

// ParseRole — роль по строке. Пусто или незнакомое слово — не роль вовсе:
// вызывающий обязан сам решить, что делать с неизвестным (обычно —
// отказать в способностях, а не угадывать роль).
func ParseRole(s string) (Role, bool) {
	switch Role(strings.TrimSpace(s)) {
	case RoleResident:
		return RoleResident, true
	case RoleGateway:
		return RoleGateway, true
	}
	return "", false
}

// roleFromIdentity — роль этого запуска. Берётся из удостоверения, а не
// из флага: закрепил её Сёма при выдаче кода, и переспрашивать машину
// об этом на каждом старте значило бы дать ей назваться кем угодно
// (design.md, решение 1).
//
// Пустая роль в удостоверении и вовсе отсутствующее удостоверение читаются
// как «житель машины»: удостоверения, заведённые до появления ролей,
// принадлежат жителю — он им и был (design.md, Migration Plan п.1), и мак
// получает свою роль без переподключения. А вот незнакомое слово не
// угадывается: такую роль возвращаем как есть, и allowedForRole не выдаст
// по ней ни одной способности.
func roleFromIdentity(inIdentity string, has bool) Role {
	if !has || strings.TrimSpace(inIdentity) == "" {
		return RoleResident
	}
	if r, ok := ParseRole(inIdentity); ok {
		return r
	}
	log.Printf("роль %q в удостоверении не распознана — способности не объявляю вовсе", inIdentity)
	return Role(strings.TrimSpace(inIdentity))
}

// CanGateway — способность шлюза: вносить к Сёме запросы из
// интернета. Единственная способность его роли, и жителю она не положена —
// он читает репозитории человека, и смотреть в интернет тем же телом нельзя.
const CanGateway = "шлюз"

// roleCapabilities — единственное место, где решается, какие способности
// положены роли (design.md, риск «Забыли про роль при добавлении новой
// способности»). TestКаждаяСпособностьРасписанаПоРолям следит, чтобы
// новая способность не осталась вне этой карты и не разошлась молча.
var roleCapabilities = map[Role]map[string]bool{
	RoleResident: {"код": true, "память": true, "уведомления": true},
	RoleGateway:  {CanGateway: true},
}

// allKnownCapabilities — полный список способностей, которые вообще умеет
// объявлять этот бинарник, независимо от роли и от того, что доступно на
// машине (candidateCapabilities сужает его по месту).
var allKnownCapabilities = []string{"код", "память", "уведомления", CanGateway}

// allowedForRole — способности из candidate, которые роли положены, и
// отдельно отсеянные — с причиной для журнала. Несписочная способность
// (в том числе из-за незнакомой роли) — отказ с названной причиной, а не
// тихий пропуск (spec: «Попытка объявить способности чужой роли»).
func allowedForRole(role Role, candidate []string) (allowed []string, denied map[string]string) {
	denied = map[string]string{}
	known, roleExists := roleCapabilities[role]
	for _, c := range candidate {
		if roleExists && known[c] {
			allowed = append(allowed, c)
			continue
		}
		why := "способность не положена роли «" + string(role) + "»"
		if !roleExists {
			why = "роль «" + string(role) + "» не распознана — способности не выдаются вовсе"
		}
		denied[c] = why
	}
	return allowed, denied
}

// hasCap — есть ли способность в уже отфильтрованном ролью списке.
func hasCap(caps []string, name string) bool {
	for _, c := range caps {
		if c == name {
			return true
		}
	}
	return false
}
