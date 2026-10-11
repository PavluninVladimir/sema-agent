package lsp

// Формы ответов языкового сервера, приведённые к человеческому виду.
// Берём только то, что нужно карточке: имена, места, вид символа.

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Position — строка и столбец, оба с нуля (так в протоколе).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Symbol — объявление в файле; дети — вложенные (методы у типа).
type Symbol struct {
	Name      string   `json:"name"`
	Detail    string   `json:"detail"`
	Kind      int      `json:"kind"`
	Range     Range    `json:"range"`
	Selection Range    `json:"selectionRange"`
	Children  []Symbol `json:"children"`
}

type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"` // 1 ошибка, 2 предупреждение, 3 подсказка
	Message  string `json:"message"`
	Source   string `json:"source"`
}

// KindName — вид символа словами: числа протокола человеку ничего не говорят.
var KindName = map[int]string{
	1: "файл", 2: "модуль", 3: "пространство", 4: "пакет", 5: "класс", 6: "метод",
	7: "свойство", 8: "поле", 9: "конструктор", 10: "перечисление", 11: "интерфейс",
	12: "функция", 13: "переменная", 14: "константа", 15: "строка", 16: "число",
	17: "логическое", 18: "массив", 19: "объект", 20: "ключ", 21: "пусто",
	22: "член перечисления", 23: "структура", 24: "событие", 25: "оператор", 26: "параметр типа",
}

func (s Symbol) KindTitle() string {
	if n, ok := KindName[s.Kind]; ok {
		return n
	}
	return "символ"
}

// SeverityTitle — уровень замечания словами.
func SeverityTitle(n int) string {
	switch n {
	case 1:
		return "ошибка"
	case 2:
		return "предупреждение"
	case 3:
		return "подсказка"
	}
	return "замечание"
}

// PathToURI — путь файла в адрес документа. Виртуальные адреса
// (`sema://module/имя.js`) остаются как есть: файла на диске нет.
func PathToURI(path string) string {
	if strings.Contains(path, "://") {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "file://" + (&url.URL{Path: abs}).EscapedPath()
}

// URIToPath — обратно; для виртуальных адресов возвращает их же.
func URIToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	return u.Path
}

// Where — место словами: файл и строка, считая с единицы.
func Where(uri string, r Range) string {
	return fmt.Sprintf("%s:%d", URIToPath(uri), r.Start.Line+1)
}
