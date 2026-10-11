package desk

// Сборка помощника на месте: исходник лежит рядом в репозитории, а
// бандл собирается на машине один раз — как OCR-помощник Сёмы. Бандл
// нужен не для красоты: без него Центр уведомлений не принимает записи,
// а LSUIElement — единственный способ не показывать значок в Dock.

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

//go:embed helper/desk.swift
var helperSource string

//go:embed helper/Info.plist
var helperPlist string

const helperName = "Сёма у часов"

// lsregister — им система узнаёт о приложении, собранном на месте.
const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

// buildHelper — путь к собранному помощнику; собирает, если его нет или
// исходник изменился.
func buildHelper(home string) (string, error) {
	app := filepath.Join(home, "tools", helperName+".app")
	bin := filepath.Join(app, "Contents", "MacOS", "semadesk")
	sum := sha256.Sum256([]byte(helperSource + helperPlist))
	want := hex.EncodeToString(sum[:])
	stamp := filepath.Join(app, "Contents", "исходник.sha256")
	if have, err := os.ReadFile(stamp); err == nil && string(have) == want {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}
	swiftc, err := exec.LookPath("swiftc")
	if err != nil {
		return "", fmt.Errorf("нет swiftc: поставьте инструменты разработчика (xcode-select --install)")
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	src := filepath.Join(home, "tools", "desk.swift")
	if err := os.WriteFile(src, []byte(helperSource), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(helperPlist), 0o644); err != nil {
		return "", err
	}
	out, err := exec.Command(swiftc, "-O", "-o", bin, src).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("помощник не собрался: %v: %s", err, clip(string(out), 400))
	}
	// подпись на месте: без неё Центр уведомлений отказывает бандлу,
	// собранному только что. Ad-hoc подписи достаточно — приложение не
	// уходит с этой машины
	if codesign, err := exec.LookPath("codesign"); err == nil {
		if out, err := exec.Command(codesign, "--force", "--deep", "--sign", "-", app).CombinedOutput(); err != nil {
			return "", fmt.Errorf("помощник не подписан: %v: %s", err, clip(string(out), 400))
		}
	}
	// LaunchServices должен знать бандл, иначе Центр уведомлений отвечает
	// «Notifications are not allowed for this application»: свежесобранное
	// приложение системе незнакомо, и разрешения у него нет
	if out, err := exec.Command(lsregister, "-f", app).CombinedOutput(); err != nil {
		log.Printf("значок у часов: бандл не зарегистрирован (%v: %s) — уведомления может не пустить Центр уведомлений",
			err, clip(string(out), 200))
	}
	if err := os.WriteFile(stamp, []byte(want), 0o644); err != nil {
		return "", err
	}
	return bin, nil
}
