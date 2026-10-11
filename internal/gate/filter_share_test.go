package gate

import "testing"

// Ссылка на файл Сёмы проходит шлюз (иначе снаружи 404, а у Сёмы всё
// зелёно), но голый префикс — нет, и предел частоты у неё строгий, как у
// входа: подпись тоже можно пытаться перебирать.
func TestSharePathKnownAndStrict(t *testing.T) {
	if !Known("/share/12.1791000000.abcdef") {
		t.Error("ссылка на файл не проходит шлюз")
	}
	if Known("/share/") {
		t.Error("голый /share/ проходит шлюз")
	}
	if !isLogin("/share/12.1791000000.abcdef") {
		t.Error("у ссылки на файл не строгий предел частоты")
	}
}
