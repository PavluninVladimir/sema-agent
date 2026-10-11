package main

// Память Сёмы глазами агента разработки. Инструменты те же, что раньше
// приходили от сервера Сёмы по сети с токеном, но дорога другая: житель
// машины держит постоянный провод и спрашивает по нему.
//
// Почему так: сервер Сёмы живёт в контейнере и пересоздаётся при каждом
// выкате — сессия MCP умирала вместе с ним, и посреди работы память
// отвечала «нет связи». Житель машины стоит под launchd, переживает
// выкаты и уже опознан сертификатом.
//
// Описания инструментов лежат здесь, а не приходят от Сёмы: список
// нужен и тогда, когда провода нет, иначе агент разработки решит, что
// памяти не существует, и больше про неё не спросит.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	semalink "github.com/PavluninVladimir/sema-agent/internal/link"
)

// memoryWait — сколько ждать восстановления провода, прежде чем сказать
// человеку про связь. Выкат Сёмы длится секунды: агент разработки не
// должен видеть в этом окне «памяти нет», иначе он запишет неверный
// вывод и пойдёт работать по нему.
const memoryWait = 30 * time.Second

// hookWait — а хуку столько ждать нельзя: он держит сессию агента
// разработки. Нет связи — молчим сразу, пропажу видно уведомлением у
// Сёмы, а не паузой у человека.
const hookWait = 2 * time.Second

// hookArg — пометка «это хук, не модель» в аргументах вызова. Наружу не
// уходит: в запрос к Сёме перекладываются только названные поля.
const hookArg = "_хук"

// memoryCaller — кто умеет спросить Сёму. Отдельным именем, чтобы
// инструменты собирались и без живого провода.
type memoryCaller interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// memoryTools — пять инструментов памяти. Каждый кладёт рабочую папку в
// запрос и возвращает ответ Сёмы как есть: пересказ памяти — это уже не
// память.
func memoryTools(conn memoryCaller) []tool {
	ask := func(name string, keys ...string) func(map[string]any) (string, error) {
		return func(a map[string]any) (string, error) {
			// хук держит сессию агента разработки синхронно, поэтому
			// ждать восстановления провода ему нельзя — только модель
			// может позволить себе подождать полминуты
			wait := memoryWait
			if _, ok := a[hookArg]; ok {
				wait = hookWait
			}
			args := map[string]any{}
			for _, k := range keys {
				if v, ok := a[k]; ok {
					args[k] = v
				}
			}
			if c := str(a, "client"); c != "" {
				args["client"] = c
			}
			args["path"] = WorkRoot(str(a, "path"))
			return memoryCallWait(conn, name, args, wait)
		}
	}
	return []tool{
		{
			Name:   "memory_session_start",
			Desc:   "Начать сессию в памяти Сёмы и получить контекст: уроки, факты и сводку прошлой сессии для этой папки.",
			Schema: schema(map[string]string{"path": "рабочая папка агента", "model": "модель агента (для правил «наружу»)", "hint": "о чём сессия, если известно"}, "path"),
			Run:    ask("memory_session_start", "model", "hint", "resume"),
		},
		{
			Name:   "memory_session_end",
			Desc:   "Закончить сессию: передать сводку (что делали, что выяснили, какие решения) и наблюдения. Сёма сделает из них сводку сессии, факты и граф.",
			Schema: memorySchema(map[string]string{"path": "рабочая папка агента", "session_id": "id сессии из session_start (необязательно)", "summary": "сводка сессии, 5–20 строк", "observations": "список коротких наблюдений (необязательно)"}, "path", "summary"),
			Run:    ask("memory_session_end", "session_id", "summary", "observations"),
		},
		{
			Name:   "memory_recall",
			Desc:   "Вспомнить из памяти Сёмы по вопросу: уроки, факты, сводки и связи из выданных областей.",
			Schema: schema(map[string]string{"path": "рабочая папка агента", "query": "вопрос или тема", "model": "модель агента"}, "path", "query"),
			Run:    ask("memory_recall", "query", "model"),
		},
		{
			Name: "code_knowledge",
			Desc: "Что мы знаем про это место в коде: грабли, уроки прошлых работ, требования и разговоры, привязанные к файлу или символу. " +
				"Спрашивать ПЕРЕД правкой чужого места: агент разработки видит, что в коде сейчас, а это — что здесь уже ломалось.",
			Schema: schema(map[string]string{"path": "рабочая папка агента", "file": "путь к файлу от корня репозитория", "symbol": "имя функции или типа (необязательно)"}, "path"),
			Run:    ask("code_knowledge", "file", "symbol"),
		},
		{
			Name: "memory_remember",
			Desc: "Записать в память Сёмы факт или урок. Повтор известного закрепляет урок. Не класть секреты. " +
				"Если урок про конкретное место в коде — назовите файл и символ: тогда он вернётся тому, кто придёт править это место (code_knowledge).",
			Schema: schema(map[string]string{"path": "рабочая папка агента", "area": "область памяти (пусто — первая выданная)", "kind": "fact | lesson | observation", "text": "одно предложение по-русски", "file": "файл, к которому относится урок (необязательно)", "symbol": "функция или тип (необязательно)", "source": "откуда знание: требование спеки, грабли, разговор (необязательно)"}, "path", "text"),
			Run:    ask("memory_remember", "area", "kind", "text", "file", "symbol", "source"),
		},
	}
}

