// Помощник жителя машины: значок у часов, меню и уведомления Сёмы.
//
// Всё решение принимает Go: сюда приходят строки JSON — состояние и
// уведомления, отсюда уходят нажатия и ответ «показал». Своей памяти у
// помощника нет: упал и поднялся — значок снова расскажет то, что скажет
// житель машины.

import AppKit
import Foundation
import UserNotifications

let out = FileHandle.standardOutput
let outLock = NSLock()

func say(_ v: [String: Any]) {
    guard let data = try? JSONSerialization.data(withJSONObject: v) else { return }
    outLock.lock()
    defer { outLock.unlock() }
    out.write(data)
    out.write("\n".data(using: .utf8)!)
}

final class Desk: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    var link = false
    var since = ""
    var err = ""
    var waiting = 0
    var address = ""
    var allowed = false

    func applicationDidFinishLaunching(_ note: Notification) {
        NSApp.setActivationPolicy(.accessory) // не в Dock
        UNUserNotificationCenter.current().delegate = self
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { ok, _ in
            DispatchQueue.main.async { self.allowed = ok }
        }
        draw()
        listen()
    }

    // ---- значок и меню ----

    func draw() {
        guard let button = item.button else { return }
        let name = link ? "bubble.left.and.text.bubble.right" : "bubble.left.and.exclamationmark.bubble.right"
        let подпись = link ? "Сёма на связи" : "Связи с Сёмой нет"
        button.image = NSImage(systemSymbolName: name, accessibilityDescription: подпись)
        button.image?.isTemplate = true
        // сколько ждёт — прямо у часов: число, когда есть что читать, и
        // точка, когда всё разобрано. Так значок отвечает, не открывая меню
        button.title = waiting > 0 ? " \(waiting)" : " •"
        button.toolTip = подпись
        item.menu = menu()
    }

    func menu() -> NSMenu {
        let m = NSMenu()
        // одна строка про связь, а не две: «на связи с 14:36» — это и есть
        // ответ на вопрос, ради которого открывают меню
        let когда = since.isEmpty ? "" : " с \(since)"
        m.addItem(тихо((link ? "Сёма на связи" : "Связи нет") + когда))
        if !err.isEmpty {
            m.addItem(тихо(err))
        }
        m.addItem(.separator())
        if waiting > 0 {
            m.addItem(тихо("Ждут вас: \(waiting)"))
        }
        let open = NSMenuItem(title: "Открыть Сёму", action: #selector(openSema), keyEquivalent: "o")
        open.target = self
        m.addItem(open)
        let again = NSMenuItem(title: "Переподключиться", action: #selector(reconnect), keyEquivalent: "")
        again.target = self
        m.addItem(again)
        let log = NSMenuItem(title: "Открыть журнал", action: #selector(openLog), keyEquivalent: "")
        log.target = self
        m.addItem(log)
        m.addItem(.separator())
        let quit = NSMenuItem(title: "Выйти", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        quit.target = NSApp // без цели пункт рисуется с чужим значком
        quit.image = nil
        m.addItem(quit)
        return m
    }

    func тихо(_ text: String) -> NSMenuItem {
        let i = NSMenuItem(title: text, action: nil, keyEquivalent: "")
        i.isEnabled = false
        return i
    }

    @objc func openSema() { say(["вид": "нажали", "экран": "notifications"]) }
    @objc func reconnect() { say(["вид": "пункт", "что": "переподключиться"]) }
    @objc func openLog() { say(["вид": "пункт", "что": "журнал"]) }

    // ---- разговор с жителем машины ----

    func listen() {
        DispatchQueue.global(qos: .utility).async {
            while let line = readLine(strippingNewline: true) {
                guard let data = line.data(using: .utf8),
                      let msg = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { continue }
                DispatchQueue.main.async { self.take(msg) }
            }
            DispatchQueue.main.async { NSApp.terminate(nil) } // труба закрылась — уходим
        }
    }

    func take(_ msg: [String: Any]) {
        switch msg["вид"] as? String {
        case "состояние":
            link = msg["связь"] as? Bool ?? false
            since = msg["с"] as? String ?? since
            err = msg["ошибка"] as? String ?? ""
            waiting = msg["ждут"] as? Int ?? 0
            address = msg["адрес"] as? String ?? address
            draw()
        case "уведомление":
            show(msg)
        default:
            break
        }
    }

    func show(_ msg: [String: Any]) {
        let id = msg["id"] as? Int ?? 0
        let content = UNMutableNotificationContent()
        content.title = msg["заголовок"] as? String ?? "Сёма"
        content.body = msg["текст"] as? String ?? ""
        content.userInfo = ["экран": msg["экран"] as? String ?? "notifications"]
        if (msg["уровень"] as? String) == "alert" { content.sound = .default }
        let req = UNNotificationRequest(identifier: "sema-\(id)", content: content, trigger: nil)
        UNUserNotificationCenter.current().add(req) { error in
            if let error {
                say(["вид": "показал", "id": id, "ок": false, "почему": error.localizedDescription])
            } else {
                say(["вид": "показал", "id": id, "ок": true])
            }
        }
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                didReceive response: UNNotificationResponse) async {
        let screen = response.notification.request.content.userInfo["экран"] as? String ?? "notifications"
        say(["вид": "нажали", "экран": screen])
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .sound]
    }
}

let app = NSApplication.shared
let desk = Desk()
app.delegate = desk
app.run()
