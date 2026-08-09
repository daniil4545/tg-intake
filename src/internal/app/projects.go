package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// readmeLimit - сколько текста README уходит в модель. Дальше начинается
// установка и лицензия, а токены платные.
const readmeLimit = 8000

// ErrBadProjectRef - в команде нет разбираемой ссылки на репозиторий.
var ErrBadProjectRef = errors.New("project reference is not a github repository")

// Projects - заведение проекта из бота. Владелец таблицы по-прежнему db.go,
// здесь сценарий: прочитать репозиторий, собрать описание, завести метки.
type Projects struct {
	cases *Cases
	gh    *GitHub
	llm   *OpenRouter
	model string
	log   *slog.Logger
}

func NewProjects(cases *Cases, gh *GitHub, llm *OpenRouter, model string, log *slog.Logger) *Projects {
	return &Projects{cases: cases, gh: gh, llm: llm, model: model, log: log}
}

// projectRef - разобранная команда: адрес репозитория и то, что автор задал сам.
type projectRef struct {
	Owner   string
	Repo    string
	Title   string
	Context string
}

// Add заводит проект по ссылке. Возвращает сохранённую строку - её же бот
// показывает автору карточкой.
func (p *Projects) Add(ctx context.Context, userID int64, args string) (ProjectConfig, error) {
	ref, err := parseProjectRef(args)
	if err != nil {
		return ProjectConfig{}, err
	}

	repo, err := p.gh.GetRepo(ctx, ref.Owner, ref.Repo)
	if err != nil {
		return ProjectConfig{}, err
	}

	source := "автор"
	title, context := ref.Title, ref.Context
	if title == "" || context == "" {
		title, context, source = p.describe(ctx, ref, repo, title, context)
	}

	project := ProjectConfig{
		Slug:    projectSlugFrom(repo.Name, ref.Repo),
		Title:   title,
		Owner:   ref.Owner,
		Repo:    ref.Repo,
		Context: context,
	}

	// Метки заводятся до сохранения и служат проверкой права на запись: строка в
	// меню, ведущая в репозиторий без доступа, хуже её отсутствия - автор
	// потратит на интервью пять минут и упрётся в отказ на публикации.
	if err := p.gh.PrepareProject(ctx, Project{
		Slug: project.Slug, Owner: project.Owner, Repo: project.Repo,
	}); err != nil {
		return ProjectConfig{}, err
	}

	if err := SyncProjects(ctx, p.cases.pool, []ProjectConfig{project}, p.log); err != nil {
		return ProjectConfig{}, err
	}

	p.log.Info("project_saved", "user_id", userID, "project", project.Slug,
		"repo", project.Owner+"/"+project.Repo, "source", source)
	return project, nil
}

// describe собирает недостающие название и контекст. Отказ модели не роняет
// команду: имя и описание репозитория есть всегда, а бот честно скажет, что
// собрал их сам.
func (p *Projects) describe(ctx context.Context, ref projectRef, repo Repo, title, context string) (string, string, string) {
	readme, err := p.gh.GetReadme(ctx, ref.Owner, ref.Repo)
	if err != nil {
		p.log.Warn("readme_unavailable", "repo", ref.Owner+"/"+ref.Repo, "error", err)
	}

	guess, err := p.ask(ctx, repo, readme)
	if err != nil {
		p.log.Warn("project_describe_failed", "repo", ref.Owner+"/"+ref.Repo, "error", err)
		return valueOr(title, repo.Name), valueOr(context, valueOr(repo.Description, repo.Name)), "репозиторий"
	}
	return valueOr(title, guess.Title), valueOr(context, guess.Context), "модель"
}

type projectGuess struct {
	Title   string `json:"title"`
	Context string `json:"context"`
}

var projectSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["title", "context"],
  "properties": {
    "title": {"type": "string"},
    "context": {"type": "string"}
  }
}`)

func (p *Projects) ask(ctx context.Context, repo Repo, readme string) (projectGuess, error) {
	if len(readme) > readmeLimit {
		readme = readme[:readmeLimit]
	}

	var material strings.Builder
	material.WriteString("Репозиторий: " + repo.FullName + "\n")
	if repo.Description != "" {
		material.WriteString("Описание: " + repo.Description + "\n")
	}
	if readme != "" {
		material.WriteString("\nREADME:\n" + readme)
	}

	raw, err := p.llm.Complete(ctx, Request{
		Step:  "project",
		Model: p.model,
		Messages: []Message{
			{Role: "system", Parts: []Part{TextPart(mustPrompt("project.md"))}},
			{Role: "user", Parts: []Part{TextPart(material.String())}},
		},
		SchemaName: "project_card",
		Schema:     projectSchema,
	})
	if err != nil {
		return projectGuess{}, err
	}

	var guess projectGuess
	if err := json.Unmarshal(raw, &guess); err != nil {
		return projectGuess{}, fmt.Errorf("decode project card: %w", err)
	}
	if strings.TrimSpace(guess.Title) == "" || strings.TrimSpace(guess.Context) == "" {
		return projectGuess{}, errors.New("model returned empty project card")
	}
	return guess, nil
}

// parseProjectRef разбирает аргумент команды: ссылку или owner/repo, следом
// необязательные название и контекст через вертикальную черту.
func parseProjectRef(args string) (projectRef, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return projectRef{}, ErrBadProjectRef
	}

	address, rest, _ := strings.Cut(args, " ")
	address = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(address), "/"), ".git")
	if i := strings.Index(address, "github.com"); i >= 0 {
		address = strings.TrimPrefix(address[i+len("github.com"):], "/")
		address = strings.TrimPrefix(address, ":")
	}
	// Хвост вида /issues или /tree/main отбрасываем: ссылку копируют со страницы.
	parts := strings.Split(strings.Trim(address, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return projectRef{}, ErrBadProjectRef
	}

	ref := projectRef{Owner: parts[0], Repo: strings.TrimSuffix(parts[1], ".git")}
	title, context, _ := strings.Cut(rest, "|")
	ref.Title = strings.TrimSpace(title)
	ref.Context = strings.TrimSpace(context)
	return ref, nil
}

// projectSlugFrom приводит имя репозитория к маске схемы тем же способом, что и
// метка автора: маска и предел callback_data у них общие.
func projectSlugFrom(name, fallback string) string {
	slug := clean(strings.ToLower(name))
	if slug == "" {
		slug = clean(strings.ToLower(fallback))
	}
	if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "-")
	}
	return slug
}