// memorySchema — как schema, но с числом и списком: сводка сессии несёт
// id и наблюдения, а строкой их не описать.
func memorySchema(props map[string]string, required ...string) map[string]any {
	out := schema(props, required...)
	p, _ := out["properties"].(map[string]any)
	if p == nil {
		return out
	}
	if v, ok := p["session_id"].(map[string]any); ok {
		v["type"] = "integer"
	}
	if v, ok := p["observations"].(map[string]any); ok {
		v["type"] = "array"
		v["items"] = map[string]any{"type": "string"}
	}
	return out
}

// memoryCall — спросить Сёму и отдать её ответ. Про обрыв провода
// говорим прямо: пустой ответ агент разработки прочитает как «в памяти
// ничего нет» и запишет неверный вывод.
func memoryCall(conn memoryCaller, name string, args map[string]any) (string, error) {
	return memoryCallWait(conn, name, args, memoryWait)
}

// memoryCallWait — то же, но со своим сроком ожидания провода. Хук ждать
// не должен: Claude Code держит его синхронно, и выключенная Сёма
// добавляла бы полминуты к каждому ответу модели.
func memoryCallWait(conn memoryCaller, name string, args map[string]any, wait time.Duration) (string, error) {
	if conn == nil {
		return "", fmt.Errorf("память недоступна: житель машины запущен без связи с Сёмой (флаг -link)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute+wait)
	defer cancel()
	raw, err := conn.Call(ctx, "память."+name, args)
	// провод рвётся на каждом выкате Сёмы: ждём, пока житель машины
	// перезвонит, и повторяем — клиент разницы видеть не должен
	for deadline := time.Now().Add(wait); err != nil &&
		errors.Is(err, semalink.ErrNoLink) && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("память Сёмы не ответила: %w", ctx.Err())
		case <-time.After(time.Second):
		}
		raw, err = conn.Call(ctx, "память."+name, args)
	}
	if err != nil {
		if errors.Is(err, semalink.ErrNoLink) {
			return "", fmt.Errorf("память недоступна: связи с Сёмой нет уже %s", wait)
		}
		return "", fmt.Errorf("память Сёмы не ответила: %w", err)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("ответ памяти не разобран: %w", err)
	}
	if out.Text == "" {
		return "Сёма ответил пусто.", nil
	}
	return out.Text, nil
}

// memoryNote — заметка в открытую сессию: её шлёт хук после каждого
// ответа, инструментом она никогда не была.
func memoryNote(conn memoryCaller, path, client, text string, wait time.Duration) error {
	if text == "" {
		return nil
	}
	args := map[string]any{"path": WorkRoot(path), "text": text}
	if client != "" {
		args["client"] = client
	}
	_, err := memoryCallWait(conn, "note", args, wait)
	return err
}

// isMemoryTool — имя из набора памяти. Нужно обёртке: рабочую папку она
// подставляет только тем инструментам, для которых это папка проекта, а
// не путь к файлу.
func isMemoryTool(name string) bool {
	switch name {
	case "memory_session_start", "memory_session_end", "memory_recall",
		"memory_remember", "code_knowledge":
		return true
	}
	return false
}
