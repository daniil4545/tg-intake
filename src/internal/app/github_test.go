package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func testLog(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestTreeDocsFiltersMarkdown: отбор Lookup работает только с md, каталоги и
// прочие расширения в дереве репозитория ему не нужны.
func TestTreeDocsFiltersMarkdown(t *testing.T) {
	server := githubStub(t, map[string]string{
		"GET /repos/acme/proj/git/trees/main": `{
			"tree": [
				{"path": "docs/architecture.md", "type": "blob", "size": 120},
				{"path": "docs/img/diagram.png", "type": "blob", "size": 900},
				{"path": "docs/plans", "type": "tree", "size": 0},
				{"path": "README.MD", "type": "blob", "size": 40}
			],
			"truncated": false
		}`,
	})
	gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

	docs, err := gh.TreeDocs(context.Background(), Project{Owner: "acme", Repo: "proj"}, "main")
	if err != nil {
		t.Fatalf("tree docs: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("файлов: %d, ожидалось 2: %+v", len(docs), docs)
	}
	sizes := map[string]int{}
	for _, d := range docs {
		sizes[d.Path] = d.Size
	}
	if sizes["docs/architecture.md"] != 120 || sizes["README.MD"] != 40 {
		t.Errorf("состав или размеры md-файлов не совпали: %+v", docs)
	}
}

// TestTreeDocsLogsTruncated: усечённое дерево не должно молчать - отбор
// увидел бы неполный список md-файлов, не зная об этом.
func TestTreeDocsLogsTruncated(t *testing.T) {
	server := githubStub(t, map[string]string{
		"GET /repos/acme/proj/git/trees/main": `{"tree": [], "truncated": true}`,
	})
	var buf bytes.Buffer
	gh := NewGitHub("token", server.URL, testStatuses, slog.New(slog.NewTextHandler(&buf, nil)))

	if _, err := gh.TreeDocs(context.Background(), Project{Owner: "acme", Repo: "proj"}, "main"); err != nil {
		t.Fatalf("tree docs: %v", err)
	}
	if !strings.Contains(buf.String(), "tree_truncated") {
		t.Errorf("предупреждение об усечённом дереве не залогировано: %s", buf.String())
	}
}

// TestFileRejectsUnsafePathWithoutRequest: путь проверяется до похода в сеть -
// модель называет его сама, и отказ не должен стоить лишнего запроса.
func TestFileRejectsUnsafePathWithoutRequest(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"пустой путь", ""},
		{"абсолютный путь", "/docs/architecture.md"},
		{"выход за пределы репозитория", "docs/../../etc/passwd"},
		{"непечатаемый символ", "docs/arch\x00itecture.md"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen := &requestLog{}
			server := recordingStub(t, seen, nil)
			gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

			_, err := gh.File(context.Background(), Project{Owner: "acme", Repo: "proj"}, c.path, "main")
			if err == nil {
				t.Fatal("небезопасный путь принят")
			}
			if len(seen.list()) != 0 {
				t.Errorf("запрос ушёл в сеть до проверки пути: %v", seen.list())
			}
		})
	}
}

// TestFileDecodesBase64WithNewlines: GitHub переносит base64 внутри JSON
// строками фиксированной длины, декодер обязан их снимать, как GetReadme.
func TestFileDecodesBase64WithNewlines(t *testing.T) {
	content := "# Заголовок\n\nПроверка декодирования содержимого."
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	var wrapped strings.Builder
	for i := 0; i < len(encoded); i += 20 {
		end := i + 20
		if end > len(encoded) {
			end = len(encoded)
		}
		wrapped.WriteString(encoded[i:end])
		wrapped.WriteString("\n")
	}

	server := githubStub(t, map[string]string{
		"GET /repos/acme/proj/contents/docs/architecture.md": fmt.Sprintf(
			`{"content": %q, "encoding": "base64"}`, wrapped.String()),
	})
	gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

	got, err := gh.File(context.Background(), Project{Owner: "acme", Repo: "proj"}, "docs/architecture.md", "main")
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if got != content {
		t.Errorf("содержимое: %q, ожидалось %q", got, content)
	}
}

// TestFileTooLargeReturnsError: файлы больше 1 МБ Contents API отдаёт без
// содержимого - пустая строка читалась бы дальше как «файл пустой», а не как
// отказ.
func TestFileTooLargeReturnsError(t *testing.T) {
	server := githubStub(t, map[string]string{
		"GET /repos/acme/proj/contents/docs/big.md": `{"content": "", "encoding": "none"}`,
	})
	gh := NewGitHub("token", server.URL, testStatuses, testLog(t))

	got, err := gh.File(context.Background(), Project{Owner: "acme", Repo: "proj"}, "docs/big.md", "main")
	if err == nil {
		t.Fatal("файл больше 1 МБ должен вернуть ошибку, а не пустую строку")
	}
	if got != "" {
		t.Errorf("содержимое не пустое при ошибке: %q", got)
	}
}
