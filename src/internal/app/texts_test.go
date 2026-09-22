package app

// R9 (docs/specs/ticket-form.md §10, AGENTS.md принцип 6): все строки на
// русском для пользователя живут в texts.go (чат) и texts_model.go (ввод
// модели), стиль texts.go - §11 приложения ticket-form.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// literal - строковый литерал исходника с позицией и разобранным значением.
type literal struct {
	pos   token.Position
	value string
}

// parsedFile - разобранный не-тестовый .go файл со своим FileSet: позиции
// разных файлов сравнивать нельзя, каждому нужен свой набор.
type parsedFile struct {
	path string
	fset *token.FileSet
	file *ast.File
}

// stringLiterals - все литералы token.STRING файла, значения без кавычек.
// ParseComments не включаем нигде: комментарии токенами STRING не бывают,
// сканеру R9 они не нужны.
func stringLiterals(fset *token.FileSet, file *ast.File) []literal {
	var out []literal
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		out = append(out, literal{pos: fset.Position(lit.Pos()), value: value})
		return true
	})
	return out
}

// cyrillicLiterals - подмножество stringLiterals со значением, содержащим
// хотя бы одну кириллическую руну: сканер R9 (AGENTS.md принцип 6).
func cyrillicLiterals(fset *token.FileSet, file *ast.File) []literal {
	var out []literal
	for _, l := range stringLiterals(fset, file) {
		if containsCyrillic(l.value) {
			out = append(out, l)
		}
	}
	return out
}

func containsCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// parseNonTestGoFiles разбирает не-тестовые .go файлы каталога, кроме имён
// из exclude. exclude == nil - без исключений.
func parseNonTestGoFiles(t *testing.T, dir string, exclude map[string]bool) []parsedFile {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("читаю каталог %s: %v", dir, err)
	}

	var out []parsedFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if exclude[name] {
			continue
		}
		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("разбираю %s: %v", path, err)
		}
		out = append(out, parsedFile{path: path, fset: fset, file: file})
	}
	return out
}

// parseGoFile разбирает один файл; отсутствие texts.go или texts_model.go -
// явный красный тест, а не skip.
func parseGoFile(t *testing.T, path string) (*token.FileSet, *ast.File) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("разбираю %s: %v", path, err)
	}
	return fset, file
}

// TestTextsInOneFile - в не-тестовых файлах пакета app и cmd/intake, кроме
// texts.go и texts_model.go, не должно оставаться литералов на кириллице
// (R9, docs/specs/ticket-form.md §10, AGENTS.md принцип 6).
func TestTextsInOneFile(t *testing.T) {
	exclude := map[string]bool{"texts.go": true, "texts_model.go": true}

	var found []literal
	for _, dir := range []string{".", "../../cmd/intake"} {
		for _, pf := range parseNonTestGoFiles(t, dir, exclude) {
			found = append(found, cyrillicLiterals(pf.fset, pf.file)...)
		}
	}

	if len(found) == 0 {
		return
	}

	sort.Slice(found, func(i, j int) bool {
		return found[i].pos.String() < found[j].pos.String()
	})

	var b strings.Builder
	fmt.Fprintf(&b, "кириллические литералы вне texts.go и texts_model.go: %d\n", len(found))
	for _, l := range found {
		fmt.Fprintf(&b, "%s: %q\n", l.pos, l.value)
	}
	t.Error(b.String())
}

// TestFindCyrillicLiterals - сценарий 2: сам сканер cyrillicLiterals на
// фикстуре с известным ответом, чтобы красный TestTextsInOneFile не мог
// оказаться следствием сломанного сканера.
func TestFindCyrillicLiterals(t *testing.T) {
	const src = "package fixture\n\n" +
		"const a = \"Привет\"\n" +
		"const b = \"## Кратко\\n\"\n" +
		"const c = `сырой`\n" +
		"const d = \"a\" + \"б\"\n" +
		"const e = 'ё'\n" +
		"\n" +
		"// \"Привет\"\n" +
		"const f = \"hello\"\n" +
		"const g = \"https://x\"\n"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("разбираю фикстуру: %v", err)
	}

	got := cyrillicLiterals(fset, file)
	sort.Slice(got, func(i, j int) bool { return got[i].pos.Offset < got[j].pos.Offset })

	want := []struct {
		line  int
		value string
	}{
		{3, "Привет"},
		{4, "## Кратко\n"},
		{5, "сырой"},
		{6, "б"},
	}

	if len(got) != len(want) {
		t.Fatalf("нашёл %d литералов, ожидал %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].pos.Line != w.line || got[i].value != w.value {
			t.Errorf("литерал %d: получил %q на строке %d, ожидал %q на строке %d",
				i, got[i].value, got[i].pos.Line, w.value, w.line)
		}
	}
}

// buttonCommandValues - значения констант texts.go с именем button* или
// command*: подписи кнопок и описания команд.
func buttonCommandValues(file *ast.File) map[string]bool {
	values := make(map[string]bool)
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "button") && !strings.HasPrefix(name.Name, "command") {
					continue
				}
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				values[v] = true
			}
		}
	}
	return values
}

