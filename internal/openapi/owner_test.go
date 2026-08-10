package openapi

import "testing"

func TestOwnerFromStandardFields(t *testing.T) {
	const spec = `{
	  "openapi": "3.0.3",
	  "info": {"title": "T", "version": "1",
	    "contact": {"name": "Команда Профиля", "email": "profile@company.ru",
	                "url": "https://wiki/teams/profile"}},
	  "externalDocs": {"url": "https://gitlab/backend/profile"},
	  "paths": {}
	}`
	o := parse(t, spec).Owner()
	if o.Team != "Команда Профиля" || o.Contact != "profile@company.ru" {
		t.Errorf("контакты из info.contact: %+v", o)
	}
	// externalDocs points at the code and outranks contact.url.
	if o.Repo != "https://gitlab/backend/profile" {
		t.Errorf("репозиторий = %q, ожидался из externalDocs", o.Repo)
	}
}

func TestOwnerFromVendorExtensions(t *testing.T) {
	const spec = `{
	  "openapi": "3.0.3",
	  "info": {"title": "T", "version": "1",
	    "contact": {"name": "устаревшее"},
	    "x-team": "profile", "x-repo": "https://gitlab/profile",
	    "x-chat": "#team-profile", "x-owners": ["Иванов", "Петров"]},
	  "paths": {}
	}`
	o := parse(t, spec).Owner()
	// Extensions say exactly what they mean, so they win over contact.name.
	if o.Team != "profile" || o.Repo != "https://gitlab/profile" {
		t.Errorf("расширения не применились: %+v", o)
	}
	if o.Contact != "#team-profile" || len(o.Owners) != 2 {
		t.Errorf("чат или люди не прочитаны: %+v", o)
	}
}

// The normal case for a long while: nothing declared anywhere.
func TestOwnerAbsentIsNotAnError(t *testing.T) {
	o := parse(t, `{"openapi":"3.0.3","info":{"title":"T","version":"1"},"paths":{}}`).Owner()
	if o.Team != "" || o.Contact != "" || o.Repo != "" || len(o.Owners) != 0 {
		t.Errorf("пустая спека дала владельца: %+v", o)
	}
}
