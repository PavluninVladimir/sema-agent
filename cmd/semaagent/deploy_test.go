package main

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Файлы службы — часть поставки, а не устное предание (design.md, решение
// 5). Значит они врут ровно так же легко, как код: флаг переименовали,
// служба его больше не понимает, и узнаётся это после перезагрузки машины,
// когда агент не поднялся. Поэтому здесь сверяется то, что передают файлы
// службы, с тем, что агент действительно умеет принять.

// serviceFiles — описания службы в deploy/ и то, как из них достаётся строка
// запуска. README и install-mac.sh здесь не при чём: в них полно чужих
// флагов (install -m, systemctl --user), и разбирать их значило бы ловить
// ложные срабатывания.
var serviceFiles = map[string]*regexp.Regexp{
	"semaagent.service":      regexp.MustCompile(`(?m)^ExecStart=(.*)$`),
	"semaagent.user.service": regexp.MustCompile(`(?m)^ExecStart=(.*)$`),
	"semaagent.plist":        regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>(.*?)</array>`),
}

// oneFlag — токен вида «-имя». Двойное тире не берём: у самого агента
// таких флагов нет, а в файлах службы попадаются чужие.
var oneFlag = regexp.MustCompile(`(^|[\s>])-([a-zA-Z][\w-]*)`)

func flagsFromServiceFile(t *testing.T, fileName string, re *regexp.Regexp) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", fileName))
	if err != nil {
		t.Fatalf("файл службы %s обязан лежать в deploy/: %v", fileName, err)
	}
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("в %s не нашлась строка запуска агента", fileName)
	}
	var out []string
	for _, f := range oneFlag.FindAllStringSubmatch(string(m[1]), -1) {
		out = append(out, f[2])
	}
	return out
}

// TestAgentKnowsServiceFileFlags — служба передаёт флаг, которого агент
// не знает, — значит после перезагрузки машины он не поднимется вовсе:
// flag.ExitOnError на неизвестном флаге выходит с ошибкой.
func TestAgentKnowsServiceFileFlags(t *testing.T) {
	fs := flag.NewFlagSet("проверка", flag.ContinueOnError)
	newFlags(fs)
	for fileName, re := range serviceFiles {
		flags := flagsFromServiceFile(t, fileName, re)
		if len(flags) == 0 {
			t.Fatalf("%s запускает агента вообще без флагов — без -link это не демон", fileName)
		}
		for _, name := range flags {
			if fs.Lookup(name) == nil {
				t.Fatalf("%s передаёт флаг -%s, которого агент не знает: служба не поднимется", fileName, name)
			}
		}
	}
}

// TestEveryServiceHoldsLink — демон это тот, кто держит провод к Сёме
// (-link): без него не открывается и местная дверь, а значит ни хуки, ни
// агенты разработки до жителя машины не доберутся.
func TestEveryServiceHoldsLink(t *testing.T) {
	for fileName, re := range serviceFiles {
		if !contains(flagsFromServiceFile(t, fileName, re), "link") {
			t.Fatalf("%s не передаёт -link: такая служба демоном не станет", fileName)
		}
	}
}

// TestServiceNamesOwnStateDir — удостоверение агента и сокет
// местной двери лежат в каталоге состояния, и на общей машине важно, чтобы
// это был именно свой каталог, а не тот, что подвернулся по умолчанию от
// пользователя службы.
func TestServiceNamesOwnStateDir(t *testing.T) {
	for fileName, re := range serviceFiles {
		if !contains(flagsFromServiceFile(t, fileName, re), "home") {
			t.Fatalf("%s не передаёт -home: каталог состояния службы обязан быть назван явно", fileName)
		}
	}
}

func contains(list []string, what string) bool {
	for _, s := range list {
		if s == what {
			return true
		}
	}
	return false
}

// TestServiceTemplatesHaveNoForeignPaths — в репозиторий кладётся шаблон, а
// не чей-то личный файл: подставляемые места названы заглушками, и имя
// владельца машины в поставку не уезжает.
func TestServiceTemplatesHaveNoForeignPaths(t *testing.T) {
	for fileName := range serviceFiles {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", fileName))
		if err != nil {
			t.Fatalf("%s: %v", fileName, err)
		}
		if strings.Contains(string(raw), "/Users/") {
			t.Fatalf("%s содержит путь к домашнему каталогу конкретного человека — в поставке должна быть заглушка", fileName)
		}
		if !strings.Contains(string(raw), "АДРЕС_СЁМЫ") {
			t.Fatalf("%s не называет адрес Сёмы заглушкой АДРЕС_СЁМЫ — значит в нём зашит чей-то адрес", fileName)
		}
	}
}
