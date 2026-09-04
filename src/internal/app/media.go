package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	tele "gopkg.in/telebot.v4"
)

// maxFileSize - потолок Bot API на скачивание. Проверяем file_size из апдейта
// до запроса файла: getFile на тяжёлом вложении всё равно кончится отказом.
const maxFileSize = 20 << 20

// ErrFileTooBig отделяет отказ автору («пришлите иначе») от сбоя скачивания,
// после которого элемент уходит в failed.
var ErrFileTooBig = errors.New("file is larger than 20 MB")

// Media - файловый слой обращения: каталог на обращение, скачивание вложений и
// удаление каталога целиком. С БД не работает: file_path обнуляет case.go.
type Media struct {
	root string
	log  *slog.Logger
}

// NewMedia создаёт корневой каталог и проверяет его на запись. Образ
// distroless/nonroot работает от непривилегированного пользователя, и упереться
// в права на первом голосовом хуже, чем не подняться вовсе.
func NewMedia(root string, log *slog.Logger) (*Media, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create media dir %s: %w", root, err)
	}

	probe := filepath.Join(root, ".probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return nil, fmt.Errorf("media dir %s is not writable: %w", root, err)
	}
	if err := os.Remove(probe); err != nil {
		return nil, fmt.Errorf("remove probe in %s: %w", root, err)
	}
	return &Media{root: root, log: log}, nil
}

// Download кладёт вложение в <root>/<caseID>/<name> и возвращает путь.
//
// file берётся из апдейта: FileSize там уже есть, поэтому лимит проверяется без
// запроса к Telegram. name задаёт вызывающий - идентификатор элемента известен
// только ему, а имя файла нужно раньше, чем строка попадёт в case_items.
func (m *Media) Download(bot *tele.Bot, caseID, name string, file tele.File) (string, error) {
	if file.FileSize > maxFileSize {
		return "", ErrFileTooBig
	}

	dir := filepath.Join(m.root, caseID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create case dir: %w", err)
	}

	path := filepath.Join(dir, name)
	// telebot.Download сам зовёт getFile, отдельный FileByID дал бы второй
	// запрос к Bot API ради того же ответа.
	if err := bot.Download(&file, path); err != nil {
		// Недокачанный файл нельзя оставлять: шаг нормализации примет его за
		// целый и отдаст модели обрезанную запись.
		_ = os.Remove(path)
		return "", fmt.Errorf("download file: %w", err)
	}
	return path, nil
}

// Drop удаляет каталог обращения целиком. Число файлов уходит в лог
// files_dropped: это единственное наблюдаемое доказательство, что медиа не
// пережило обращение.
func (m *Media) Drop(caseID string) error {
	dir := filepath.Join(m.root, caseID)

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read case dir: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("drop case dir: %w", err)
	}

	m.log.Info("files_dropped", "case_id", caseID, "files", len(entries))
	return nil
}
