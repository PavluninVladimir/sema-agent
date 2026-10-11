package main

import (
	"testing"

	"github.com/PavluninVladimir/sema-agent/internal/desk"
)

// TestGatewayDeniedCodeReading — spec: «Шлюзу не дают чтение
// кода»: способности жителя машины (код, память) роли шлюза не
// перепадают, даже если бинарник умеет их объявить.
func TestGatewayDeniedCodeReading(t *testing.T) {
	allowed, denied := allowedForRole(RoleGateway, []string{"код", "память", "уведомления"})
	if len(allowed) != 0 {
		t.Fatalf("шлюзу не должно достаться ни одной способности жителя, получил %v", allowed)
	}
	for _, name := range []string{"код", "память", "уведомления"} {
		if denied[name] == "" {
			t.Fatalf("отказ по способности %q обязан называть причину", name)
		}
	}
}

// TestResidentDeniedGatewayCapability — spec: «Жителю машины не дают
// шлюзовую»: множества способностей ролей не пересекаются. Как
// только у шлюза появится своя способность (agent-gateway), этот
// тест поймает случайное совпадение с ролью жителя.
func TestResidentDeniedGatewayCapability(t *testing.T) {
	for name := range roleCapabilities[RoleGateway] {
		if roleCapabilities[RoleResident][name] {
			t.Fatalf("способность %q одновременно у жителя машины и у шлюза — роли обязаны не пересекаться", name)
		}
	}
}

// TestEveryCapabilityAssignedToRoles — design.md, риск «Забыли про роль
// при добавлении новой способности»: то, чего candidateCapabilities может
// вернуть, обязано быть решено хотя бы для одной роли явно. Способность
// без места в этой карте — забытая строка, а не молчаливое расхождение.
func TestEveryCapabilityAssignedToRoles(t *testing.T) {
	for _, name := range allKnownCapabilities {
		found := false
		for _, caps := range roleCapabilities {
			if caps[name] {
				found = true
			}
		}
		if !found {
			t.Fatalf("способность %q не положена ни одной роли — не вписана в roleCapabilities", name)
		}
	}
}

// TestUnlistedCapabilityDenied — способность, которой роль не знает,
// отсеивается с причиной, а не пропадает молча и не проходит по умолчанию.
func TestUnlistedCapabilityDenied(t *testing.T) {
	allowed, denied := allowedForRole(RoleResident, []string{"код", "выдумка"})
	if len(allowed) != 1 || allowed[0] != "код" {
		t.Fatalf("ожидал только «код» в разрешённых, получил %v", allowed)
	}
	if denied["выдумка"] == "" {
		t.Fatalf("несписочная способность должна попасть в отказ с причиной")
	}
}

// TestUnknownRoleGetsNothing — роль, которой ядро не знает
// (испорченное или чужое удостоверение), не получает ни одной способности:
// отказ по умолчанию, а не угадывание.
func TestUnknownRoleGetsNothing(t *testing.T) {
	allowed, denied := allowedForRole(Role("гость"), []string{"код", "память"})
	if len(allowed) != 0 {
		t.Fatalf("незнакомая роль не должна получать способности, получила %v", allowed)
	}
	for _, name := range []string{"код", "память"} {
		if denied[name] == "" {
			t.Fatalf("отказ по %q обязан называть причину даже для незнакомой роли", name)
		}
	}
}

func TestParseRole(t *testing.T) {
	if r, ok := ParseRole("житель"); !ok || r != RoleResident {
		t.Fatalf("житель не разобрался: %v %v", r, ok)
	}
	if r, ok := ParseRole("  шлюз \n"); !ok || r != RoleGateway {
		t.Fatalf("шлюз (с пробелами) не разобрался: %v %v", r, ok)
	}
	if _, ok := ParseRole("кто-то"); ok {
		t.Fatalf("незнакомое слово не должно разбираться как роль")
	}
	if _, ok := ParseRole(""); ok {
		t.Fatalf("пустая строка не должна разбираться как роль")
	}
}

// TestMachineCapabilitiesCoveredByRoles — вторая половина того же правила:
// проверять roleCapabilities по allKnownCapabilities мало, если сам
// candidateCapabilities вернёт что-то третье. Новая способность, добавленная
// в candidateCapabilities и забытая в ролях, ломает тест здесь — а не тихо
// объявляется Сёме мимо разделения ролей.
func TestMachineCapabilitiesCoveredByRoles(t *testing.T) {
	known := map[string]bool{}
	for _, name := range allKnownCapabilities {
		known[name] = true
	}
	for _, name := range candidateCapabilities() {
		if !known[name] {
			t.Fatalf("способность %q машина объявляет, а в allKnownCapabilities её нет — роли о ней не знают", name)
		}
	}
}

// TestNotificationsOnlyWhereShowable — tasks 4.3: на Linux
// способность «уведомления» не объявляется вовсе, раз показать их нечем.
// Обе стороны правила проверяются здесь, а не по той машине, на которой
// случилось запустить тест: на маке рабочий стол есть всегда, и половина
// правила там не проверялась бы вовсе.
func TestNotificationsOnlyWhereShowable(t *testing.T) {
	if !hasCap(machineCapabilities(true), "уведомления") {
		t.Fatalf("есть чем показывать, а способность «уведомления» не объявлена")
	}
	if hasCap(machineCapabilities(false), "уведомления") {
		t.Fatalf("показывать нечем, а способность «уведомления» всё равно объявлена: Сёма поверит в доставку, которой не будет")
	}
	// и то, что зовёт сама машина, идёт той же дорогой — иначе развилка
	// осталась бы правильной, а candidateCapabilities обошла бы её
	if hasCap(candidateCapabilities(), "уведомления") != desk.Available() {
		t.Fatalf("candidateCapabilities разошлась с рабочим столом машины (доступен=%v)", desk.Available())
	}
}

// TestOldIdentityIsResident — design.md, Migration Plan п.1: агент,
// заведённый до появления ролей, получает роль жителя машины без
// переподключения. Он ею и был, и терять из-за молчащего поля способности
// он не должен.
func TestOldIdentityIsResident(t *testing.T) {
	for _, tc := range []struct {
		name string
		role string
		has  bool
		want Role
	}{
		{"удостоверение до ролей", "", true, RoleResident},
		{"удостоверения нет вовсе", "", false, RoleResident},
		{"пробелы вместо роли", "  \n", true, RoleResident},
		{"роль названа", "шлюз", true, RoleGateway},
	} {
		if got := roleFromIdentity(tc.role, tc.has); got != tc.want {
			t.Fatalf("%s: хотели роль %q, получили %q", tc.name, tc.want, got)
		}
	}
}

// TestBrokenRoleInIdentityNotGuessed — чужое или испорченное
// удостоверение не должно доставаться жителю машины по доброте: роль
// возвращается как есть, и способностей по ней не выдаётся ни одной.
func TestBrokenRoleInIdentityNotGuessed(t *testing.T) {
	role := roleFromIdentity("гость", true)
	if role == RoleResident || role == RoleGateway {
		t.Fatalf("незнакомая роль не должна превращаться в известную, получили %q", role)
	}
	if allowed, _ := allowedForRole(role, candidateCapabilities()); len(allowed) != 0 {
		t.Fatalf("по незнакомой роли не должно выдаваться способностей, получили %v", allowed)
	}
}

func TestHasCap(t *testing.T) {
	caps := []string{"код", "память"}
	if !hasCap(caps, "код") {
		t.Fatalf("код должен быть в списке")
	}
	if hasCap(caps, "уведомления") {
		t.Fatalf("уведомлений в списке не было")
	}
}
