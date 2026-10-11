package link

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEnrollRoleFromSemaBeatsRequested — код выдавала Сёма, и роль
// у него была решена ещё тогда (design.md, решение 1: «роль выбирается
// человеком в момент выдачи одноразового кода»). Если её ответ называет
// роль, он главнее того, что попросила сама машина.
func TestEnrollRoleFromSemaBeatsRequested(t *testing.T) {
	var got struct {
		Role string `json:"role"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cert":"","root":"корень","role":"шлюз"}`))
	}))
	defer srv.Close()

	id, err := Enroll(srv.URL, "код-приглашения", "житель", []string{"код"})
	if err != nil {
		t.Fatalf("вход по приглашению: %v", err)
	}
	if got.Role != "житель" {
		t.Fatalf("агент обязан назвать серверу запрошенную роль, получили %q", got.Role)
	}
	if id.Role != "шлюз" {
		t.Fatalf("роль из ответа Сёмы должна победить запрошенную, получили %q", id.Role)
	}
}

// TestEnrollWithoutRoleKeepsRequested — старый сервер, который
// ещё не умеет отвечать ролью: сохраняем то, что попросили сами, а не
// теряем роль вовсе.
func TestEnrollWithoutRoleKeepsRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cert":"","root":"корень"}`))
	}))
	defer srv.Close()

	id, err := Enroll(srv.URL, "код-приглашения", "житель", nil)
	if err != nil {
		t.Fatalf("вход по приглашению: %v", err)
	}
	if id.Role != "житель" {
		t.Fatalf("без роли в ответе должна остаться запрошенная, получили %q", id.Role)
	}
}

// TestSaveLoadKeepsRole — роль переживает запись на диск и чтение
// назад: main.go решает по ней, какие способности объявлять при каждом
// запуске, а не только сразу после enroll.
func TestSaveLoadKeepsRole(t *testing.T) {
	dir := t.TempDir()
	want := Identity{Name: "испытание", Role: "шлюз", Cert: "c", Key: "k", Root: "r"}
	if err := Save(dir, want); err != nil {
		t.Fatalf("сохранение: %v", err)
	}
	got, ok := Load(dir)
	if !ok {
		t.Fatalf("удостоверение не прочиталось")
	}
	if got.Role != want.Role {
		t.Fatalf("роль не пережила сохранение: хотели %q, получили %q", want.Role, got.Role)
	}
}

// TestLoadOldIdentityWithoutRole — удостоверение, заведённое до
// появления ролей, не должно ломать Load: роль просто пустая, а решение
// «считать жителем машины» — дело вызывающего (main.go), не этого пакета.
func TestLoadOldIdentityWithoutRole(t *testing.T) {
	dir := t.TempDir()
	old := Identity{Name: "старый", Cert: "c", Key: "k", Root: "r"} // Role пуст умышленно
	if err := Save(dir, old); err != nil {
		t.Fatalf("сохранение: %v", err)
	}
	got, ok := Load(dir)
	if !ok {
		t.Fatalf("старое удостоверение без роли должно оставаться читаемым")
	}
	if got.Role != "" {
		t.Fatalf("роль не должна была появиться из ниоткуда: %q", got.Role)
	}
}
