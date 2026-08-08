package app

import (
	"regexp"
	"testing"
)

func TestAuthorSlug(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

	cases := []struct {
		name string
		user User
		want string
	}{
		{"кириллица без username", User{ID: 1, First: "Иван", Last: "Петров"}, "ivan-petrov"},
		{"username с заглавными", User{ID: 2, First: "Иван", Username: "Ivan_Petrov"}, "ivan-petrov"},
		{"имя без пригодных символов", User{ID: 42, First: "🙂"}, "user-42"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := authorSlug(c.user)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if !valid.MatchString(got) {
				t.Errorf("slug %q is not a valid label value", got)
			}
		})
	}
}
