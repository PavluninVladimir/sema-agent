package main

// Приложение к карточке: что Сёма знает про это место. Карточка говорит,
// что в коде сейчас; память — что здесь уже ломалось, каким принципом
// держится пакет и о чём договаривались. Одна дверь: агент разработки
// получает и то и другое одним вызовом, а не вспоминает спросить
// code_knowledge сам (за месяц — дважды).
//
// Приложение не задерживает карточку: запрос по проводу идёт с коротким
// сроком, и по его истечении карточка уходит с пометкой «память
// недоступна». Пометка обязательна: молчание неотличимо от «ничего не
// известно» — тот же принцип, что у диагностики ярусом ниже.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	semalink "github.com/PavluninVladimir/sema-agent/internal/link"
	"github.com/PavluninVladimir/sema-agent/internal/lsp"
)

// knowledgeWaitDefault — сколько карточка ждёт память. Карточка обещает
// быть дешевле файла, а дешевле — это и быстрее.
const knowledgeWaitDefault = 800 * time.Millisecond

// knowledgeTitle — заголовок раздела в карточке.
const knowledgeTitle = "Что здесь известно"

// placeArg — место карточки для памяти Сёмы: папка, файл, символ.
type placeArg struct {
	Dir    string `json:"dir,omitempty"`
	File   string `json:"file,omitempty"`
	Symbol string `json:"symbol,omitempty"`
}

// cardPlaces — места карточки от корня репозитория: папка файла, сам
// файл, каждый символ. Репозиторий — имя git-корня.
func cardPlaces(path string, symbols []string) (repo string, places []placeArg) {
	root := repoRoot(path)
	if root == path {
		// корня не нашлось, и repoRoot вернул сам путь: считаем от папки файла
		root = filepath.Dir(path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(path)
	}
	rel = filepath.ToSlash(rel)
	repo = filepath.Base(root)
	if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." && dir != "" {
		places = append(places, placeArg{Dir: dir})
	}
	places = append(places, placeArg{File: rel})
	for _, name := range symbols {
		if name != "" {
			places = append(places, placeArg{File: rel, Symbol: name})
		}
	}
	return repo, places
}

// attachKnowledge — дописать к карточке раздел из памяти. Ничего не
// возвращает: итог — в самой карточке, пометка или приложение.
func (s *server) attachKnowledge(c *lsp.Card, path, client string) {
	if c == nil || c.Text == "" || client == "" {
		return // карточку спросила сама Сёма или она пуста: приложение не к чему
	}
	if s.sema == nil {
		c.Text = strings.TrimRight(c.Text, "\n") + "\n\n" + knowledgeTitle + ": память недоступна — житель машины запущен без связи с Сёмой.\n"
		return
	}
	repo, places := cardPlaces(path, c.Symbols)
	wait := s.knowledgeWait
	if wait <= 0 {
		wait = knowledgeWaitDefault
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	raw, err := s.sema.Call(ctx, "память.code_knowledge", map[string]any{
		"path": WorkRoot(filepath.Dir(path)), "client": client, "repo": repo, "places": places})
	var note string
	switch {
	case err == nil:
		var out struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &out) != nil {
			note = "память недоступна — ответ Сёмы не разобран"
			break
		}
		text := strings.TrimSpace(out.Text)
		switch {
		case strings.HasPrefix(text, "Области памяти"):
			// «вам не выдано» — не то же, что «записей нет»: сказать, где выдать
			note = "области памяти этому агенту в этой папке не выданы — выдать можно на экране «Память» Сёмы (адрес memory)"
		case strings.HasPrefix(text, "Про это место записей нет"):
			note = "записей про это место нет; это не значит, что тут безопасно — знание могло не получить привязки"
		case text == "":
			note = "память ответила пусто"
		default:
			c.Text = strings.TrimRight(c.Text, "\n") + "\n\n" + knowledgeTitle + ":\n" + text + "\n"
			c.WithKnowledge = true
			return
		}
	case errors.Is(err, semalink.ErrNoLink):
		note = "память недоступна — связи с Сёмой нет"
	case errors.Is(err, context.DeadlineExceeded):
		note = fmt.Sprintf("память недоступна — Сёма не ответил за %s; спросите code_knowledge отдельно", wait)
	default:
		note = "память недоступна — " + err.Error()
	}
	c.Text = strings.TrimRight(c.Text, "\n") + "\n\n" + knowledgeTitle + ": " + note + ".\n"
}