var quoteRef = regexp.MustCompile(`«([^»]+)»`)

// TestTextsQuoteButtons - сценарий 3: ссылка на кнопку в тексте texts.go
// должна быть дословной (§11) - совпадать со значением константы button*
// или command*. Совпадение с "%" внутри «ёлочек» пропускаем - это шаблон
// подстановки (например «%s»), а не название кнопки; остальной литерал
// проверяется, даже если формат сидит в другой его части.
func TestTextsQuoteButtons(t *testing.T) {
	fset, file := parseGoFile(t, "texts.go")
	values := buttonCommandValues(file)

	for _, l := range stringLiterals(fset, file) {
		for _, m := range quoteRef.FindAllStringSubmatch(l.value, -1) {
			x := m[1]
			if strings.Contains(x, "%") {
				continue
			}
			if !values[x] {
				t.Errorf("%s: ссылка «%s» не равна ни одной константе button*/command*", l.pos, x)
			}
		}
	}
}

var styleForbiddenWords = []string{
	"пожалуйста",
	"база", "базы", "базе", "базу", "базой", "баз", "базам", "базами", "базах",
	"очередь", "очереди", "очередью", "очередей", "очередям", "очередями", "очередях",
	"лог", "логе", "логи", "логов", "логам", "логами", "логах", "логом",
}

// containsWholeWord ищет word как целое слово в s без учёта регистра.
// regexp.\b тут не годится: \b в RE2 определён по ASCII-словам, кириллица
// для него везде "не слово", поэтому граница проверяется вручную по соседним
// рунам.
func containsWholeWord(s, word string) bool {
	lower := strings.ToLower(s)
	word = strings.ToLower(word)

	for start := 0; ; {
		i := strings.Index(lower[start:], word)
		if i < 0 {
			return false
		}
		pos := start + i

		before := rune(0)
		if pos > 0 {
			before, _ = utf8.DecodeLastRuneInString(lower[:pos])
		}
		after := rune(0)
		if end := pos + len(word); end < len(lower) {
			after, _ = utf8.DecodeRuneInString(lower[end:])
		}

		if !isCyrillicLetter(before) && !isCyrillicLetter(after) {
			return true
		}
		start = pos + 1
	}
}

func isCyrillicLetter(r rune) bool {
	return unicode.IsLetter(r) && unicode.Is(unicode.Cyrillic, r)
}

// isStyleEmoji - эмодзи по правилу ревью спеки: U+2600-27BF и U+1F000 и выше.
func isStyleEmoji(r rune) bool {
	return (r >= 0x2600 && r <= 0x27BF) || r >= 0x1F000
}

// TestTextsStyle - сценарий 4: литералы texts.go без слов, запрещённых §11
// («пожалуйста», технические «база», «очередь», «лог» во всех падежах), и
// без эмодзи. texts_model.go не смотрим - там стиль не меняется (R9,
// AGENTS.md принцип 6): B переносится байт в байт и после переноса не правится.
func TestTextsStyle(t *testing.T) {
	fset, file := parseGoFile(t, "texts.go")

	for _, l := range stringLiterals(fset, file) {
		for _, w := range styleForbiddenWords {
			if containsWholeWord(l.value, w) {
				t.Errorf("%s: запрещённое слово %q в %q", l.pos, w, l.value)
			}
		}
		for _, r := range l.value {
			if isStyleEmoji(r) {
				t.Errorf("%s: эмодзи %U в %q", l.pos, r, l.value)
			}
		}
	}
}

// TestPanelButtonsUnchanged - рубеж 3a: перенос и стиль не меняют ни байта в
// подписях нижней панели, описаниях команд и текстах, зафиксированных §11.
// Панель проверяется по символам doneBtn, menuBtn, resetBtn - их имена
// перенос не трогает (переименование - шум в диффе); у команд и текстов §11
// своего символа нет, поэтому сравниваются сами константы: так правка любой
// из них ловится по имени, а не по случайному совпадению значения с чужой
// константой в пакете.
func TestPanelButtonsUnchanged(t *testing.T) {
	t.Run("панель", func(t *testing.T) {
		cases := map[string]string{
			"Готово": doneBtn.Text,
			"Меню":   menuBtn.Text,
			"Сброс":  resetBtn.Text,
		}
		for want, got := range cases {
			if got != want {
				t.Errorf("получил %q, ожидал %q", got, want)
			}
		}
	})

	t.Run("команды и §11", func(t *testing.T) {
		cases := map[string]string{
			// команды бота (SetCommands, bot.go)
			"Меню":            commandMenu,
			"Мои тикеты":      commandTickets,
			"Добавить проект": commandAddProject,
			"Сброс":           commandReset,
			// тексты §11, не меняются переносом и стилем
			"Всё так":            buttonAllTrue,
			"Отправить как есть": buttonSkip,
			"Публикую":           buttonPublish,
			"Поправить":          buttonFix,
			"Этот экран устарел": toastStale,
		}
		for want, got := range cases {
			if got != want {
				t.Errorf("получил %q, ожидал %q", got, want)
			}
		}
	})
}
